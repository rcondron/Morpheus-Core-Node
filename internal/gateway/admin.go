package gateway

import (
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/units"
)

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	if s.d.Status == nil {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, s.d.Status(r.Context()))
}

func (s *Server) recentEvents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"events":   s.d.Bus.RecentEvents(100),
		"requests": s.d.Bus.RecentRequests(100),
		"totals":   s.d.Bus.Totals(),
	})
}

// ModelView is the admin listing of a marketplace model.
type ModelView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Alias    string   `json:"alias"`
	Tags     []string `json:"tags"`
	Bids     int      `json:"bids"`
	MinPrice string   `json:"minPricePerSecond,omitempty"`
	MaxPrice string   `json:"maxPricePerSecond,omitempty"`
	MinHour  string   `json:"minPricePerHour,omitempty"`
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" {
		if err := s.d.Market.Refresh(r.Context()); err != nil {
			writeError(w, http.StatusBadGateway, "api_error", "marketplace_unavailable", err.Error())
			return
		}
	}
	models, err := s.d.Market.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "marketplace_unavailable", err.Error())
		return
	}
	out := make([]ModelView, 0, len(models))
	for _, m := range models {
		v := ModelView{ID: m.ID.Hex(), Name: m.Name, Alias: m.Alias, Tags: m.Tags, Bids: m.Bids}
		if m.MinPrice != nil {
			v.MinPrice, v.MaxPrice = m.MinPrice.String(), m.MaxPrice.String()
			v.MinHour = units.FormatToken(new(bigInt).Mul(m.MinPrice, hour), 6)
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// BidView is the admin listing of a provider bid.
type BidView struct {
	ID       string  `json:"id"`
	Provider string  `json:"provider"`
	Endpoint string  `json:"endpoint"`
	Price    string  `json:"pricePerSecond"`
	Score    float64 `json:"score"`
	Stake    string  `json:"estimatedStake,omitempty"`
}

func (s *Server) adminBids(w http.ResponseWriter, r *http.Request) {
	m, err := s.d.Market.Resolve(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error())
		return
	}
	bids, err := s.d.Market.RatedBids(r.Context(), m.ID)
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "marketplace_unavailable", err.Error())
		return
	}
	dur := time.Hour
	if d, err := time.ParseDuration(r.URL.Query().Get("duration")); err == nil && d > 0 {
		dur = d
	}
	out := make([]BidView, 0, len(bids))
	for _, b := range bids {
		v := BidView{ID: b.Bid.ID.Hex(), Provider: b.Bid.Provider.Hex(), Endpoint: b.Provider.Endpoint, Price: b.Bid.PricePerSecond.String(), Score: b.Score}
		if st, err := s.d.Market.EstimateStake(r.Context(), b.Bid.PricePerSecond, dur); err == nil {
			v.Stake = st.String()
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": m.Alias, "modelId": m.ID.Hex(), "duration": dur.String(), "bids": out})
}

func (s *Server) needSessions(w http.ResponseWriter) bool {
	if s.d.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "consumer_disabled", "this node runs in provider-only mode")
		return false
	}
	return true
}

func (s *Server) listSessions(w http.ResponseWriter, _ *http.Request) {
	if !s.needSessions(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions":    s.d.Sessions.Sessions(),
		"pools":       s.d.Sessions.Pools(),
		"lockedStake": s.d.Sessions.LockedStake().String(),
	})
}

