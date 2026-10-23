package provider

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// health tracks a backend's recent behavior for the model health self-report
// consumers read from our pong (reference: providers/full/model-health).
type health struct {
	mu          sync.Mutex
	status      string
	errorKind   string
	failStreak  int
	limitStreak int
	lastChecked time.Time
	lastHealthy time.Time
	latency     time.Duration
	httpStatus  int
}

// failLimit consecutive failures flip a model to unhealthy (or degraded when
// every failure was a 429).
const failLimit = 3

func (h *health) ok() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failStreak, h.limitStreak, h.httpStatus, h.errorKind = 0, 0, 0, ""
	h.status = morrpc.HealthHealthy
	h.lastChecked, h.lastHealthy = time.Now(), time.Now()
}

func (h *health) fail(status int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failStreak++
	h.limitStreak = 0
	h.lastChecked = time.Now()
	h.httpStatus = status
	if h.failStreak >= failLimit {
		h.status, h.errorKind = morrpc.HealthUnhealthy, "session_errors"
	}
}

func (h *health) limited() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.limitStreak++
	h.lastChecked = time.Now()
	h.httpStatus = http.StatusTooManyRequests
	if h.limitStreak >= failLimit {
		h.status, h.errorKind = morrpc.HealthDegraded, "session_errors"
	}
}

// probed records a scheduled probe result.
func (h *health) probed(status, errorKind string, httpStatus int, latency time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.errorKind, h.httpStatus, h.latency = status, errorKind, httpStatus, latency
	h.lastChecked = time.Now()
	if status == morrpc.HealthHealthy {
		h.lastHealthy, h.failStreak, h.limitStreak = h.lastChecked, 0, 0
	}
}

