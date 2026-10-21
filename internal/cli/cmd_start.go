package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/buildinfo"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/node"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

func commonAddr(s string) common.Address { return common.HexToAddress(s) }

func cmdStart(a *App, args []string) error {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	headless := fs.Bool("headless", false, "")
	level := fs.String("log-level", "", "")
	format := fs.String("log-format", "", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if *level != "" {
		cfg.Log.Level = *level
	}
	if *format != "" {
		cfg.Log.Format = *format
	}
	if c, ok := a.running(); ok {
		return usagef("Use `morcore status`, or stop the other node first.", "a node is already running from %s (%s)", a.Home, c.Base)
	}
	w, err := a.unlock(cfg)
	if err != nil {
		return err
	}
	if cfg.Wallet.Address != "" && w.Address().Hex() != cfg.Wallet.Address && os.Getenv("MORCORE_PRIVATE_KEY") == "" {
		a.warn("wallet %s differs from configured %s", w.Address().Hex(), cfg.Wallet.Address)
	}

	dashboard := !*headless && !a.JSON && tui.Interactive()
	var logOut io.Writer = os.Stderr
	logPath := ""
	if dashboard {
		logPath = cfg.Log.File
		if logPath == "" {
			logPath = filepath.Join(a.Home, config.LogDir, "morcore.log")
		}
		if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
			return err
		}
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		logOut = f
	}
	log := logx.New(logx.Options{Level: cfg.Log.Level, Format: cfg.Log.Format, Writer: logOut})
	if dashboard {
		log = logx.New(logx.Options{Level: cfg.Log.Level, Format: "json", Writer: logOut})
	}

	n, err := node.New(cfg, w, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !dashboard {
		log.Info("starting Morpheus Core Node", "version", buildinfo.Version, "network", cfg.NetworkInfo().Label, "role", cfg.Role, "wallet", w.Address().Hex())
		return n.Run(ctx, func() { announce(a, n, log) })
	}

	ready := make(chan struct{})
	done := make(chan error, 1)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { done <- n.Run(runCtx, func() { close(ready) }) }()

	var startErr error
	_ = tui.Spin("Starting node on "+cfg.NetworkInfo().Label, func() error {
		select {
		case <-ready:
			return nil
		case startErr = <-done:
			return startErr
		}
	})
	if startErr != nil {
		return startErr
	}
	warnFunding(a, n)
	time.Sleep(600 * time.Millisecond)

	dashErr := tui.RunDashboard(func() node.Status { return n.Status(runCtx) }, logPath)
	cancel()
	msg := "Shutting down"
	if cfg.Sessions.CloseOnShutdown && len(n.Status(ctx).Sessions) > 0 {
		msg = "Closing sessions (returning unused stake) and shutting down"
	}
	var runErr error
	_ = tui.Spin(msg, func() error {
		runErr = <-done
		return runErr
	})
	if dashErr != nil {
		return dashErr
	}
	return runErr
}

// announce prints connection details for headless runs.
func announce(a *App, n *node.Node, log *slog.Logger) {
	if n.GatewayAddr != "" {
		log.Info("ready", "agent_api", "http://"+n.GatewayAddr+"/v1", "keys", n.Keys.Count())
		if n.Keys.Count() == 0 && n.Sessions != nil {
			log.Warn("no API keys yet; create one with `morcore keys create <name>`")
		}
	}
	if n.ProviderAddr != "" {
		log.Info("provider ready", "listen", n.ProviderAddr, "public", n.Cfg.Provider.PublicEndpoint)
	}
	warnFunding(a, n)
}

func warnFunding(a *App, n *node.Node) {
	ctx, cancel := timeout(10 * time.Second)
	defer cancel()
	eth, err1 := n.Client.BalanceAt(ctx, n.Wallet.Address())
	mor, err2 := n.K.MORBalance(ctx, n.Wallet.Address())
	if err1 != nil || err2 != nil {
		return
	}
	if eth.Sign() == 0 {
		a.warn("wallet %s has no ETH on %s for gas — fund it to open sessions or register", n.Wallet.Address().Hex(), n.Cfg.NetworkInfo().Label)
	}
	if n.Sessions != nil && mor.Cmp(big.NewInt(0)) == 0 {
		a.warn("wallet has no MOR; sessions escrow MOR (%s)", units.MOR(mor))
	}
}

func cmdVersion(a *App, _ []string) error {
	if a.JSON {
		return a.printJSON(map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit, "date": buildinfo.Date})
	}
	fmt.Fprintln(a.Out, buildinfo.String())
	return nil
}
