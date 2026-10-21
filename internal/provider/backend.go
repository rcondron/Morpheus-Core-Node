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

	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

type forwardStats struct {
	status int
	ttft   time.Duration
	dur    time.Duration
	in     int
	out    int
}

// embeddingsURL derives the embeddings endpoint from a chat-completions URL.
func embeddingsURL(chatURL string) string {
	if i := strings.LastIndex(chatURL, "/chat/completions"); i >= 0 {
		return chatURL[:i] + "/embeddings"
	}
	return strings.TrimSuffix(chatURL, "/") + "/embeddings"
}

// modelsURL derives the model-listing endpoint used for health probes.
func modelsURL(chatURL string) string {
	if i := strings.LastIndex(chatURL, "/chat/completions"); i >= 0 {
		return chatURL[:i] + "/models"
	}
	return strings.TrimSuffix(chatURL, "/") + "/models"
}

// forward relays a consumer request to the model backend and streams each
// response chunk back, encrypted to the consumer's key and signed by us.
func (p *Provider) forward(ctx context.Context, m *Model, message []byte, userPub []byte, id string, send func(*morrpc.RPCResponse) error) (*forwardStats, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(message, &req); err != nil {
		return nil, fmt.Errorf("prompt is not a JSON object: %w", err)
	}
	url := m.APIURL
	var kind string
	if raw, ok := req["type"]; ok {
		_ = json.Unmarshal(raw, &kind)
		delete(req, "type")
	}
	switch kind {
	case "", "chat_completion":
	case "embeddings":
		url = embeddingsURL(m.APIURL)
	default:
		return nil, fmt.Errorf("request type %q is not supported by this provider", kind)
	}
	backendModel, _ := json.Marshal(m.Backend)
	req["model"] = backendModel
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	stream := false
	if raw, ok := req["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}

	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if stream {
		hreq.Header.Set("Accept", "text/event-stream")
	}
	if m.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+m.APIKey)
	}
	st := &forwardStats{in: estimateTokens(body)}
	start := time.Now()
	resp, err := p.http.Do(hreq)
	if err != nil {
		return st, fmt.Errorf("model backend unreachable: %w", err)
	}
	defer resp.Body.Close()
	st.status = resp.StatusCode

	emit := func(payload []byte) error {
		if st.ttft == 0 {
			st.ttft = time.Since(start)
		}
		enc, err := morrpc.EncryptTo(userPub, payload)
		if err != nil {
			return err
		}
		r, err := morrpc.PromptResponse(id, enc, p.w)
		if err != nil {
			return err
		}
		return send(r)
	}
	defer func() { st.dur = time.Since(start) }()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var parsed any
		if json.Unmarshal(raw, &parsed) != nil {
			parsed = map[string]any{"error": map[string]any{"message": strings.TrimSpace(string(raw)), "type": "upstream_error"}}
		}
		payload, _ := json.Marshal(morrpc.AIEngineError{ProviderModelError: parsed, StatusCode: resp.StatusCode})
		enc, err := morrpc.EncryptTo(userPub, payload)
		if err != nil {
			return st, err
		}
		r, err := morrpc.SignedError(id, enc, p.w)
		if err != nil {
			return st, err
		}
		return st, send(r)
	}

	if strings.HasPrefix(strings.TrimSpace(resp.Header.Get("Content-Type")), "text/event-stream") {
		return st, readSSE(resp.Body, st, emit)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return st, fmt.Errorf("read backend response: %w", err)
	}
	st.in, st.out = usageOf(raw, st.in)
	return st, emit(bytes.TrimSpace(raw))
}

// readSSE relays a server-sent-events stream chunk by chunk.
func readSSE(r io.Reader, st *forwardStats, emit func([]byte) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	chars := 0
	for sc.Scan() {
		line := sc.Bytes()
		data, ok := bytes.CutPrefix(line, []byte("data:"))
		if !ok {
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		if bytes.Equal(data, []byte(morrpc.StreamDone)) {
			done, _ := json.Marshal(morrpc.StreamDone)
			if st.out == 0 {
				st.out = (chars + 3) / 4
			}
			return emit(done)
		}
		if in, out := usageOf(data, 0); out > 0 {
			st.in, st.out = in, out
		}
		chars += contentChars(data)
		if err := emit(append([]byte(nil), data...)); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("backend stream: %w", err)
	}
	if st.out == 0 {
		st.out = (chars + 3) / 4
	}
	if st.ttft == 0 {
		return errors.New("backend closed the stream without data")
	}
	return nil
}

func usageOf(raw []byte, fallbackIn int) (in, out int) {
	var v struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return fallbackIn, 0
	}
	if v.Usage != nil && v.Usage.PromptTokens+v.Usage.CompletionTokens > 0 {
		return v.Usage.PromptTokens, v.Usage.CompletionTokens
	}
	n := 0
	for _, c := range v.Choices {
		n += len(c.Message.Content)
	}
	return fallbackIn, (n + 3) / 4
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

func estimateTokens(body []byte) int { return (len(body) + 3) / 4 }
