package consumer

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Close settles a tracked session on-chain.
func (m *Manager) Close(ctx context.Context, id common.Hash) error {
	m.mu.Lock()
	s := m.sessions[id]
	m.mu.Unlock()
	if s == nil {
		return m.closeUntracked(ctx, id)
	}
	if st := s.State(); st != StateActive {
		return fmt.Errorf("session is %s", st)
	}
	s.setState(StateClosing)
	if err := m.closeSession(ctx, s, "requested"); err != nil {
		s.setState(StateActive)
		return err
	}
	return nil
}

// closeSession obtains a closeout receipt and submits it. A provider-signed
// receipt settles cleanly; if the provider cannot be reached the node signs
// its own, which the contract records as a dispute.
func (m *Manager) closeSession(ctx context.Context, s *Session, reason string) error {
	receipt, sig, err := m.providerReport(ctx, s.ID, s.Endpoint, s.ProviderKey, s.Provider)
	if err != nil {
		m.log.Warn("provider report unavailable; closing with our own receipt", "session", s.ID.Hex(), "err", err)
		tps, ttft, in, out := s.reportStats()
		receipt, sig, err = morrpc.SignedReport(morrpc.Report{
			SessionID: s.ID, ChainID: m.k.ChainID, Timestamp: morrpc.Now(),
			TPSScaled1000: tps, TTFTMs: ttft, InputTokens: in, OutputTokens: out,
		}, m.w)
		if err != nil {
			return err
		}
	}
	if _, err := m.tx.CloseSession(ctx, receipt, sig); err != nil && !alreadyClosed(err) {
		return err
	}
	s.setState(StateClosed)
	m.mu.Lock()
	m.removeLocked(s)
	m.mu.Unlock()
	m.persist()
	m.log.Info("session closed", "session", s.ID.Hex(), "model", s.ModelName, "reason", reason)
	m.emitf(events.Info, "session %s (%s) closed: %s", short(s.ID), s.ModelName, reason)
	return nil
}

func alreadyClosed(err error) bool {
	var re *chain.RevertError
	return errors.As(err, &re) && re.Name == "SessionAlreadyClosed"
}

