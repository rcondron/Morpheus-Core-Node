package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/node"
	"github.com/rcondron/Morpheus-Core-Node/internal/store"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

const passwordFile = "wallet.password"

// cmdDefault is `morcore` with no arguments: set up on first run, otherwise start.
func cmdDefault(a *App) error {
	if !config.Exists(a.Home) {
		if !tui.Interactive() {
			return usagef("Run `morcore init --help` for non-interactive setup.", "no node configured in %s", a.Home)
		}
		return cmdInit(a, nil)
	}
	return cmdStart(a, nil)
}

func cmdInit(a *App, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "")
	force := fs.Bool("force", false, "")
	network := fs.String("network", "", "")
	role := fs.String("role", "", "")
	rpc := fs.String("rpc", "", "")
	keyEnv := fs.String("key-env", "", "")
	mnemonicEnv := fs.String("mnemonic-env", "", "")
	index := fs.Int("index", 0, "")
	passwordEnv := fs.String("password-env", "", "")
	autoUnlock := fs.Bool("auto-unlock", false, "")
	apiKey := fs.String("api-key", "", "")
	listen := fs.String("listen", "", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	if err := os.MkdirAll(a.Home, 0o700); err != nil {
		return err
	}

	var existing *config.Config
	if config.Exists(a.Home) {
		cfg, err := config.Load(a.Home)
		if err == nil {
			existing = cfg
		}
		if *yes && !*force {
			return usagef("Pass --force to overwrite it.", "a node is already configured in %s", a.Home)
		}
	}

	if *yes || !tui.Interactive() {
		if !*yes {
			return usagef("Run in a terminal, or pass --yes with flags (see `morcore init --help`).", "setup needs a terminal")
		}
		return initNonInteractive(a, nonInteractive{
			network: *network, role: *role, rpc: *rpc, keyEnv: *keyEnv, mnemonicEnv: *mnemonicEnv, index: *index,
			passwordEnv: *passwordEnv, autoUnlock: *autoUnlock, apiKey: *apiKey, listen: *listen,
		})
	}

	opt := tui.WizardOptions{Home: a.Home, Existing: existing}
	if existing != nil {
		if addr, err := wallet.KeystoreAddress(existing.Path(existing.Wallet.Keystore)); err == nil {
			opt.ExistingAddress = addr.Hex()
		}
	}
	setup, err := tui.RunWizard(context.Background(), opt)
	if err != nil {
		return err
	}
	secret, err := persist(a, setup)
	if err != nil {
		return err
	}
	a.printReady(setup.Config, secret)
	if setup.StartNow {
		if setup.Password != "" {
			os.Setenv("MORCORE_PASSWORD", setup.Password)
			defer os.Unsetenv("MORCORE_PASSWORD")
		}
		return cmdStart(a, nil)
	}
	fmt.Fprintln(a.Out, tui.Subtle.Render("Start the node any time with: ")+tui.Code.Render("morcore"))
	return nil
}

// persist writes the wallet, config, admin token and first API key.
func persist(a *App, s *tui.Setup) (string, error) {
	cfg := s.Config
	cfg.Home = a.Home
	if cfg.Wallet.Keystore == "" {
		cfg.Wallet.Keystore = config.KeystoreFile
	}
	if !s.KeepWallet {
		ks := cfg.Path(cfg.Wallet.Keystore)
		if store.Exists(ks) {
			backup := ks + ".bak"
			if err := os.Rename(ks, backup); err != nil {
				return "", err
			}
			a.warn("previous wallet keystore moved to %s", backup)
		}
		if err := tui.Spin("Encrypting wallet", func() error { return s.Wallet.SaveKeystore(ks, s.Password) }); err != nil {
			return "", err
		}
		cfg.Wallet.Address = s.Wallet.Address().Hex()
	}
	if s.AutoUnlock {
		if err := store.WriteFile(filepath.Join(a.Home, passwordFile), []byte(s.Password), 0o600); err != nil {
			return "", err
		}
		cfg.Wallet.PasswordFile = passwordFile
	} else if cfg.Wallet.PasswordFile == passwordFile {
		_ = os.Remove(filepath.Join(a.Home, passwordFile))
		cfg.Wallet.PasswordFile = ""
	}
	if err := cfg.Validate(); err != nil {
		return "", err
	}
	if err := cfg.Save(a.Home); err != nil {
		return "", err
	}
	if _, err := node.EnsureAdminToken(a.Home); err != nil {
		return "", err
	}
	if s.KeyName == "" {
		return "", nil
	}
	ks, err := apikeys.Open(cfg.Path(config.APIKeysFile), logx.Discard())
	if err != nil {
		return "", err
	}
	secret, _, err := ks.Create(apikeys.CreateOptions{Name: s.KeyName})
	if err != nil {
		if strings.Contains(err.Error(), "already exists") {
			a.warn("an API key named %q already exists; keeping it", s.KeyName)
			return "", nil
		}
		return "", err
	}
	return secret, nil
}

type nonInteractive struct {
	network, role, rpc, keyEnv, mnemonicEnv, passwordEnv, apiKey, listen string
	index                                                                int
	autoUnlock                                                           bool
}

