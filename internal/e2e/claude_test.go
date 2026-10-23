package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/provider"
)

// claudeBackend imitates Anthropic's Messages API: a plain answer, a streamed
// one, and an overload reported as a stream error event.
func claudeBackend(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model     string `json:"model"`
			MaxTokens int    `json:"max_tokens"`
			System    any    `json:"system"`
			Stream    bool   `json:"stream"`
			Messages  []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if r.Header.Get("x-api-key") != "claude-key" || r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(401)
			fmt.Fprint(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" { // the provider's health probe
			fmt.Fprint(w, `{"data":[{"type":"model","id":"claude-test-model"}],"has_more":false}`)
			return
		}
		if r.URL.Path != "/v1/messages" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.NotFound(w, r)
			return
		}
		if req.Model != "claude-test-model" || req.MaxTokens <= 0 || req.System == nil || len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			w.WriteHeader(400)
			fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"unexpected request for %s"}}`, req.Model)
			return
		}
		prompt := string(req.Messages[0].Content)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-test-model","content":[{"type":"text","text":"Claude says hi"}],"stop_reason":"end_turn","usage":{"input_tokens":11,"output_tokens":4}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		event := func(name, data string) { fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, data) }
		if strings.Contains(prompt, "overload") {
			event("error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		event("message_start", `{"type":"message_start","message":{"id":"msg_2","type":"message","role":"assistant","model":"claude-test-model","content":[],"usage":{"input_tokens":11,"output_tokens":1}}}`)
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		for _, part := range []string{"Streamed ", "from ", "Claude"} {
			event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, part))
		}
		event("content_block_stop", `{"type":"content_block_stop","index":0}`)
		if strings.Contains(prompt, "cut") {
			return // the connection drops before message_stop
		}
		event("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
		event("message_stop", `{"type":"message_stop"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestClaudeResale(t *testing.T) {
	claude := claudeBackend(t)
	h := newHarness(t, 2, 0, offering{name: "claude", model: &provider.Model{
		APIType: config.APIClaude, APIURL: claude.URL + "/v1/messages", APIKey: "claude-key", Backend: "claude-test-model"}})
	key := h.newKey(apikeys.CreateOptions{Name: "claude"})
	chat := func(prompt string, stream bool) (*http.Response, []byte) {
		return h.do("POST", "/v1/chat/completions", key, map[string]any{"model": "claude", "stream": stream,
			"messages": []any{map[string]any{"role": "system", "content": "Be brief."}, map[string]any{"role": "user", "content": prompt}}})
	}

	resp, body := chat("hello", false)
	var out struct {
		Object  string `json:"object"`
		Choices []struct {
			Message      struct{ Content string } `json:"message"`
			FinishReason string                   `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt     int `json:"prompt_tokens"`
			Completion int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &out) != nil || out.Object != "chat.completion" || len(out.Choices) != 1 ||
		out.Choices[0].Message.Content != "Claude says hi" || out.Choices[0].FinishReason != "stop" || out.Usage.Prompt != 11 || out.Usage.Completion != 4 {
		t.Fatalf("chat completion: %d %s", resp.StatusCode, body)
	}

	resp, body = chat("hello", true)
	text, done := "", false
	for _, line := range strings.Split(string(body), "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		if data == "[DONE]" {
			done = true
			continue
		}
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta struct{ Content string } `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil || chunk.Object != "chat.completion.chunk" {
			t.Fatalf("bad chunk %s", data)
		}
		for _, c := range chunk.Choices {
			text += c.Delta.Content
		}
	}
	if resp.StatusCode != 200 || text != "Streamed from Claude" || !done {
		t.Fatalf("streamed completion: %d %q done=%v\n%s", resp.StatusCode, text, done, body)
	}

	// Anthropic's overload error event arrives before any content: the
	// client sees Anthropic's status and an OpenAI-style error body.
	resp, body = chat("overload", true)
	if resp.StatusCode != 529 || !strings.Contains(string(body), "overloaded_error") {
		t.Fatalf("overloaded: %d %s", resp.StatusCode, body)
	}
	// A stream cut before message_stop must not look complete: the gateway
	// ends it with an error event.
	resp, body = chat("cut", true)
	if !strings.Contains(string(body), `"error":{"message":"provider failed: provider error: Claude stream ended before the message was complete"`) {
		t.Fatalf("truncated stream ended cleanly: %d %s", resp.StatusCode, body)
	}
	// A request the translation cannot express is the client's error.
	resp, body = h.do("POST", "/v1/chat/completions", key, map[string]any{"model": "claude",
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_audio", "input_audio": map[string]any{"data": "AAAA", "format": "wav"}}}}}})
	if resp.StatusCode != 400 {
		t.Fatalf("untranslatable request: %d %s", resp.StatusCode, body)
	}
}
