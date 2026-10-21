package consumer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Result describes a completed routed request.
type Result struct {
	TTFT         time.Duration
	InputTokens  int
	OutputTokens int
	Chunks       int
}

// ProviderError is a failure attributable to the provider rather than the
// request. BeforeOutput is true when nothing reached the client yet, so the
// request can safely be retried on another session.
type ProviderError struct {
	Err          error
	BeforeOutput bool
}

func (e *ProviderError) Error() string { return "provider failed: " + e.Err.Error() }
func (e *ProviderError) Unwrap() error { return e.Err }

// Send routes an OpenAI-shaped request body over the leased session and calls
// onChunk with each decrypted response payload (a JSON chunk, a full JSON
// response, or an error body). It returns token/latency statistics.
func (m *Manager) Send(ctx context.Context, l *Lease, body []byte, onChunk func(payload []byte) error) (*Result, error) {
	s := l.Session
	msg, err := morrpc.PromptRequest(newRequestID(), s.ID, body, m.w)
	if err != nil {
		return nil, err
	}
	res := &Result{InputTokens: estimatePromptTokens(body)}
	start := time.Now()
	var usageIn, usageOut, contentChars int
	clientErr := false
	err = m.rpc.Stream(ctx, s.Endpoint, msg, m.opt.ProviderTimeout, m.opt.ProviderTimeout, func(r *morrpc.RPCResponse) (bool, error) {
		payload, err := morrpc.DecodeInference(r, s.ProviderKey, m.w)
		if err != nil {
			return true, err
		}
		if res.Chunks == 0 {
			res.TTFT = time.Since(start)
		}
		if isDone(payload) {
			return true, nil
		}
		res.Chunks++
		in, out, chars, final := inspect(payload)
		if in > 0 {
			usageIn = in
		}
		if out > 0 {
			usageOut = out
		}
		contentChars += chars
		if err := onChunk(payload); err != nil {
			clientErr = true
			return true, err
		}
		return final, nil
	})
	if usageIn > 0 {
		res.InputTokens = usageIn
	}
	res.OutputTokens = usageOut
	if res.OutputTokens == 0 {
		res.OutputTokens = (contentChars + 3) / 4
	}
	if err == nil {
		return res, nil
	}
	if clientErr || errors.Is(err, context.Canceled) {
		return res, err
	}
	var ee *morrpc.EngineError
	if errors.As(err, &ee) {
		return res, err // the model backend's answer (e.g. 429): pass through
	}
	return res, &ProviderError{Err: err, BeforeOutput: res.Chunks == 0}
}

func isDone(payload []byte) bool {
	p := bytes.TrimSpace(payload)
	return bytes.Equal(p, []byte(`"[DONE]"`)) || bytes.Equal(p, []byte(`[DONE]`))
}

// inspect pulls usage figures and content size out of a response payload and
// reports whether it is a complete (non-streaming) response.
func inspect(payload []byte) (in, out, chars int, final bool) {
	var v struct {
		Object  string `json:"object"`
		Usage   *usage `json:"usage"`
		FromPrv *usage `json:"usage_from_provider"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
			} `json:"delta"`
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(payload, &v) != nil {
		return 0, 0, len(payload), false
	}
	for _, u := range []*usage{v.Usage, v.FromPrv} {
		if u != nil && u.PromptTokens+u.CompletionTokens > 0 {
			in, out = u.PromptTokens, u.CompletionTokens
		}
	}
	for _, c := range v.Choices {
		chars += len(c.Delta.Content) + len(c.Delta.ReasoningContent) + len(c.Delta.Reasoning) + len(c.Message.Content)
	}
	final = v.Object == "chat.completion" || v.Object == "list" || (v.Object == "" && len(v.Data) > 0)
	return in, out, chars, final
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

func estimatePromptTokens(body []byte) int {
	var req struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Input json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return len(body) / 4
	}
	n := len(req.Input)
	for _, msg := range req.Messages {
		n += len(msg.Content)
	}
	return (n + 3) / 4
}

// Describe returns a short human description of a session for logs.
func (s *Session) Describe() string {
	return fmt.Sprintf("%s via %s", s.ModelName, shortAddr(s.Provider))
}
