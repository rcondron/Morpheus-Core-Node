package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/gateway"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

var hour = big.NewInt(3600)

func perHour(perSecondWei string) string {
	v, ok := new(big.Int).SetString(perSecondWei, 10)
	if !ok {
		return "-"
	}
	return units.FormatToken(new(big.Int).Mul(v, hour), 4) + " MOR/h"
}

func cmdModels(a *App, args []string) error {
	fs := flag.NewFlagSet("models", flag.ContinueOnError)
	all := fs.Bool("all", false, "")
	refresh := fs.Bool("refresh", false, "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	var models []gateway.ModelView
	if c, ok := a.running(); ok {
		var out struct{ Models []gateway.ModelView }
		path := "/admin/models"
		if *refresh {
			path += "?refresh=1"
		}
		if err := c.Do("GET", path, nil, &out); err != nil {
			return err
		}
		models = out.Models
	} else {
		ro, err := a.readOnly(cfg)
		if err != nil {
			return err
		}
		ctx, cancel := timeout(90 * time.Second)
		defer cancel()
		err = tui.Spin("Reading the marketplace on "+cfg.NetworkInfo().Label, func() error {
			ms, err := ro.market.Models(ctx)
			for _, m := range ms {
				v := gateway.ModelView{ID: m.ID.Hex(), Name: m.Name, Alias: m.Alias, Tags: m.Tags, Bids: m.Bids}
				if m.MinPrice != nil {
					v.MinPrice, v.MaxPrice = m.MinPrice.String(), m.MaxPrice.String()
				}
				models = append(models, v)
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	if !*all {
		kept := models[:0]
		for _, m := range models {
			if m.Bids > 0 && !marketplace.IsStorageTags(m.Tags) { // storage: see morcore storage providers
				kept = append(kept, m)
			}
		}
		models = kept
	}
	sort.SliceStable(models, func(i, j int) bool { return models[i].Bids > models[j].Bids })
	if a.JSON {
		return a.printJSON(models)
	}
	if len(models) == 0 {
		a.info("No models with active offers right now. Try %s.", tui.Code.Render("morcore models --all"))
		return nil
	}
	t := a.table()
	header(t, "MODEL", "OFFERS", "FROM", "TAGS", "ID")
	for _, m := range models {
		price := "-"
		if m.MinPrice != "" {
			price = perHour(m.MinPrice)
		}
		fmt.Fprintf(t, "%s\t%d\t%s\t%s\t%s\n", tui.Strong.Render(m.Alias), m.Bids, price, strings.Join(m.Tags, ","), tui.Subtle.Render(shortHash(m.ID)))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Subtle.Render("Use the MODEL name as \"model\" in API calls. Prices are per hour of session time; stake is escrowed and unused stake returns on close."))
	return nil
}

func cmdBids(a *App, args []string) error {
	fs := flag.NewFlagSet("bids", flag.ContinueOnError)
	dur := fs.Duration("duration", time.Hour, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("Example: morcore bids llama-3.3-70b", "bids needs a model name or id")
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	var res struct {
		Model    string
		ModelID  string
		Duration string
		Bids     []gateway.BidView
	}
	if c, ok := a.running(); ok {
		if err := c.Do("GET", "/admin/models/"+pos[0]+"/bids?duration="+dur.String(), nil, &res); err != nil {
			return err
		}
	} else {
		ro, err := a.readOnly(cfg)
		if err != nil {
			return err
		}
		ctx, cancel := timeout(90 * time.Second)
		defer cancel()
		err = tui.Spin("Ranking provider offers", func() error {
			m, err := ro.market.Resolve(ctx, pos[0])
			if err != nil {
				return err
			}
			res.Model, res.ModelID = m.Alias, m.ID.Hex()
			bids, err := ro.market.RatedBids(ctx, m.ID)
			for _, b := range bids {
				v := gateway.BidView{ID: b.Bid.ID.Hex(), Provider: b.Bid.Provider.Hex(), Endpoint: b.Provider.Endpoint, Price: b.Bid.PricePerSecond.String(), Score: b.Score}
				if st, err := ro.market.EstimateStake(ctx, b.Bid.PricePerSecond, *dur); err == nil {
					v.Stake = st.String()
				}
				res.Bids = append(res.Bids, v)
			}
			return err
		})
		if err != nil {
			return err
		}
	}
	if a.JSON {
		return a.printJSON(res)
	}
	fmt.Fprintf(a.Out, "%s %s\n\n", tui.Heading.Render(res.Model), tui.Subtle.Render(res.ModelID))
	if len(res.Bids) == 0 {
		a.info("No active offers.")
		return nil
	}
	t := a.table()
	header(t, "#", "PROVIDER", "ENDPOINT", "PRICE", "SCORE", "STAKE FOR "+strings.TrimSuffix(dur.String(), "0m0s"), "BID")
	for i, b := range res.Bids {
		stake := "-"
		if v, ok := new(big.Int).SetString(b.Stake, 10); ok {
			stake = units.MOR(v)
		}
		fmt.Fprintf(t, "%d\t%s\t%s\t%s\t%.2f\t%s\t%s\n", i+1, tui.ShortAddr(b.Provider), b.Endpoint, perHour(b.Price), b.Score, stake, tui.Subtle.Render(shortHash(b.ID)))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(a.Out)
	fmt.Fprintln(a.Out, tui.Subtle.Render("Ranked like the reference node: throughput, latency, uptime, success rate and stake, divided by price."))
	return nil
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

func sessionsList(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if c, ok := a.running(); ok {
		var out struct {
			Sessions    []consumer.Snapshot
			Pools       []consumer.PoolInfo
			LockedStake string
		}
		if err := c.Do("GET", "/admin/sessions", nil, &out); err != nil {
			return err
		}
		if a.JSON {
			return a.printJSON(out)
		}
		if len(out.Sessions) == 0 {
			a.info("No open sessions. They open automatically when an agent calls a model (or: %s).", tui.Code.Render("morcore sessions open <model>"))
			return nil
		}
		t := a.table()
		header(t, "SESSION", "MODEL", "PROVIDER", "STATE", "ENDS IN", "REQUESTS", "TOKENS", "STAKE")
		for _, s := range out.Sessions {
			state := s.State
			if s.Busy {
				state += " (busy)"
			}
			stake, _ := new(big.Int).SetString(s.Stake, 10)
			fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", shortHash(s.ID), tui.Strong.Render(s.Model), tui.ShortAddr(s.Provider), state,
				time.Until(s.EndsAt).Truncate(time.Minute), s.Requests, humanTokens(s.TokensIn+s.TokensOut), units.MOR(stake))
		}
		_ = t.Flush()
		locked, _ := new(big.Int).SetString(out.LockedStake, 10)
		fmt.Fprintf(a.Out, "\n%s %s\n", tui.Subtle.Render("Escrowed:"), units.MOR(locked))
		return nil
	}

	// Node not running: show the wallet's recent on-chain sessions.
	ro, err := a.readOnly(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := timeout(60 * time.Second)
	defer cancel()
	var sessions []*chain.Session
	err = tui.Spin("Reading your recent sessions from chain", func() error {
		ids, err := ro.k.RecentUserSessionIDs(ctx, commonAddr(cfg.Wallet.Address), 20)
		if err != nil {
			return err
		}
		sessions, err = ro.k.GetSessions(ctx, ids)
		return err
	})
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(sessions)
	}
	if len(sessions) == 0 {
		a.info("No sessions yet for %s.", cfg.Wallet.Address)
		return nil
	}
	t := a.table()
	header(t, "SESSION", "OPENED", "ENDS", "STATE", "STAKE")
	open := 0
	for _, s := range sessions {
		state := tui.Subtle.Render("closed")
		if !s.Closed() {
			open++
			state = tui.Warning.Render("open")
			if time.Now().After(s.EndsAtTime()) {
				state = tui.Danger.Render("ended, not closed")
			}
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", shortHash(s.ID.Hex()), s.OpenedAtTime().Local().Format("Jan 02 15:04"),
			s.EndsAtTime().Local().Format("Jan 02 15:04"), state, units.MOR(s.Stake))
	}
	_ = t.Flush()
	if open > 0 {
		fmt.Fprintf(a.Out, "\n%s %s\n", tui.Subtle.Render("Node is not running. Stake stays escrowed until sessions close:"), tui.Code.Render("morcore sessions close all"))
	}
	return nil
}

func sessionsOpen(a *App, args []string) error {
	fs := flag.NewFlagSet("sessions open", flag.ContinueOnError)
	bid := fs.String("bid", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("Example: morcore sessions open llama-3.3-70b", "sessions open needs a model")
	}
	c, ok := a.running()
	if !ok {
		return usagef("Start it with `morcore start` (sessions are only useful while the node routes traffic).", "the node is not running")
	}
	var snap consumer.Snapshot
	err = tui.Spin("Opening a session for "+pos[0]+" (provider handshake + on-chain escrow)", func() error {
		return c.Do("POST", "/admin/sessions", map[string]string{"model": pos[0], "bid": *bid}, &snap)
	})
	if err != nil {
		return err
	}
	if a.JSON {
		return a.printJSON(snap)
	}
	stake, _ := new(big.Int).SetString(snap.Stake, 10)
	a.ok("session %s open with %s until %s (escrowed %s)", shortHash(snap.ID), tui.ShortAddr(snap.Provider), snap.EndsAt.Local().Format(time.Kitchen), units.MOR(stake))
	return nil
}

func sessionsClose(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("Example: morcore sessions close all", "sessions close needs a session id or \"all\"")
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if c, ok := a.running(); ok {
		var ids []string
		if args[0] == "all" {
			var out struct{ Sessions []consumer.Snapshot }
			if err := c.Do("GET", "/admin/sessions", nil, &out); err != nil {
				return err
			}
			for _, s := range out.Sessions {
				ids = append(ids, s.ID)
			}
		} else {
			ids = []string{args[0]}
		}
		for _, id := range ids {
			if err := tui.Spin("Closing "+shortHash(id), func() error { return c.Do("DELETE", "/admin/sessions/"+id, nil, nil) }); err != nil {
				return err
			}
		}
		if len(ids) == 0 {
			a.info("No open sessions.")
		}
		return nil
	}

	// Offline: close directly with the wallet.
	w, err := a.unlock(cfg)
	if err != nil {
		return err
	}
	ro, err := a.readOnly(cfg)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	tx := chain.NewTxSender(ro.k, w, cfg.Chain.LegacyTx, log)
	mgr := consumer.New(consumer.Options{Duration: time.Hour}, w, ro.k, tx, ro.market, morrpc.DefaultClient(), events.New(), log)
	ctx, cancel := timeout(10 * time.Minute)
	defer cancel()
	var ids []common.Hash
	if args[0] == "all" {
		recent, err := ro.k.RecentUserSessionIDs(ctx, w.Address(), 50)
		if err != nil {
			return err
		}
		sessions, err := ro.k.GetSessions(ctx, recent)
		if err != nil {
			return err
		}
		for _, s := range sessions {
			if !s.Closed() && s.Exists() {
				ids = append(ids, s.ID)
			}
		}
	} else {
		ids = []common.Hash{common.HexToHash(args[0])}
	}
	if len(ids) == 0 {
		a.info("No open sessions.")
		return nil
	}
	var failed []error
	for _, id := range ids {
		if err := tui.Spin("Closing "+shortHash(id.Hex()), func() error { return mgr.Close(ctx, id) }); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// ---------------------------------------------------------------------------
// Wallet
// ---------------------------------------------------------------------------

func walletShow(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	ro, err := a.readOnly(cfg)
	if err != nil {
		return err
	}
	addr := commonAddr(cfg.Wallet.Address)
	ctx, cancel := timeout(30 * time.Second)
	defer cancel()
	var eth, mor, avail, hold *big.Int
	err = tui.Spin("Reading balances", func() error {
		var err error
		if eth, err = ro.client.BalanceAt(ctx, addr); err != nil {
			return err
		}
		if mor, err = ro.k.MORBalance(ctx, addr); err != nil {
			return err
		}
		avail, hold, err = ro.k.UserStakesOnHold(ctx, addr)
		return err
	})
	if err != nil {
		return err
	}
	nw := cfg.NetworkInfo()
	if a.JSON {
		return a.printJSON(map[string]any{"address": addr.Hex(), "network": nw.Label, "ethWei": eth.String(), "morWei": mor.String(),
			"withdrawableWei": avail.String(), "onHoldWei": hold.String()})
	}
	t := a.table()
	row := func(label, val string) { fmt.Fprintf(t, "%s\t%s\n", tui.Subtle.Render(label), val) }
	row("Address", tui.Code.Render(addr.Hex()))
	row("Network", nw.Label)
	row("MOR", tui.Strong.Render(units.MOR(mor)))
	row("ETH", units.ETH(eth))
	row("Released stake", units.MOR(avail)+tui.Subtle.Render(" withdrawable now"))
	row("On hold", units.MOR(hold)+tui.Subtle.Render(" unlocks at the next UTC day"))
	if nw.ExplorerURL != "" {
		row("Explorer", tui.Link.Render(nw.ExplorerURL+"/address/"+addr.Hex()))
	}
	_ = t.Flush()
	if avail.Sign() > 0 {
		fmt.Fprintf(a.Out, "\n%s %s\n", tui.Subtle.Render("Claim released stake with"), tui.Code.Render("morcore wallet withdraw"))
	}
	return nil
}

func walletWithdraw(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	var amount *big.Int
	if c, ok := a.running(); ok {
		var out struct {
			Amount string
			Tx     string
		}
		if err := tui.Spin("Withdrawing released stake", func() error { return c.Do("POST", "/admin/stakes/withdraw", nil, &out) }); err != nil {
			return err
		}
		amount, _ = new(big.Int).SetString(out.Amount, 10)
	} else {
		w, err := a.unlock(cfg)
		if err != nil {
			return err
		}
		ro, err := a.readOnly(cfg)
		if err != nil {
			return err
		}
		log := slog.New(slog.NewTextHandler(io.Discard, nil))
		mgr := consumer.New(consumer.Options{Duration: time.Hour}, w, ro.k, chain.NewTxSender(ro.k, w, cfg.Chain.LegacyTx, log), ro.market, morrpc.DefaultClient(), events.New(), log)
		ctx, cancel := timeout(5 * time.Minute)
		defer cancel()
		if err := tui.Spin("Withdrawing released stake", func() error {
			var err error
			amount, _, err = mgr.Withdraw(ctx)
			return err
		}); err != nil {
			return err
		}
	}
	if amount == nil || amount.Sign() == 0 {
		a.info("Nothing to withdraw yet — released stake unlocks at the start of the next UTC day.")
		return nil
	}
	a.ok("withdrew %s", units.MOR(amount))
	return nil
}

func walletExport(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if !a.confirm("Print the private key for " + cfg.Wallet.Address + "? Anyone who sees it controls the funds.") {
		return errors.New("not confirmed")
	}
	w, err := a.unlock(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintln(a.Out, w.PrivateKeyHex())
	return nil
}
