package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// forwardStats describes one served request for session stats and health.
type forwardStats struct {
	status int // upstream HTTP status (0 when no response)
	ttft   time.Duration
	dur    time.Duration
	in     int
	out    int
}

// request is a decoded session.prompt payload.
type request struct {
	kind string                     // morrpc.Type* (chat when absent)
	body map[string]json.RawMessage // the payload minus "type"
	file string                     // provider-managed upload (transcriptions)
}

func parseRequest(message []byte) (*request, error) {
	var body map[string]json.RawMessage
	if err := json.Unmarshal(message, &body); err != nil || body == nil {
		return nil, invalid("request is not a JSON object")
	}
	r := &request{kind: morrpc.TypeChat, body: body}
	if raw, ok := body["type"]; ok {
		var kind string
		_ = json.Unmarshal(raw, &kind)
		delete(body, "type")
		if kind != "" {
			r.kind = kind
		}
	}
	// A file path is only ever set by this node for its own uploads.
	for k := range body {
		if strings.EqualFold(k, "FilePath") || strings.EqualFold(k, "file_path") {
			delete(body, k)
		}
	}
	return r, nil
}

func (r *request) stream() bool {
	var s bool
	for k, v := range r.body {
		if strings.EqualFold(k, "stream") {
			_ = json.Unmarshal(v, &s)
		}
	}
	return s
}

// withModel returns the body as JSON with "model" replaced by the backend's name.
func (r *request) withModel(backend string) ([]byte, error) {
	for k := range r.body {
		if strings.EqualFold(k, "model") {
			delete(r.body, k)
		}
	}
	r.body["model"], _ = json.Marshal(backend)
	return json.Marshal(r.body)
}

// emitter sends response chunks to the consumer: each payload is encrypted to
// the user's key and signed, exactly like the reference provider.
type emitter struct {
	p       *Provider
	id      string
	userPub []byte
	send    func(*morrpc.RPCResponse) error
	st      *forwardStats
	start   time.Time
	sent    int
}

func (e *emitter) raw(payload []byte) error {
	if e.st.ttft == 0 {
		e.st.ttft = time.Since(e.start)
	}
	enc, err := morrpc.EncryptTo(e.userPub, payload)
	if err != nil {
		return err
	}
	r, err := morrpc.PromptResponse(e.id, enc, e.p.w)
	if err != nil {
		return err
	}
	e.sent++
	return e.send(r)
}

func (e *emitter) json(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return e.raw(b)
}

// done sends the "[DONE]" control message that ends a chat stream.
func (e *emitter) done() error {
	b, _ := json.Marshal(morrpc.StreamDone)
	return e.raw(b)
}

// upstreamError forwards a backend error as a signed, encrypted engine error
// carrying the upstream status, which consumers pass through to their client.
func (e *emitter) upstreamError(status int, body []byte) error {
	var parsed any
	if json.Unmarshal(body, &parsed) != nil {
		parsed = map[string]any{"error": map[string]any{"message": strings.TrimSpace(string(body)), "type": "upstream_error"}}
	}
	return e.engineError(status, parsed)
}

func (e *emitter) engineError(status int, providerModelError any) error {
	e.st.status = status
	payload, _ := json.Marshal(morrpc.AIEngineError{ProviderModelError: providerModelError, StatusCode: status})
	enc, err := morrpc.EncryptTo(e.userPub, payload)
	if err != nil {
		return err
	}
	r, err := morrpc.SignedError(e.id, enc, e.p.w)
	if err != nil {
		return err
	}
	e.sent++
	return e.send(r)
}

// errUnsupported marks requests a backend type cannot serve.
var errUnsupported = badRequest{errors.New("request type is not supported by this model")}

// badRequest marks errors caused by the consumer's request. They are answered
// with a signed 400, which consumers pass to their client instead of failing
// over or blaming the session.
type badRequest struct{ error }

func (b badRequest) Unwrap() error { return b.error }

func invalid(format string, args ...any) error { return badRequest{fmt.Errorf(format, args...)} }

