package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/store"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// Options tune session management.
type Options struct {
	Duration        time.Duration // length of newly opened sessions
	MaxPerModel     int           // concurrent sessions per model (each serves one request at a time)
	MaxTotal        int           // concurrent sessions overall
	MaxStake        *big.Int      // cap on MOR escrowed across open sessions (nil/0 = no cap)
	IdleTimeout     time.Duration // close sessions idle this long (0 = never)
	RenewBefore     time.Duration // pre-open a replacement this long before a busy session ends
	Direct          bool          // pay providers directly instead of staking for stipend
	ProviderTimeout time.Duration // wait for a provider's first and subsequent response chunks
	QueueTimeout    time.Duration // how long a request may wait for a free session
	HealthPolicy    string        // permissive | preferred | strict
	CloseOnShutdown bool
	AutoWithdraw    bool
	StorePath       string
	// Leased reports models whose sessions another component manages (storage
	// leases); Reconcile leaves them alone.
	Leased func(modelID common.Hash) bool
}

var (
	ErrNoCapacity      = errors.New("no session capacity for this model (raise sessions.max_per_model / max_total)")
	ErrQueueTimeout    = errors.New("timed out waiting for a free session")
	ErrClosed          = errors.New("node is shutting down")
	ErrSessionNotFound = errors.New("session not found")
)

const (
	usableMargin     = 15 * time.Second // never start a request this close to session end
	scaleUpDelay     = 2 * time.Second  // wait this long for a busy session before opening another
	openRetryBackoff = 30 * time.Second // after a failed open, fail fast for this long
)

// Manager owns all consumer sessions.
type Manager struct {
	opt    Options
	w      *wallet.Wallet
	k      *chain.Contracts
	tx     *chain.TxSender
	market *marketplace.Market
	rpc    *morrpc.Client
	log    *slog.Logger
	bus    *events.Bus

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[common.Hash]*Session
	pools    map[common.Hash]*pool
	closing  bool

	authMu    sync.Mutex
	ownerOf   map[common.Address]common.Address
	persistMu sync.Mutex
}

type pool struct {
	modelID    common.Hash
	modelName  string
	model      marketplace.Model
	sessions   []*Session
	opening    int
	notify     chan struct{}
	lastDemand time.Time
	openErr    error
	openErrAt  time.Time
}

func (p *pool) wake() {
	close(p.notify)
	p.notify = make(chan struct{})
}

// New creates a session manager.
func New(opt Options, w *wallet.Wallet, k *chain.Contracts, tx *chain.TxSender, market *marketplace.Market, rpc *morrpc.Client, bus *events.Bus, log *slog.Logger) *Manager {
	if opt.MaxPerModel < 1 {
		opt.MaxPerModel = 1
	}
	if opt.MaxTotal < opt.MaxPerModel {
		opt.MaxTotal = opt.MaxPerModel
	}
	if opt.QueueTimeout <= 0 {
		opt.QueueTimeout = 2 * time.Minute
	}
	if opt.ProviderTimeout <= 0 {
		opt.ProviderTimeout = 2 * time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		opt: opt, w: w, k: k, tx: tx, market: market, rpc: rpc, bus: bus, log: log,
		ctx: ctx, cancel: cancel,
		sessions: map[common.Hash]*Session{},
		pools:    map[common.Hash]*pool{},
		ownerOf:  map[common.Address]common.Address{},
	}
}

// Market returns the marketplace reader.
func (m *Manager) Market() *marketplace.Market { return m.market }

func (m *Manager) poolLocked(model *marketplace.Model) *pool {
	p := m.pools[model.ID]
	if p == nil {
		p = &pool{modelID: model.ID, modelName: model.Alias, model: *model, notify: make(chan struct{})}
		m.pools[model.ID] = p
	}
	return p
}

func (m *Manager) activeCountLocked() (n int) {
	for _, p := range m.pools {
		n += p.opening
		for _, s := range p.sessions {
			if st := s.State(); st == StateActive || st == StateClosing {
				n++
			}
		}
	}
	return n
}

func (m *Manager) canGrowLocked(p *pool) bool {
	inPool := p.opening
	for _, s := range p.sessions {
		if s.State() == StateActive {
			inPool++
		}
	}
	return inPool < m.opt.MaxPerModel && m.activeCountLocked() < m.opt.MaxTotal
}

// Lease is exclusive use of one session for one request.
type Lease struct {
	Session *Session
	m       *Manager
	start   time.Time
	once    sync.Once
}

