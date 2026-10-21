// Package consumer is the consumer half of the routing node: it opens
// marketplace sessions on demand, pools them per model, routes agent requests
// across them concurrently and settles them on-chain.
package consumer

import (
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Session states.
const (
	StateActive  = "active"
	StateClosing = "closing"
	StateClosed  = "closed"
	StateFailed  = "failed"
)

// Session is an open marketplace session this node can route requests over.
type Session struct {
	ID          common.Hash      `json:"id"`
	ModelID     common.Hash      `json:"modelId"`
	ModelName   string           `json:"modelName"`
	BidID       common.Hash      `json:"bidId"`
	Provider    common.Address   `json:"provider"`
	Endpoint    string           `json:"endpoint"`
	ProviderKey morrpc.HexString `json:"providerKey"`
	Price       *big.Int         `json:"pricePerSecond"`
	Stake       *big.Int         `json:"stake"`
	OpenedAt    time.Time        `json:"openedAt"`
	EndsAt      time.Time        `json:"endsAt"`
	Direct      bool             `json:"directPayment"`
	Adopted     bool             `json:"adopted,omitempty"`

	Stats Stats `json:"stats"`

	mu       sync.Mutex
	state    string
	slot     chan struct{}
	lastUsed time.Time
	inflight int
	failures int
}

// Stats accumulate per-session quality data for the closeout report.
type Stats struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
	TTFTms       []int64 `json:"ttftMs,omitempty"`
	TPSx1000     []int64 `json:"tpsX1000,omitempty"`
}

const maxSamples = 256

func (s *Session) init() {
	if s.slot == nil {
		s.slot = make(chan struct{}, 1)
	}
	if s.state == "" {
		s.state = StateActive
	}
	if s.lastUsed.IsZero() {
		s.lastUsed = time.Now()
	}
}

// State returns the lifecycle state.
func (s *Session) State() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Session) setState(st string) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
}

// usable reports whether new requests may be routed here: active, healthy and
// with enough time left that a request will not run into the session end.
func (s *Session) usable(now time.Time, margin time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state == StateActive && s.failures < 3 && s.EndsAt.Sub(now) > margin
}

// tryAcquire claims the session's request slot (providers serve one request
// per session at a time).
func (s *Session) tryAcquire() bool {
	select {
	case s.slot <- struct{}{}:
		s.mu.Lock()
		s.inflight++
		s.lastUsed = time.Now()
		s.mu.Unlock()
		return true
	default:
		return false
	}
}

func (s *Session) release(ok bool, in, out int, ttft, dur time.Duration) {
	s.mu.Lock()
	s.inflight--
	s.lastUsed = time.Now()
	s.Stats.Requests++
	if ok {
		s.failures = 0
		s.Stats.InputTokens += int64(in)
		s.Stats.OutputTokens += int64(out)
		if ttft > 0 {
			s.Stats.TTFTms = appendCapped(s.Stats.TTFTms, ttft.Milliseconds())
		}
		if secs := dur.Seconds(); secs > 0 && out > 0 {
			s.Stats.TPSx1000 = appendCapped(s.Stats.TPSx1000, int64(float64(out)/secs*1000))
		}
	} else {
		s.failures++
		s.Stats.Errors++
	}
	s.mu.Unlock()
	<-s.slot
}

func appendCapped(xs []int64, v int64) []int64 {
	xs = append(xs, v)
	if len(xs) > maxSamples {
		xs = xs[len(xs)-maxSamples:]
	}
	return xs
}

func (s *Session) failing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failures >= 3
}

// Busy reports whether a request is in flight.
func (s *Session) Busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inflight > 0
}

// IdleFor reports how long the session has had no traffic.
func (s *Session) IdleFor(now time.Time) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inflight > 0 {
		return 0
	}
	return now.Sub(s.lastUsed)
}

// reportStats returns the averages used in a user-signed closeout report.
func (s *Session) reportStats() (tps, ttft uint32, in, out uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return uint32(avg(s.Stats.TPSx1000)), uint32(avg(s.Stats.TTFTms)), uint32(s.Stats.InputTokens), uint32(s.Stats.OutputTokens)
}

func avg(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	var t int64
	for _, x := range xs {
		t += x
	}
	return t / int64(len(xs))
}

// Snapshot is a read-only view for status displays.
type Snapshot struct {
	ID        string    `json:"id"`
	Model     string    `json:"model"`
	ModelID   string    `json:"modelId"`
	Provider  string    `json:"provider"`
	Endpoint  string    `json:"endpoint"`
	State     string    `json:"state"`
	Busy      bool      `json:"busy"`
	OpenedAt  time.Time `json:"openedAt"`
	EndsAt    time.Time `json:"endsAt"`
	Stake     string    `json:"stakeWei"`
	Price     string    `json:"pricePerSecondWei"`
	Requests  int64     `json:"requests"`
	Errors    int64     `json:"errors"`
	TokensIn  int64     `json:"inputTokens"`
	TokensOut int64     `json:"outputTokens"`
	Adopted   bool      `json:"adopted,omitempty"`
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		ID: s.ID.Hex(), Model: s.ModelName, ModelID: s.ModelID.Hex(), Provider: s.Provider.Hex(), Endpoint: s.Endpoint,
		State: s.state, Busy: s.inflight > 0, OpenedAt: s.OpenedAt, EndsAt: s.EndsAt,
		Stake: bigStr(s.Stake), Price: bigStr(s.Price), Requests: s.Stats.Requests, Errors: s.Stats.Errors,
		TokensIn: s.Stats.InputTokens, TokensOut: s.Stats.OutputTokens, Adopted: s.Adopted,
	}
}

func bigStr(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return v.String()
}
