// Package marketplace reads the Morpheus compute marketplace: which models are
// offered, by whom, at what price, and which bids are best.
package marketplace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
)

// Model is a marketplace model with a summary of its live bids.
type Model struct {
	ID        common.Hash    `json:"id"`
	Name      string         `json:"name"`
	Alias     string         `json:"alias"`
	Tags      []string       `json:"tags"`
	Owner     common.Address `json:"owner"`
	CreatedAt time.Time      `json:"createdAt"`
	Bids      int            `json:"bids"`
	MinPrice  *big.Int       `json:"minPricePerSecond,omitempty"`
	MaxPrice  *big.Int       `json:"maxPricePerSecond,omitempty"`
}

// IsTEE reports whether the model requires a trusted execution environment.
func (m *Model) IsTEE() bool {
	for _, t := range m.Tags {
		if strings.EqualFold(t, "tee") {
			return true
		}
	}
	return false
}

// RatedBid is a bid with its provider and score.
type RatedBid struct {
	Bid      *chain.Bid
	Provider *chain.Provider
	Score    float64
}

// ErrModelNotFound is returned when a model name or id is unknown.
var ErrModelNotFound = errors.New("model not found on the marketplace")

// Market caches marketplace reads.
type Market struct {
	k       *chain.Contracts
	log     *slog.Logger
	weights Weights
	allow   map[common.Address]bool
	deny    map[common.Address]bool
	ttl     time.Duration

	mu       sync.RWMutex
	models   []Model
	byKey    map[string]int
	loadedAt time.Time
	loading  chan struct{}

	econMu   sync.Mutex
	supply   *big.Int
	budget   *big.Int
	minStake *big.Int
	econAt   time.Time
}

// New builds a Market. allow/deny are provider address lists (hex).
func New(k *chain.Contracts, allow, deny []string, log *slog.Logger) *Market {
	toSet := func(list []string) map[common.Address]bool {
		s := map[common.Address]bool{}
		for _, a := range list {
			if common.IsHexAddress(a) {
				s[common.HexToAddress(a)] = true
			}
		}
		return s
	}
	return &Market{k: k, log: log, weights: DefaultWeights, allow: toSet(allow), deny: toSet(deny), ttl: 5 * time.Minute}
}

// Contracts exposes the underlying contract bindings.
func (m *Market) Contracts() *chain.Contracts { return m.k }

