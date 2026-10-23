// Package anthropic translates between the OpenAI Chat Completions format that
// Morpheus consumers speak and Anthropic's Messages API, so a provider can
// resell Claude models: Request converts a consumer's request, Response and
// Stream turn Claude's answer into chat.completion and chat.completion.chunk
// objects, and ErrorBody normalizes Anthropic error bodies.
//
// Anthropic's request surface differs between Claude generations, so Request
// reads the generation from the model ID: Claude 4.7 and later reject
// temperature, top_p, top_k and budget_tokens thinking; models before 4.6 have
// no adaptive thinking or effort control; the newest Opus, Sonnet, Fable and
// Mythos generations reject forced tool use (rejectsForcedTools). IDs it
// cannot read are treated as current models. Translated bodies are written for anthropic-version
// APIVersion.
package anthropic

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// APIVersion is the anthropic-version header to send with translated
// requests. Version 2026-10-21 rejects temperature, top_p and top_k on every
// model, and Request still forwards them to Claude 4.6 and older.
const APIVersion = "2023-06-01"

// fallbackMaxTokens bounds the answer when neither the request nor the caller
// does; Anthropic requires max_tokens.
const fallbackMaxTokens = 8192

// Claude versions (major*100+minor) at which the request surface changes.
const (
	claude37 = 307 // extended thinking, sized by budget_tokens
	claude4  = 400 // temperature and top_p may no longer be combined
	claude46 = 406 // adaptive thinking and output_config.effort
	claude47 = 407 // temperature, top_p and top_k rejected; xhigh effort
	latest   = 1 << 20
)

// thinkingBudgets sizes budget_tokens per reasoning_effort for Claude 3.7 to
// 4.5, which take a token budget instead of an effort level.
var thinkingBudgets = map[string]int{"low": 1024, "medium": 4096, "high": 16384, "xhigh": 32768}

// chatRequest holds the OpenAI request fields that have an Anthropic
// equivalent; decoding into it drops everything else.
type chatRequest struct {
	Messages            []chatMessage   `json:"messages"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	TopK                *int            `json:"top_k"`
	Stop                json.RawMessage `json:"stop"`
	Stream              bool            `json:"stream"`
	User                string          `json:"user"`
	SafetyIdentifier    string          `json:"safety_identifier"`
	Tools               []chatTool      `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	ParallelToolCalls   *bool           `json:"parallel_tool_calls"`
	ReasoningEffort     string          `json:"reasoning_effort"`
}

type chatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []chatToolCall  `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type chatToolCall struct {
	ID       string `json:"id"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type chatPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Refusal  string          `json:"refusal"`
	ImageURL json.RawMessage `json:"image_url"`
}

type chatTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// messagesRequest is the Anthropic Messages request body.
type messagesRequest struct {
	Model         string        `json:"model"`
	MaxTokens     int           `json:"max_tokens"`
	System        string        `json:"system,omitempty"`
	Messages      []message     `json:"messages"`
	Tools         []tool        `json:"tools,omitempty"`
	ToolChoice    *toolChoice   `json:"tool_choice,omitempty"`
	Thinking      *thinking     `json:"thinking,omitempty"`
	OutputConfig  *outputConfig `json:"output_config,omitempty"`
	Temperature   *float64      `json:"temperature,omitempty"`
	TopP          *float64      `json:"top_p,omitempty"`
	TopK          *int          `json:"top_k,omitempty"`
	StopSequences []string      `json:"stop_sequences,omitempty"`
	Metadata      *metadata     `json:"metadata,omitempty"`
	Stream        bool          `json:"stream,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

// block is any Anthropic content block; fields a type does not use stay empty.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Source    *imageSource    `json:"source,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   []block         `json:"content,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type toolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type outputConfig struct {
	Effort string `json:"effort"`
}

type metadata struct {
	UserID string `json:"user_id"`
}

// Request translates an OpenAI chat completions request body into an
// Anthropic Messages request for model. defaultMaxTokens applies when the
// request sets neither max_tokens nor max_completion_tokens (8192 when it is
// not positive). stream reports whether the consumer asked for streaming.
//
// System and developer messages, wherever they appear, become the system
// prompt, one paragraph per text part. Fields without an Anthropic equivalent
// (n, logprobs, penalties, response_format, seed, stream_options, ...) are
// dropped, as are legacy functions and role "function" messages. Tool call
// arguments that are not a JSON object are sent as {}: they are history the
// consumer cannot repair.
func Request(openaiBody []byte, model string, defaultMaxTokens int) (body []byte, stream bool, err error) {
	if model == "" {
		return nil, false, errors.New("no Anthropic model given")
	}
	var req chatRequest
	if err := json.Unmarshal(openaiBody, &req); err != nil {
		return nil, false, fmt.Errorf("invalid chat completions request: %w", err)
	}
	system, msgs, err := convertMessages(req.Messages)
	if err != nil {
		return nil, false, err
	}
	out := messagesRequest{Model: model, MaxTokens: maxTokens(req, defaultMaxTokens), Messages: msgs, Stream: req.Stream}
	if out.StopSequences, err = stopSequences(req.Stop); err != nil {
		return nil, false, err
	}
	if id := cmp.Or(req.SafetyIdentifier, req.User); id != "" && len(id) <= 256 {
		out.Metadata = &metadata{UserID: id}
	}
	family, gen := parseModel(model)
	note, err := out.setTools(req, family, gen)
	if err != nil {
		return nil, false, err
	}
	if note != "" {
		system = append(system, note)
	}
	out.System = strings.Join(system, "\n\n")
	out.setReasoning(req.ReasoningEffort, gen)
	out.setSampling(req, gen)
	body, err = marshal(out)
	return body, req.Stream, err
}

// convertMessages hoists system and developer messages into the system prompt
// and folds the rest into alternating user and assistant turns.
func convertMessages(in []chatMessage) (system []string, msgs []message, err error) {
	add := func(role string, blocks ...block) {
		if len(blocks) == 0 {
			return
		}
		if n := len(msgs); n > 0 && msgs[n-1].Role == role {
			msgs[n-1].Content = append(msgs[n-1].Content, blocks...)
			return
		}
		msgs = append(msgs, message{Role: role, Content: blocks})
	}
	for i, m := range in {
		blocks, err := contentBlocks(m.Content)
		if err != nil {
			return nil, nil, fmt.Errorf("messages[%d]: %w", i, err)
		}
		switch m.Role {
		case "system", "developer":
			for _, b := range blocks {
				if b.Type != "text" {
					return nil, nil, fmt.Errorf("messages[%d]: %s messages may only contain text", i, m.Role)
				}
				system = append(system, b.Text)
			}
		case "user":
			add("user", blocks...)
		case "assistant":
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, block{Type: "tool_use", ID: toolID(tc.ID), Name: tc.Function.Name, Input: toolInput(tc.Function.Arguments)})
			}
			add("assistant", blocks...)
		case "tool":
			add("user", block{Type: "tool_result", ToolUseID: toolID(m.ToolCallID), Content: blocks})
		case "function":
			// Legacy function results carry no call ID to pair with a tool_use block.
		default:
			return nil, nil, fmt.Errorf("messages[%d]: unsupported role %q", i, m.Role)
		}
	}
	if len(msgs) == 0 {
		return nil, nil, errors.New("messages must include a user or assistant message")
	}
	if msgs[0].Role != "user" {
		// Claude conversations open with a user turn; chat UIs often start
		// with the assistant's greeting.
		msgs = slices.Insert(msgs, 0, message{Role: "user", Content: []block{{Type: "text", Text: "."}}})
	}
	rank := func(b block) int {
		if b.Type == "tool_result" {
			return 0
		}
		return 1
	}
	for _, m := range msgs {
		if m.Role == "user" {
			// Anthropic wants a turn's tool results ahead of anything else in it.
			slices.SortStableFunc(m.Content, func(a, b block) int { return rank(a) - rank(b) })
		}
	}
	return system, msgs, nil
}

// contentBlocks converts OpenAI message content (a string, null or an array of
// parts) into Anthropic blocks, dropping the blank text Anthropic rejects.
func contentBlocks(raw json.RawMessage) ([]block, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return textBlock(s), nil
	}
	var parts []chatPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, errors.New("content must be a string or an array of content parts")
	}
	var blocks []block
	for _, p := range parts {
		switch p.Type {
		case "text":
			blocks = append(blocks, textBlock(p.Text)...)
		case "refusal":
			blocks = append(blocks, textBlock(p.Refusal)...)
		case "image_url":
			b, err := imageBlock(p.ImageURL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, b)
		default:
			return nil, fmt.Errorf("content part type %q is not supported", p.Type)
		}
	}
	return blocks, nil
}

func textBlock(s string) []block {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return []block{{Type: "text", Text: s}}
}

// imageBlock converts an image_url part: data URLs become base64 sources and
// http(s) URLs are left for Anthropic to fetch.
func imageBlock(raw json.RawMessage) (block, error) {
	var img struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(raw, &img) != nil {
		_ = json.Unmarshal(raw, &img.URL) // some clients send the URL as a bare string
	}
	u := img.URL
	switch {
	case strings.HasPrefix(u, "data:"):
		meta, data, ok := strings.Cut(u[len("data:"):], ",")
		if !ok || !strings.HasSuffix(meta, ";base64") || data == "" {
			return block{}, errors.New("image data URLs must be base64-encoded")
		}
		mediaType, _, _ := strings.Cut(meta, ";")
		if mediaType == "image/jpg" {
			mediaType = "image/jpeg" // common misspelling Anthropic rejects
		}
		return block{Type: "image", Source: &imageSource{Type: "base64", MediaType: mediaType, Data: data}}, nil
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		return block{Type: "image", Source: &imageSource{Type: "url", URL: u}}, nil
	}
	return block{}, errors.New("image_url must be a data: or http(s) URL")
}

