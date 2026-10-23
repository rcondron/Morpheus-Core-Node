package anthropic

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// Stream translates one streamed Anthropic Messages response into OpenAI
// chat.completion.chunk objects. It is not safe for concurrent use.
type Stream struct {
	id      string
	model   string
	created int64
	started bool                // the role-announcing first chunk was sent
	tools   map[int]*streamTool // tool_use blocks by content block index
	usage   usage
}

// streamTool tracks the OpenAI tool_calls entry of one tool_use block.
type streamTool struct {
	index int    // position in tool_calls, counted from 0
	input string // input given whole in content_block_start, if any
	args  bool   // an arguments fragment was forwarded
}

// streamEvent holds the fields of the stream events this package translates.
// Content blocks and deltas stay raw until their type is known, so block and
// delta types newer than this package cannot break a stream.
type streamEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage usage  `json:"usage"`
	} `json:"message"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	Usage        *usage          `json:"usage"`
	Error        *apiError       `json:"error"`
}

// eventDelta is the delta of a content_block_delta or message_delta event.
type eventDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}

// chunk is an OpenAI chat.completion.chunk object.
type chunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chatUsage    `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index        int     `json:"index"`
	Delta        delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

type delta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitempty"`
}

type toolCallDelta struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function functionCall `json:"function"`
}

// NewStream starts translating a stream. Chunks report model unless the
// stream's message_start names the model serving it.
func NewStream(model string) *Stream {
	now := time.Now()
	return &Stream{
		id:      fmt.Sprintf("chatcmpl-%x", now.UnixNano()), // replaced by the message ID
		model:   model,
		created: now.Unix(),
		tools:   map[int]*streamTool{},
	}
}

// Event handles one SSE event's data payload (the JSON after "data:"; the
// event type is inside it as "type"). It returns the chunks to forward, in
// order, and done=true after message_stop, when the caller should end the
// OpenAI stream with [DONE]. An error event is returned as an *Error. Pings,
// signatures and event types this package does not translate produce nothing.
func (s *Stream) Event(data []byte) (chunks [][]byte, done bool, err error) {
	var ev streamEvent
	if err := json.Unmarshal(data, &ev); err != nil {
		return nil, false, fmt.Errorf("invalid Anthropic stream event: %w", err)
	}
	var out []chunk
	switch ev.Type {
	case "message_start":
		if ev.Message.ID != "" {
			s.id = "chatcmpl-" + ev.Message.ID
		}
		if ev.Message.Model != "" {
			s.model = ev.Message.Model
		}
		s.usage = ev.Message.Usage
	case "content_block_start":
		b, err := decodeBlock(ev.ContentBlock)
		if err != nil {
			return nil, false, err
		}
		out = s.blockStart(ev.Index, b)
	case "content_block_delta":
		var d eventDelta
		if err := json.Unmarshal(ev.Delta, &d); err != nil {
			switch d.Type {
			case "text_delta", "thinking_delta", "input_json_delta":
				return nil, false, fmt.Errorf("invalid Anthropic %s: %w", d.Type, err)
			}
			return nil, false, nil
		}
		out = s.blockDelta(ev.Index, d)
	case "content_block_stop":
		if t := s.tools[ev.Index]; t != nil && !t.args {
			// Clients parse arguments as JSON, so a call that streamed no
			// fragments still needs an object.
			out = append(out, s.toolChunk(toolCallDelta{Index: t.index, Function: functionCall{Arguments: cmp.Or(t.input, "{}")}}))
		}
	case "message_delta":
		var d eventDelta
		if len(ev.Delta) > 0 {
			if err := json.Unmarshal(ev.Delta, &d); err != nil {
				return nil, false, fmt.Errorf("invalid Anthropic message_delta: %w", err)
			}
		}
		if u := ev.Usage; u != nil {
			// message_delta usage is cumulative; input counts appear only when
			// they changed during the turn (server tools).
			if u.OutputTokens > 0 {
				s.usage.OutputTokens = u.OutputTokens
			}
			if u.InputTokens+u.CacheCreationInputTokens+u.CacheReadInputTokens > 0 {
				s.usage.InputTokens, s.usage.CacheCreationInputTokens, s.usage.CacheReadInputTokens = u.InputTokens, u.CacheCreationInputTokens, u.CacheReadInputTokens
			}
		}
		c := s.chunk(delta{})
		reason := finishReason(d.StopReason)
		c.Choices[0].FinishReason = &reason
		c.Usage = s.usage.openAI()
		out = append(out, c)
	case "message_stop":
		return nil, true, nil
	case "error":
		if ev.Error == nil {
			return nil, false, &Error{Type: "api_error", Message: "stream error without details"}
		}
		return nil, false, &Error{Type: ev.Error.Type, Message: ev.Error.Message}
	default:
		return nil, false, nil // ping, and event types newer than this package
	}
	if !s.started && (ev.Type == "message_start" || len(out) > 0) {
		s.started = true
		empty := ""
		out = slices.Insert(out, 0, s.chunk(delta{Role: "assistant", Content: &empty}))
	}
	chunks = make([][]byte, 0, len(out))
	for _, c := range out {
		b, err := marshal(c)
		if err != nil {
			return nil, false, err
		}
		chunks = append(chunks, b)
	}
	return chunks, false, nil
}

func (s *Stream) blockStart(index int, b contentBlock) []chunk {
	switch b.Type {
	case "text":
		if b.Text != "" {
			return []chunk{s.chunk(delta{Content: &b.Text})}
		}
	case "thinking":
		if b.Thinking != "" {
			return []chunk{s.chunk(delta{ReasoningContent: b.Thinking})}
		}
	case "tool_use":
		t := &streamTool{index: len(s.tools)}
		if args := arguments(b.Input); args != "{}" {
			t.input = args
		}
		s.tools[index] = t
		return []chunk{s.toolChunk(toolCallDelta{Index: t.index, ID: b.ID, Type: "function", Function: functionCall{Name: b.Name}})}
	}
	return nil // redacted_thinking, server tool blocks and newer types
}

func (s *Stream) blockDelta(index int, d eventDelta) []chunk {
	switch d.Type {
	case "text_delta":
		if d.Text != "" {
			return []chunk{s.chunk(delta{Content: &d.Text})}
		}
	case "thinking_delta":
		if d.Thinking != "" {
			return []chunk{s.chunk(delta{ReasoningContent: d.Thinking})}
		}
	case "input_json_delta":
		// Server tool blocks stream input too; only client tool calls are forwarded.
		if t := s.tools[index]; t != nil && d.PartialJSON != "" {
			t.args = true
			return []chunk{s.toolChunk(toolCallDelta{Index: t.index, Function: functionCall{Arguments: d.PartialJSON}})}
		}
	}
	return nil // signature_delta, citations_delta and newer types
}

func (s *Stream) chunk(d delta) chunk {
	return chunk{ID: s.id, Object: "chat.completion.chunk", Created: s.created, Model: s.model, Choices: []chunkChoice{{Delta: d}}}
}

func (s *Stream) toolChunk(tc toolCallDelta) chunk {
	return s.chunk(delta{ToolCalls: []toolCallDelta{tc}})
}
