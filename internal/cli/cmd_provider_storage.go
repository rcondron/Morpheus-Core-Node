package cli

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/kubo"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

const providerStorageHelp = `Sell storage: users' files are pinned on your IPFS (Kubo) node while their
storage leases (marketplace sessions on your storage listing) pay you, and
for a grace period after (provider.storage.grace) before they are unpinned.

Setup:
  1. Run Kubo (ipfs daemon) next to the node; its RPC API stays private.
  2. morcore provider storage --ipfs-api http://127.0.0.1:5001
  3. morcore provider storage-listing 100GiB   (or --listing <id> to reuse one)
  4. morcore provider bid <listing> --price-gib-month 0.2
  5. Restart the node.

Flags:
  --ipfs-api URL        Kubo RPC API (default http://127.0.0.1:5001)
  --ipfs-auth VALUE     Authorization header for the API ("Bearer …" or "Basic …")
  --listing ID          also serve an existing storage listing
  --max-file SIZE       largest single file you accept (default 4GiB)
  --max-total SIZE      quota you sell across live leases (default: Kubo's StorageMax)
  --grace DURATION      keep files this long after coverage ends (default 72h)
  --gateway URL         public IPFS gateway to advertise to users (optional)
  --accept-stipend      also accept leases paid from the compute budget
  --disable             stop selling storage`

