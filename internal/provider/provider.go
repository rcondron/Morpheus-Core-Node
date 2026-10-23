// Package provider is the provider half of the routing node: it accepts
// consumer sessions over MOR-RPC (TCP, port 3333 by default), checks them
// against the chain, and serves prompts from OpenAI-compatible backends.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/store"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

// Model maps an on-chain model id to a backend.
type Model struct {
	ID             common.Hash
	Name           string
	APIType        string // config.API* (empty = OpenAI-compatible)
	APIURL         string // backend endpoint (see endpointFor)
	APIKey         string
	Backend        string // model name sent to the backend
	MaxSessions    int    // concurrent sessions accepted (0 = unlimited)
	CapacityPolicy string // config.Capacity* (empty = simple)
	Parameters     map[string]string
}

// Type returns the backend API type.
func (m *Model) Type() string {
	if m.APIType == "" {
		return config.APIOpenAI
	}
	return m.APIType
}

// servesUploads reports whether the backend takes uploaded files (audio).
func (m *Model) servesUploads() bool { return m.Type() == config.APIOpenAI }

// Options configure the provider.
type Options struct {
	Models    []Model
	Agents    []Agent
	StatePath string
	Version   string
	AutoClaim bool
	HTTP      *http.Client // backend client (tests); default has no timeout
}

// StorageService sells storage on listings this provider serves (see
// internal/storage). Storage sessions are leases: they take no model slot,
// and their requests are the storage.* methods.
type StorageService interface {
	Serves(listing common.Hash) bool
	Admit(ctx context.Context, listing common.Hash, user common.Address) error
	Handle(ctx context.Context, msg *morrpc.RPCMessage, remote string, send func(*morrpc.RPCResponse) error) error
	HealthReports(bids map[common.Hash]common.Hash) []morrpc.ModelHealthReport
}

// Provider serves marketplace sessions.
type Provider struct {
	opt     Options
	w       *wallet.Wallet
	k       *chain.Contracts
	tx      *chain.TxSender
	log     *slog.Logger
	bus     *events.Bus
	http    *http.Client
	storage StorageService

	uploads uploads

	mu        sync.Mutex
	models    map[common.Hash]*Model
	agents    map[common.Hash]*Agent
	sessions  map[common.Hash]*sessionInfo
	approvals map[common.Hash][]time.Time
	slots     map[common.Hash]chan struct{}
	health    map[common.Hash]*health
	bids      map[common.Hash]common.Hash
	signerFor map[common.Address]bool
	served    int64
	dirty     bool
}

type sessionInfo struct {
	ID       common.Hash    `json:"id"`
	User     common.Address `json:"user"`
	Provider common.Address `json:"provider"`
	ModelID  common.Hash    `json:"modelId"`
	EndsAt   time.Time      `json:"endsAt"`
	Closed   bool           `json:"closed"`
	Requests int64          `json:"requests"`
	In       int64          `json:"inputTokens"`
	Out      int64          `json:"outputTokens"`
	TTFT     []int64        `json:"ttftMs,omitempty"`
	TPS      []int64        `json:"tpsX1000,omitempty"`
	LastUsed time.Time      `json:"lastUsed,omitzero"`
	fetched  time.Time
}

// New builds a provider.
func New(opt Options, w *wallet.Wallet, k *chain.Contracts, tx *chain.TxSender, bus *events.Bus, log *slog.Logger) *Provider {
	p := &Provider{
		opt: opt, w: w, k: k, tx: tx, log: log, bus: bus,
		http:      opt.HTTP,
		uploads:   uploads{m: map[string]*upload{}},
		sessions:  map[common.Hash]*sessionInfo{},
		approvals: map[common.Hash][]time.Time{},
		slots:     map[common.Hash]chan struct{}{},
		health:    map[common.Hash]*health{},
		bids:      map[common.Hash]common.Hash{},
		signerFor: map[common.Address]bool{w.Address(): true},
	}
	if p.http == nil {
		p.http = &http.Client{Timeout: 0}
	}
	p.Reload(opt.Models, opt.Agents)
	p.load()
	return p
}

