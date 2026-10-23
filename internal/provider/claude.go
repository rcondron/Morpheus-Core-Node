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

	"github.com/rcondron/Morpheus-Core-Node/internal/anthropic"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = anthropic.APIVersion
	// claudeMaxTokens applies when a request sets no max_tokens (the reference default).
	claudeMaxTokens = 1024
)

// claude serves OpenAI chat completions from Anthropic's Messages API,
// translating requests, responses and streams in both directions.
func (p *Provider) claude(ctx context.Context, m *Model, r *request, e *emitter) error {
	if r.kind != morrpc.TypeChat {
		return fmt.Errorf("%w: %s", errUnsupported, r.kind)
	}
	openaiBody, err := r.withModel(m.Backend)
	if err != nil {
		return err
	}
	e.st.in = estimatePromptTokens(openaiBody)
	body, stream, err := anthropic.Request(openaiBody, m.Backend, claudeMaxTokens)
	if err != nil {
		return invalid("translate request for Claude: %v", err)
	}
	url := m.APIURL
	if url == "" {
		url = anthropicURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", anthropicVersion)
	if m.APIKey != "" {
		req.Header.Set("x-api-key", m.APIKey)
	}
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("model backend unreachable: %w", err)
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return e.upstreamError(resp.StatusCode, anthropic.ErrorBody(resp.StatusCode, raw))
	}
	if !isEventStream(resp.Header) {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			return fmt.Errorf("read Claude response: %w", err)
		}
		out, err := anthropic.Response(raw)
		if err != nil {
			return err
		}
		in, completion := usageOf(out)
		e.st.in, e.st.out = in, completion
		return e.raw(finishChat(out, "chat.completion", in, completion))
	}

	ts := anthropic.NewStream(m.Backend)
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	chars, finished := 0, false
	for sc.Scan() {
		data, ok := sseData(sc.Bytes())
		if !ok {
			continue
		}
		chunks, done, err := ts.Event(data)
		if err != nil {
			// Anthropic reports failures such as overload as error events,
			// often before any content; relay those like an HTTP error.
			var ae *anthropic.Error
			if e.sent == 0 && errors.As(err, &ae) {
				return e.engineError(ae.Status(), json.RawMessage(anthropic.ErrorBody(ae.Status(), data)))
			}
			return fmt.Errorf("Claude stream: %w", err)
		}
		for _, c := range chunks {
			chars += contentChars(c)
			if in, out := usageOf(c); in+out > 0 {
				e.st.in, e.st.out = in, out
			}
			if isFinalChunk(c) {
				if e.st.out == 0 {
					e.st.out = (chars + 3) / 4
				}
				c = finishChat(c, "chat.completion.chunk", e.st.in, e.st.out)
			}
			if err := e.raw(c); err != nil {
				return err
			}
		}
		if done {
			finished = true
			break
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("Claude stream: %w", err)
	}
	if !finished {
		return errors.New("Claude stream ended before the message was complete")
	}
	return e.done()
}