// Release returns the session to its pool and records the outcome.
func (l *Lease) Release(ok bool, inTokens, outTokens int, ttft time.Duration) {
	l.once.Do(func() {
		l.Session.release(ok, inTokens, outTokens, ttft, time.Since(l.start))
		l.m.mu.Lock()
		if p := l.m.pools[l.Session.ModelID]; p != nil {
			p.wake()
		}
		l.m.mu.Unlock()
	})
}

// Acquire leases a free session for model, opening new sessions on demand
// (up to the configured limits) and queueing when all sessions are busy.
// Providers in exclude are skipped (used for failover).
func (m *Manager) Acquire(ctx context.Context, model *marketplace.Model, exclude map[common.Address]bool) (*Lease, error) {
	deadline := time.Now().Add(m.opt.QueueTimeout)
	waitStart := time.Now()
	for {
		m.mu.Lock()
		if m.closing {
			m.mu.Unlock()
			return nil, ErrClosed
		}
		p := m.poolLocked(model)
		now := time.Now()
		p.lastDemand = now
		usable := 0
		for _, s := range orderForRouting(p.sessions) {
			if exclude[s.Provider] || !s.usable(now, usableMargin) {
				continue
			}
			usable++
			if s.tryAcquire() {
				m.mu.Unlock()
				return &Lease{Session: s, m: m, start: time.Now()}, nil
			}
		}
		inBackoff := p.openErr != nil && now.Sub(p.openErrAt) < openRetryBackoff
		if p.opening == 0 && !inBackoff && m.canGrowLocked(p) && (usable == 0 || now.Sub(waitStart) >= scaleUpDelay) {
			p.opening++
			m.startOpenLocked(p, exclude)
		}
		if usable == 0 && p.opening == 0 {
			err := p.openErr
			if err == nil {
				err = ErrNoCapacity
			}
			m.mu.Unlock()
			return nil, err
		}
		wait := p.notify
		m.mu.Unlock()

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, ErrQueueTimeout
		}
		timer := time.NewTimer(min(remaining, scaleUpDelay))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-wait:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// AcquireSession leases a specific session (clients may pin one with the
// session_id header, as with the reference proxy-router), waiting while busy.
func (m *Manager) AcquireSession(ctx context.Context, id common.Hash) (*Lease, error) {
	deadline := time.Now().Add(m.opt.QueueTimeout)
	for {
		m.mu.Lock()
		s := m.sessions[id]
		if s == nil || m.closing {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: %s", ErrSessionNotFound, id.Hex())
		}
		if !s.usable(time.Now(), usableMargin) {
			m.mu.Unlock()
			return nil, fmt.Errorf("%w: session %s is %s or ending", ErrSessionNotFound, short(id), s.State())
		}
		if s.tryAcquire() {
			m.mu.Unlock()
			return &Lease{Session: s, m: m, start: time.Now()}, nil
		}
		wait := m.pools[s.ModelID].notify
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		case <-time.After(time.Until(deadline)):
			return nil, ErrQueueTimeout
		}
	}
}

// orderForRouting prefers sessions with the most time left so that sessions
// about to expire drain naturally.
func orderForRouting(in []*Session) []*Session {
	out := append([]*Session(nil), in...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].EndsAt.After(out[j].EndsAt) })
	return out
}

func (m *Manager) startOpenLocked(p *pool, exclude map[common.Address]bool) {
	model := p.model
	excl := map[common.Address]bool{}
	for a := range exclude {
		excl[a] = true
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
		defer cancel()
		s, err := m.open(ctx, &model, excl, common.Hash{}, m.routingTerms())
		m.mu.Lock()
		p.opening--
		if err != nil {
			p.openErr, p.openErrAt = err, time.Now()
		} else {
			p.openErr = nil
			m.addLocked(s)
		}
		p.wake()
		m.mu.Unlock()
		if err != nil {
			m.log.Warn("could not open session", "model", model.Alias, "err", err)
			m.bus.Emit(events.Warn, "session", fmt.Sprintf("open %s failed: %v", model.Alias, err))
			return
		}
		m.persist()
	}()
}

func (m *Manager) addLocked(s *Session) {
	s.init()
	m.sessions[s.ID] = s
	p := m.pools[s.ModelID]
	if p == nil {
		p = &pool{modelID: s.ModelID, modelName: s.ModelName, model: marketplace.Model{ID: s.ModelID, Name: s.ModelName, Alias: s.ModelName}, notify: make(chan struct{})}
		m.pools[s.ModelID] = p
	}
	for _, x := range p.sessions {
		if x.ID == s.ID {
			return
		}
	}
	p.sessions = append(p.sessions, s)
}

