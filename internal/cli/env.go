package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"golang.org/x/term"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/node"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

func (a *App) config() (*config.Config, error) {
	cfg, err := config.Load(a.Home)
	if errors.Is(err, config.ErrNotInitialized) {
		return nil, usagef("Run `morcore init` (or just `morcore`) to set up.", "no node configured in %s", a.Home)
	}
	return cfg, err
}

// unlock returns the node wallet, resolving the password from
// $MORCORE_PASSWORD, the configured password file, or an interactive prompt.
// $MORCORE_PRIVATE_KEY overrides the keystore entirely (containers).
func (a *App) unlock(cfg *config.Config) (*wallet.Wallet, error) {
	if pk := os.Getenv("MORCORE_PRIVATE_KEY"); pk != "" {
		return wallet.FromHex(pk)
	}
	path := cfg.Path(cfg.Wallet.Keystore)
	if _, err := os.Stat(path); err != nil {
		return nil, usagef("Run `morcore init` to create or import a wallet.", "wallet keystore not found at %s", path)
	}
	try := func(pw string) (*wallet.Wallet, error) {
		var w *wallet.Wallet
		err := tui.Spin("Unlocking wallet", func() error {
			var err error
			w, err = wallet.LoadKeystore(path, pw)
			return err
		})
		return w, err
	}
	if pw := os.Getenv("MORCORE_PASSWORD"); pw != "" {
		return try(pw)
	}
	if cfg.Wallet.PasswordFile != "" {
		b, err := os.ReadFile(cfg.Path(cfg.Wallet.PasswordFile))
		if err == nil {
			return try(strings.TrimRight(string(b), "\r\n"))
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, usagef("Set MORCORE_PASSWORD or configure wallet.password_file.", "wallet is locked and there is no terminal to ask for the password")
	}
	for attempt := 0; attempt < 3; attempt++ {
		fmt.Fprintf(a.Err, "%s Wallet password for %s: ", tui.Good.Render("?"), tui.ShortAddr(cfg.Wallet.Address))
		pw, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(a.Err)
		if err != nil {
			return nil, err
		}
		w, err := try(string(pw))
		if errors.Is(err, wallet.ErrWrongPassword) {
			continue
		}
		return w, err
	}
	return nil, wallet.ErrWrongPassword
}

// running returns an admin client when a node is running from this home.
func (a *App) running() (*Client, bool) {
	li, ok := node.ReadLock(a.Home)
	if !ok || !node.Alive(li.Gateway) {
		return nil, false
	}
	tok, err := os.ReadFile(filepath.Join(a.Home, config.AdminTokenFile))
	if err != nil {
		return nil, false
	}
	return NewClient("http://"+li.Gateway, strings.TrimSpace(string(tok))), true
}

// readOnly connects to the chain without unlocking the wallet.
type readOnly struct {
	cfg    *config.Config
	client *chain.Client
	k      *chain.Contracts
	market *marketplace.Market
}

func (a *App) readOnly(cfg *config.Config) (*readOnly, error) {
	nw := cfg.NetworkInfo()
	c, err := chain.NewClient(cfg.Chain.RPCURLs, logx.Discard())
	if err != nil {
		return nil, err
	}
	k := chain.NewContracts(c, common.HexToAddress(nw.DiamondAddress), common.HexToAddress(nw.MORTokenAddress), big.NewInt(nw.ChainID))
	return &readOnly{cfg: cfg, client: c, k: k, market: marketplace.New(k, cfg.Sessions.ProviderAllowlist, cfg.Sessions.ProviderDenylist, logx.Discard())}, nil
}

func timeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func (a *App) printJSON(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func (a *App) table() *tabwriter.Writer {
	return tabwriter.NewWriter(a.Out, 0, 0, 2, ' ', 0)
}

func header(w io.Writer, cols ...string) {
	for i, c := range cols {
		if i > 0 {
			fmt.Fprint(w, "\t")
		}
		fmt.Fprint(w, tui.Subtle.Render(c))
	}
	fmt.Fprintln(w)
}

func (a *App) ok(format string, args ...any) {
	fmt.Fprintf(a.Out, "%s %s\n", tui.CheckMk, fmt.Sprintf(format, args...))
}

func (a *App) warn(format string, args ...any) {
	fmt.Fprintf(a.Err, "%s %s\n", tui.WarnMk, fmt.Sprintf(format, args...))
}

func (a *App) info(format string, args ...any) {
	fmt.Fprintf(a.Out, "%s\n", fmt.Sprintf(format, args...))
}

func (a *App) confirm(question string) bool {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false
	}
	fmt.Fprintf(a.Err, "%s %s %s ", tui.Good.Render("?"), question, tui.Subtle.Render("[y/N]"))
	var resp string
	_, _ = fmt.Fscanln(os.Stdin, &resp)
	resp = strings.ToLower(strings.TrimSpace(resp))
	return resp == "y" || resp == "yes"
}

func shortHash(h string) string {
	if len(h) > 14 {
		return h[:10] + "…" + h[len(h)-4:]
	}
	return h
}
