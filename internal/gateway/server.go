// Package gateway serves the node's HTTP API: an OpenAI-compatible inference
// surface for agents (/v1) and a management surface (/admin) for the CLI.
package gateway

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/buildinfo"
	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
)

// Deps are the node components the gateway serves.
type Deps struct {
	Market     *marketplace.Market
	Sessions   *consumer.Manager // nil when the consumer role is off
	Keys       *apikeys.Store
	Bus        *events.Bus
	AdminToken string
	Status     func(ctx context.Context) any
	CORS       []string
	Log        *slog.Logger
}

// Server is the HTTP API.
type Server struct {
	d   Deps
	mux *http.ServeMux
	srv *http.Server
}

// New builds the API server.
func New(d Deps) *Server {
	s := &Server{d: d, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", s.health)
	m.HandleFunc("GET /", s.root)

	m.Handle("GET /v1/models", s.auth(apikeys.ScopeInference, s.listModels))
	m.Handle("GET /v1/models/{id...}", s.auth(apikeys.ScopeInference, s.getModel))
	m.Handle("POST /v1/chat/completions", s.auth(apikeys.ScopeInference, s.chatCompletions))
	m.Handle("POST /v1/embeddings", s.auth(apikeys.ScopeInference, s.embeddings))

	m.Handle("GET /admin/status", s.auth(apikeys.ScopeSessions, s.status))
	m.Handle("GET /admin/events", s.auth(apikeys.ScopeSessions, s.recentEvents))
	m.Handle("GET /admin/models", s.auth(apikeys.ScopeSessions, s.adminModels))
	m.Handle("GET /admin/models/{id}/bids", s.auth(apikeys.ScopeSessions, s.adminBids))
	m.Handle("GET /admin/sessions", s.auth(apikeys.ScopeSessions, s.listSessions))
	m.Handle("POST /admin/sessions", s.auth(apikeys.ScopeSessions, s.openSession))
	m.Handle("DELETE /admin/sessions/{id}", s.auth(apikeys.ScopeSessions, s.closeSession))
	m.Handle("POST /admin/stakes/withdraw", s.auth(apikeys.ScopeSessions, s.withdraw))
	m.Handle("GET /admin/keys", s.auth(apikeys.ScopeAdmin, s.listKeys))
	m.Handle("POST /admin/keys", s.auth(apikeys.ScopeAdmin, s.createKey))
	m.Handle("PATCH /admin/keys/{ref}", s.auth(apikeys.ScopeAdmin, s.updateKey))
	m.Handle("DELETE /admin/keys/{ref}", s.auth(apikeys.ScopeAdmin, s.revokeKey))
	m.Handle("POST /admin/keys/{ref}/rotate", s.auth(apikeys.ScopeAdmin, s.rotateKey))
}

// Handler exposes the router (with middleware) for tests and embedding.
func (s *Server) Handler() http.Handler { return s.middleware(s.mux) }

// Serve listens on addr until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.srv = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 16,
	}
	errc := make(chan error, 1)
	go func() { errc <- s.srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(sctx)
		return nil
	}
}

// ---------------------------------------------------------------------------
// Auth
// ---------------------------------------------------------------------------

type principal struct {
	admin bool
	key   apikeys.Key
}

func (p principal) name() string {
	if p.admin {
		return "admin"
	}
	return p.key.Name
}

type ctxKey struct{}

func principalFrom(r *http.Request) principal {
	p, _ := r.Context().Value(ctxKey{}).(principal)
	return p
}

func bearer(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return strings.TrimSpace(after)
		}
		if after, ok := strings.CutPrefix(h, "bearer "); ok {
			return strings.TrimSpace(after)
		}
	}
	if k := r.Header.Get("X-Api-Key"); k != "" {
		return strings.TrimSpace(k)
	}
	return ""
}

func (s *Server) auth(scope string, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := bearer(r)
		if tok == "" {
			writeError(w, http.StatusUnauthorized, "authentication_error", "missing_api_key",
				"Provide an API key: Authorization: Bearer sk-mor-…  (create one with `morcore keys create`)")
			return
		}
		var p principal
		if s.d.AdminToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.d.AdminToken)) == 1 {
			p.admin = true
		} else {
			k, err := s.d.Keys.Authenticate(tok)
			if err != nil {
				writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", err.Error())
				return
			}
			if !k.HasScope(scope) {
				writeError(w, http.StatusForbidden, "permission_error", "insufficient_scope",
					"this key does not have the "+scope+" scope")
				return
			}
			p.key = k
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, p)))
	})
}

// ---------------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------------

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		reqID := r.Header.Get("X-Request-Id")
		if reqID == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			reqID = hex.EncodeToString(b)
		}
		w.Header().Set("X-Request-Id", reqID)
		w.Header().Set("Server", "morcore/"+buildinfo.Version)
		if s.cors(w, r) {
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				s.d.Log.Error("panic in handler", "path", r.URL.Path, "panic", v)
				if rec.status == 0 {
					writeError(rec, http.StatusInternalServerError, "api_error", "internal_error", "internal error")
				}
			}
			if r.URL.Path != "/healthz" {
				s.d.Log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", rec.status, "ms", time.Since(start).Milliseconds(), "id", reqID)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// cors applies configured CORS rules; it returns true when it fully handled a preflight.
func (s *Server) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || len(s.d.CORS) == 0 {
		return false
	}
	allowed := false
	for _, o := range s.d.CORS {
		if o == "*" || strings.EqualFold(o, origin) {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Request-Id, session_id")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
	h.Set("Access-Control-Expose-Headers", "X-Request-Id, X-Morpheus-Session, X-Morpheus-Provider")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

// writeError emits an OpenAI-style error body.
func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": code, "param": nil}})
}

func writeLimitError(w http.ResponseWriter, err error) {
	var le *apikeys.LimitError
	if errors.As(err, &le) {
		secs := int(le.RetryAfter.Seconds() + 0.999)
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
	}
	code := "rate_limit_exceeded"
	if errors.Is(err, apikeys.ErrQuota) {
		code = "insufficient_quota"
	}
	writeError(w, http.StatusTooManyRequests, "rate_limit_error", code, err.Error())
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "version": buildinfo.Version})
}

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not_found_error", "not_found", "no route for "+r.URL.Path)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":    "Morpheus Core Node",
		"version": buildinfo.Version,
		"openai":  "/v1",
		"docs":    "https://github.com/rcondron/Morpheus-Core-Node#agent-api",
	})
}
