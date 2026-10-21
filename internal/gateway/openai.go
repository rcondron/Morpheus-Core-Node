package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

const maxBody = 16 << 20

type openAIModel struct {
	ID       string         `json:"id"`
	Object   string         `json:"object"`
	Created  int64          `json:"created"`
	OwnedBy  string         `json:"owned_by"`
	Morpheus map[string]any `json:"morpheus"`
}

func toOpenAIModel(m marketplace.Model) openAIModel {
	info := map[string]any{"id": m.ID.Hex(), "name": m.Name, "bids": m.Bids, "tags": m.Tags}
	if m.MinPrice != nil {
		info["min_price_per_second"] = m.MinPrice.String()
	}
	return openAIModel{ID: m.Alias, Object: "model", Created: m.CreatedAt.Unix(), OwnedBy: "morpheus", Morpheus: info}
}

func (s *Server) allowed(p principal, m marketplace.Model) bool {
	return p.admin || p.key.AllowsModel(m.Alias, m.Name, m.ID.Hex())
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.d.Market.Models(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "api_error", "marketplace_unavailable", "could not read the marketplace: "+err.Error())
		return
	}
	p := principalFrom(r)
	data := []openAIModel{}
	for _, m := range models {
		if m.Bids > 0 && s.allowed(p, m) {
			data = append(data, toOpenAIModel(m))
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *Server) getModel(w http.ResponseWriter, r *http.Request) {
	m, err := s.d.Market.Resolve(r.Context(), r.PathValue("id"))
	if err != nil || !s.allowed(principalFrom(r), *m) {
		writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("model %q not found", r.PathValue("id")))
		return
	}
	writeJSON(w, http.StatusOK, toOpenAIModel(*m))
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) { s.infer(w, r, "chat") }
func (s *Server) embeddings(w http.ResponseWriter, r *http.Request)      { s.infer(w, r, "embeddings") }

// infer routes one OpenAI-shaped request to a marketplace provider.
func (s *Server) infer(w http.ResponseWriter, r *http.Request, kind string) {
	ctx := r.Context()
	p := principalFrom(r)
	if s.d.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "consumer_disabled", "this node runs in provider-only mode")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body too large or unreadable")
		return
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "request body must be a JSON object")
		return
	}
	var modelName string
	if err := json.Unmarshal(req["model"], &modelName); err != nil || strings.TrimSpace(modelName) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "model_required", "\"model\" is required (see GET /v1/models)")
		return
	}
	stream := false
	if raw, ok := req["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	switch kind {
	case "chat":
		if _, ok := req["messages"]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "messages_required", "\"messages\" is required")
			return
		}
	case "embeddings":
		stream = false
		if _, ok := req["input"]; !ok {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "input_required", "\"input\" is required")
			return
		}
		req["type"] = json.RawMessage(`"embeddings"`)
	}

	pinned := r.Header.Get("session_id")
	if pinned == "" {
		pinned = r.Header.Get("X-Morpheus-Session")
	}

	model, err := s.d.Market.Resolve(ctx, modelName)
	if err != nil {
		if errors.Is(err, marketplace.ErrModelNotFound) {
			writeError(w, http.StatusNotFound, "invalid_request_error", "model_not_found", fmt.Sprintf("model %q is not offered on the Morpheus marketplace (see GET /v1/models)", modelName))
		} else {
			writeError(w, http.StatusBadGateway, "api_error", "marketplace_unavailable", err.Error())
		}
		return
	}
	if !s.allowed(p, *model) {
		writeError(w, http.StatusForbidden, "permission_error", "model_not_allowed", fmt.Sprintf("this API key may not use %q", model.Alias))
		return
	}
	if !p.admin {
		release, err := s.d.Keys.Admit(p.key.ID)
		if err != nil {
			writeLimitError(w, err)
			return
		}
		defer release()
	}

	forward, err := json.Marshal(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", err.Error())
		return
	}

	rec := events.Request{Time: time.Now(), Key: p.name(), Model: model.Alias, Kind: kind, Stream: stream}
	exclude := map[common.Address]bool{}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		lease, err := s.acquire(ctx, model, pinned, exclude)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if lastErr != nil {
				err = fmt.Errorf("%v (after provider failure: %v)", err, lastErr)
			}
			status := s.writeAcquireError(w, err)
			s.finish(p, rec, status, nil, err)
			return
		}
		sess := lease.Session
		rec.Session, rec.Provider = sess.ID.Hex(), sess.Provider.Hex()
		out := newResponder(w, stream, sess)
		res, err := s.d.Sessions.Send(ctx, lease, forward, out.chunk)
		var ee *morrpc.EngineError
		sessionOK := err == nil || errors.As(err, &ee) || ctx.Err() != nil
		lease.Release(sessionOK, tokens(res, true), tokens(res, false), ttft(res))

		var pe *consumer.ProviderError
		if errors.As(err, &pe) && pe.BeforeOutput && pinned == "" && ctx.Err() == nil {
			exclude[sess.Provider] = true
			lastErr = pe.Err
			s.d.Log.Warn("provider failed before responding; retrying on another session", "model", model.Alias, "provider", sess.Provider.Hex(), "err", pe.Err)
			continue
		}
		status := out.finish(err)
		s.finish(p, rec, status, res, err)
		return
	}
	writeError(w, http.StatusBadGateway, "api_error", "provider_error", "providers failed: "+errString(lastErr))
	s.finish(p, rec, http.StatusBadGateway, nil, lastErr)
}