func (s *Server) openSession(w http.ResponseWriter, r *http.Request) {
	if !s.needSessions(w) {
		return
	}
	var body struct {
		Model string `json:"model"`
		Bid   string `json:"bid"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.Model == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model_required", "body must be {\"model\": \"<name or id>\"}")
		return
	}
	m, err := s.d.Market.Resolve(r.Context(), body.Model)
	if err != nil {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", err.Error())
		return
	}
	var bid common.Hash
	if body.Bid != "" {
		bid = common.HexToHash(body.Bid)
	}
	sess, err := s.d.Sessions.Open(r.Context(), m, bid)
	if err != nil {
		var np *consumer.NoProviderError
		status := http.StatusBadGateway
		if errors.As(err, &np) || errors.Is(err, consumer.ErrNoCapacity) {
			status = http.StatusServiceUnavailable
		}
		writeError(w, status, "api_error", "open_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sess.Snapshot())
}

func (s *Server) closeSession(w http.ResponseWriter, r *http.Request) {
	if !s.needSessions(w) {
		return
	}
	id := r.PathValue("id")
	if !strings.HasPrefix(id, "0x") || len(id) != 66 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad_session_id", "session id must be 0x-prefixed 32-byte hex")
		return
	}
	if err := s.d.Sessions.Close(r.Context(), common.HexToHash(id)); err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "close_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"closed": id})
}

func (s *Server) withdraw(w http.ResponseWriter, r *http.Request) {
	if !s.needSessions(w) {
		return
	}
	amt, tx, err := s.d.Sessions.Withdraw(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "withdraw_failed", err.Error())
		return
	}
	res := map[string]any{"amount": amt.String()}
	if tx != (common.Hash{}) {
		res["tx"] = tx.Hex()
	}
	writeJSON(w, http.StatusOK, res)
}

// KeyView is an API key as shown to operators (never the hash or secret).
type KeyView struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Prefix      string         `json:"prefix"`
	Status      string         `json:"status"`
	Scopes      []string       `json:"scopes"`
	Models      []string       `json:"models,omitempty"`
	RPM         int            `json:"rpm,omitempty"`
	Concurrency int            `json:"concurrency,omitempty"`
	DailyTokens int64          `json:"dailyTokens,omitempty"`
	CreatedAt   time.Time      `json:"createdAt"`
	ExpiresAt   *time.Time     `json:"expiresAt,omitempty"`
	LastUsedAt  *time.Time     `json:"lastUsedAt,omitempty"`
	Usage       apikeys.Usage  `json:"usage"`
	Secret      string         `json:"secret,omitempty"`
	Extra       map[string]any `json:"-"`
}

func ViewKey(k apikeys.Key) KeyView {
	return KeyView{
		ID: k.ID, Name: k.Name, Prefix: k.Prefix, Status: k.Status(time.Now()), Scopes: k.Scopes, Models: k.Models,
		RPM: k.RPM, Concurrency: k.Concurrency, DailyTokens: k.DailyTokens, CreatedAt: k.CreatedAt,
		ExpiresAt: k.ExpiresAt, LastUsedAt: k.LastUsedAt, Usage: k.Usage,
	}
}

func (s *Server) listKeys(w http.ResponseWriter, _ *http.Request) {
	keys := s.d.Keys.List()
	out := make([]KeyView, len(keys))
	for i, k := range keys {
		out[i] = ViewKey(k)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// KeyRequest is the body of key create/update calls.
type KeyRequest struct {
	Name        *string   `json:"name"`
	Scopes      []string  `json:"scopes"`
	Models      *[]string `json:"models"`
	RPM         *int      `json:"rpm"`
	Concurrency *int      `json:"concurrency"`
	DailyTokens *int64    `json:"dailyTokens"`
	ExpiresIn   *string   `json:"expiresIn"`
}

func (k KeyRequest) ttl() (*time.Duration, error) {
	if k.ExpiresIn == nil {
		return nil, nil
	}
	if *k.ExpiresIn == "" || *k.ExpiresIn == "never" {
		var zero time.Duration
		return &zero, nil
	}
	d, err := ParseTTL(*k.ExpiresIn)
	return &d, err
}

// ParseTTL accepts Go durations plus day units ("30d").
func ParseTTL(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if strings.HasSuffix(v, "d") {
		d, err := time.ParseDuration(strings.TrimSuffix(v, "d") + "h")
		return d * 24, err
	}
	return time.ParseDuration(v)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var req KeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	opts := apikeys.CreateOptions{Scopes: req.Scopes}
	if req.Name != nil {
		opts.Name = *req.Name
	}
	if req.Models != nil {
		opts.Models = *req.Models
	}
	if req.RPM != nil {
		opts.RPM = *req.RPM
	}
	if req.Concurrency != nil {
		opts.Concurrency = *req.Concurrency
	}
	if req.DailyTokens != nil {
		opts.DailyTokens = *req.DailyTokens
	}
	ttl, err := req.ttl()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad_expiry", err.Error())
		return
	}
	if ttl != nil {
		opts.TTL = *ttl
	}
	secret, k, err := s.d.Keys.Create(opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "create_failed", err.Error())
		return
	}
	v := ViewKey(k)
	v.Secret = secret
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	var req KeyRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}
	ttl, err := req.ttl()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "bad_expiry", err.Error())
		return
	}
	k, err := s.d.Keys.Update(r.PathValue("ref"), apikeys.Update{
		Name: req.Name, Scopes: req.Scopes, Models: req.Models, RPM: req.RPM, Concurrency: req.Concurrency,
		DailyTokens: req.DailyTokens, TTL: ttl,
	})
	if err != nil {
		s.keyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ViewKey(k))
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.d.Keys.Revoke(r.PathValue("ref"))
	if err != nil {
		s.keyError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ViewKey(k))
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) {
	secret, k, err := s.d.Keys.Rotate(r.PathValue("ref"))
	if err != nil {
		s.keyError(w, err)
		return
	}
	v := ViewKey(k)
	v.Secret = secret
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) keyError(w http.ResponseWriter, err error) {
	if errors.Is(err, apikeys.ErrNotFound) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "key_not_found", err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, "invalid_request_error", "key_error", err.Error())
}

type bigInt = big.Int

var hour = big.NewInt(3600)
