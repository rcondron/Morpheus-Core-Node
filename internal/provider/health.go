package provider

import (
	"context"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// health tracks a backend's recent behavior for the model health self-report
// consumers read from our pong.
type health struct {
	mu          sync.Mutex
	status      string
	failStreak  int
	limitStreak int
	lastChecked time.Time
	lastHealthy time.Time
	latency     time.Duration
	httpStatus  int
}

func (h *health) ok() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failStreak, h.limitStreak, h.httpStatus = 0, 0, 0
	h.status = morrpc.HealthHealthy
	h.lastChecked, h.lastHealthy = time.Now(), time.Now()
}

func (h *health) fail() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failStreak++
	h.lastChecked = time.Now()
	if h.failStreak >= 3 {
		h.status = morrpc.HealthUnhealthy
	}
}

func (h *health) limited() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.limitStreak++
	h.lastChecked = time.Now()
	h.httpStatus = http.StatusTooManyRequests
	if h.limitStreak >= 3 {
		h.status = morrpc.HealthDegraded
	}
}

func (p *Provider) healthStatus(id common.Hash) string {
	h := p.health[id]
	if h == nil {
		return morrpc.HealthNoModel
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (p *Provider) healthReports() []morrpc.ModelHealthReport {
	p.mu.Lock()
	bids := make(map[common.Hash]common.Hash, len(p.bids))
	for k, v := range p.bids {
		bids[k] = v
	}
	p.mu.Unlock()
	out := make([]morrpc.ModelHealthReport, 0, len(p.models))
	for id, m := range p.models {
		h := p.health[id]
		h.mu.Lock()
		r := morrpc.ModelHealthReport{
			ModelID: id.Hex(), ModelName: m.Name, ModelType: "LLM", Status: h.status,
			LastChecked: h.lastChecked.Unix(), LatencyMs: h.latency.Milliseconds(), HTTPStatus: h.httpStatus,
		}
		if !h.lastHealthy.IsZero() {
			r.LastHealthy = h.lastHealthy.Unix()
		}
		h.mu.Unlock()
		if bid, ok := bids[id]; ok {
			r.HasActiveBid, r.BidID = true, bid.Hex()
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelName < out[j].ModelName })
	return out
}

// probeAll checks every backend is reachable without spending tokens: it
// lists models (GET …/models). Missing listing endpoints count as reachable.
func (p *Provider) probeAll(ctx context.Context) {
	var wg sync.WaitGroup
	for id, m := range p.models {
		wg.Add(1)
		go func(id common.Hash, m *Model) {
			defer wg.Done()
			h := p.health[id]
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(cctx, http.MethodGet, modelsURL(m.APIURL), nil)
			if err != nil {
				return
			}
			if m.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+m.APIKey)
			}
			start := time.Now()
			resp, err := p.http.Do(req)
			if err != nil {
				p.log.Warn("model backend unreachable", "model", m.Name, "url", m.APIURL, "err", err)
				h.mu.Lock()
				h.status, h.lastChecked, h.failStreak = morrpc.HealthUnhealthy, time.Now(), 3
				h.mu.Unlock()
				return
			}
			resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode >= 500:
				p.log.Warn("model backend rejected health probe", "model", m.Name, "status", resp.StatusCode)
				h.mu.Lock()
				h.status, h.lastChecked, h.httpStatus = morrpc.HealthUnhealthy, time.Now(), resp.StatusCode
				h.mu.Unlock()
			default:
				h.ok()
				h.mu.Lock()
				h.latency = time.Since(start)
				h.mu.Unlock()
			}
		}(id, m)
	}
	wg.Wait()
}