func (m *Manager) removeLocked(s *Session) {
	delete(m.sessions, s.ID)
	if p := m.pools[s.ModelID]; p != nil {
		kept := p.sessions[:0]
		for _, x := range p.sessions {
			if x.ID != s.ID {
				kept = append(kept, x)
			}
		}
		p.sessions = kept
		p.wake()
	}
}

// Open explicitly opens a session for a model (optionally a specific bid).
func (m *Manager) Open(ctx context.Context, model *marketplace.Model, bidID common.Hash) (*Session, error) {
	m.mu.Lock()
	p := m.poolLocked(model)
	if bidID == (common.Hash{}) && !m.canGrowLocked(p) {
		m.mu.Unlock()
		return nil, ErrNoCapacity
	}
	p.opening++
	m.mu.Unlock()

	s, err := m.open(ctx, model, nil, bidID, m.routingTerms())
	m.mu.Lock()
	p.opening--
	if err == nil {
		m.addLocked(s)
	}
	p.wake()
	m.mu.Unlock()
	if err == nil {
		m.persist()
	}
	return s, err
}

// OpenLease buys a session on a specific bid for the caller to manage itself
// (storage leases): paid directly from the stake, for duration, outside the
// routing pools, their stake cap and idle closing.
func (m *Manager) OpenLease(ctx context.Context, model *marketplace.Model, bidID common.Hash, duration time.Duration) (*Session, error) {
	return m.open(ctx, model, nil, bidID, openTerms{duration: duration, direct: true, lease: true})
}

// CloseLease closes a session opened with OpenLease, with the provider's
// receipt when it answers.
func (m *Manager) CloseLease(ctx context.Context, s *Session, reason string) error {
	return m.closeSession(ctx, s, reason)
}

// Sessions returns snapshots of all tracked sessions.
func (m *Manager) Sessions() []Snapshot {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	sort.Slice(list, func(i, j int) bool { return list[i].OpenedAt.Before(list[j].OpenedAt) })
	out := make([]Snapshot, len(list))
	for i, s := range list {
		out[i] = s.Snapshot()
	}
	return out
}

// PoolInfo summarizes per-model routing state.
type PoolInfo struct {
	Model    string `json:"model"`
	Sessions int    `json:"sessions"`
	Busy     int    `json:"busy"`
	Opening  int    `json:"opening"`
	LastErr  string `json:"lastError,omitempty"`
}

func (m *Manager) Pools() []PoolInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PoolInfo, 0, len(m.pools))
	for _, p := range m.pools {
		pi := PoolInfo{Model: p.modelName, Opening: p.opening}
		for _, s := range p.sessions {
			pi.Sessions++
			if s.Busy() {
				pi.Busy++
			}
		}
		if p.openErr != nil && time.Since(p.openErrAt) < 10*time.Minute {
			pi.LastErr = p.openErr.Error()
		}
		if pi.Sessions == 0 && pi.Opening == 0 && pi.LastErr == "" {
			continue
		}
		out = append(out, pi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// LockedStake is the MOR escrowed in sessions this node tracks.
func (m *Manager) LockedStake() *big.Int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lockedStakeLocked()
}

func (m *Manager) lockedStakeLocked() *big.Int {
	total := new(big.Int)
	for _, s := range m.sessions {
		if st := s.State(); (st == StateActive || st == StateClosing) && s.Stake != nil {
			total.Add(total, s.Stake)
		}
	}
	return total
}

// Run performs housekeeping until ctx is cancelled: closing expired, idle and
// failing sessions, pre-opening replacements for busy models and withdrawing
// released stake.
func (m *Manager) Run(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	withdraw := time.NewTicker(15 * time.Minute)
	defer withdraw.Stop()
	if m.opt.AutoWithdraw {
		m.goTask(func(ctx context.Context) { m.autoWithdraw(ctx) })
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			m.housekeep()
		case <-withdraw.C:
			if m.opt.AutoWithdraw {
				m.goTask(func(ctx context.Context) { m.autoWithdraw(ctx) })
			}
		}
	}
}

func (m *Manager) goTask(fn func(ctx context.Context)) {
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Minute)
		defer cancel()
		fn(ctx)
	}()
}

