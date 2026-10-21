package consumer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// NoProviderError explains why no provider could be used for a model.
type NoProviderError struct {
	Model   string
	Reasons []string
}

func (e *NoProviderError) Error() string {
	if len(e.Reasons) == 0 {
		return fmt.Sprintf("no provider is currently offering %s", e.Model)
	}
	return fmt.Sprintf("no provider accepted a session for %s: %s", e.Model, strings.Join(e.Reasons, "; "))
}

// probe is the result of pinging a candidate provider.
type probe struct {
	bid     marketplace.RatedBid
	pubKey  []byte
	health  string // reported status for the bid's model ("" = not reported)
	latency time.Duration
	err     error
}

func newRequestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Ping checks a provider is reachable and speaks for the given address.
// It returns the provider's public key and the model health it reports.
func (m *Manager) Ping(ctx context.Context, endpoint string, provider common.Address) ([]byte, []morrpc.ModelHealthReport, time.Duration, error) {
	nonce := make([]byte, 8)
	_, _ = rand.Read(nonce)
	msg, err := morrpc.PingRequest(newRequestID(), nonce, m.w)
	if err != nil {
		return nil, nil, 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	start := time.Now()
	res, err := m.rpc.Call(ctx, endpoint, msg)
	if err != nil {
		return nil, nil, 0, err
	}
	rtt := time.Since(start)
	pong, pub, signer, err := morrpc.VerifyPong(res, nonce)
	if err != nil {
		return nil, nil, rtt, err
	}
	if err := m.authorized(ctx, provider, signer); err != nil {
		return nil, nil, rtt, err
	}
	return pub, pong.Models, rtt, nil
}

// authorized mirrors the Diamond's receipt check: the signer must be the
// provider itself, the owner of a contract provider, or a delegate.
func (m *Manager) authorized(ctx context.Context, provider, signer common.Address) error {
	if signer == provider {
		return nil
	}
	if owner, err := m.contractOwner(ctx, provider); err == nil && owner != (common.Address{}) && owner == signer {
		return nil
	}
	if ok, err := m.k.IsRightsDelegated(ctx, signer, provider, chain.RulesProvider); err == nil && ok {
		return nil
	}
	return fmt.Errorf("provider responded with key %s, which is not authorized for %s", signer.Hex(), provider.Hex())
}

var ownerSelector = crypto.Keccak256([]byte("owner()"))[:4]

func (m *Manager) contractOwner(ctx context.Context, addr common.Address) (common.Address, error) {
	m.authMu.Lock()
	owner, ok := m.ownerOf[addr]
	m.authMu.Unlock()
	if ok {
		return owner, nil
	}
	code, err := m.k.C.CodeAt(ctx, addr)
	if err != nil {
		return common.Address{}, err
	}
	if len(code) > 0 {
		out, err := m.k.C.CallContract(ctx, chain.CallMsg{To: &addr, Data: ownerSelector})
		if err == nil && len(out) >= 32 {
			owner = common.BytesToAddress(out[:32])
		}
	}
	m.authMu.Lock()
	m.ownerOf[addr] = owner
	m.authMu.Unlock()
	return owner, nil
}

func healthOf(reports []morrpc.ModelHealthReport, modelID common.Hash) string {
	for _, r := range reports {
		if strings.EqualFold(r.ModelID, modelID.Hex()) {
			return r.Status
		}
	}
	return ""
}

func blocked(status string) bool {
	switch status {
	case morrpc.HealthUnhealthy, morrpc.HealthTeeUnverified, morrpc.HealthNoModel:
		return true
	}
	return false
}

func good(status string) bool {
	return status == morrpc.HealthHealthy || status == morrpc.HealthDegraded
}

// probeAll pings candidates concurrently.
func (m *Manager) probeAll(ctx context.Context, cands []marketplace.RatedBid, modelID common.Hash) []probe {
	out := make([]probe, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c marketplace.RatedBid) {
			defer wg.Done()
			pub, models, rtt, err := m.Ping(ctx, c.Provider.Endpoint, c.Bid.Provider)
			out[i] = probe{bid: c, pubKey: pub, health: healthOf(models, modelID), latency: rtt, err: err}
		}(i, c)
	}
	wg.Wait()
	return out
}

// order applies the health policy to probed candidates (already score-sorted).
func order(policy string, probes []probe) []probe {
	var healthy, unknown []probe
	providers := map[common.Address]bool{}
	for _, p := range probes {
		if p.err != nil || blocked(p.health) {
			continue
		}
		providers[p.bid.Bid.Provider] = true
		if good(p.health) {
			healthy = append(healthy, p)
		} else {
			unknown = append(unknown, p)
		}
	}
	switch policy {
	case "strict":
		if len(healthy) > 0 || len(providers) > 1 {
			return healthy
		}
		return unknown // a single provider is still worth trying
	case "preferred":
		return append(healthy, unknown...)
	default:
		var out []probe
		for _, p := range probes {
			if p.err == nil && !blocked(p.health) {
				out = append(out, p)
			}
		}
		return out
	}
}