// reject answers a bad request.
func (e *emitter) reject(err error) error {
	return e.engineError(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}})
}

// forward serves one request with the model's backend adapter.
func (p *Provider) forward(ctx context.Context, m *Model, r *request, e *emitter) error {
	switch m.Type() {
	case config.APIOpenAI:
		return p.openAI(ctx, m, r, e)
	case config.APIClaude:
		return p.claude(ctx, m, r, e)
	case config.APIProdiaSD, config.APIProdiaSDXL:
		return p.prodiaV1(ctx, m, r, e)
	case config.APIProdiaV2:
		return p.prodiaV2(ctx, m, r, e)
	case config.APIHyperbolicSD:
		return p.hyperbolic(ctx, m, r, e)
	case config.APIDecisions:
		return p.decisions(ctx, m, r, e)
	}
	return fmt.Errorf("unknown api_type %q", m.APIType)
}

// ---------------------------------------------------------------------------
// OpenAI-compatible backends
// ---------------------------------------------------------------------------

// openAIPaths are the endpoint suffixes an api_url may end with.
var openAIPaths = []string{"/chat/completions", "/embeddings", "/audio/transcriptions", "/audio/speech", "/completions", "/images/generations"}

// endpointFor resolves where a request of a given path goes. The configured
// URL is used as is for its own modality (the reference node's rule); a URL
// ending in a different OpenAI path is re-pointed, so one chat URL also
// serves the backend's embeddings and audio endpoints.
func endpointFor(apiURL, path string) string {
	u := strings.TrimRight(apiURL, "/")
	for _, s := range openAIPaths {
		if strings.HasSuffix(u, s) {
			if s == path || (s == "/completions" && path == "/chat/completions") {
				return u
			}
			return strings.TrimSuffix(u, s) + path
		}
	}
	if path == "/chat/completions" {
		return u // custom chat endpoint
	}
	return u + path
}

func (p *Provider) openAI(ctx context.Context, m *Model, r *request, e *emitter) error {
	switch r.kind {
	case morrpc.TypeChat:
		return p.openAIChat(ctx, m, r, e)
	case morrpc.TypeEmbeddings:
		return p.openAIJSON(ctx, m, r, e, endpointFor(m.APIURL, "/embeddings"))
	case morrpc.TypeTranscription:
		return p.transcribe(ctx, m, r, e)
	case morrpc.TypeSpeech:
		return p.speech(ctx, m, r, e)
	}
	return fmt.Errorf("%w: %s", errUnsupported, r.kind)
}

func (p *Provider) post(ctx context.Context, m *Model, url string, contentType string, body io.Reader, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if m.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model backend unreachable: %w", err)
	}
	return resp, nil
}

func (p *Provider) openAIChat(ctx context.Context, m *Model, r *request, e *emitter) error {
	stream := r.stream()
	body, err := r.withModel(m.Backend)
	if err != nil {
		return err
	}
	e.st.in = estimatePromptTokens(body)
	accept := ""
	if stream {
		accept = "text/event-stream"
	}
	resp, err := p.post(ctx, m, endpointFor(m.APIURL, "/chat/completions"), "application/json", bytes.NewReader(body), accept)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return e.upstreamError(resp.StatusCode, raw)
	}
	if isEventStream(resp.Header) {
		return relayChatStream(resp.Body, e)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read backend response: %w", err)
	}
	in, out := usageOf(raw)
	if in == 0 && out == 0 {
		in, out = e.st.in, estimateCompletion(raw)
	}
	e.st.in, e.st.out = in, out
	return e.raw(finishChat(raw, "chat.completion", in, out))
}

// openAIJSON posts the request and relays the JSON answer as one chunk.
func (p *Provider) openAIJSON(ctx context.Context, m *Model, r *request, e *emitter, url string) error {
	body, err := r.withModel(m.Backend)
	if err != nil {
		return err
	}
	e.st.in = estimatePromptTokens(body)
	resp, err := p.post(ctx, m, url, "application/json", bytes.NewReader(body), "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read backend response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return e.upstreamError(resp.StatusCode, raw)
	}
	if in, _ := usageOf(raw); in > 0 {
		e.st.in = in
	}
	return e.raw(bytes.TrimSpace(raw))
}