func (m *Manager) housekeep() {
	now := time.Now()
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		return
	}
	var toClose []*Session
	reasons := map[common.Hash]string{}
	for _, s := range m.sessions {
		if s.State() != StateActive {
			continue
		}
		switch {
		case !now.Before(s.EndsAt):
			toClose, reasons[s.ID] = append(toClose, s), "expired"
		case s.failing() && !s.Busy():
			toClose, reasons[s.ID] = append(toClose, s), "provider failing"
		case m.opt.IdleTimeout > 0 && !s.Adopted && s.IdleFor(now) >= m.opt.IdleTimeout:
			toClose, reasons[s.ID] = append(toClose, s), "idle"
		}
	}
	for _, s := range toClose {
		s.setState(StateClosing)
	}
	// Pre-open a replacement when a model in active use is about to lose its
	// last usable session, so agents never hit a cold start.
	if m.opt.RenewBefore > 0 {
		for _, p := range m.pools {
			if p.opening > 0 || now.Sub(p.lastDemand) > max(m.opt.IdleTimeout, 5*time.Minute) {
				continue
			}
			future := 0
			for _, s := range p.sessions {
				if s.usable(now.Add(m.opt.RenewBefore), usableMargin) {
					future++
				}
			}
			inBackoff := p.openErr != nil && now.Sub(p.openErrAt) < openRetryBackoff
			if future == 0 && len(p.sessions) > 0 && !inBackoff && m.canGrowLocked(p) {
				p.opening++
				m.log.Info("pre-opening replacement session", "model", p.modelName)
				m.startOpenLocked(p, nil)
			}
		}
	}
	m.mu.Unlock()

	for _, s := range toClose {
		s, reason := s, reasons[s.ID]
		m.goTask(func(ctx context.Context) {
			if err := m.closeSession(ctx, s, reason); err != nil {
				m.log.Warn("close session failed; will retry", "session", s.ID.Hex(), "err", err)
				s.setState(StateActive)
				s.mu.Lock()
				s.failures = 3 // keep it out of rotation
				s.mu.Unlock()
			}
		})
	}
}

// Shutdown stops background work and, if configured, closes this node's
// sessions so their unused stake is released.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	m.closing = true
	var open []*Session
	for _, s := range m.sessions {
		if s.State() == StateActive && !s.Adopted {
			open = append(open, s)
		}
	}
	for _, p := range m.pools {
		p.wake()
	}
	m.mu.Unlock()

	if m.opt.CloseOnShutdown && len(open) > 0 {
		m.log.Info("closing sessions before exit", "count", len(open))
		var wg sync.WaitGroup
		for _, s := range open {
			wg.Add(1)
			go func(s *Session) {
				defer wg.Done()
				if err := m.closeSession(ctx, s, "shutdown"); err != nil {
					m.log.Warn("could not close session; it will be closed on next start", "session", s.ID.Hex(), "err", err)
					s.setState(StateActive)
				}
			}(s)
		}
		wg.Wait()
	}
	// Let in-flight background work (closing expired sessions, an open that is
	// mid-transaction) finish within the shutdown deadline, then cancel the rest.
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	m.cancel()
	<-done
	m.persist()
}

// ---------------------------------------------------------------------------
// Persistence
// ---------------------------------------------------------------------------

type persisted struct {
	Version  int        `json:"version"`
	Sessions []*Session `json:"sessions"`
}

func (m *Manager) persist() {
	if m.opt.StorePath == "" {
		return
	}
	m.persistMu.Lock()
	defer m.persistMu.Unlock()
	m.mu.Lock()
	var list []*Session
	for _, s := range m.sessions {
		if st := s.State(); st == StateActive || st == StateClosing {
			s.mu.Lock()
			cp := &Session{
				ID: s.ID, ModelID: s.ModelID, ModelName: s.ModelName, BidID: s.BidID, Provider: s.Provider,
				Endpoint: s.Endpoint, ProviderKey: s.ProviderKey, Price: s.Price, Stake: s.Stake,
				OpenedAt: s.OpenedAt, EndsAt: s.EndsAt, Direct: s.Direct, Adopted: s.Adopted, Stats: s.Stats,
			}
			cp.Stats.TTFTms = append([]int64(nil), s.Stats.TTFTms...)
			cp.Stats.TPSx1000 = append([]int64(nil), s.Stats.TPSx1000...)
			s.mu.Unlock()
			list = append(list, cp)
		}
	}
	m.mu.Unlock()
	if err := store.WriteJSON(m.opt.StorePath, persisted{Version: 1, Sessions: list}, 0o600); err != nil {
		m.log.Warn("could not save sessions", "err", err)
	}
}

func (m *Manager) loadPersisted() []*Session {
	var p persisted
	if _, err := store.ReadJSON(m.opt.StorePath, &p); err != nil {
		m.log.Warn("could not read saved sessions", "err", err)
	}
	return p.Sessions
}

func (m *Manager) emitf(level, format string, args ...any) {
	m.bus.Emit(level, "session", fmt.Sprintf(format, args...))
}

func short(h common.Hash) string { return h.Hex()[:10] }

func mor(v *big.Int) string { return units.MOR(v) }