// open buys a new session for model from the best available provider.
func (m *Manager) open(ctx context.Context, model *marketplace.Model, exclude map[common.Address]bool, onlyBid common.Hash) (*Session, error) {
	bids, err := m.market.RatedBids(ctx, model.ID)
	if err != nil {
		return nil, fmt.Errorf("read bids for %s: %w", model.Alias, err)
	}
	me := m.w.Address()
	var cands []marketplace.RatedBid
	for _, b := range bids {
		if b.Bid.Provider == me || exclude[b.Bid.Provider] {
			continue
		}
		if onlyBid != (common.Hash{}) && b.Bid.ID != onlyBid {
			continue
		}
		cands = append(cands, b)
	}
	if len(cands) == 0 {
		return nil, &NoProviderError{Model: model.Alias}
	}
	if len(cands) > 8 {
		cands = cands[:8]
	}

	supply, budget, _, err := m.market.Economics(ctx)
	if err != nil {
		return nil, fmt.Errorf("read marketplace economics: %w", err)
	}
	balance, err := m.k.MORBalance(ctx, me)
	if err != nil {
		return nil, fmt.Errorf("read MOR balance: %w", err)
	}

	probes := m.probeAll(ctx, cands, model.ID)
	var reasons []string
	for _, p := range probes {
		switch {
		case p.err != nil:
			reasons = append(reasons, fmt.Sprintf("%s unreachable (%v)", shortAddr(p.bid.Bid.Provider), p.err))
		case blocked(p.health):
			reasons = append(reasons, fmt.Sprintf("%s reports model %s", shortAddr(p.bid.Bid.Provider), p.health))
		}
	}

	durSec := int64(m.opt.Duration / time.Second)
	for _, p := range order(m.opt.HealthPolicy, probes) {
		stake := marketplace.SessionStake(p.bid.Bid.PricePerSecond, durSec, supply, budget)
		if stake == nil {
			return nil, errors.New("today's compute budget is zero; the marketplace is not accepting sessions")
		}
		if stake.Cmp(balance) > 0 {
			reasons = append(reasons, fmt.Sprintf("%s needs %s stake but the wallet holds %s", shortAddr(p.bid.Bid.Provider), mor(stake), mor(balance)))
			continue
		}
		if m.opt.MaxStake != nil && m.opt.MaxStake.Sign() > 0 {
			locked := m.LockedStake()
			if new(big.Int).Add(locked, stake).Cmp(m.opt.MaxStake) > 0 {
				reasons = append(reasons, fmt.Sprintf("stake cap reached (%s locked, %s more needed, cap %s)", mor(locked), mor(stake), mor(m.opt.MaxStake)))
				continue
			}
		}

		sr, err := m.initiate(ctx, p, stake)
		if err != nil {
			reasons = append(reasons, fmt.Sprintf("%s declined (%v)", shortAddr(p.bid.Bid.Provider), err))
			continue
		}

		m.emitf(events.Info, "opening %s session with %s (stake %s)", model.Alias, shortAddr(p.bid.Bid.Provider), mor(stake))
		id, err := m.tx.OpenSession(ctx, stake, m.opt.Direct, sr.Approval, sr.ApprovalSig, m.allowanceTarget(stake))
		if err != nil {
			return nil, fmt.Errorf("open session on-chain: %w", err)
		}
		cs, err := m.readSession(ctx, id)
		if err != nil {
			return nil, err
		}
		s := &Session{
			ID: id, ModelID: model.ID, ModelName: model.Alias, BidID: p.bid.Bid.ID, Provider: p.bid.Bid.Provider,
			Endpoint: morrpc.NormalizeEndpoint(p.bid.Provider.Endpoint), ProviderKey: sr.PubKey,
			Price: p.bid.Bid.PricePerSecond, Stake: stake, OpenedAt: cs.OpenedAtTime(), EndsAt: cs.EndsAtTime(), Direct: m.opt.Direct,
		}
		s.init()
		m.log.Info("session opened", "model", model.Alias, "session", s.ID.Hex(), "provider", s.Provider.Hex(), "stake", mor(stake), "ends", s.EndsAt.Format(time.Kitchen))
		m.emitf(events.Info, "session %s open for %s until %s", short(s.ID), model.Alias, s.EndsAt.Format(time.Kitchen))
		return s, nil
	}
	return nil, &NoProviderError{Model: model.Alias, Reasons: reasons}
}

// allowanceTarget approves headroom for a few more sessions so most opens are
// a single transaction, bounded by the stake cap when one is configured.
func (m *Manager) allowanceTarget(stake *big.Int) *big.Int {
	t := new(big.Int).Mul(stake, big.NewInt(int64(max(m.opt.MaxTotal, 1))))
	if m.opt.MaxStake != nil && m.opt.MaxStake.Sign() > 0 && t.Cmp(m.opt.MaxStake) > 0 {
		t = new(big.Int).Set(m.opt.MaxStake)
	}
	if t.Cmp(stake) < 0 {
		t = new(big.Int).Set(stake)
	}
	return t
}

// initiate performs the session.request handshake and validates the approval.
func (m *Manager) initiate(ctx context.Context, p probe, stake *big.Int) (*morrpc.SessionRes, error) {
	msg, err := morrpc.SessionRequest(newRequestID(), stake, p.bid.Bid.ID, m.w)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	res, err := m.rpc.Call(cctx, p.bid.Provider.Endpoint, msg)
	if err != nil {
		return nil, err
	}
	provider := p.bid.Bid.Provider
	return morrpc.VerifySessionRes(res, m.w.Address(), p.bid.Bid.ID, m.k.ChainID, func(signer common.Address) error {
		return m.authorized(ctx, provider, signer)
	})
}

// readSession polls until a freshly opened session is visible on the RPC node.
func (m *Manager) readSession(ctx context.Context, id common.Hash) (*chain.Session, error) {
	var lastErr error
	for i := 0; i < 10; i++ {
		s, err := m.k.GetSession(ctx, id)
		if err == nil && s.EndsAt != nil && s.EndsAt.Sign() > 0 {
			return s, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return nil, fmt.Errorf("session %s opened but not readable yet: %v", id.Hex(), lastErr)
}

func shortAddr(a common.Address) string {
	h := a.Hex()
	return h[:6] + "…" + h[len(h)-4:]
}