// relayChatStream forwards a server-sent-events chat stream chunk by chunk and
// ends it with "[DONE]".
func relayChatStream(r io.Reader, e *emitter) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	chars := 0
	for sc.Scan() {
		data, ok := sseData(sc.Bytes())
		if !ok {
			continue
		}
		if bytes.Equal(data, []byte(morrpc.StreamDone)) {
			break
		}
		chars += contentChars(data)
		in, out := usageOf(data)
		if in+out > 0 {
			e.st.in, e.st.out = in, out
		}
		if isFinalChunk(data) {
			if e.st.out == 0 {
				e.st.out = (chars + 3) / 4
			}
			data = finishChat(data, "chat.completion.chunk", e.st.in, e.st.out)
		} else {
			data = ensureObject(data, "chat.completion.chunk")
		}
		if err := e.raw(append([]byte(nil), data...)); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("backend stream: %w", err)
	}
	if e.sent == 0 {
		return errors.New("backend closed the stream without data")
	}
	if e.st.out == 0 {
		e.st.out = (chars + 3) / 4
	}
	return e.done()
}

func isEventStream(h http.Header) bool {
	return strings.HasPrefix(strings.TrimSpace(h.Get("Content-Type")), "text/event-stream")
}

// sseData extracts the payload of an SSE "data:" line.
func sseData(line []byte) ([]byte, bool) {
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return nil, false
	}
	data = bytes.TrimSpace(data)
	return data, len(data) > 0
}

// ensureObject adds the "object" field some OpenAI-compatible servers omit;
// reference consumers recognize chunks and completions by it.
func ensureObject(payload []byte, object string) []byte {
	if bytes.Contains(payload, []byte(`"object"`)) {
		return payload
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return payload
	}
	m["object"], _ = json.Marshal(object)
	out, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return out
}

// finishChat completes a final chat payload: the "object" field and the
// provider's own token count ("usage_from_provider", as the reference adds;
// the backend's "usage" is left untouched).
func finishChat(payload []byte, object string, in, out int) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(payload, &m) != nil {
		return payload
	}
	if _, ok := m["object"]; !ok {
		m["object"], _ = json.Marshal(object)
	}
	m["usage_from_provider"], _ = json.Marshal(map[string]int{"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out})
	b, err := json.Marshal(m)
	if err != nil {
		return payload
	}
	return b
}

func isFinalChunk(chunk []byte) bool {
	var v struct {
		Usage   json.RawMessage `json:"usage"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
	}
	if json.Unmarshal(chunk, &v) != nil {
		return false
	}
	if len(v.Usage) > 0 && string(v.Usage) != "null" {
		return true
	}
	for _, c := range v.Choices {
		if c.FinishReason != nil && *c.FinishReason != "" {
			return true
		}
	}
	return false
}

func usageOf(raw []byte) (in, out int) {
	var v struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &v) != nil || v.Usage == nil {
		return 0, 0
	}
	return v.Usage.PromptTokens, v.Usage.CompletionTokens
}

func estimateCompletion(raw []byte) int {
	var v struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return 0
	}
	n := 0
	for _, c := range v.Choices {
		n += len(c.Message.Content) + len(c.Message.ReasoningContent)
	}
	return (n + 3) / 4
}

func contentChars(chunk []byte) int {
	var v struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(chunk, &v) != nil {
		return 0
	}
	n := 0
	for _, c := range v.Choices {
		n += len(c.Delta.Content) + len(c.Delta.ReasoningContent) + len(c.Delta.Reasoning)
	}
	return n
}

// estimatePromptTokens approximates input tokens (≈4 characters per token)
// from the message contents or embedding input.
func estimatePromptTokens(body []byte) int {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return (len(body) + 3) / 4
	}
	n := len(req.Input)
	for _, msg := range req.Messages {
		n += len(msg.Content) + 4
	}
	return (n + 3) / 4
}
