package anthropic

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxErrorText caps how much of a non-JSON error body (a proxy's HTML page)
// becomes the relayed error message.
const maxErrorText = 1024

// errorStatus is the HTTP status Anthropic answers with for each error type.
var errorStatus = map[string]int{
	"invalid_request_error": http.StatusBadRequest,
	"authentication_error":  http.StatusUnauthorized,
	"billing_error":         http.StatusPaymentRequired,
	"permission_error":      http.StatusForbidden,
	"not_found_error":       http.StatusNotFound,
	"request_too_large":     http.StatusRequestEntityTooLarge,
	"rate_limit_error":      http.StatusTooManyRequests,
	"api_error":             http.StatusInternalServerError,
	"timeout_error":         http.StatusGatewayTimeout,
	"overloaded_error":      529,
}

// Error is an error Anthropic reported instead of a message, such as the
// overloaded_error event that can interrupt a stream.
type Error struct {
	Type    string // e.g. "overloaded_error", "rate_limit_error"
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("anthropic %s: %s", e.Type, e.Message) }

// Status returns the HTTP status Anthropic uses for the error's type (500 when
// unknown), for relaying a mid-stream error with ErrorBody.
func (e *Error) Status() int {
	if s, ok := errorStatus[e.Type]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// anthropicMessage is a non-streaming Messages response (or an error body).
type anthropicMessage struct {
	ID         string            `json:"id"`
	Type       string            `json:"type"`
	Model      string            `json:"model"`
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      usage             `json:"usage"`
	Error      *apiError         `json:"error"`
}

// contentBlock holds the fields of the content block types this package translates.
type contentBlock struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	ID       string          `json:"id"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
}

type usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

type apiError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// completion is an OpenAI chat.completion object.
type completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []completionChoice `json:"choices"`
	Usage   *chatUsage         `json:"usage"`
}

type completionChoice struct {
	Index        int               `json:"index"`
	Message      completionMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type completionMessage struct {
	Role             string     `json:"role"`
	Content          *string    `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCall `json:"tool_calls,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

type chatUsage struct {
	PromptTokens        int           `json:"prompt_tokens"`
	CompletionTokens    int           `json:"completion_tokens"`
	TotalTokens         int           `json:"total_tokens"`
	PromptTokensDetails *tokenDetails `json:"prompt_tokens_details,omitempty"`
}

type tokenDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// errorObject is the body of an OpenAI-style error response.
type errorObject struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// Response translates a non-streaming Anthropic Messages response into an
// OpenAI chat.completion object. Text blocks become the message content (null
// when the answer is only tool calls), tool_use blocks its tool_calls and
// thinking blocks its reasoning_content. An Anthropic error body comes back as
// an *Error.
func Response(anthropicBody []byte) ([]byte, error) {
	var m anthropicMessage
	if err := json.Unmarshal(anthropicBody, &m); err != nil {
		return nil, fmt.Errorf("invalid Anthropic response: %w", err)
	}
	if m.Type == "error" && m.Error != nil {
		return nil, &Error{Type: m.Error.Type, Message: m.Error.Message}
	}
	if m.Type != "message" {
		return nil, fmt.Errorf("unexpected Anthropic response type %q", m.Type)
	}
	var text, reasoning strings.Builder
	var calls []toolCall
	for _, raw := range m.Content {
		b, err := decodeBlock(raw)
		if err != nil {
			return nil, err
		}
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "thinking":
			reasoning.WriteString(b.Thinking)
		case "tool_use":
			calls = append(calls, toolCall{ID: b.ID, Type: "function", Function: functionCall{Name: b.Name, Arguments: arguments(b.Input)}})
		}
	}
	msg := completionMessage{Role: "assistant", ReasoningContent: reasoning.String(), ToolCalls: calls}
	if text.Len() > 0 || len(calls) == 0 {
		s := text.String()
		msg.Content = &s
	}
	return marshal(completion{
		ID:      "chatcmpl-" + m.ID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   m.Model,
		Choices: []completionChoice{{Message: msg, FinishReason: finishReason(m.StopReason)}},
		Usage:   m.Usage.openAI(),
	})
}

// decodeBlock decodes a content block. Only the block types this package
// translates must decode cleanly; newer types are ignored whatever their shape.
func decodeBlock(raw json.RawMessage) (contentBlock, error) {
	var b contentBlock
	if err := json.Unmarshal(raw, &b); err != nil {
		switch b.Type {
		case "text", "thinking", "tool_use":
			return b, fmt.Errorf("invalid Anthropic %s block: %w", b.Type, err)
		}
		return contentBlock{}, nil
	}
	return b, nil
}

// arguments renders a tool_use input as OpenAI's JSON-encoded arguments string.
func arguments(input json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, input) != nil || buf.Len() == 0 || buf.String() == "null" {
		return "{}"
	}
	return buf.String()
}

// finishReason maps an Anthropic stop_reason onto OpenAI's finish_reason.
func finishReason(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return "stop" // end_turn, stop_sequence, pause_turn and stop reasons added later
}

// openAI converts usage. Anthropic counts cached prompt tokens apart from
// input_tokens; OpenAI's prompt_tokens includes them.
func (u usage) openAI() *chatUsage {
	in := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	out := &chatUsage{PromptTokens: in, CompletionTokens: u.OutputTokens, TotalTokens: in + u.OutputTokens}
	if u.CacheReadInputTokens > 0 {
		out.PromptTokensDetails = &tokenDetails{CachedTokens: u.CacheReadInputTokens}
	}
	return out
}

// ErrorBody normalizes an Anthropic error response body into an OpenAI-style
// error object, {"error":{"message","type","param","code"}}, to relay with the
// same HTTP status. type keeps Anthropic's error type (rate_limit_error,
// overloaded_error, ...), derived from status when the body names none, and
// code is the status as a string. A body that is not JSON (a proxy's error
// page) becomes the message.
func ErrorBody(status int, body []byte) []byte {
	var e struct {
		Error apiError `json:"error"`
	}
	var msg, typ string
	if err := json.Unmarshal(body, &e); err == nil {
		msg, typ = e.Error.Message, e.Error.Type
	} else if text := strings.TrimSpace(string(body)); len(text) > maxErrorText {
		msg = strings.ToValidUTF8(text[:maxErrorText], "") + "…"
	} else {
		msg = text
	}
	if typ == "" {
		typ = errorType(status)
	}
	msg = cmp.Or(msg, http.StatusText(status), "upstream error")
	out, _ := marshal(struct {
		Error errorObject `json:"error"`
	}{errorObject{Message: msg, Type: typ, Code: strconv.Itoa(status)}})
	return out
}

// errorType names the Anthropic error type for an HTTP status.
func errorType(status int) string {
	for typ, s := range errorStatus {
		if s == status {
			return typ
		}
	}
	if status >= 500 {
		return "api_error"
	}
	return "invalid_request_error"
}