// providerStorage configures and checks the storage service.
func providerStorage(a *App, args []string) error {
	fs := flag.NewFlagSet("provider storage", flag.ContinueOnError)
	api := fs.String("ipfs-api", "", "")
	auth := fs.String("ipfs-auth", "", "")
	listing := fs.String("listing", "", "")
	maxFile := fs.String("max-file", "", "")
	maxTotal := fs.String("max-total", "", "")
	grace := fs.Duration("grace", 0, "")
	gw := fs.String("gateway", "", "")
	stipend := fs.Bool("accept-stipend", false, "")
	disable := fs.Bool("disable", false, "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	ps := &cfg.Provider.Storage
	changed := false
	set := func(dst *string, v string) {
		if v != "" && *dst != v {
			*dst, changed = v, true
		}
	}
	set(&ps.IPFSAPI, *api)
	set(&ps.IPFSAuth, *auth)
	set(&ps.MaxFileSize, *maxFile)
	set(&ps.MaxTotal, *maxTotal)
	set(&ps.Gateway, *gw)
	if *grace > 0 && ps.Grace.Duration != *grace {
		ps.Grace.Duration, changed = *grace, true
	}
	if *stipend && !ps.AcceptStipend {
		ps.AcceptStipend, changed = true, true
	}
	ctx, cancel := timeout(2 * time.Minute)
	defer cancel()
	if *listing != "" {
		if err := a.addStorageListing(ctx, cfg, *listing); err != nil {
			return err
		}
		changed = true
	}
	if *disable {
		if ps.Enabled {
			ps.Enabled, changed = false, true
		}
	} else if !ps.Enabled && (changed || len(args) == 0) {
		ps.Enabled, changed = true, true
		a.enableProviderRole(cfg)
	}
	if changed {
		if err := cfg.Validate(); err != nil {
			return err
		}
		if err := cfg.Save(a.Home); err != nil {
			return err
		}
	}
	if !ps.Enabled {
		a.ok("not selling storage")
		return nil
	}
	return a.storageReport(ctx, cfg, changed)
}

// enableProviderRole switches the node to selling when it only bought.
func (a *App) enableProviderRole(cfg *config.Config) {
	if cfg.ProviderEnabled() {
		return
	}
	if cfg.Role == config.RoleConsumer {
		cfg.Role = config.RoleBoth
	} else {
		cfg.Role = config.RoleProvider
	}
	cfg.Provider.Enabled = true
	a.info("Selling requires the provider role: this node becomes %s.", tui.Strong.Render(cfg.Role))
}

// addStorageListing adds an on-chain storage listing to the config.
func (a *App) addStorageListing(ctx context.Context, cfg *config.Config, id string) error {
	if !strings.HasPrefix(id, "0x") || len(id) != 66 {
		return usagef("Find listings with `morcore models --all` (tag storage).", "%q is not a listing id", id)
	}
	ro, err := a.readOnly(cfg)
	if err != nil {
		return err
	}
	m, err := ro.k.GetModel(ctx, common.HexToHash(id))
	if err != nil {
		return err
	}
	if marketplace.StorageQuota(m.Tags) == 0 {
		return fmt.Errorf("%s (%s) is not a storage listing (tags %s)", shortHash(id), m.Name, strings.Join(m.Tags, ","))
	}
	for _, l := range cfg.Provider.Storage.Listings {
		if strings.EqualFold(l, id) {
			return nil
		}
	}
	cfg.Provider.Storage.Listings = append(cfg.Provider.Storage.Listings, common.HexToHash(id).Hex())
	return nil
}

// storageReport checks Kubo and shows the listings and bids.
func (a *App) storageReport(ctx context.Context, cfg *config.Config, changed bool) error {
	ps := cfg.Provider.Storage
	if changed {
		a.ok("storage settings saved; restart the node to apply them")
	}
	kc, err := kubo.New(ps.IPFSAPI, kubo.WithAuthorization(ps.IPFSAuth))
	if err != nil {
		return err
	}
	v, err := kc.Version(ctx)
	if err != nil {
		fmt.Fprintf(a.Out, "%s IPFS node at %s: %v\n", tui.CrossMk, ps.IPFSAPI, err)
		a.info("  Install Kubo (https://docs.ipfs.tech/install/command-line/) and run %s", tui.Code.Render("ipfs daemon"))
	} else {
		a.ok("IPFS node at %s (Kubo %s)", ps.IPFSAPI, v)
		if st, err := kc.RepoStat(ctx); err == nil {
			capacity := ps.MaxTotal
			if b, _ := config.ParseBytes(capacity); b == 0 {
				capacity = config.FormatBytes(st.StorageMax) + " (Kubo StorageMax)"
			}
			a.info("  repo %s used; selling up to %s; max file %s; grace %s", config.FormatBytes(st.RepoSize), capacity, ps.MaxFileSize, ps.Grace.Duration)
		}
	}
	if len(ps.Listings) == 0 {
		a.warn("no storage listing yet: create one with %s, or serve an existing one with %s",
			tui.Code.Render("morcore provider storage-listing 100GiB"), tui.Code.Render("morcore provider storage --listing <id>"))
		return nil
	}
	ro, err := a.readOnly(cfg)
	if err != nil {
		return err
	}
	bids := map[common.Hash]*big.Int{}
	if ids, err := ro.k.ProviderActiveBidIDs(ctx, commonAddr(cfg.Wallet.Address)); err == nil {
		if list, err := ro.k.GetBids(ctx, ids); err == nil {
			for _, b := range list {
				bids[common.Hash(b.ModelId)] = b.PricePerSecond
			}
		}
	}
	t := a.table()
	header(t, "LISTING", "QUOTA", "YOUR BID", "PER GiB-MONTH", "ID")
	for _, l := range ps.Listings {
		id := common.HexToHash(l)
		name, quota := "?", uint64(0)
		if m, err := ro.k.GetModel(ctx, id); err == nil {
			name, quota = m.Name, marketplace.StorageQuota(m.Tags)
		}
		bid, perGiB := tui.Warning.Render("none"), "-"
		if p := bids[id]; p != nil {
			bid = perHour(p.String())
			if quota > 0 {
				perGiB = units.MOR(perGiBMonth(p, quota))
			}
		}
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\n", tui.Strong.Render(name), config.FormatBytes(quota), bid, perGiB, tui.Subtle.Render(shortHash(id.Hex())))
	}
	if err := t.Flush(); err != nil {
		return err
	}
	for _, l := range ps.Listings {
		if bids[common.HexToHash(l)] == nil {
			a.info("\nPost a price: %s", tui.Code.Render("morcore provider bid "+l+" --price-gib-month 0.2"))
			break
		}
	}
	return nil
}

func perGiBMonth(pricePerSecond *big.Int, quota uint64) *big.Int {
	v := new(big.Int).Mul(pricePerSecond, big.NewInt(30*24*3600))
	v.Mul(v, big.NewInt(1<<30))
	return v.Div(v, new(big.Int).SetUint64(quota))
}

// priceForGiBMonth converts a MOR per GiB-month price into a bid's price per
// second for a listing of quota bytes.
func priceForGiBMonth(perGiBMonthWei *big.Int, quota uint64) *big.Int {
	v := new(big.Int).Mul(perGiBMonthWei, new(big.Int).SetUint64(quota))
	return v.Div(v, big.NewInt((1<<30)*30*24*3600))
}

// providerStorageListing registers a storage listing: a marketplace model
// whose sessions each buy quota bytes of storage.
func providerStorageListing(a *App, args []string) error {
	fs := flag.NewFlagSet("provider storage-listing", flag.ContinueOnError)
	name := fs.String("name", "", "")
	stakeStr := fs.String("stake", "", "")
	fresh := fs.Bool("new", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("Example: morcore provider storage-listing 100GiB   (the storage one lease buys)", "storage-listing needs a quota")
	}
	quota, err := config.ParseBytes(pos[0])
	if err != nil || quota < 1<<20 {
		return usagef("Use a size like 10GiB or 1TiB.", "invalid quota %q", pos[0])
	}
	e, err := a.providerEnv(true)
	if err != nil {
		return err
	}
	ctx, cancel := timeout(10 * time.Minute)
	defer cancel()
	tags := marketplace.StorageTags(quota)
	if !*fresh {
		// Bidding on an existing listing keeps the market in one place.
		if models, err := e.ro.market.Models(ctx); err == nil {
			for _, m := range models {
				if marketplace.StorageQuota(m.Tags) == quota {
					if err := a.addStorageListing(ctx, e.cfg, m.ID.Hex()); err != nil {
						return err
					}
					e.cfg.Provider.Storage.Enabled = true
					a.enableProviderRole(e.cfg)
					if err := e.cfg.Save(a.Home); err != nil {
						return err
					}
					a.ok("serving the existing %s listing %s (%d bid(s)); pass --new to mint your own", config.FormatBytes(quota), tui.Strong.Render(m.Name), m.Bids)
					a.checkCapacity(ctx, e.cfg, quota)
					a.info("Next: %s", tui.Code.Render("morcore provider bid "+m.ID.Hex()+" --price-gib-month 0.2"))
					return nil
				}
			}
		}
	}
	stake, err := e.ro.k.ModelMinimumStake(ctx)
	if err != nil {
		return err
	}
	if *stakeStr != "" {
		if stake, err = units.ParseToken(*stakeStr); err != nil {
			return err
		}
	}
	if *name == "" {
		*name = "IPFS storage " + strings.ReplaceAll(config.FormatBytes(quota), " ", "")
	}
	var base common.Hash
	_, _ = rand.Read(base[:])
	var id common.Hash
	if err := tui.Spin("Registering storage listing "+*name, func() error {
		var err error
		id, err = e.tx.ModelRegister(ctx, base, common.Hash{}, stake, *name, tags)
		return err
	}); err != nil {
		return err
	}
	e.cfg.Provider.Storage.Listings = append(e.cfg.Provider.Storage.Listings, id.Hex())
	e.cfg.Provider.Storage.Enabled = true
	a.enableProviderRole(e.cfg)
	if err := e.cfg.Save(a.Home); err != nil {
		return err
	}
	a.ok("listing %s registered: %s per lease (id %s)", tui.Strong.Render(*name), config.FormatBytes(quota), tui.Code.Render(id.Hex()))
	a.checkCapacity(ctx, e.cfg, quota)
	a.info("Next: %s, then restart the node.", tui.Code.Render("morcore provider bid "+id.Hex()+" --price-gib-month 0.2"))
	return nil
}

var errNotStorage = errors.New("--price-gib-month only applies to storage listings")

// checkCapacity warns when one lease would not fit the storage this node
// sells: Kubo's StorageMax (10GB by default) or provider.storage.max_total.
// The node refuses leases larger than its capacity.
func (a *App) checkCapacity(ctx context.Context, cfg *config.Config, quota uint64) {
	ps := cfg.Provider.Storage
	capacity, _ := config.ParseBytes(ps.MaxTotal)
	source := "provider.storage.max_total"
	if capacity == 0 {
		kc, err := kubo.New(ps.IPFSAPI, kubo.WithAuthorization(ps.IPFSAuth))
		if err != nil {
			return
		}
		st, err := kc.RepoStat(ctx)
		if err != nil {
			a.warn("could not read the IPFS node's capacity (%v); check it with `morcore doctor` once Kubo runs", err)
			return
		}
		capacity, source = st.StorageMax, "Kubo's Datastore.StorageMax"
	}
	if quota > capacity {
		a.warn("one lease (%s) is more than the %s you sell (%s), so leases would be refused. Raise it, e.g. %s, or set %s",
			config.FormatBytes(quota), config.FormatBytes(capacity), source,
			tui.Code.Render("ipfs config Datastore.StorageMax 500GB"), tui.Code.Render("morcore provider storage --max-total 500GiB"))
	}
}