// toolID maps a tool call ID onto the characters Anthropic allows
// ([a-zA-Z0-9_-]); calls and results map alike, so they stay paired.
func toolID(id string) string {
	return strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' {
			return r
		}
		return '_'
	}, id)
}

// toolInput turns a tool call's arguments (a JSON-encoded string, or the
// object itself from lenient clients) into Anthropic's input object; anything
// else becomes {}.
func toolInput(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj == nil {
		return json.RawMessage(`{}`)
	}
	return raw
}

// setTools converts tool definitions and tool_choice. Forced tool use on a
// model that rejects it becomes auto plus the returned system prompt note,
// the substitute Anthropic recommends.
func (r *messagesRequest) setTools(req chatRequest, family string, gen int) (note string, err error) {
	for i, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		schema, err := inputSchema(t.Function.Parameters)
		if err != nil {
			return "", fmt.Errorf("tools[%d]: %w", i, err)
		}
		r.Tools = append(r.Tools, tool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
	}
	if len(r.Tools) == 0 {
		// Anthropic rejects tool_use history without tool definitions, so
		// declare the tools the history used and forbid new calls.
		for _, name := range usedTools(r.Messages) {
			r.Tools = append(r.Tools, tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)})
		}
		if len(r.Tools) > 0 {
			r.ToolChoice = &toolChoice{Type: "none"}
		}
		return "", nil
	}
	choice, err := parseToolChoice(req.ToolChoice)
	if err != nil {
		return "", err
	}
	if forced(choice) && rejectsForcedTools(family, gen) {
		note = "Use the provided tools to answer."
		if choice.Type == "tool" {
			note = fmt.Sprintf("Use the %s tool to answer.", choice.Name)
		}
		choice = &toolChoice{Type: "auto"}
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		if choice == nil {
			choice = &toolChoice{Type: "auto"}
		}
		if choice.Type != "none" {
			choice.DisableParallelToolUse = true
		}
	}
	r.ToolChoice = choice
	return note, nil
}

// inputSchema returns a function's parameters as an object JSON Schema,
// which Anthropic requires even for tools without parameters.
func inputSchema(raw json.RawMessage) (json.RawMessage, error) {
	if isNull(raw) {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	var schema map[string]json.RawMessage
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil, errors.New("function parameters must be a JSON Schema object")
	}
	if _, ok := schema["type"]; ok {
		return raw, nil
	}
	schema["type"] = json.RawMessage(`"object"`)
	return marshal(schema)
}

// parseToolChoice maps OpenAI's tool_choice; nil leaves Anthropic's default (auto).
func parseToolChoice(raw json.RawMessage) (*toolChoice, error) {
	if isNull(raw) {
		return nil, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "auto":
			return &toolChoice{Type: "auto"}, nil
		case "required":
			return &toolChoice{Type: "any"}, nil
		case "none":
			return &toolChoice{Type: "none"}, nil
		}
		return nil, fmt.Errorf("unsupported tool_choice %q", s)
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, errors.New("tool_choice must be a string or an object")
	}
	if name := cmp.Or(obj.Function.Name, obj.Name); obj.Type == "function" && name != "" {
		return &toolChoice{Type: "tool", Name: name}, nil
	}
	return &toolChoice{Type: "auto"}, nil // allowed_tools and other modes Claude cannot express
}

func forced(c *toolChoice) bool { return c != nil && (c.Type == "any" || c.Type == "tool") }

// rejectsForcedTools reports the generations that answer tool_choice any/tool
// with a 400.
func rejectsForcedTools(family string, gen int) bool {
	switch family {
	case "opus", "sonnet":
		return gen >= 505
	case "fable", "mythos":
		return gen >= 501
	}
	return false
}

// usedTools lists the tool names the conversation's tool_use blocks call.
func usedTools(msgs []message) []string {
	var names []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if b.Type == "tool_use" && b.Name != "" && !slices.Contains(names, b.Name) {
				names = append(names, b.Name)
			}
		}
	}
	return names
}

