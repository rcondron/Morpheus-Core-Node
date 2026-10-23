package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

const (
	evStart = `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":25,"output_tokens":1}}}`
	evPing  = `{"type": "ping"}`
	evStop  = `{"type":"message_stop"}`
)

func evBlockStart(i int, block string) string {
	return fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, i, block)
}

func evBlockStop(i int) string { return fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i) }

// evDelta builds a content_block_delta whose delta has the given type and one string field.
func evDelta(i int, typ, field, value string) string {
	v, _ := json.Marshal(value)
	return fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":{"type":%q,%q:%s}}`, i, typ, field, v)
}

func evText(i int, s string) string     { return evDelta(i, "text_delta", "text", s) }
func evThinking(i int, s string) string { return evDelta(i, "thinking_delta", "thinking", s) }
func evJSON(i int, s string) string     { return evDelta(i, "input_json_delta", "partial_json", s) }

func evMessageDelta(stop string, outputTokens int) string {
	return fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, stop, outputTokens)
}

// view is a translated chunk reduced to what the tests compare.
type view struct {
	delta  string // compact choices[0].delta
	finish string // "" while finish_reason is null
	usage  string // compact usage, "" when absent
}

type streamResult struct {
	views     []view
	done      bool
	id, model string
}

// feed runs events through s, checking the fields every chunk must carry for
// Morpheus consumers and that the id, model and created time never change.
func feed(t *testing.T, s *Stream, events ...string) streamResult {
	t.Helper()
	var res streamResult
	var created int64
	for i, ev := range events {
		if res.done {
			t.Fatalf("event %d arrived after done", i)
		}
		chunks, done, err := s.Event([]byte(ev))
		if err != nil {
			t.Fatalf("event %d %s: %v", i, ev, err)
		}
		res.done = done
		for _, c := range chunks {
			var ch struct {
				ID      *string                      `json:"id"`
				Object  string                       `json:"object"`
				Created *int64                       `json:"created"`
				Model   *string                      `json:"model"`
				Choices []map[string]json.RawMessage `json:"choices"`
				Usage   json.RawMessage              `json:"usage"`
			}
			if err := json.Unmarshal(c, &ch); err != nil {
				t.Fatalf("chunk is not JSON: %v\n%s", err, c)
			}
			if ch.ID == nil || *ch.ID == "" || ch.Object != "chat.completion.chunk" || ch.Created == nil || ch.Model == nil || len(ch.Choices) != 1 {
				t.Fatalf("chunk lacks required fields: %s", c)
			}
			choice := ch.Choices[0]
			if string(choice["index"]) != "0" || choice["delta"] == nil || choice["finish_reason"] == nil {
				t.Fatalf("choice lacks index, delta or finish_reason: %s", c)
			}
			if res.id == "" {
				res.id, res.model, created = *ch.ID, *ch.Model, *ch.Created
			} else if *ch.ID != res.id || *ch.Model != res.model || *ch.Created != created {
				t.Fatalf("chunk identity changed mid-stream: %s", c)
			}
			v := view{delta: compact(t, choice["delta"])}
			_ = json.Unmarshal(choice["finish_reason"], &v.finish)
			if len(ch.Usage) > 0 {
				v.usage = compact(t, ch.Usage)
			}
			res.views = append(res.views, v)
		}
	}
	return res
}

func compact(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("invalid JSON %s: %v", raw, err)
	}
	return buf.String()
}

func checkViews(t *testing.T, got, want []view) {
	t.Helper()
	if !slices.Equal(got, want) {
		var b strings.Builder
		for _, v := range got {
			fmt.Fprintf(&b, "\t%+v\n", v)
		}
		t.Fatalf("chunks:\n%swant %d chunks:\n\t%+v", b.String(), len(want), want)
	}
}

const roleChunk = `{"role":"assistant","content":""}`

