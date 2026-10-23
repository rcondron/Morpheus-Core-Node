package cli

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/provider"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

type providerEnv struct {
	cfg *config.Config
	w   *wallet.Wallet
	ro  *readOnly
	tx  *chain.TxSender
}

func (a *App) providerEnv(needWallet bool) (*providerEnv, error) {
	cfg, err := a.config()
	if err != nil {
		return nil, err
	}
	ro, err := a.readOnly(cfg)
	if err != nil {
		return nil, err
	}
	e := &providerEnv{cfg: cfg, ro: ro}
	if needWallet {
		if e.w, err = a.unlock(cfg); err != nil {
			return nil, err
		}
		e.tx = chain.NewTxSender(ro.k, e.w, cfg.Chain.LegacyTx, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}
	return e, nil
}

func providerStatus(a *App, args []string) error {
	e, err := a.providerEnv(false)
	if err != nil {
		return err
	}
	addr := commonAddr(e.cfg.Wallet.Address)
	ctx, cancel := timeout(60 * time.Second)
	defer cancel()
	var p *chain.Provider
	var bids []*chain.Bid
	var minStake *big.Int
	err = tui.Spin("Reading provider registration", func() error {
		var err error
		if p, err = e.ro.k.GetProvider(ctx, addr); err != nil {
			return err
		}
		if minStake, err = e.ro.k.ProviderMinimumStake(ctx); err != nil {
			return err
		}
		ids, err := e.ro.k.ProviderActiveBidIDs(ctx, addr)
		if err != nil {
			return err
		}
		bids, err = e.ro.k.GetBids(ctx, ids)
		return err
	})
	if err != nil {
		return err
	}
	registered := p.CreatedAt != nil && p.CreatedAt.Sign() > 0 && !p.IsDeleted
	if a.JSON {
		return a.printJSON(map[string]any{"address": addr.Hex(), "registered": registered, "endpoint": p.Endpoint, "stakeWei": p.Stake.String(), "bids": bids})
	}
	t := a.table()
	row := func(label, val string) { fmt.Fprintf(t, "%s\t%s\n", tui.Subtle.Render(label), val) }
	row("Provider", tui.Code.Render(addr.Hex()))
	if registered {
		row("Registered", tui.Good.Render("yes")+tui.Subtle.Render(" since "+time.Unix(p.CreatedAt.Int64(), 0).Format("2006-01-02")))
		row("Endpoint", p.Endpoint)
		row("Stake", units.MOR(p.Stake))
		if e.cfg.Provider.PublicEndpoint != "" && p.Endpoint != e.cfg.Provider.PublicEndpoint {
			row("", tui.Warning.Render("config public_endpoint is "+e.cfg.Provider.PublicEndpoint+" — run `morcore provider register` to update"))
		}
	} else {
		row("Registered", tui.Warning.Render("no")+tui.Subtle.Render(fmt.Sprintf(" — minimum stake %s", units.MOR(minStake))))
	}
	_ = t.Flush()
	fmt.Fprintln(a.Out)
	if len(bids) == 0 {
		a.info("No active bids. Offer a model with %s", tui.Code.Render("morcore provider bid <model> --price <MOR per hour>"))
		return nil
	}
	tb := a.table()
	header(tb, "BID", "MODEL", "PRICE", "CONFIGURED")
	for _, b := range bids {
		cfgd := tui.Danger.Render("no backend in config")
		for _, m := range e.cfg.Provider.Models {
			if strings.EqualFold(m.ModelID, common.Hash(b.ModelId).Hex()) {
				cfgd = tui.Good.Render(m.Name)
			}
		}
		fmt.Fprintf(tb, "%s\t%s\t%s\t%s\n", shortHash(b.ID.Hex()), shortHash(common.Hash(b.ModelId).Hex()), perHour(b.PricePerSecond.String()), cfgd)
	}
	return tb.Flush()
}

func providerRegister(a *App, args []string) error {
	fs := flag.NewFlagSet("provider register", flag.ContinueOnError)
	endpoint := fs.String("endpoint", "", "")
	stakeStr := fs.String("stake", "", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	e, err := a.providerEnv(true)
	if err != nil {
		return err
	}
	if *endpoint == "" {
		*endpoint = e.cfg.Provider.PublicEndpoint
	}
	if *endpoint == "" {
		return usagef("Pass --endpoint host:3333 or set provider.public_endpoint.", "no public endpoint configured")
	}
	ctx, cancel := timeout(10 * time.Minute)
	defer cancel()
	cur, err := e.ro.k.GetProvider(ctx, e.w.Address())
	if err != nil {
		return err
	}
	minStake, err := e.ro.k.ProviderMinimumStake(ctx)
	if err != nil {
		return err
	}
	stake := new(big.Int)
	if *stakeStr != "" {
		if stake, err = units.ParseToken(*stakeStr); err != nil {
			return err
		}
	} else if cur.Stake == nil || cur.Stake.Cmp(minStake) < 0 {
		stake = new(big.Int).Set(minStake)
		if cur.Stake != nil {
			stake.Sub(stake, cur.Stake)
		}
	}
	a.info("Registering %s at %s (adding %s stake; minimum %s)", tui.ShortAddr(e.w.Address().Hex()), tui.Code.Render(*endpoint), units.MOR(stake), units.MOR(minStake))
	var tx common.Hash
	if err := tui.Spin("Sending registration", func() error {
		var err error
		tx, err = e.tx.ProviderRegister(ctx, stake, *endpoint)
		return err
	}); err != nil {
		return err
	}
	if e.cfg.Provider.PublicEndpoint != *endpoint {
		e.cfg.Provider.PublicEndpoint = *endpoint
		_ = e.cfg.Save(a.Home)
	}
	a.ok("registered (tx %s)", shortHash(tx.Hex()))
	return nil
}

func providerBid(a *App, args []string) error {
	fs := flag.NewFlagSet("provider bid", flag.ContinueOnError)
	price := fs.String("price", "", "")
	pricePerSecond := fs.String("price-per-second", "", "")
	perGiB := fs.String("price-gib-month", "", "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*price == "" && *pricePerSecond == "" && *perGiB == "") {
		return usagef("Example: morcore provider bid llama-3.3-70b --price 0.5   (MOR per hour; storage: --price-gib-month)", "provider bid needs a model and --price")
	}
	e, err := a.providerEnv(true)
	if err != nil {
		return err
	}
	ctx, cancel := timeout(10 * time.Minute)
	defer cancel()
	model, err := e.ro.market.Resolve(ctx, pos[0])
	if err != nil {
		return err
	}
	var perSec *big.Int
	quota := marketplace.StorageQuota(model.Tags)
	if *perGiB != "" {
		if quota == 0 {
			return errNotStorage
		}
		v, err := units.ParseToken(*perGiB)
		if err != nil {
			return err
		}
		perSec = priceForGiBMonth(v, quota)
	} else if *pricePerSecond != "" {
		v, ok := new(big.Int).SetString(*pricePerSecond, 10)
		if !ok {
			return usagef("", "--price-per-second must be an integer amount of MOR wei")
		}
		perSec = v
	} else {
		perHourWei, err := units.ParseToken(*price)
		if err != nil {
			return err
		}
		perSec = provider.PriceFromHourly(perHourWei)
	}
	lo, hi, err := e.ro.k.BidPriceBounds(ctx)
	if err == nil && (perSec.Cmp(lo) < 0 || perSec.Cmp(hi) > 0) {
		return fmt.Errorf("price %s is outside the marketplace range %s – %s", perHour(perSec.String()), perHour(lo.String()), perHour(hi.String()))
	}
	fee, _ := e.ro.k.BidFee(ctx)
	if rated, err := e.ro.market.RatedBids(ctx, model.ID); err == nil {
		var others []*big.Int
		for _, rb := range rated {
			if rb.Bid.Provider != e.w.Address() {
				others = append(others, rb.Bid.PricePerSecond)
			}
		}
		a.info("Market for %s: %s", model.Alias, marketPrices(others))
	}
	configured := false
	for _, m := range e.cfg.Provider.Models {
		configured = configured || strings.EqualFold(m.ModelID, model.ID.Hex())
	}
	for _, l := range e.cfg.Provider.Storage.Listings {
		configured = configured || (e.cfg.Provider.Storage.Enabled && strings.EqualFold(l, model.ID.Hex()))
	}
	a.info("Offer %s at %s (bid fee %s).", tui.Strong.Render(model.Alias), perHour(perSec.String()), units.MOR(fee))
	if quota > 0 {
		a.info("That is %s per GiB-month (%s per lease of %s).", units.MOR(perGiBMonth(perSec, quota)), units.MOR(new(big.Int).Mul(perSec, big.NewInt(86400))), config.FormatBytes(quota))
	}
	switch {
	case !configured && quota > 0:
		a.warn("this node does not serve this storage listing; add it with `morcore provider storage --listing %s`", model.ID.Hex())
	case !configured:
		a.warn("this model has no backend in config.toml; consumers will be refused until you add one under [[provider.models]]")
	}
	if !*yes && tui.Interactive() && !a.confirm("Post this bid?") {
		return errors.New("not confirmed")
	}
	var bid common.Hash
	if err := tui.Spin("Posting bid", func() error {
		var err error
		bid, err = e.tx.PostModelBid(ctx, model.ID, perSec)
		return err
	}); err != nil {
		return err
	}
	a.ok("bid %s live for %s", shortHash(bid.Hex()), model.Alias)
	return nil
}

func providerUnbid(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("Find bid ids with `morcore provider status`.", "provider unbid needs a bid id")
	}
	e, err := a.providerEnv(true)
	if err != nil {
		return err
	}
	ctx, cancel := timeout(5 * time.Minute)
	defer cancel()
	id := common.HexToHash(args[0])
	if len(args[0]) < 66 {
		ids, err := e.ro.k.ProviderActiveBidIDs(ctx, e.w.Address())
		if err != nil {
			return err
		}
		for _, b := range ids {
			if strings.HasPrefix(b.Hex(), strings.TrimSuffix(args[0], "…")) {
				id = b
			}
		}
	}
	if err := tui.Spin("Deleting bid "+shortHash(id.Hex()), func() error {
		_, err := e.tx.DeleteModelBid(ctx, id)
		return err
	}); err != nil {
		return err
	}
	a.ok("bid withdrawn")
	return nil
}

func providerClaim(a *App, args []string) error {
	e, err := a.providerEnv(true)
	if err != nil {
		return err
	}
	ctx, cancel := timeout(15 * time.Minute)
	defer cancel()
	p := provider.New(provider.Options{}, e.w, e.ro.k, e.tx, events.New(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	var n int
	if err := tui.Spin("Claiming earnings from finished sessions", func() error {
		var err error
		n, err = p.ClaimAll(ctx)
		return err
	}); err != nil {
		return err
	}
	if n == 0 {
		a.info("Nothing to claim — consumers' closes already paid out, or sessions are still running.")
		return nil
	}
	a.ok("claimed earnings from %d session(s)", n)
	return nil
}

func providerModelRegister(a *App, args []string) error {
	fs := flag.NewFlagSet("provider model-register", flag.ContinueOnError)
	tags := fs.String("tags", "", "")
	stakeStr := fs.String("stake", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("Prefer bidding on an existing model (see `morcore models --all`) over minting duplicates.", "model-register needs a model name")
	}
	e, err := a.providerEnv(true)
	if err != nil {
		return err
	}
	ctx, cancel := timeout(10 * time.Minute)
	defer cancel()
	stake, err := e.ro.k.ModelMinimumStake(ctx)
	if err != nil {
		return err
	}
	if *stakeStr != "" {
		if stake, err = units.ParseToken(*stakeStr); err != nil {
			return err
		}
	}
	var base common.Hash
	_, _ = rand.Read(base[:])
	var id common.Hash
	if err := tui.Spin("Registering model "+pos[0], func() error {
		var err error
		id, err = e.tx.ModelRegister(ctx, base, common.Hash{}, stake, pos[0], splitCSV(*tags))
		return err
	}); err != nil {
		return err
	}
	a.ok("model %s registered with id %s", tui.Strong.Render(pos[0]), tui.Code.Render(id.Hex()))
	a.info("Add it under [[provider.models]] in %s, then: %s", e.cfg.Path(config.FileName), tui.Code.Render("morcore provider bid "+id.Hex()+" --price <MOR/hour>"))
	return nil
}

func providerDeregister(a *App, args []string) error {
	e, err := a.providerEnv(true)
	if err != nil {
		return err
	}
	if !a.confirm("Deregister " + e.w.Address().Hex() + " as a provider? Delete your bids first.") {
		return errors.New("not confirmed")
	}
	ctx, cancel := timeout(5 * time.Minute)
	defer cancel()
	if err := tui.Spin("Deregistering", func() error {
		_, err := e.tx.ProviderDeregister(ctx)
		return err
	}); err != nil {
		return err
	}
	a.ok("deregistered; stake returns to the wallet per marketplace rules")
	return nil
}