func errString(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}

func tokens(r *consumer.Result, in bool) int {
	if r == nil {
		return 0
	}
	if in {
		return r.InputTokens
	}
	return r.OutputTokens
}

func ttft(r *consumer.Result) time.Duration {
	if r == nil {
		return 0
	}
	return r.TTFT
}

func (s *Server) acquire(ctx context.Context, model *marketplace.Model, pinned string, exclude map[common.Address]bool) (*consumer.Lease, error) {
	if pinned != "" {
		if !strings.HasPrefix(pinned, "0x") || len(pinned) != 66 {
			return nil, fmt.Errorf("%w: malformed session id", consumer.ErrSessionNotFound)
		}
		return s.d.Sessions.AcquireSession(ctx, common.HexToHash(pinned))
	}
	return s.d.Sessions.Acquire(ctx, model, exclude)
}

func (s *Server) writeAcquireError(w http.ResponseWriter, err error) int {
	var np *consumer.NoProviderError
	switch {
	case errors.As(err, &np):
		writeError(w, http.StatusServiceUnavailable, "api_error", "no_provider", err.Error())
		return http.StatusServiceUnavailable
	case errors.Is(err, consumer.ErrQueueTimeout):
		w.Header().Set("Retry-After", "5")
		writeError(w, http.StatusServiceUnavailable, "api_error", "queue_timeout", "all sessions for this model are busy; try again")
		return http.StatusServiceUnavailable
	case errors.Is(err, consumer.ErrNoCapacity):
		writeError(w, http.StatusServiceUnavailable, "api_error", "no_capacity", err.Error())
		return http.StatusServiceUnavailable
	case errors.Is(err, consumer.ErrSessionNotFound):
		writeError(w, http.StatusNotFound, "invalid_request_error", "session_not_found", err.Error())
		return http.StatusNotFound
	case errors.Is(err, consumer.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "api_error", "shutting_down", err.Error())
		return http.StatusServiceUnavailable
	default:
		writeError(w, http.StatusBadGateway, "api_error", "session_error", err.Error())
		return http.StatusBadGateway
	}
}

func (s *Server) finish(p principal, rec events.Request, status int, res *consumer.Result, err error) {
	rec.Status = status
	rec.Latency = time.Since(rec.Time)
	if res != nil {
		rec.TTFT, rec.InputTokens, rec.OutputTokens = res.TTFT, res.InputTokens, res.OutputTokens
	}
	if err != nil {
		rec.Error = err.Error()
	}
	if !p.admin {
		s.d.Keys.Record(p.key.ID, rec.InputTokens, rec.OutputTokens, status >= 400)
	}
	s.d.Bus.Record(rec)
}