func initNonInteractive(a *App, o nonInteractive) error {
	if o.network == "" {
		o.network = "mainnet"
	}
	if _, err := config.LookupNetwork(o.network); err != nil {
		return usagef("", "%v", err)
	}
	cfg := config.Default(o.network)
	if o.role != "" {
		cfg.Role = o.role
	}
	if o.rpc != "" {
		cfg.Chain.RPCURLs = strings.Split(o.rpc, ",")
	}
	if o.listen != "" {
		cfg.Gateway.Listen = o.listen
	}
	pwVar := o.passwordEnv
	if pwVar == "" {
		pwVar = "MORCORE_PASSWORD"
	}
	password := os.Getenv(pwVar)
	if len(password) < 8 {
		return usagef("Export a password of 8+ characters and pass --password-env.", "$%s is empty or shorter than 8 characters", pwVar)
	}
	var w *wallet.Wallet
	var err error
	switch {
	case o.keyEnv != "":
		w, err = wallet.FromHex(os.Getenv(o.keyEnv))
	case o.mnemonicEnv != "":
		w, err = wallet.FromMnemonic(os.Getenv(o.mnemonicEnv), "", o.index)
	default:
		var phrase string
		if phrase, err = wallet.NewMnemonic(false); err == nil {
			w, err = wallet.FromMnemonic(phrase, "", 0)
			if err == nil {
				fmt.Fprintln(a.Err, tui.Warning.Render("New wallet recovery phrase (store it safely, shown once):"))
				fmt.Fprintln(a.Err, phrase)
			}
		}
	}
	if err != nil {
		return err
	}
	secret, err := persist(a, &tui.Setup{Config: cfg, Wallet: w, Password: password, AutoUnlock: o.autoUnlock, KeyName: o.apiKey})
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(map[string]any{"address": w.Address().Hex(), "home": a.Home, "apiKey": secret, "gateway": cfg.Gateway.Listen})
	}
	a.printReady(cfg, secret)
	return nil
}

// printReady explains what to do next after setup.
func (a *App) printReady(cfg *config.Config, secret string) {
	nw := cfg.NetworkInfo()
	w := a.Out
	fmt.Fprintln(w)
	var lines []string
	lines = append(lines, tui.Good.Render("✓ Node configured")+tui.Subtle.Render("  ("+a.Home+")"), "")
	lines = append(lines, tui.Subtle.Render("Wallet  ")+tui.Code.Render(cfg.Wallet.Address)+tui.Subtle.Render("  on "+nw.Label))

	ro, err := a.readOnly(cfg)
	if err == nil {
		ctx, cancel := timeout(8 * time.Second)
		eth, err1 := ro.client.BalanceAt(ctx, commonAddr(cfg.Wallet.Address))
		mor, err2 := ro.k.MORBalance(ctx, commonAddr(cfg.Wallet.Address))
		cancel()
		if err1 == nil && err2 == nil {
			lines = append(lines, tui.Subtle.Render("Balance ")+units.MOR(mor)+" · "+units.ETH(eth))
			if mor.Sign() == 0 || eth.Sign() == 0 {
				lines = append(lines, "", tui.Warning.Render("Fund this address on "+nw.Label+" before using the marketplace:"),
					"  • ETH pays transaction gas (a few dollars lasts a long time)",
					"  • MOR is escrowed while sessions are open and returned when they close")
				if cfg.Network == "testnet" {
					lines = append(lines, "  • Testnet: get Sepolia ETH from a BASE faucet and test MOR from the Morpheus team")
				}
			}
		}
	}
	if cfg.ConsumerEnabled() {
		lines = append(lines, "", tui.Subtle.Render("Agent API ")+tui.Code.Render("http://"+cfg.Gateway.Listen+"/v1"))
		if secret != "" {
			lines = append(lines, tui.Subtle.Render("API key   ")+tui.Strong.Render(secret), tui.Warning.Render("          copy it now — it is not shown again"))
		}
	}
	if cfg.ProviderEnabled() {
		lines = append(lines, "", tui.Subtle.Render("Provider  ")+cfg.Provider.Listen+" → "+tui.Code.Render(cfg.Provider.PublicEndpoint),
			tui.Subtle.Render("Next: ")+tui.Code.Render("morcore provider register")+tui.Subtle.Render(" then ")+tui.Code.Render("morcore provider bid <model> --price <MOR/hour>"))
	}
	fmt.Fprintln(w, tui.Box.Render(strings.Join(lines, "\n")))
	if cfg.ConsumerEnabled() {
		key := secret
		if key == "" {
			key = "sk-mor-…"
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, tui.Heading.Render("Point any OpenAI-compatible agent at it:"))
		fmt.Fprintln(w, tui.Code.Render("  export OPENAI_BASE_URL=http://"+cfg.Gateway.Listen+"/v1"))
		fmt.Fprintln(w, tui.Code.Render("  export OPENAI_API_KEY="+key))
		fmt.Fprintln(w, tui.Subtle.Render("  morcore models        # see what's available"))
		fmt.Fprintln(w, tui.Subtle.Render("  morcore chat          # try a model right in the terminal"))
	}
	fmt.Fprintln(w)
}

var errNoConsumer = errors.New("this node is configured as provider-only")