// providerReport asks the provider for its signed session report.
func (m *Manager) providerReport(ctx context.Context, id common.Hash, endpoint string, pub []byte, provider common.Address) ([]byte, []byte, error) {
	if endpoint == "" || len(pub) == 0 {
		return nil, nil, errors.New("provider endpoint or key unknown")
	}
	msg, err := morrpc.ReportRequest(newRequestID(), id, m.w)
	if err != nil {
		return nil, nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	res, err := m.rpc.Call(cctx, endpoint, msg)
	if err != nil {
		return nil, nil, err
	}
	rr, err := morrpc.VerifyReportRes(res, pub)
	if err != nil {
		return nil, nil, err
	}
	rep, err := morrpc.DecodeReport(rr.Message)
	if err != nil {
		return nil, nil, fmt.Errorf("undecodable report: %w", err)
	}
	if rep.SessionID != id || rep.ChainID.Cmp(m.k.ChainID) != 0 {
		return nil, nil, errors.New("report is for a different session or chain")
	}
	signer, err := morrpc.RecoverEthMessage(rr.Message, rr.SignedReport)
	if err != nil {
		return nil, nil, err
	}
	if err := m.authorized(ctx, provider, signer); err != nil {
		return nil, nil, err
	}
	return rr.Message, rr.SignedReport, nil
}

// closeUntracked closes a session of ours that this node is not routing over
// (for example one opened by another app with the same wallet).
func (m *Manager) closeUntracked(ctx context.Context, id common.Hash) error {
	cs, err := m.k.GetSession(ctx, id)
	if err != nil {
		return err
	}
	if cs.User != m.w.Address() {
		return errors.New("session belongs to another wallet")
	}
	if cs.Closed() {
		return errors.New("session is already closed")
	}
	s, err := m.describe(ctx, cs)
	if err != nil {
		return err
	}
	s.init()
	return m.closeSession(ctx, s, "requested")
}

// describe rebuilds routing info for an on-chain session (bid → provider →
// ping for the provider key).
func (m *Manager) describe(ctx context.Context, cs *chain.Session) (*Session, error) {
	bid, err := m.k.GetBid(ctx, cs.BidId)
	if err != nil {
		return nil, err
	}
	prov, err := m.k.GetProvider(ctx, bid.Provider)
	if err != nil {
		return nil, err
	}
	s := &Session{
		ID: cs.ID, ModelID: bid.ModelId, BidID: bid.ID, Provider: bid.Provider,
		Endpoint: morrpc.NormalizeEndpoint(prov.Endpoint), Price: bid.PricePerSecond, Stake: cs.Stake,
		OpenedAt: cs.OpenedAtTime(), EndsAt: cs.EndsAtTime(), Direct: cs.IsDirectPaymentFromUser, Adopted: true,
	}
	s.ModelName = common.Hash(bid.ModelId).Hex()[:10]
	if md, err := m.market.Resolve(ctx, common.Hash(bid.ModelId).Hex()); err == nil {
		s.ModelName = md.Alias
	}
	if pub, _, _, err := m.Ping(ctx, s.Endpoint, s.Provider); err == nil {
		s.ProviderKey = pub
	}
	return s, nil
}

// Reconcile restores saved sessions and inspects the wallet's recent on-chain
// sessions: still-open ones are adopted for routing, expired ones are closed
// so their stake is released.
func (m *Manager) Reconcile(ctx context.Context) error {
	me := m.w.Address()
	saved := map[common.Hash]*Session{}
	for _, s := range m.loadPersisted() {
		saved[s.ID] = s
	}
	ids, err := m.k.RecentUserSessionIDs(ctx, me, 30)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	for id := range saved {
		found := false
		for _, x := range ids {
			if x == id {
				found = true
				break
			}
		}
		if !found {
			ids = append(ids, id)
		}
	}
	onchain, err := m.k.GetSessions(ctx, ids)
	if err != nil {
		return fmt.Errorf("read sessions: %w", err)
	}
	now := time.Now()
	var expired []*Session
	restored, adopted := 0, 0
	for _, cs := range onchain {
		if cs.User != me || cs.Closed() || !cs.Exists() {
			continue
		}
		s := saved[cs.ID]
		if s == nil {
			if s, err = m.describe(ctx, cs); err != nil {
				m.log.Warn("cannot inspect session", "session", cs.ID.Hex(), "err", err)
				continue
			}
			if m.opt.Leased != nil && m.opt.Leased(s.ModelID) {
				continue // a storage lease: its owner reconciles it
			}
		} else {
			s.EndsAt = cs.EndsAtTime()
		}
		s.init()
		if !now.Before(s.EndsAt.Add(-usableMargin)) {
			s.setState(StateClosing)
			expired = append(expired, s)
			continue
		}
		if len(s.ProviderKey) == 0 {
			m.log.Warn("provider unreachable; session not routable until it responds", "session", cs.ID.Hex())
			s.mu.Lock()
			s.failures = 3
			s.mu.Unlock()
		}
		m.mu.Lock()
		m.addLocked(s)
		m.mu.Unlock()
		if saved[cs.ID] != nil {
			restored++
		} else {
			adopted++
		}
	}
	if restored+adopted > 0 {
		m.log.Info("resumed open sessions", "restored", restored, "adopted", adopted)
	}
	for _, s := range expired {
		s := s
		m.goTask(func(ctx context.Context) {
			if err := m.closeSession(ctx, s, "expired while offline"); err != nil {
				m.log.Warn("could not close expired session", "session", s.ID.Hex(), "err", err)
			}
		})
	}
	m.persist()
	return nil
}

// Withdraw claims stake released from closed sessions. It returns the amount
// claimed (zero when nothing was available).
func (m *Manager) Withdraw(ctx context.Context) (*big.Int, common.Hash, error) {
	avail, _, err := m.k.UserStakesOnHold(ctx, m.w.Address())
	if err != nil {
		return nil, common.Hash{}, err
	}
	if avail.Sign() == 0 {
		return avail, common.Hash{}, nil
	}
	tx, err := m.tx.WithdrawUserStakes(ctx)
	if err != nil {
		return nil, common.Hash{}, err
	}
	m.emitf(events.Info, "withdrew %s of released stake", mor(avail))
	return avail, tx, nil
}

func (m *Manager) autoWithdraw(ctx context.Context) {
	amt, _, err := m.Withdraw(ctx)
	if err != nil {
		m.log.Debug("auto-withdraw skipped", "err", err)
		return
	}
	if amt.Sign() > 0 {
		m.log.Info("withdrew released stake", "amount", mor(amt))
	}
}