func (p *Provider) healthStatus(id common.Hash) string {
	p.mu.Lock()
	h := p.health[id]
	p.mu.Unlock()
	if h == nil {
		return morrpc.HealthNoModel
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

// modelType classifies a model for the report: by adapter, and for
// OpenAI-compatible backends by the endpoint it is configured with.
func modelType(m *Model) string {
	switch m.Type() {
	case config.APIClaude:
		return "LLM"
	case config.APIProdiaSD, config.APIProdiaSDXL, config.APIProdiaV2, config.APIHyperbolicSD:
		return "IMAGE"
	case config.APIDecisions:
		return "DECISIONS"
	}
	u := strings.ToLower(strings.TrimRight(m.APIURL, "/"))
	switch {
	case strings.HasSuffix(u, "/embeddings"):
		return "EMBEDDING"
	case strings.HasSuffix(u, "/audio/transcriptions"):
		return "STT"
	case strings.HasSuffix(u, "/audio/speech"):
		return "TTS"
	}
	return "LLM"
}

// probeURL is the cheap listing endpoint used to check a backend without
// spending tokens; empty means the model type is not probed ("skipped").
func probeURL(m *Model) string {
	switch m.Type() {
	case config.APIOpenAI:
		switch modelType(m) {
		case "LLM", "EMBEDDING":
			return endpointFor(m.APIURL, "/models")
		}
	case config.APIClaude:
		u := m.APIURL
		if u == "" {
			u = anthropicURL
		}
		return strings.TrimSuffix(strings.TrimRight(u, "/"), "/messages") + "/models"
	}
	return ""
}

func (p *Provider) healthReports() []morrpc.ModelHealthReport {
	p.mu.Lock()
	bids := make(map[common.Hash]common.Hash, len(p.bids))
	for k, v := range p.bids {
		bids[k] = v
	}
	models := make([]*Model, 0, len(p.models))
	for _, m := range p.models {
		models = append(models, m)
	}
	healths := make(map[common.Hash]*health, len(p.health))
	for k, v := range p.health {
		healths[k] = v
	}
	agents := map[common.Hash]bool{}
	for id := range p.agents {
		agents[id] = true
	}
	p.mu.Unlock()

	out := make([]morrpc.ModelHealthReport, 0, len(models))
	for _, m := range models {
		r := morrpc.ModelHealthReport{ModelID: m.ID.Hex(), ModelName: m.Name, ModelType: modelType(m)}
		bid, hasBid := bids[m.ID]
		if hasBid {
			r.HasActiveBid, r.BidID = true, bid.Hex()
		}
		h := healths[m.ID]
		h.mu.Lock()
		r.Status, r.ErrorKind, r.HTTPStatus = h.status, h.errorKind, h.httpStatus
		r.LastChecked, r.LatencyMs = unix(h.lastChecked), h.latency.Milliseconds()
		r.LastHealthy = unix(h.lastHealthy)
		h.mu.Unlock()
		if !hasBid {
			r.Status = morrpc.HealthNoBid
		}
		out = append(out, r)
	}
	storage := p.storage
	if storage != nil {
		out = append(out, storage.HealthReports(bids)...)
	}
	for modelID, bid := range bids {
		if p.model(modelID) == nil && !agents[modelID] && (storage == nil || !storage.Serves(modelID)) {
			out = append(out, morrpc.ModelHealthReport{
				ModelID: modelID.Hex(), HasActiveBid: true, BidID: bid.Hex(),
				Status: morrpc.HealthNoModel, LastChecked: time.Now().Unix(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelID < out[j].ModelID })
	return out
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// probeAll checks every model with an active bid without spending tokens:
// it lists the backend's models (GET …/models). Image, audio and decisions
// backends have no free probe and report "skipped" until real traffic
// shows how they behave.
func (p *Provider) probeAll(ctx context.Context) {
	p.mu.Lock()
	var models []*Model
	for id, m := range p.models {
		if _, ok := p.bids[id]; ok {
			models = append(models, m)
		}
	}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, m := range models {
		wg.Add(1)
		go func(m *Model) {
			defer wg.Done()
			p.probe(ctx, m)
		}(m)
	}
	wg.Wait()
}

func (p *Provider) probe(ctx context.Context, m *Model) {
	p.mu.Lock()
	h := p.health[m.ID]
	p.mu.Unlock()
	if h == nil {
		return
	}
	url := probeURL(m)
	if url == "" {
		h.mu.Lock()
		if h.lastChecked.IsZero() {
			h.status = morrpc.HealthSkipped
		}
		h.mu.Unlock()
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	if m.Type() == config.APIClaude {
		req.Header.Set("x-api-key", m.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)
	} else if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	start := time.Now()
	resp, err := p.http.Do(req)
	if err != nil {
		kind := "connection"
		if cctx.Err() != nil {
			kind = "timeout"
		}
		p.log.Warn("model backend unreachable", "model", m.Name, "err", err)
		h.probed(morrpc.HealthUnhealthy, kind, 0, 0)
		return
	}
	resp.Body.Close()
	latency := time.Since(start)
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		h.probed(morrpc.HealthDegraded, "rate_limited", resp.StatusCode, latency)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode >= 500:
		p.log.Warn("model backend rejected health probe", "model", m.Name, "status", resp.StatusCode)
		h.probed(morrpc.HealthUnhealthy, "bad_response", resp.StatusCode, latency)
	default:
		// 404 means no listing endpoint, which still proves the host is up.
		h.probed(morrpc.HealthHealthy, "", 0, latency)
	}
}

// CheckBackend probes one model's backend the way the health sweep does, for
// `morcore doctor`. skipped means the type has no free probe.
func CheckBackend(ctx context.Context, m Model) (ok, skipped bool, detail string) {
	url := probeURL(&m)
	if url == "" {
		return true, true, "no free health probe for " + modelType(&m) + " backends; checked with real traffic"
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url, nil)
	if err != nil {
		return false, false, err.Error()
	}
	if m.Type() == config.APIClaude {
		req.Header.Set("x-api-key", m.APIKey)
		req.Header.Set("anthropic-version", anthropicVersion)
	} else if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false, "unreachable: " + err.Error()
	}
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, false, "the backend rejected the API key"
	case resp.StatusCode == http.StatusPaymentRequired:
		return false, false, "the upstream account is out of credit (HTTP 402)"
	case resp.StatusCode >= 500:
		return false, false, fmt.Sprintf("HTTP %d from the backend", resp.StatusCode)
	}
	return true, false, "reachable"
}
