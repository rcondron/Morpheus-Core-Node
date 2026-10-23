// Package e2e runs the whole routing path in-process: an agent calls the
// gateway, the consumer opens a session on a fake chain, prompts travel over
// real MOR-RPC/TCP to our provider, which calls a fake OpenAI backend.
package e2e

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/chain/chaintest"
	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/events"
	"github.com/rcondron/Morpheus-Core-Node/internal/gateway"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/ipfstest"
	"github.com/rcondron/Morpheus-Core-Node/internal/ipfs/kubo"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
	"github.com/rcondron/Morpheus-Core-Node/internal/provider"
	"github.com/rcondron/Morpheus-Core-Node/internal/storage"
	"github.com/rcondron/Morpheus-Core-Node/internal/wallet"
)

func logger() *slog.Logger {
	if os.Getenv("E2E_VERBOSE") != "" {
		return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeBackend imitates an OpenAI-compatible model server.
type fakeBackend struct {
	srv      *httptest.Server
	inflight atomic.Int32
	peak     atomic.Int32
	calls    atomic.Int32
	delay    time.Duration
}

func newBackend(delay time.Duration) *fakeBackend {
	b := &fakeBackend{delay: delay}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"backend-model"}]}`)
			return
		}
		b.calls.Add(1)
		n := b.inflight.Add(1)
		defer b.inflight.Add(-1)
		for {
			p := b.peak.Load()
			if n <= p || b.peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(b.delay)
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "backend-model" {
			http.Error(w, `{"error":{"message":"wrong model name"}}`, http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"model":"backend-model","usage":{"prompt_tokens":3,"total_tokens":3}}`)
			return
		}
		msgs := req["messages"].([]any)
		last := msgs[len(msgs)-1].(map[string]any)["content"].(string)
		if last == "rate-limit-me" {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"slow down","type":"rate_limit"}}`)
			return
		}
		answer := "echo: " + last
		if req["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for i, word := range strings.Fields(answer) {
				if i > 0 {
					word = " " + word
				}
				chunk, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "backend-model",
					"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": word}}}})
				fmt.Fprintf(w, "data: %s\n\n", chunk)
				w.(http.Flusher).Flush()
			}
			fmt.Fprintf(w, "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"backend-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":7,\"total_tokens\":12}}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "r1", "object": "chat.completion", "created": 1, "model": "backend-model",
			"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": answer}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}})
	}))
	return b
}

type harness struct {
	t        *testing.T
	ids      map[string]common.Hash
	fc       *chaintest.Chain
	backend  *fakeBackend
	consumer *wallet.Wallet
	provW    *wallet.Wallet
	modelID  common.Hash
	mgr      *consumer.Manager
	keys     *apikeys.Store
	gw       *httptest.Server
	bus      *events.Bus
	admin    string
	cancel   context.CancelFunc

	// Storage, when an offering is a storage listing.
	kubo       *ipfstest.Node
	storageSrv *storage.Server
	store      *storage.Store
	ck         *chain.Contracts
	market     *marketplace.Market
	dir        string
}

// offering is an extra model, agent or storage listing the test provider
// sells.
type offering struct {
	name  string
	model *provider.Model // ID is filled in
	agent *provider.Agent
	quota uint64 // a storage listing of this many bytes per lease
}

func newHarness(t *testing.T, maxPerModel int, backendDelay time.Duration, extra ...offering) *harness {
	t.Helper()
	h := &harness{t: t, admin: "admin-token-123", bus: events.New(), ids: map[string]common.Hash{}}
	log := logger()
	h.fc = chaintest.New(84532)
	t.Cleanup(h.fc.Close)
	h.backend = newBackend(backendDelay)
	t.Cleanup(h.backend.srv.Close)

	var err error
	h.consumer, _ = wallet.Generate()
	h.provW, _ = wallet.Generate()
	h.fc.Fund(h.consumer.Address(), chaintest.MORs(1), chaintest.MORs(100))
	h.fc.Fund(h.provW.Address(), chaintest.MORs(1), chaintest.MORs(10))

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	t.Cleanup(cancel)

	// Provider side.
	pc, _ := chain.NewClient([]string{h.fc.URL()}, log)
	pk := chain.NewContracts(pc, chaintest.Diamond, chaintest.MOR, big.NewInt(84532))
	ptx := chain.NewTxSender(pk, h.provW, false, log)
	h.modelID = h.fc.AddModel("llama-3.3-70b", "llm")
	models := []provider.Model{{ID: h.modelID, Name: "llama-3.3-70b", APIURL: h.backend.srv.URL + "/v1/chat/completions", Backend: "backend-model"}}
	var agents []provider.Agent
	var listings []common.Hash
	for _, o := range extra {
		if o.quota > 0 {
			id := h.fc.AddModel(o.name, marketplace.StorageTags(o.quota)...)
			h.ids[o.name] = id
			listings = append(listings, id)
			continue
		}
		id := h.fc.AddModel(o.name)
		h.ids[o.name] = id
		if o.model != nil {
			m := *o.model
			m.ID, m.Name = id, o.name
			models = append(models, m)
		} else {
			a := *o.agent
			a.ID, a.Name = id, o.name
			agents = append(agents, a)
		}
	}
	prov := provider.New(provider.Options{Models: models, Agents: agents, Version: "test"}, h.provW, pk, ptx, events.New(), log)
	h.dir = t.TempDir()
	if len(listings) > 0 {
		h.kubo = ipfstest.New()
		t.Cleanup(h.kubo.Close)
		kc, err := kubo.New(h.kubo.URL)
		if err != nil {
			t.Fatal(err)
		}
		h.storageSrv = storage.NewServer(storage.ServerOptions{
			Listings: listings, MaxFileBytes: 64 << 20, Grace: time.Hour, StatePath: filepath.Join(h.dir, "storage_provider.json"), Version: "test",
		}, h.provW, pk, kc, prov.ServesFor, events.New(), log)
		prov.SetStorage(h.storageSrv)
		go h.storageSrv.Run(ctx)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &morrpc.Server{Handler: prov.Handle, Log: log}
	go func() { _ = srv.Serve(ctx, ln) }()
	go prov.Run(ctx)

	h.fc.AddProvider(h.provW.Address(), ln.Addr().String())
	h.fc.AddBid(h.provW.Address(), h.modelID, big.NewInt(1_000_000_000_000_000)) // 0.001 MOR/s
	for _, id := range h.ids {
		h.fc.AddBid(h.provW.Address(), id, big.NewInt(1_000_000_000_000_000))
	}

	// A second, unreachable provider that must be skipped.
	dead, _ := wallet.Generate()
	h.fc.AddProvider(dead.Address(), "127.0.0.1:1")
	h.fc.AddBid(dead.Address(), h.modelID, big.NewInt(500_000_000_000_000))

	// Consumer side.
	cc, _ := chain.NewClient([]string{h.fc.URL()}, log)
	ck := chain.NewContracts(cc, chaintest.Diamond, chaintest.MOR, big.NewInt(84532))
	ctx2 := chain.NewTxSender(ck, h.consumer, false, log)
	market := marketplace.New(ck, nil, nil, log)
	h.ck, h.market = ck, market
	dir := h.dir
	h.mgr = consumer.New(consumer.Options{
		Duration: 10 * time.Minute, MaxPerModel: maxPerModel, MaxTotal: 4, IdleTimeout: time.Hour,
		ProviderTimeout: 20 * time.Second, QueueTimeout: 30 * time.Second, HealthPolicy: "permissive",
		CloseOnShutdown: true, StorePath: filepath.Join(dir, "sessions.json"),
	}, h.consumer, ck, ctx2, market, morrpc.DefaultClient(), h.bus, log)
	go h.mgr.Run(ctx)

	h.keys, err = apikeys.Open(filepath.Join(dir, "apikeys.json"), log)
	if err != nil {
		t.Fatal(err)
	}
	h.store = h.newStore("storage.json")
	gw := gateway.New(gateway.Deps{Market: market, Sessions: h.mgr, Keys: h.keys, Bus: h.bus, AdminToken: h.admin, Log: log,
		Status: func(context.Context) any { return map[string]any{"ok": true} }, Files: h.store})
	h.gw = httptest.NewServer(gw.Handler())
	t.Cleanup(h.gw.Close)
	return h
}

// newStore opens a user file store on the consumer wallet (a fresh manifest
// per name).
func (h *harness) newStore(manifest string) *storage.Store {
	return storage.NewStore(storage.StoreOptions{
		Path: filepath.Join(h.dir, manifest), TempDir: filepath.Join(h.dir, "tmp"), Lease: time.Hour, RenewBefore: 10 * time.Minute,
		Replicas: 1, Encrypt: true, AuditInterval: time.Hour,
	}, h.consumer, h.ck, h.market, h.mgr, morrpc.DefaultClient(), h.bus, logger())
}

func (h *harness) do(method, path, key string, body any) (*http.Response, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, h.gw.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func (h *harness) newKey(opts apikeys.CreateOptions) string {
	secret, _, err := h.keys.Create(opts)
	if err != nil {
		h.t.Fatal(err)
	}
	return secret
}

func chat(content string, stream bool) map[string]any {
	return map[string]any{"model": "llama-3.3-70b", "stream": stream, "messages": []any{map[string]any{"role": "user", "content": content}}}
}

func TestEndToEnd(t *testing.T) {
	h := newHarness(t, 2, 0)
	key := h.newKey(apikeys.CreateOptions{Name: "agent"})

	// Unauthenticated and bad keys are rejected.
	if resp, _ := h.do("GET", "/v1/models", "", nil); resp.StatusCode != 401 {
		t.Fatalf("no key: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/models", "sk-mor-wrong", nil); resp.StatusCode != 401 {
		t.Fatalf("bad key: %d", resp.StatusCode)
	}

	// Model listing.
	resp, body := h.do("GET", "/v1/models", key, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"id":"llama-3.3-70b"`) {
		t.Fatalf("models: %d %s", resp.StatusCode, body)
	}

	// Non-streaming chat: opens a session on-chain and routes the prompt.
	resp, body = h.do("POST", "/v1/chat/completions", key, chat("hello there", false))
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Object != "chat.completion" || out.Choices[0].Message.Content != "echo: hello there" {
		t.Fatalf("unexpected completion %s", body)
	}
	sessID := common.HexToHash(resp.Header.Get("X-Morpheus-Session"))
	cs := h.fc.Session(sessID)
	if cs == nil || cs.User != h.consumer.Address() || cs.Closed() {
		t.Fatalf("session not open on chain: %+v", cs)
	}
	if got := resp.Header.Get("X-Morpheus-Provider"); got != h.provW.Address().Hex() {
		t.Fatalf("routed to %s, want the live provider", got)
	}

	// Streaming chat.
	req, _ := http.NewRequest("POST", h.gw.URL+"/v1/chat/completions", strings.NewReader(mustJSON(chat("stream me please", true))))
	req.Header.Set("Authorization", "Bearer "+key)
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if sresp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream content type %q", sresp.Header.Get("Content-Type"))
	}
	var text strings.Builder
	sawDone := false
	sc := bufio.NewScanner(sresp.Body)
	for sc.Scan() {
		line, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		if line == "[DONE]" {
			sawDone = true
			break
		}
		var ch struct {
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		_ = json.Unmarshal([]byte(line), &ch)
		if len(ch.Choices) > 0 {
			text.WriteString(ch.Choices[0].Delta.Content)
		}
	}
	sresp.Body.Close()
	if !sawDone || text.String() != "echo: stream me please" {
		t.Fatalf("stream text %q done=%v", text.String(), sawDone)
	}

	// Embeddings use the same session pool and the backend's /embeddings route.
	resp, body = h.do("POST", "/v1/embeddings", key, map[string]any{"model": "llama-3.3-70b", "input": "vectorize"})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"embedding":[0.1,0.2]`) {
		t.Fatalf("embeddings: %d %s", resp.StatusCode, body)
	}

	// Backend errors keep their status code.
	resp, body = h.do("POST", "/v1/chat/completions", key, chat("rate-limit-me", false))
	if resp.StatusCode != 429 || !strings.Contains(string(body), "slow down") {
		t.Fatalf("backend 429 not propagated: %d %s", resp.StatusCode, body)
	}

	// Unknown model.
	resp, _ = h.do("POST", "/v1/chat/completions", key, map[string]any{"model": "nope", "messages": []any{}})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown model: %d", resp.StatusCode)
	}

	// Usage is tracked per key.
	k, _ := h.keys.Get("agent")
	if k.Usage.Requests < 4 || k.Usage.OutputTokens == 0 {
		t.Fatalf("usage not recorded: %+v", k.Usage)
	}

	// Admin API: list sessions, then close.
	resp, body = h.do("GET", "/admin/sessions", h.admin, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), sessID.Hex()) {
		t.Fatalf("admin sessions: %d %s", resp.StatusCode, body)
	}
	resp, body = h.do("DELETE", "/admin/sessions/"+sessID.Hex(), h.admin, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("close: %d %s", resp.StatusCode, body)
	}
	cs = h.fc.Session(sessID)
	if !cs.Closed() {
		t.Fatal("session still open on chain")
	}
	if cs.CloseoutType.Sign() != 0 {
		t.Fatal("session closed with a disputed (user-signed) receipt; provider receipt expected")
	}
}

func TestConcurrentRequestsScaleAcrossSessions(t *testing.T) {
	h := newHarness(t, 2, 400*time.Millisecond)
	key := h.newKey(apikeys.CreateOptions{Name: "swarm"})

	const n = 8
	var wg sync.WaitGroup
	errs := make(chan string, n)
	sessions := sync.Map{}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := fmt.Sprintf("job %d", i)
			resp, body := h.do("POST", "/v1/chat/completions", key, chat(msg, false))
			if resp.StatusCode != 200 || !strings.Contains(string(body), "echo: "+msg) {
				errs <- fmt.Sprintf("request %d: %d %s", i, resp.StatusCode, body)
				return
			}
			sessions.Store(resp.Header.Get("X-Morpheus-Session"), true)
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	count := 0
	sessions.Range(func(any, any) bool { count++; return true })
	if count != 2 {
		t.Fatalf("expected requests spread over 2 sessions, got %d", count)
	}
	if h.backend.peak.Load() < 2 {
		t.Fatalf("backend never saw parallel requests (peak %d)", h.backend.peak.Load())
	}
	if opened := h.fc.TxCount["openSession"]; opened != 2 {
		t.Fatalf("opened %d sessions on-chain, want 2", opened)
	}
}

func TestKeyLimitsAndScopes(t *testing.T) {
	h := newHarness(t, 1, 0)
	limited := h.newKey(apikeys.CreateOptions{Name: "limited", RPM: 1})
	other := h.newKey(apikeys.CreateOptions{Name: "other-models", Models: []string{"mistral"}})

	if resp, body := h.do("POST", "/v1/chat/completions", limited, chat("one", false)); resp.StatusCode != 200 {
		t.Fatalf("first request: %d %s", resp.StatusCode, body)
	}
	resp, _ := h.do("POST", "/v1/chat/completions", limited, chat("two", false))
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rate limit: %d", resp.StatusCode)
	}
	if resp, _ := h.do("POST", "/v1/chat/completions", other, chat("x", false)); resp.StatusCode != 403 {
		t.Fatalf("model allowlist: %d", resp.StatusCode)
	}
	if resp, body := h.do("GET", "/v1/models", other, nil); resp.StatusCode != 200 || strings.Contains(string(body), "llama") {
		t.Fatalf("allowlisted listing leaked models: %s", body)
	}
	// Inference keys cannot manage keys.
	if resp, _ := h.do("GET", "/admin/keys", limited, nil); resp.StatusCode != 403 {
		t.Fatalf("scope check: %d", resp.StatusCode)
	}
	// Admin API mints keys whose secrets work immediately.
	resp, body := h.do("POST", "/admin/keys", h.admin, map[string]any{"name": "minted", "rpm": 100})
	if resp.StatusCode != 201 {
		t.Fatalf("create key: %d %s", resp.StatusCode, body)
	}
	var kv struct{ Secret string }
	_ = json.Unmarshal(body, &kv)
	if resp, _ := h.do("GET", "/v1/models", kv.Secret, nil); resp.StatusCode != 200 {
		t.Fatalf("minted key rejected: %d", resp.StatusCode)
	}
	if resp, _ := h.do("DELETE", "/admin/keys/minted", h.admin, nil); resp.StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	if resp, _ := h.do("GET", "/v1/models", kv.Secret, nil); resp.StatusCode != 401 {
		t.Fatal("revoked key still works")
	}
}

func TestShutdownClosesSessionsAndReconcileAdopts(t *testing.T) {
	h := newHarness(t, 1, 0)
	key := h.newKey(apikeys.CreateOptions{Name: "a"})
	resp, body := h.do("POST", "/v1/chat/completions", key, chat("hi", false))
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, body)
	}
	id := common.HexToHash(resp.Header.Get("X-Morpheus-Session"))

	// Simulate a crash: a fresh manager with no local state must find and
	// adopt the still-open session from chain.
	log := logger()
	cc, _ := chain.NewClient([]string{h.fc.URL()}, log)
	ck := chain.NewContracts(cc, chaintest.Diamond, chaintest.MOR, big.NewInt(84532))
	m2 := consumer.New(consumer.Options{Duration: 10 * time.Minute, MaxPerModel: 1, MaxTotal: 2, ProviderTimeout: 10 * time.Second, CloseOnShutdown: true},
		h.consumer, ck, chain.NewTxSender(ck, h.consumer, false, log), marketplace.New(ck, nil, nil, log), morrpc.DefaultClient(), events.New(), log)
	if err := m2.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range m2.Sessions() {
		found = found || (s.ID == id.Hex() && s.Adopted)
	}
	if !found {
		t.Fatalf("session %s not adopted: %+v", id.Hex(), m2.Sessions())
	}

	// Adopted sessions are not closed on shutdown (they may belong to another
	// app); the original manager closes its own.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m2.Shutdown(ctx)
	if h.fc.Session(id).Closed() {
		t.Fatal("adopted session was closed")
	}
	h.mgr.Shutdown(ctx)
	if !h.fc.Session(id).Closed() {
		t.Fatal("owned session not closed on shutdown")
	}
	if bal := h.fc.MORBalance(h.consumer.Address()); bal.Cmp(chaintest.MORs(100)) != 0 {
		t.Fatalf("stake not returned, balance %s", bal)
	}
}

func TestExpiredSessionClosedOnReconcile(t *testing.T) {
	h := newHarness(t, 1, 0)
	key := h.newKey(apikeys.CreateOptions{Name: "a"})
	resp, _ := h.do("POST", "/v1/chat/completions", key, chat("hi", false))
	id := common.HexToHash(resp.Header.Get("X-Morpheus-Session"))
	h.fc.ExpireSession(id)

	log := logger()
	cc, _ := chain.NewClient([]string{h.fc.URL()}, log)
	ck := chain.NewContracts(cc, chaintest.Diamond, chaintest.MOR, big.NewInt(84532))
	m2 := consumer.New(consumer.Options{Duration: 10 * time.Minute, MaxPerModel: 1, MaxTotal: 2, ProviderTimeout: 10 * time.Second},
		h.consumer, ck, chain.NewTxSender(ck, h.consumer, false, log), marketplace.New(ck, nil, nil, log), morrpc.DefaultClient(), events.New(), log)
	if err := m2.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	m2.Shutdown(ctx) // waits for the background close
	if !h.fc.Session(id).Closed() {
		t.Fatal("expired session not closed")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