// setReasoning maps reasoning_effort onto Claude's thinking controls. From
// Claude 4.6 on it enables adaptive thinking at the matching
// output_config.effort ("none" and "minimal" only lower the effort, since the
// newest models cannot turn thinking off). Claude 3.7 to 4.5 get a
// budget_tokens budget from thinkingBudgets, capped at half of max_tokens and
// skipped below Anthropic's 1024 minimum. Older models cannot think.
// Thinking is never combined with forced tool use, which Anthropic rejects.
func (r *messagesRequest) setReasoning(effort string, gen int) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" || forced(r.ToolChoice) {
		return
	}
	switch {
	case gen >= claude46:
		level := effort
		switch effort {
		case "none", "minimal":
			r.OutputConfig = &outputConfig{Effort: "low"}
			return
		case "low", "medium", "high":
		case "xhigh":
			if gen < claude47 {
				level = "high" // xhigh arrived with Claude 4.7
			}
		default:
			return
		}
		r.Thinking = &thinking{Type: "adaptive"}
		r.OutputConfig = &outputConfig{Effort: level}
	case gen >= claude37:
		budget := min(thinkingBudgets[effort], r.MaxTokens/2)
		if budget < 1024 || !budgetThinkingAllowed(r.Messages) {
			return
		}
		r.Thinking = &thinking{Type: "enabled", BudgetTokens: budget}
	}
}

// budgetThinkingAllowed reports whether budget_tokens thinking may be enabled.
// It may not with an assistant prefill, nor while a tool round trip is
// pending: that assistant turn would have to begin with the signed thinking
// block an OpenAI client cannot send back.
func budgetThinkingAllowed(msgs []message) bool {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" {
			return i < len(msgs)-1 && !slices.ContainsFunc(msgs[i].Content, func(b block) bool { return b.Type == "tool_use" })
		}
	}
	return true
}

// setSampling forwards temperature (clamped to Claude's 0..1), top_p and top_k
// where the model takes them: not with thinking, not from Claude 4.7 on, and
// from Claude 4 on only one of temperature and top_p (temperature wins).
func (r *messagesRequest) setSampling(req chatRequest, gen int) {
	if r.Thinking != nil || gen >= claude47 {
		return
	}
	if t := req.Temperature; t != nil {
		v := min(max(*t, 0), 1)
		r.Temperature = &v
	}
	if p := req.TopP; p != nil && (r.Temperature == nil || gen < claude4) {
		v := min(max(*p, 0), 1)
		r.TopP = &v
	}
	if k := req.TopK; k != nil && *k > 0 {
		r.TopK = k
	}
}

func maxTokens(req chatRequest, def int) int {
	for _, v := range []*int{req.MaxCompletionTokens, req.MaxTokens} {
		if v != nil && *v > 0 {
			return *v
		}
	}
	if def > 0 {
		return def
	}
	return fallbackMaxTokens
}

// stopSequences converts stop (a string or an array of strings), dropping
// whitespace-only sequences, which Anthropic rejects.
func stopSequences(raw json.RawMessage) ([]string, error) {
	if isNull(raw) {
		return nil, nil
	}
	var list []string
	var one string
	if json.Unmarshal(raw, &one) == nil {
		list = []string{one}
	} else if json.Unmarshal(raw, &list) != nil {
		return nil, errors.New("stop must be a string or an array of strings")
	}
	var out []string
	for _, s := range list {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// parseModel reads the family and version from a Claude model ID:
// claude-sonnet-4-5-20250929 is ("sonnet", 405), claude-3-7-sonnet-latest
// ("sonnet", 307) and us.anthropic.claude-opus-4-1-20250805-v1:0 ("opus",
// 401). An ID without a version is taken for a current model.
func parseModel(model string) (family string, gen int) {
	id := strings.ToLower(model)
	if i := strings.Index(id, "claude-"); i >= 0 {
		id = id[i+len("claude-"):]
		if j := strings.IndexAny(id, "@:"); j >= 0 {
			id = id[:j] // Vertex "@date" and Bedrock ":0" suffixes
		}
		fields := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '.' })
		for i, f := range fields {
			if n, ok := versionPart(f); ok && gen == 0 {
				gen = n * 100
				if i+1 < len(fields) {
					if minor, ok := versionPart(fields[i+1]); ok {
						gen += minor
					}
				}
			} else if family == "" && strings.Trim(f, "abcdefghijklmnopqrstuvwxyz") == "" {
				family = f
			}
		}
	}
	if gen == 0 {
		gen = latest
	}
	return family, gen
}

// versionPart parses a one- or two-digit version number; longer runs of
// digits in model IDs are snapshot dates.
func versionPart(s string) (int, bool) {
	if len(s) == 0 || len(s) > 2 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func isNull(raw json.RawMessage) bool { return len(raw) == 0 || string(raw) == "null" }

// marshal is json.Marshal without HTML escaping, so the <, > and & common in
// code travel as written instead of as Unicode escapes.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