// Models returns the cached catalog, refreshing it when stale.
func (m *Market) Models(ctx context.Context) ([]Model, error) {
	m.mu.RLock()
	fresh := m.models != nil && time.Since(m.loadedAt) < m.ttl
	models := m.models
	m.mu.RUnlock()
	if fresh {
		return models, nil
	}
	if err := m.Refresh(ctx); err != nil {
		if models != nil {
			m.log.Warn("model catalog refresh failed; serving cached copy", "err", err)
			return models, nil
		}
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.models, nil
}

// Refresh reloads the catalog from chain. Concurrent callers share one load.
func (m *Market) Refresh(ctx context.Context) error {
	m.mu.Lock()
	if ch := m.loading; ch != nil {
		m.mu.Unlock()
		select {
		case <-ch:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ch := make(chan struct{})
	m.loading = ch
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.loading = nil
		m.mu.Unlock()
		close(ch)
	}()

	models, err := m.load(ctx)
	if err != nil {
		return err
	}
	byKey := map[string]int{}
	for i, md := range models {
		byKey[strings.ToLower(md.ID.Hex())] = i
		byKey[strings.ToLower(md.Alias)] = i
	}
	for i, md := range models { // plain names resolve to the first (best-supplied) entry
		if _, ok := byKey[strings.ToLower(md.Name)]; !ok {
			byKey[strings.ToLower(md.Name)] = i
		}
	}
	m.mu.Lock()
	m.models, m.byKey, m.loadedAt = models, byKey, time.Now()
	m.mu.Unlock()
	return nil
}

func (m *Market) load(ctx context.Context) ([]Model, error) {
	ids, err := m.k.ActiveModelIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	raw, err := m.k.GetModels(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("read models: %w", err)
	}

	// Active bid ids per model, fetched with bounded parallelism.
	bidIDs := make([][]common.Hash, len(raw))
	errs := make([]error, len(raw))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, md := range raw {
		if md.IsDeleted {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id common.Hash) {
			defer wg.Done()
			defer func() { <-sem }()
			bidIDs[i], errs[i] = m.k.ModelActiveBidIDs(ctx, id)
		}(i, md.ID)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("list bids: %w", err)
	}
	var all []common.Hash
	for _, b := range bidIDs {
		all = append(all, b...)
	}
	bids, err := m.k.GetBids(ctx, all)
	if err != nil {
		return nil, fmt.Errorf("read bids: %w", err)
	}
	byID := make(map[common.Hash]*chain.Bid, len(bids))
	for _, b := range bids {
		byID[b.ID] = b
	}

	out := make([]Model, 0, len(raw))
	for i, md := range raw {
		if md.IsDeleted {
			continue
		}
		mod := Model{ID: md.ID, Name: strings.TrimSpace(md.Name), Tags: md.Tags, Owner: md.Owner, CreatedAt: time.Unix(md.CreatedAt.Int64(), 0)}
		for _, id := range bidIDs[i] {
			b := byID[id]
			if b == nil || !b.Active() || m.deny[b.Provider] {
				continue
			}
			mod.Bids++
			if mod.MinPrice == nil || b.PricePerSecond.Cmp(mod.MinPrice) < 0 {
				mod.MinPrice = b.PricePerSecond
			}
			if mod.MaxPrice == nil || b.PricePerSecond.Cmp(mod.MaxPrice) > 0 {
				mod.MaxPrice = b.PricePerSecond
			}
		}
		if mod.Name == "" {
			mod.Name = md.ID.Hex()
		}
		out = append(out, mod)
	}
	assignAliases(out)
	return out, nil
}

// assignAliases gives every model a unique, human friendly API id. Models
// sharing a display name are ordered by supply (bids, then age); the best
// supplied one keeps the bare name and the rest get a short id suffix.
func assignAliases(models []Model) {
	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if !strings.EqualFold(a.Name, b.Name) {
			return strings.ToLower(a.Name) < strings.ToLower(b.Name)
		}
		if a.Bids != b.Bids {
			return a.Bids > b.Bids
		}
		return a.CreatedAt.Before(b.CreatedAt)
	})
	seen := map[string]bool{}
	for i := range models {
		key := strings.ToLower(models[i].Name)
		if !seen[key] {
			models[i].Alias = models[i].Name
			seen[key] = true
			continue
		}
		models[i].Alias = models[i].Name + "@" + models[i].ID.Hex()[2:10]
	}
}

// Resolve finds a model by alias, name or hex id.
func (m *Market) Resolve(ctx context.Context, nameOrID string) (*Model, error) {
	key := strings.ToLower(strings.TrimSpace(nameOrID))
	if _, err := m.Models(ctx); err != nil {
		if strings.HasPrefix(key, "0x") && len(key) == 66 {
			return m.modelByID(ctx, common.HexToHash(key))
		}
		return nil, err
	}
	m.mu.RLock()
	i, ok := m.byKey[key]
	var md Model
	if ok {
		md = m.models[i]
	}
	m.mu.RUnlock()
	if ok {
		return &md, nil
	}
	if strings.HasPrefix(key, "0x") && len(key) == 66 {
		return m.modelByID(ctx, common.HexToHash(key))
	}
	return nil, fmt.Errorf("%w: %q", ErrModelNotFound, nameOrID)
}

