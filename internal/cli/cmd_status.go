package cli

import (
	"context"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/node"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

func cmdStatus(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	c, ok := a.running()
	if !ok {
		if a.JSON {
			return a.printJSON(map[string]any{"running": false, "home": a.Home, "address": cfg.Wallet.Address, "network": cfg.NetworkInfo().Label})
		}
		a.info("%s Node is %s  %s", tui.Subtle.Render("○"), tui.Warning.Render("not running"), tui.Subtle.Render("("+a.Home+")"))
		a.info("  %s %s on %s", tui.Subtle.Render("wallet"), cfg.Wallet.Address, cfg.NetworkInfo().Label)
		a.info("  Start it with %s", tui.Code.Render("morcore"))
		return nil
	}
	var st node.Status
	if err := c.Do("GET", "/admin/status", nil, &st); err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(st)
	}
	parse := func(s string) *big.Int {
		v, _ := new(big.Int).SetString(s, 10)
		return v
	}
	t := a.table()
	row := func(label, val string) { fmt.Fprintf(t, "%s\t%s\n", tui.Subtle.Render(label), val) }
	row("Node", tui.Good.Render("● running")+tui.Subtle.Render(fmt.Sprintf("  %s · up %s", st.Version, time.Since(st.StartedAt).Truncate(time.Second))))
	row("Network", fmt.Sprintf("%s (chain %d) · block %d", st.Network, st.ChainID, st.Block))
	row("Role", st.Role)
	row("Wallet", st.Address)
	row("Balances", units.MOR(parse(st.MOR))+" · "+units.ETH(parse(st.ETH)))
	switch {
	case st.Gateway != "" && st.Role != config.RoleProvider:
		row("Agent API", tui.Code.Render("http://"+st.Gateway+"/v1")+tui.Subtle.Render(fmt.Sprintf("  %d active keys", st.Keys)))
		row("Traffic", fmt.Sprintf("%d requests (%d errors) · %s tokens in / %s out", st.Totals.Requests, st.Totals.Errors,
			humanTokens(st.Totals.InputTokens), humanTokens(st.Totals.OutputTokens)))
		row("Sessions", fmt.Sprintf("%d open · %s escrowed", len(st.Sessions), units.MOR(parse(st.LockedStake))))
	case st.Gateway != "":
		row("Admin API", "http://"+st.Gateway)
	}
	if st.Provider != nil {
		row("Provider", fmt.Sprintf("%s → %s · %d models · %d live sessions · %d requests served",
			st.ProviderListen, st.PublicEndpoint, st.Provider.Models, st.Provider.ActiveSessions, st.Provider.Requests))
	}
	row("RPC", st.RPC)
	return t.Flush()
}