func TestStreamText(t *testing.T) {
	res := feed(t, NewStream("claude-alias"),
		evStart,
		evBlockStart(0, `{"type":"text","text":""}`),
		evPing,
		evText(0, "Hello"),
		evText(0, " world"),
		evBlockStop(0),
		evMessageDelta("end_turn", 15),
		evStop,
	)
	if !res.done {
		t.Fatal("not done after message_stop")
	}
	if res.id != "chatcmpl-msg_1" || res.model != "claude-opus-5-5" {
		t.Errorf("id %q model %q", res.id, res.model)
	}
	checkViews(t, res.views, []view{
		{delta: roleChunk},
		{delta: `{"content":"Hello"}`},
		{delta: `{"content":" world"}`},
		{delta: `{}`, finish: "stop", usage: `{"prompt_tokens":25,"completion_tokens":15,"total_tokens":40}`},
	})
}

func TestStreamInterleavedThinkingAndText(t *testing.T) {
	res := feed(t, NewStream("m"),
		evStart,
		evBlockStart(0, `{"type":"thinking","thinking":""}`),
		evThinking(0, "Let me think."),
		evThinking(0, " Twice."),
		evDelta(0, "signature_delta", "signature", "EqQBCgIYAhIM"),
		evBlockStop(0),
		evBlockStart(1, `{"type":"text","text":""}`),
		evText(1, "First part."),
		evBlockStop(1),
		evBlockStart(2, `{"type":"redacted_thinking","data":"EmwKAhgB"}`),
		evBlockStop(2),
		evBlockStart(3, `{"type":"thinking","thinking":""}`),
		evThinking(3, "More."),
		evThinking(3, ""),
		evBlockStop(3),
		evBlockStart(4, `{"type":"text","text":""}`),
		evText(4, " Second part."),
		evBlockStop(4),
		evMessageDelta("end_turn", 60),
		evStop,
	)
	checkViews(t, res.views, []view{
		{delta: roleChunk},
		{delta: `{"reasoning_content":"Let me think."}`},
		{delta: `{"reasoning_content":" Twice."}`},
		{delta: `{"content":"First part."}`},
		{delta: `{"reasoning_content":"More."}`},
		{delta: `{"content":" Second part."}`},
		{delta: `{}`, finish: "stop", usage: `{"prompt_tokens":25,"completion_tokens":60,"total_tokens":85}`},
	})
}

func TestStreamToolCall(t *testing.T) {
	res := feed(t, NewStream("m"),
		evStart,
		evBlockStart(0, `{"type":"text","text":""}`),
		evText(0, "Let me check."),
		evBlockStop(0),
		evBlockStart(1, `{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}`),
		evJSON(1, ""),
		evJSON(1, `{"city":`),
		evJSON(1, ` "Par`),
		evJSON(1, `is"}`),
		evBlockStop(1),
		evMessageDelta("tool_use", 40),
		evStop,
	)
	checkViews(t, res.views, []view{
		{delta: roleChunk},
		{delta: `{"content":"Let me check."}`},
		{delta: `{"tool_calls":[{"index":0,"id":"toolu_01","type":"function","function":{"name":"get_weather","arguments":""}}]}`},
		{delta: `{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`},
		{delta: `{"tool_calls":[{"index":0,"function":{"arguments":" \"Par"}}]}`},
		{delta: `{"tool_calls":[{"index":0,"function":{"arguments":"is\"}"}}]}`},
		{delta: `{}`, finish: "tool_calls", usage: `{"prompt_tokens":25,"completion_tokens":40,"total_tokens":65}`},
	})
	calls := assemble(t, res.views).calls
	if len(calls) != 1 || !sameJSON(t, []byte(calls[0].args), `{"city":"Paris"}`) {
		t.Fatalf("assembled calls %+v", calls)
	}
}

func TestStreamTwoToolCalls(t *testing.T) {
	res := feed(t, NewStream("m"),
		evStart,
		evBlockStart(0, `{"type":"thinking","thinking":""}`),
		evThinking(0, "Two lookups."),
		evBlockStop(0),
		evBlockStart(1, `{"type":"tool_use","id":"toolu_a","name":"get_weather","input":{}}`),
		evJSON(1, `{"city": "Oslo"`),
		evJSON(1, `}`),
		evBlockStop(1),
		evBlockStart(2, `{"type":"tool_use","id":"toolu_b","name":"get_time","input":{}}`),
		evJSON(2, `{"tz":`),
		evJSON(2, ` "CET"}`),
		evBlockStop(2),
		evMessageDelta("tool_use", 90),
		evStop,
	)
	got := assemble(t, res.views)
	want := []call{{id: "toolu_a", name: "get_weather", args: `{"city": "Oslo"}`}, {id: "toolu_b", name: "get_time", args: `{"tz": "CET"}`}}
	if !slices.Equal(got.calls, want) || got.finish != "tool_calls" || got.reasoning != "Two lookups." {
		t.Fatalf("assembled %+v", got)
	}
}