// Reload swaps the served models and agents. Requests in flight keep the
// definitions they started with; sessions for removed models fail from the
// next request on, so drop the bid and let sessions drain before removing one.
func (p *Provider) Reload(models []Model, agents []Agent) (added, removed []common.Hash) {
	nextModels := make(map[common.Hash]*Model, len(models))
	for i := range models {
		m := models[i]
		if m.Backend == "" {
			m.Backend = m.Name
		}
		nextModels[m.ID] = &m
	}
	nextAgents := make(map[common.Hash]*Agent, len(agents))
	for i := range agents {
		a := agents[i]
		nextAgents[a.ID] = &a
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	known := func(id common.Hash) bool { return p.models[id] != nil || p.agents[id] != nil }
	for id := range nextModels {
		if !known(id) {
			added = append(added, id)
		}
	}
	for id := range nextAgents {
		if !known(id) {
			added = append(added, id)
		}
	}
	for id := range p.models {
		if nextModels[id] == nil && nextAgents[id] == nil {
			removed = append(removed, id)
		}
	}
	for id := range p.agents {
		if nextModels[id] == nil && nextAgents[id] == nil {
			removed = append(removed, id)
		}
	}
	p.models, p.agents = nextModels, nextAgents
	for id := range nextModels {
		if p.health[id] == nil {
			p.health[id] = &health{status: morrpc.HealthHealthy}
		}
	}
	return added, removed
}

func (p *Provider) model(id common.Hash) *Model {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.models[id]
}

// SetStorage adds a storage service; call it before serving.
func (p *Provider) SetStorage(s StorageService) { p.storage = s }

// Handle dispatches one MOR-RPC request.
func (p *Provider) Handle(ctx context.Context, msg *morrpc.RPCMessage, remote string, send func(*morrpc.RPCResponse) error) error {
	if strings.HasPrefix(msg.Method, "storage.") {
		if p.storage == nil {
			return errors.New("this provider does not sell storage")
		}
		return p.storage.Handle(ctx, msg, remote, send)
	}
	switch msg.Method {
	case morrpc.MethodPing:
		return p.ping(msg, send)
	case morrpc.MethodSessionReq:
		return p.sessionRequest(ctx, msg, remote, send)
	case morrpc.MethodSessionPrompt:
		return p.prompt(ctx, msg, send)
	case morrpc.MethodSessionReport:
		return p.report(ctx, msg, send)
	case morrpc.MethodStreamStart:
		return p.streamStart(ctx, msg, send)
	case morrpc.MethodStreamChunk:
		return p.streamChunk(ctx, msg, send)
	case morrpc.MethodStreamEnd:
		return p.streamEnd(ctx, msg, send)
	case morrpc.MethodAgentTools:
		return p.agentTools(ctx, msg, send)
	case morrpc.MethodAgentCall:
		return p.agentCall(ctx, msg, send)
	default:
		return fmt.Errorf("unsupported method %q", msg.Method)
	}
}

func (p *Provider) ping(msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.PingReq
	if err := json.Unmarshal(msg.Params, &req); err != nil || len(req.Nonce) == 0 {
		return errors.New("invalid ping")
	}
	res, err := morrpc.PongResponse(msg.ID, req.Nonce, p.opt.Version, p.healthReports(), p.w)
	if err != nil {
		return err
	}
	return send(res)
}

// ServesFor reports whether this node's key may act for an on-chain provider
// address: the address itself, or a contract provider whose owner() is us.
func (p *Provider) ServesFor(ctx context.Context, provider common.Address) bool {
	p.mu.Lock()
	ok, known := p.signerFor[provider]
	p.mu.Unlock()
	if known {
		return ok
	}
	ok = false
	if code, err := p.k.C.CodeAt(ctx, provider); err == nil && len(code) > 0 {
		if out, err := p.k.C.CallContract(ctx, chain.CallMsg{To: &provider, Data: common.FromHex("0x8da5cb5b")}); err == nil && len(out) >= 32 {
			ok = common.BytesToAddress(out[:32]) == p.w.Address()
		}
	}
	p.mu.Lock()
	p.signerFor[provider] = ok
	p.mu.Unlock()
	return ok
}

func (p *Provider) sessionRequest(ctx context.Context, msg *morrpc.RPCMessage, remote string, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.SessionReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid session request: %w", err)
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	if !morrpc.VerifyParams(req, sig, req.Key) {
		return morrpc.ErrInvalidSignature
	}
	if morrpc.PubKeyAddress(req.Key) != req.User {
		return errors.New("session key does not match user wallet")
	}
	bid, err := p.k.GetBid(ctx, req.BidID)
	if err != nil {
		return fmt.Errorf("unknown bid: %w", err)
	}
	if !bid.Active() {
		return errors.New("bid is no longer active")
	}
	if !p.ServesFor(ctx, bid.Provider) {
		return errors.New("bid belongs to another provider")
	}
	id := common.Hash(bid.ModelId)
	if p.storage != nil && p.storage.Serves(id) {
		if err := p.storage.Admit(ctx, id, req.User); err != nil {
			return err
		}
		res, err := morrpc.SessionResponse(msg.ID, req.User, req.BidID, p.k.ChainID, p.w)
		if err != nil {
			return err
		}
		p.log.Info("approved storage lease", "user", req.User.Hex(), "listing", id.Hex(), "remote", remote)
		p.bus.Emit(events.Info, "provider", fmt.Sprintf("approved storage lease for %s", short(req.User.Hex())))
		return send(res)
	}
	p.mu.Lock()
	model, agent := p.models[id], p.agents[id]
	p.mu.Unlock()
	var name string
	var limit int
	var policy string
	switch {
	case model != nil:
		if p.healthStatus(id) == morrpc.HealthUnhealthy {
			return errors.New("model backend is currently unavailable")
		}
		name, limit, policy = model.Name, model.MaxSessions, model.CapacityPolicy
	case agent != nil:
		name, limit, policy = agent.Name, agent.MaxSessions, agent.CapacityPolicy
	default:
		return errors.New("model is not served by this provider")
	}
	if !p.reserve(id, limit, policy) {
		return errors.New("provider at capacity for this model")
	}
	res, err := morrpc.SessionResponse(msg.ID, req.User, req.BidID, p.k.ChainID, p.w)
	if err != nil {
		return err
	}
	p.log.Info("approved session request", "user", req.User.Hex(), "model", name, "remote", remote)
	p.bus.Emit(events.Info, "provider", fmt.Sprintf("approved session for %s (%s)", name, short(req.User.Hex())))
	return send(res)
}

// idleWindow is how recently a session must have been used to count against
// capacity under the idle_timeout policy.
const idleWindow = 15 * time.Minute

// reserve applies the per-model session limit, counting live sessions and
// approvals handed out in the last ten minutes that have not shown up yet.
func (p *Provider) reserve(id common.Hash, limit int, policy string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	recent := p.approvals[id][:0]
	for _, t := range p.approvals[id] {
		if now.Sub(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	p.approvals[id] = recent
	if limit <= 0 {
		p.approvals[id] = append(recent, now)
		return true
	}
	live := 0
	for _, s := range p.sessions {
		if s.ModelID != id || s.Closed || !s.EndsAt.After(now) {
			continue
		}
		if policy == config.CapacityIdleTimeout && now.Sub(s.LastUsed) > idleWindow {
			continue // idle sessions free their slot
		}
		live++
	}
	if live+len(recent) >= limit {
		return false
	}
	p.approvals[id] = append(recent, now)
	return true
}

// session loads (and caches) an on-chain session served by this node.
func (p *Provider) session(ctx context.Context, id common.Hash) (*sessionInfo, error) {
	p.mu.Lock()
	si := p.sessions[id]
	fresh := si != nil && time.Since(si.fetched) < time.Minute
	p.mu.Unlock()
	if fresh {
		return si, nil
	}
	cs, err := p.k.GetSession(ctx, id)
	if err != nil {
		if si != nil {
			return si, nil // transient RPC failure: trust the cached copy
		}
		return nil, fmt.Errorf("session cannot be loaded: %w", err)
	}
	bid, err := p.k.GetBid(ctx, cs.BidId)
	if err != nil {
		return nil, fmt.Errorf("session bid cannot be loaded: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if si == nil {
		si = &sessionInfo{ID: id}
		p.sessions[id] = si
		// The first prompt of a newly opened session consumes its approval.
		if a := p.approvals[common.Hash(bid.ModelId)]; len(a) > 0 {
			p.approvals[common.Hash(bid.ModelId)] = a[1:]
		}
	}
	si.User, si.Provider, si.ModelID = cs.User, bid.Provider, common.Hash(bid.ModelId)
	si.EndsAt, si.Closed, si.fetched = cs.EndsAtTime(), cs.Closed(), time.Now()
	p.dirty = true
	return si, nil
}

func (p *Provider) slot(id common.Hash) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	ch := p.slots[id]
	if ch == nil {
		ch = make(chan struct{}, 1)
		p.slots[id] = ch
	}
	return ch
}

// acquire waits for the session's single request slot: consumers send one
// request per session at a time and expect providers to serialize the rest.
func (p *Provider) acquire(ctx context.Context, si *sessionInfo) (release func(), err error) {
	slot := p.slot(si.ID)
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// authorize loads a live session served by this node and checks that sig
// (over params with its signature cleared) was made by the session's user.
// It returns the user's public key, which responses are encrypted to.
func (p *Provider) authorize(ctx context.Context, id common.Hash, params any, sig morrpc.HexString) (*sessionInfo, []byte, error) {
	si, err := p.session(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if si.Closed {
		return nil, nil, errors.New("session is closed")
	}
	if time.Now().After(si.EndsAt) {
		return nil, nil, errors.New("session expired")
	}
	if !p.ServesFor(ctx, si.Provider) {
		return nil, nil, errors.New("session not bound to this provider")
	}
	pub, signer, err := morrpc.RecoverParams(params, sig)
	if err != nil || signer != si.User {
		return nil, nil, morrpc.ErrInvalidSignature
	}
	return si, pub, nil
}

func (p *Provider) prompt(ctx context.Context, msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.SessionPromptReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid prompt: %w", err)
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	si, userPub, err := p.authorize(ctx, req.SessionID, req, sig)
	if err != nil {
		return err
	}
	r, err := parseRequest([]byte(req.Message))
	if err != nil {
		e := &emitter{p: p, id: msg.ID, userPub: userPub, send: send, st: &forwardStats{}, start: time.Now()}
		return e.reject(err)
	}
	return p.serve(ctx, si, r, userPub, msg.ID, send)
}

// serve runs one request on the session's model and records the outcome.
func (p *Provider) serve(ctx context.Context, si *sessionInfo, r *request, userPub []byte, id string, send func(*morrpc.RPCResponse) error) error {
	model := p.model(si.ModelID)
	if model == nil {
		return errors.New("model is not served by this provider")
	}
	release, err := p.acquire(ctx, si)
	if err != nil {
		return err
	}
	defer release()
	e := &emitter{p: p, id: id, userPub: userPub, send: send, st: &forwardStats{}, start: time.Now()}
	err = p.forward(ctx, model, r, e)
	e.st.dur = time.Since(e.start)
	var br badRequest
	if errors.As(err, &br) && e.sent == 0 {
		p.recordResult(si, model, nil, nil) // the request's fault, not the backend's
		if errors.Is(err, errUnsupported.error) {
			err = invalid("%s requests are not supported by %s (a %s model)", r.kind, model.Name, model.Type())
		}
		return e.reject(err)
	}
	p.recordResult(si, model, e.st, err)
	return err
}

func (p *Provider) recordResult(si *sessionInfo, m *Model, st *forwardStats, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.served++
	si.Requests++
	si.LastUsed = time.Now()
	if st != nil {
		si.In += int64(st.in)
		si.Out += int64(st.out)
		if st.ttft > 0 {
			si.TTFT = capped(si.TTFT, st.ttft.Milliseconds())
		}
		if d := st.dur.Seconds(); d > 0 && st.out > 0 {
			si.TPS = capped(si.TPS, int64(float64(st.out)/d*1000))
		}
	}
	p.dirty = true
	h := p.health[m.ID]
	if h == nil || st == nil {
		return
	}
	switch {
	case err == nil && st.status < 400:
		h.ok()
	case st.status == http.StatusTooManyRequests:
		h.limited()
	case st.status == 400 || st.status == 403 || st.status == 413 || st.status == 422:
		// consumer-side mistakes do not count against backend health
	default:
		h.fail(st.status)
	}
}

func capped(xs []int64, v int64) []int64 {
	xs = append(xs, v)
	if len(xs) > 256 {
		xs = xs[len(xs)-256:]
	}
	return xs
}

func (p *Provider) report(ctx context.Context, msg *morrpc.RPCMessage, send func(*morrpc.RPCResponse) error) error {
	var req morrpc.SessionReportReq
	if err := json.Unmarshal(msg.Params, &req); err != nil {
		return fmt.Errorf("invalid report request: %w", err)
	}
	if !strings.HasPrefix(req.Message, "0x") || len(req.Message) != 66 {
		return errors.New("invalid session id")
	}
	si, err := p.session(ctx, common.HexToHash(req.Message))
	if err != nil {
		return err
	}
	if !p.ServesFor(ctx, si.Provider) {
		return errors.New("session not bound to this provider")
	}
	sig := req.Signature
	req.Signature = morrpc.HexString{}
	if _, signer, err := morrpc.RecoverParams(req, sig); err != nil || signer != si.User {
		return morrpc.ErrInvalidSignature
	}
	p.mu.Lock()
	r := morrpc.Report{
		SessionID: si.ID, ChainID: p.k.ChainID,
		TPSScaled1000: uint32(mean(si.TPS)), TTFTMs: uint32(mean(si.TTFT)),
		InputTokens: uint32(si.In), OutputTokens: uint32(si.Out),
	}
	p.mu.Unlock()
	res, err := morrpc.ReportResponse(msg.ID, r, p.w)
	if err != nil {
		return err
	}
	return send(res)
}

func mean(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	var t int64
	for _, x := range xs {
		t += x
	}
	return t / int64(len(xs))
}

// Stats summarize provider activity.
type Stats struct {
	Models         int   `json:"models"`
	ActiveSessions int   `json:"activeSessions"`
	Requests       int64 `json:"requests"`
}

func (p *Provider) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := Stats{Models: len(p.models) + len(p.agents), Requests: p.served}
	now := time.Now()
	for _, s := range p.sessions {
		if !s.Closed && s.EndsAt.After(now) {
			st.ActiveSessions++
		}
	}
	return st
}

// Run performs background work: persisting state, refreshing bids, probing
// backends and (optionally) claiming earnings from finished sessions.
func (p *Provider) Run(ctx context.Context) {
	p.refreshBids(ctx)
	p.probeAll(ctx)
	save := time.NewTicker(30 * time.Second)
	probe := time.NewTicker(5 * time.Minute)
	claim := time.NewTicker(time.Hour)
	defer save.Stop()
	defer probe.Stop()
	defer claim.Stop()
	for {
		select {
		case <-ctx.Done():
			p.save()
			p.uploads.expireAll()
			return
		case <-save.C:
			p.save()
			p.uploads.expire()
		case <-probe.C:
			p.refreshBids(ctx)
			p.probeAll(ctx)
		case <-claim.C:
			if p.opt.AutoClaim && p.tx != nil {
				if n, err := p.ClaimAll(ctx); err != nil {
					p.log.Warn("claiming provider earnings failed", "err", err)
				} else if n > 0 {
					p.log.Info("claimed provider earnings", "sessions", n)
				}
			}
		}
	}
}

// Refresh re-reads our bids and probes backends now (after a reload).
func (p *Provider) Refresh(ctx context.Context) {
	p.refreshBids(ctx)
	p.probeAll(ctx)
}

func (p *Provider) refreshBids(ctx context.Context) {
	ids, err := p.k.ProviderActiveBidIDs(ctx, p.w.Address())
	if err != nil {
		p.log.Debug("could not list own bids", "err", err)
		return
	}
	bids, err := p.k.GetBids(ctx, ids)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bids = map[common.Hash]common.Hash{}
	for _, b := range bids {
		if b.Active() {
			p.bids[common.Hash(b.ModelId)] = b.ID
		}
	}
}

// ClaimAll collects earnings for ended, unclosed sessions of this provider.
func (p *Provider) ClaimAll(ctx context.Context) (int, error) {
	ids, err := p.k.RecentProviderSessionIDs(ctx, p.w.Address(), 100)
	if err != nil {
		return 0, err
	}
	sessions, err := p.k.GetSessions(ctx, ids)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	claimed := 0
	for _, s := range sessions {
		if s.Closed() || now.Before(s.EndsAtTime()) || s.ProviderWithdrawnAmount == nil || s.ProviderWithdrawnAmount.Sign() > 0 {
			continue
		}
		if _, err := p.tx.ClaimForProvider(ctx, s.ID); err != nil {
			p.log.Warn("claim failed", "session", s.ID.Hex(), "err", err)
			continue
		}
		claimed++
	}
	return claimed, nil
}

type stateFile struct {
	Version  int            `json:"version"`
	Sessions []*sessionInfo `json:"sessions"`
}

func (p *Provider) load() {
	if p.opt.StatePath == "" {
		return
	}
	var f stateFile
	if _, err := store.ReadJSON(p.opt.StatePath, &f); err != nil {
		p.log.Warn("could not read provider state", "err", err)
		return
	}
	for _, s := range f.Sessions {
		p.sessions[s.ID] = s
	}
}

func (p *Provider) save() {
	if p.opt.StatePath == "" {
		return
	}
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	var list []*sessionInfo
	for id, s := range p.sessions {
		if s.EndsAt.Before(cutoff) {
			delete(p.sessions, id)
			delete(p.slots, id)
			continue
		}
		cp := *s
		cp.TTFT = append([]int64(nil), s.TTFT...)
		cp.TPS = append([]int64(nil), s.TPS...)
		list = append(list, &cp)
	}
	p.dirty = false
	p.mu.Unlock()
	if err := store.WriteJSON(p.opt.StatePath, stateFile{Version: 1, Sessions: list}, 0o600); err != nil {
		p.log.Warn("could not save provider state", "err", err)
	}
}

func short(s string) string {
	if len(s) > 10 {
		return s[:6] + "…" + s[len(s)-4:]
	}
	return s
}

// PriceFromHourly converts a MOR-per-hour price to the per-second wei price bids use.
func PriceFromHourly(perHourWei *big.Int) *big.Int {
	return new(big.Int).Div(perHourWei, big.NewInt(3600))
}