func cmdConfig(a *App, args []string) error {
	path := filepath.Join(a.Home, config.FileName)
	if len(args) == 0 || args[0] == "path" {
		a.info("%s", path)
		return nil
	}
	if args[0] != "show" {
		return usagef("Use `morcore config` or `morcore config show`.", "unknown config subcommand %q", args[0])
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	redacted := *cfg
	redacted.Provider.Models = nil
	for _, m := range cfg.Provider.Models {
		if m.APIKey != "" {
			m.APIKey = "•••"
		}
		redacted.Provider.Models = append(redacted.Provider.Models, m)
	}
	for i, u := range redacted.Chain.RPCURLs {
		redacted.Chain.RPCURLs[i] = chain.Redact(u)
	}
	if a.JSON {
		return a.printJSON(redacted)
	}
	fmt.Fprintln(a.Out, tui.Subtle.Render("# "+path))
	return toml.NewEncoder(a.Out).Encode(redacted)
}

type check struct {
	name   string
	status int // 0 ok, 1 warn, 2 fail
	detail string
}

func cmdDoctor(a *App, args []string) error {
	var checks []check
	add := func(name string, status int, detail string, args ...any) {
		checks = append(checks, check{name, status, fmt.Sprintf(detail, args...)})
	}
	defer func() {
		if a.JSON {
			_ = a.printJSON(checks)
			return
		}
		for _, c := range checks {
			mark := tui.CheckMk
			switch c.status {
			case 1:
				mark = tui.WarnMk
			case 2:
				mark = tui.CrossMk
			}
			fmt.Fprintf(a.Out, "%s %-22s %s\n", mark, c.name, tui.Subtle.Render(c.detail))
		}
	}()

	cfg, err := config.Load(a.Home)
	if err != nil {
		add("config", 2, "%v", err)
		return nil
	}
	add("config", 0, "%s (%s, %s)", filepath.Join(a.Home, config.FileName), cfg.Role, cfg.NetworkInfo().Label)

	addr, err := wallet.KeystoreAddress(cfg.Path(cfg.Wallet.Keystore))
	switch {
	case os.Getenv("MORCORE_PRIVATE_KEY") != "":
		add("wallet", 0, "using $MORCORE_PRIVATE_KEY")
	case err != nil:
		add("wallet", 2, "keystore missing: %v", err)
	default:
		unlock := "password prompted at start"
		if os.Getenv("MORCORE_PASSWORD") != "" || cfg.Wallet.PasswordFile != "" {
			unlock = "unlocks automatically"
		}
		add("wallet", 0, "%s · %s", addr.Hex(), unlock)
	}

	nw := cfg.NetworkInfo()
	ro, err := a.readOnly(cfg)
	if err != nil {
		add("rpc", 2, "%v", err)
		return nil
	}
	ctx, cancel := timeout(30 * time.Second)
	defer cancel()
	start := time.Now()
	id, err := ro.client.ChainID(ctx)
	switch {
	case err != nil:
		add("rpc", 2, "%s unreachable: %v", ro.client.Endpoint(), err)
		return nil
	case id.Int64() != nw.ChainID:
		add("rpc", 2, "endpoint is chain %d, expected %d", id.Int64(), nw.ChainID)
		return nil
	default:
		st := 0
		note := ""
		if len(cfg.Chain.RPCURLs) > 1 && strings.Contains(strings.Join(cfg.Chain.RPCURLs, ","), "base.org") {
			st, note = 1, " · public endpoints are rate-limited; set your own for production"
		}
		add("rpc", st, "%s · %dms%s", ro.client.Endpoint(), time.Since(start).Milliseconds(), note)
	}
	if fee, err := ro.k.BidFee(ctx); err != nil {
		add("marketplace", 2, "Diamond %s not readable: %v", tui.ShortAddr(nw.DiamondAddress), err)
	} else {
		add("marketplace", 0, "Diamond %s · bid fee %s", tui.ShortAddr(nw.DiamondAddress), units.MOR(fee))
	}

	wa := commonAddr(cfg.Wallet.Address)
	if eth, err := ro.client.BalanceAt(ctx, wa); err == nil {
		st := 0
		if eth.Sign() == 0 {
			st = 2
		}
		add("gas (ETH)", st, "%s", units.ETH(eth))
	}
	if mor, err := ro.k.MORBalance(ctx, wa); err == nil {
		st := 0
		if mor.Sign() == 0 && cfg.ConsumerEnabled() {
			st = 2
		}
		add("MOR", st, "%s", units.MOR(mor))
	}

	if c, ok := a.running(); ok {
		add("node", 0, "running at %s", c.Base)
	} else {
		add("node", 1, "not running (start with `morcore`)")
		if cfg.ConsumerEnabled() {
			if ln, err := net.Listen("tcp", cfg.Gateway.Listen); err != nil {
				add("agent API port", 2, "%s unavailable: %v", cfg.Gateway.Listen, err)
			} else {
				ln.Close()
				add("agent API port", 0, "%s free", cfg.Gateway.Listen)
			}
		}
	}
	if cfg.ConsumerEnabled() {
		if ks, err := apikeys.Open(cfg.Path(config.APIKeysFile), logx.Discard()); err == nil {
			n := ks.Count()
			st := 0
			if n == 0 {
				st = 1
			}
			add("API keys", st, "%d active", n)
		}
	}

	if cfg.ProviderEnabled() {
		doctorProvider(ctx, cfg, ro, add)
	}
	return nil
}

func doctorProvider(ctx context.Context, cfg *config.Config, ro *readOnly, add func(string, int, string, ...any)) {
	p, err := ro.k.GetProvider(ctx, commonAddr(cfg.Wallet.Address))
	switch {
	case err != nil:
		add("provider", 2, "%v", err)
	case p.CreatedAt == nil || p.CreatedAt.Sign() == 0 || p.IsDeleted:
		add("provider", 2, "not registered on-chain (morcore provider register)")
	case p.Endpoint != cfg.Provider.PublicEndpoint:
		add("provider", 1, "on-chain endpoint %s ≠ config %s", p.Endpoint, cfg.Provider.PublicEndpoint)
	default:
		add("provider", 0, "registered at %s · stake %s", p.Endpoint, units.MOR(p.Stake))
	}
	if len(cfg.Provider.Models) == 0 {
		add("models", 2, "no [[provider.models]] configured")
	}
	client := http.Client{Timeout: 8 * time.Second}
	for _, m := range cfg.Provider.Models {
		url := strings.TrimSuffix(m.APIURL, "/chat/completions") + "/models"
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if m.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+m.APIKey)
		}
		resp, err := client.Do(req)
		if err != nil {
			add("backend "+m.Name, 2, "%s unreachable: %v", m.APIURL, err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode >= 500 {
			add("backend "+m.Name, 2, "HTTP %d from %s", resp.StatusCode, url)
		} else {
			add("backend "+m.Name, 0, "%s reachable", m.APIURL)
		}
	}
	ids, err := ro.k.ProviderActiveBidIDs(ctx, commonAddr(cfg.Wallet.Address))
	if err == nil {
		st := 0
		if len(ids) == 0 {
			st = 1
		}
		add("bids", st, "%d active", len(ids))
	}
}