func TestStreamToolCallWithoutArgumentFragments(t *testing.T) {
	res := feed(t, NewStream("m"),
		evStart,
		evBlockStart(0, `{"type":"tool_use","id":"toolu_1","name":"list_files","input":{}}`),
		evBlockStop(0),
		evBlockStart(1, `{"type":"tool_use","id":"toolu_2","name":"read_file","input":{"path":"a.txt"}}`),
		evBlockStop(1),
		evMessageDelta("tool_use", 20),
	)
	checkViews(t, res.views, []view{
		{delta: roleChunk},
		{delta: `{"tool_calls":[{"index":0,"id":"toolu_1","type":"function","function":{"name":"list_files","arguments":""}}]}`},
		{delta: `{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`},
		{delta: `{"tool_calls":[{"index":1,"id":"toolu_2","type":"function","function":{"name":"read_file","arguments":""}}]}`},
		{delta: `{"tool_calls":[{"index":1,"function":{"arguments":"{\"path\":\"a.txt\"}"}}]}`},
		{delta: `{}`, finish: "tool_calls", usage: `{"prompt_tokens":25,"completion_tokens":20,"total_tokens":45}`},
	})
}

func TestStreamErrorEvent(t *testing.T) {
	s := NewStream("m")
	feed(t, s, evStart, evBlockStart(0, `{"type":"text","text":""}`), evText(0, "Hel"))
	chunks, done, err := s.Event([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	var ae *Error
	if !errors.As(err, &ae) || ae.Type != "overloaded_error" || ae.Message != "Overloaded" || ae.Status() != 529 {
		t.Fatalf("got %v", err)
	}
	if chunks != nil || done {
		t.Fatalf("error event produced chunks %q, done %v", chunks, done)
	}
	if _, _, err := NewStream("m").Event([]byte(`{"type":"error"}`)); !errors.As(err, &ae) || ae.Status() != 500 {
		t.Fatalf("error event without details: %v", err)
	}
}

func TestStreamIgnoresUntranslatedEvents(t *testing.T) {
	res := feed(t, NewStream("m"),
		evStart,
		evPing,
		`{"type":"a_future_event","payload":{"x":[1,2]}}`,
		evBlockStart(0, `{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{}}`),
		evJSON(0, `{"query":"news"}`),
		evBlockStop(0),
		evBlockStart(1, `{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://example.com"}]}`),
		evBlockStop(1),
		evBlockStart(2, `{"type":"text","text":""}`),
		`{"type":"content_block_delta","index":2,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com"}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"a_future_delta","text":7}}`,
		evBlockStop(2),
		evMessageDelta("end_turn", 5),
		evStop,
	)
	checkViews(t, res.views, []view{
		{delta: roleChunk},
		{delta: `{}`, finish: "stop", usage: `{"prompt_tokens":25,"completion_tokens":5,"total_tokens":30}`},
	})
}

func TestStreamUsageUpdatedByMessageDelta(t *testing.T) {
	res := feed(t, NewStream("m"),
		`{"type":"message_start","message":{"id":"msg_2","model":"claude-opus-5-5","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":1}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":30,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":512,"server_tool_use":{"web_search_requests":1}}}`,
	)
	checkViews(t, res.views, []view{
		{delta: roleChunk},
		{delta: `{}`, finish: "length", usage: `{"prompt_tokens":1130,"completion_tokens":512,"total_tokens":1642,"prompt_tokens_details":{"cached_tokens":1000}}`},
	})
}

func TestStreamWithoutMessageStart(t *testing.T) {
	res := feed(t, NewStream("claude-alias"), evText(0, "orphan"))
	if !strings.HasPrefix(res.id, "chatcmpl-") || res.model != "claude-alias" {
		t.Errorf("id %q model %q", res.id, res.model)
	}
	checkViews(t, res.views, []view{{delta: roleChunk}, {delta: `{"content":"orphan"}`}})
}

func TestStreamRejectsMalformedEvents(t *testing.T) {
	for _, ev := range []string{
		``,
		`not json`,
		`[DONE]`,
		`{"type":"content_block_delta","index":"0"}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":5}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":{}}}`,
		`{"type":"message_delta","delta":{"stop_reason":1}}`,
		`{"type":"message_start","message":{"usage":{"input_tokens":"many"}}}`,
		evBlockStart(0, `{"type":"text","text":5}`),
		evBlockStart(0, `{"type":"tool_use","id":1}`),
	} {
		if chunks, _, err := NewStream("m").Event([]byte(ev)); err == nil {
			t.Errorf("%q accepted, produced %q", ev, chunks)
		}
	}
}

// TestStreamTranscript replays an Anthropic SSE transcript the way a provider
// reads it (data lines only) and folds the chunks as an OpenAI client would.
func TestStreamTranscript(t *testing.T) {
	f, err := os.Open("testdata/tool_use_stream.sse")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			events = append(events, strings.TrimSpace(data))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}

	res := feed(t, NewStream("claude-sonnet"), events...)
	if !res.done {
		t.Fatal("transcript did not finish")
	}
	if res.id != "chatcmpl-msg_01Q8Rz2nYwTGNfGqvW8xkp3e" || res.model != "claude-sonnet-4-5-20250929" {
		t.Errorf("id %q model %q", res.id, res.model)
	}
	for i, v := range res.views {
		last := i == len(res.views)-1
		if strings.Contains(v.delta, `"role"`) != (i == 0) || (v.finish != "") != last || (v.usage != "") != last {
			t.Errorf("chunk %d out of place: %+v", i, v)
		}
	}
	got := assemble(t, res.views)
	want := assembled{
		content:   "I'll check the weather and the time in Paris for you.",
		reasoning: "The user wants the weather in Paris and the local time there. Both tools can run at once.",
		calls: []call{
			{id: "toolu_01T1x1fJ34qAmk2tNTrN7Up6", name: "get_weather", args: `{"location": "Paris, France", "unit": "celsius"}`},
			{id: "toolu_01VkUcrA9JnK4bqP4yDK3Hd8", name: "get_time", args: `{"timezone": "Europe/Paris"}`},
		},
		finish: "tool_calls",
		usage:  `{"prompt_tokens":2560,"completion_tokens":187,"total_tokens":2747,"prompt_tokens_details":{"cached_tokens":2048}}`,
	}
	if got.content != want.content || got.reasoning != want.reasoning || got.finish != want.finish || got.usage != want.usage {
		t.Errorf("assembled\n\t%+v\nwant\n\t%+v", got, want)
	}
	if !slices.Equal(got.calls, want.calls) {
		t.Errorf("tool calls\n\t%+v\nwant\n\t%+v", got.calls, want.calls)
	}
	for _, c := range got.calls {
		if !json.Valid([]byte(c.args)) {
			t.Errorf("%s arguments are not JSON: %s", c.name, c.args)
		}
	}
}

type call struct{ id, name, args string }

// assembled is a streamed answer as an OpenAI client puts it together.
type assembled struct {
	content, reasoning string
	calls              []call
	finish, usage      string
}

// assemble folds chunk deltas: text and reasoning concatenate, and tool call
// fragments join by their index.
func assemble(t *testing.T, views []view) assembled {
	t.Helper()
	var m assembled
	var content, reasoning strings.Builder
	for _, v := range views {
		var d struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal([]byte(v.delta), &d); err != nil {
			t.Fatalf("delta %s: %v", v.delta, err)
		}
		content.WriteString(d.Content)
		reasoning.WriteString(d.ReasoningContent)
		for _, tc := range d.ToolCalls {
			if tc.Index == len(m.calls) {
				m.calls = append(m.calls, call{})
			} else if tc.Index > len(m.calls) {
				t.Fatalf("tool call index %d skips ahead of %d", tc.Index, len(m.calls))
			}
			c := &m.calls[tc.Index]
			c.id += tc.ID
			c.name += tc.Function.Name
			c.args += tc.Function.Arguments
		}
		if v.finish != "" {
			m.finish, m.usage = v.finish, v.usage
		}
	}
	m.content, m.reasoning = content.String(), reasoning.String()
	return m
}