// responder adapts provider payloads to the client's requested format.
type responder struct {
	w       http.ResponseWriter
	stream  bool
	sess    *consumer.Session
	started bool
	chunks  [][]byte
}

func newResponder(w http.ResponseWriter, stream bool, sess *consumer.Session) *responder {
	return &responder{w: w, stream: stream, sess: sess}
}

func (o *responder) headers() {
	h := o.w.Header()
	h.Set("X-Morpheus-Session", o.sess.ID.Hex())
	h.Set("X-Morpheus-Provider", o.sess.Provider.Hex())
}

func (o *responder) chunk(payload []byte) error {
	if !o.stream {
		o.chunks = append(o.chunks, append([]byte(nil), payload...))
		return nil
	}
	if !o.started {
		o.started = true
		o.headers()
		h := o.w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
		h.Set("X-Accel-Buffering", "no")
		o.w.WriteHeader(http.StatusOK)
	}
	if _, err := fmt.Fprintf(o.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// finish completes the response and returns the HTTP status sent.
func (o *responder) finish(err error) int {
	var ee *morrpc.EngineError
	if err != nil && !o.started {
		o.headers()
		if errors.As(err, &ee) {
			o.w.Header().Set("Content-Type", "application/json")
			o.w.WriteHeader(ee.HTTPStatus())
			_, _ = o.w.Write(engineErrorBody(ee))
			return ee.HTTPStatus()
		}
		var pe *consumer.ProviderError
		if errors.As(err, &pe) {
			writeError(o.w, http.StatusBadGateway, "api_error", "provider_error", err.Error())
			return http.StatusBadGateway
		}
		writeError(o.w, http.StatusBadGateway, "api_error", "upstream_error", err.Error())
		return http.StatusBadGateway
	}
	if o.stream {
		if err != nil {
			msg, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "api_error"}})
			_, _ = fmt.Fprintf(o.w, "data: %s\n\n", msg)
		}
		_, _ = io.WriteString(o.w, "data: [DONE]\n\n")
		if f, ok := o.w.(http.Flusher); ok {
			f.Flush()
		}
		return http.StatusOK
	}
	o.headers()
	o.w.Header().Set("Content-Type", "application/json")
	o.w.WriteHeader(http.StatusOK)
	switch len(o.chunks) {
	case 0:
		_, _ = o.w.Write([]byte(`{}`))
	case 1:
		_, _ = o.w.Write(o.chunks[0])
	default:
		_, _ = o.w.Write(aggregate(o.chunks))
	}
	return http.StatusOK
}

// engineErrorBody returns the backend's own error object when it looks like
// one, otherwise wraps it OpenAI-style.
func engineErrorBody(ee *morrpc.EngineError) []byte {
	if m, ok := ee.ProviderModelError.(map[string]any); ok {
		if _, has := m["error"]; has {
			b, _ := json.Marshal(m)
			return b
		}
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": fmt.Sprint(ee.ProviderModelError), "type": "provider_model_error", "code": ee.StatusCode,
	}})
	return b
}

// aggregate folds streamed chat chunks into one chat.completion for clients
// that asked for a non-streaming response.
func aggregate(chunks [][]byte) []byte {
	var (
		id, model, finish string
		created           int64
		content           strings.Builder
		usage             json.RawMessage
	)
	for _, c := range chunks {
		var v struct {
			ID      string          `json:"id"`
			Model   string          `json:"model"`
			Created int64           `json:"created"`
			Usage   json.RawMessage `json:"usage"`
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal(c, &v) != nil {
			continue
		}
		if id == "" {
			id, model, created = v.ID, v.Model, v.Created
		}
		for _, ch := range v.Choices {
			content.WriteString(ch.Delta.Content)
			if ch.FinishReason != "" {
				finish = ch.FinishReason
			}
		}
		if len(v.Usage) > 0 && string(v.Usage) != "null" {
			usage = v.Usage
		}
	}
	out := map[string]any{
		"id": id, "object": "chat.completion", "created": created, "model": model,
		"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": content.String()}, "finish_reason": finish}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	b, _ := json.Marshal(out)
	return b
}