func (m *Market) modelByID(ctx context.Context, id common.Hash) (*Model, error) {
	raw, err := m.k.GetModel(ctx, id)
	if err != nil {
		return nil, err
	}
	if raw.CreatedAt == nil || raw.CreatedAt.Sign() == 0 || raw.IsDeleted {
		return nil, fmt.Errorf("%w: %s", ErrModelNotFound, id.Hex())
	}
	return &Model{ID: id, Name: raw.Name, Alias: raw.Name, Tags: raw.Tags, Owner: raw.Owner, CreatedAt: time.Unix(raw.CreatedAt.Int64(), 0)}, nil
}

// RatedBids returns the model's active bids, best first, after allow/deny filtering.
func (m *Market) RatedBids(ctx context.Context, modelID common.Hash) ([]RatedBid, error) {
	ids, err := m.k.ModelActiveBidIDs(ctx, modelID)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	bids, err := m.k.GetBids(ctx, ids)
	if err != nil {
		return nil, err
	}
	var active []*chain.Bid
	var provAddrs []common.Address
	seen := map[common.Address]bool{}
	for _, b := range bids {
		if !b.Active() || m.deny[b.Provider] || (len(m.allow) > 0 && !m.allow[b.Provider]) {
			continue
		}
		active = append(active, b)
		if !seen[b.Provider] {
			seen[b.Provider] = true
			provAddrs = append(provAddrs, b.Provider)
		}
	}
	if len(active) == 0 {
		return nil, nil
	}
	provs, err := m.k.GetProviders(ctx, provAddrs)
	if err != nil {
		return nil, err
	}
	provByAddr := map[common.Address]*chain.Provider{}
	for _, p := range provs {
		provByAddr[p.Address] = p
	}
	bidProviders := make([]common.Address, len(active))
	for i, b := range active {
		bidProviders[i] = b.Provider
	}
	stats, err := m.k.GetProviderModelStats(ctx, modelID, bidProviders)
	if err != nil {
		return nil, err
	}
	modelStats, err := m.k.GetModelStats(ctx, modelID)
	if err != nil {
		return nil, err
	}
	_, _, minStake, err := m.Economics(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]RatedBid, 0, len(active))
	for i, b := range active {
		p := provByAddr[b.Provider]
		if p == nil || p.IsDeleted || strings.TrimSpace(p.Endpoint) == "" {
			continue
		}
		s := Score(m.weights, ScoreInput{ProviderModel: stats[i], Model: modelStats, ProviderStake: p.Stake, PricePerSecond: b.PricePerSecond, MinStake: minStake})
		if math.IsNaN(s) {
			continue
		}
		out = append(out, RatedBid{Bid: b, Provider: p, Score: s})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out, nil
}

// Economics returns MOR supply, today's compute budget and provider minimum
// stake, cached for a minute (they change daily).
func (m *Market) Economics(ctx context.Context) (supply, budget, minStake *big.Int, err error) {
	m.econMu.Lock()
	defer m.econMu.Unlock()
	if m.supply != nil && time.Since(m.econAt) < time.Minute {
		return m.supply, m.budget, m.minStake, nil
	}
	if supply, err = m.k.TotalMORSupply(ctx); err != nil {
		return nil, nil, nil, err
	}
	if budget, err = m.k.TodaysBudget(ctx); err != nil {
		return nil, nil, nil, err
	}
	if minStake, err = m.k.ProviderMinimumStake(ctx); err != nil {
		return nil, nil, nil, err
	}
	m.supply, m.budget, m.minStake, m.econAt = supply, budget, minStake, time.Now()
	return supply, budget, minStake, nil
}

// EstimateStake prices a session of duration for a bid.
func (m *Market) EstimateStake(ctx context.Context, pricePerSecond *big.Int, duration time.Duration) (*big.Int, error) {
	supply, budget, _, err := m.Economics(ctx)
	if err != nil {
		return nil, err
	}
	stake := SessionStake(pricePerSecond, int64(duration/time.Second), supply, budget)
	if stake == nil {
		return nil, errors.New("marketplace budget is zero; sessions cannot be opened right now")
	}
	return stake, nil
}
