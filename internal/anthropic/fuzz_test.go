package anthropic

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// The fuzz targets run their seeds with every go test; explore further with
// e.g. go test -fuzz=FuzzRequest ./internal/anthropic/.

func FuzzRequest(f *testing.F) {
	for _, body := range []string{
		`{"messages":[{"role":"user","content":"hi"}]}`,
		`{"messages":[{"role":"system","content":[{"type":"text","text":"s"}]},{"role":"assistant","content":"a"},{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"temperature":3,"top_p":0.5,"top_k":5,"stop":["x"," "],"user":"u"}`,
		`{"messages":[{"role":"user","content":"q"},{"role":"assistant","content":null,"tool_calls":[{"id":"c:1","function":{"name":"f","arguments":"{\"a\":"}}]},{"role":"tool","tool_call_id":"c:1","content":"r"}],"reasoning_effort":"high","max_tokens":3000}`,
		`{"messages":[{"role":"user","content":"q"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"properties":{}}}}],"tool_choice":{"type":"function","function":{"name":"f"}},"parallel_tool_calls":false,"reasoning_effort":"xhigh"}`,
		`{"messages":[{"role":"assistant","content":"prefill"}],"reasoning_effort":"low","max_completion_tokens":2049}`,
		`{"messages":[]}`,
		`{"messages":[{"role":"user","content":[{"type":"text"}]}]}`,
		`nul`,
	} {
		for _, model := range []string{"claude-opus-5-5", "claude-sonnet-4-5", "claude-3-7-sonnet-latest", "claude-3-5-haiku", "x"} {
			f.Add(body, model)
		}
	}
	f.Fuzz(func(t *testing.T, body, model string) {
		out, _, err := Request([]byte(body), model, 1024)
		if err != nil {
			return
		}
		var r messagesRequest
		if err := json.Unmarshal(out, &r); err != nil {
			t.Fatalf("invalid output %s: %v", out, err)
		}
		if (utf8.ValidString(model) && r.Model != model) || r.MaxTokens <= 0 || len(r.Messages) == 0 || r.Messages[0].Role != "user" {
			t.Fatalf("bad request shape: %s", out)
		}
		for i, m := range r.Messages {
			if len(m.Content) == 0 || (i > 0 && m.Role == r.Messages[i-1].Role) || (m.Role != "user" && m.Role != "assistant") {
				t.Fatalf("message %d breaks alternation or is empty: %s", i, out)
			}
			for _, b := range m.Content {
				if b.Type == "text" && strings.TrimSpace(b.Text) == "" {
					t.Fatalf("blank text block: %s", out)
				}
				var input map[string]json.RawMessage
				if b.Type == "tool_use" && (json.Unmarshal(b.Input, &input) != nil || input == nil) {
					t.Fatalf("tool_use input is not an object: %s", out)
				}
			}
		}
		if th := r.Thinking; th != nil {
			if th.Type == "enabled" && (th.BudgetTokens < 1024 || th.BudgetTokens >= r.MaxTokens) {
				t.Fatalf("invalid thinking budget: %s", out)
			}
			if r.Temperature != nil || r.TopP != nil || r.TopK != nil {
				t.Fatalf("sampling parameters sent with thinking: %s", out)
			}
		}
		if r.Temperature != nil && r.TopP != nil {
			if _, gen := parseModel(model); gen >= claude4 {
				t.Fatalf("temperature and top_p combined for Claude 4+: %s", out)
			}
		}
		if v := r.Temperature; v != nil && (*v < 0 || *v > 1) {
			t.Fatalf("temperature out of range: %s", out)
		}
	})
}

func FuzzResponse(f *testing.F) {
	f.Add([]byte(`{"id":"msg_1","type":"message","model":"m","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t","name":"n","input":{"a":[1]}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`))
	f.Add([]byte(`{"type":"message","content":[{"type":"thinking","thinking":"x"},{"type":"tool_use","input":null},{"type":"mystery","text":1}]}`))
	f.Add([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	f.Add([]byte(`{"type":"message","content":null}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		out, err := Response(body)
		if err != nil {
			return
		}
		var c completion
		if err := json.Unmarshal(out, &c); err != nil || c.Object != "chat.completion" || len(c.Choices) != 1 || c.Usage == nil {
			t.Fatalf("bad completion %s: %v", out, err)
		}
		for _, tc := range c.Choices[0].Message.ToolCalls {
			if !json.Valid([]byte(tc.Function.Arguments)) {
				t.Fatalf("tool arguments are not JSON: %s", out)
			}
		}
	})
}

func FuzzStream(f *testing.F) {
	transcript, err := os.ReadFile("testdata/tool_use_stream.sse")
	if err != nil {
		f.Fatal(err)
	}
	var events []string
	for _, line := range strings.Split(string(transcript), "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			events = append(events, data)
		}
	}
	f.Add(strings.Join(events, "\n"))
	f.Add(evStart + "\n" + evBlockStart(0, `{"type":"tool_use","id":"t","name":"n","input":{"k":1}}`) + "\n" + evBlockStop(0) + "\n" + evMessageDelta("tool_use", 3))
	f.Add(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n" + evText(0, "after error"))
	f.Add(evJSON(4, "{") + "\n" + evBlockStop(4) + "\n" + `{"type":"message_delta"}`)
	f.Fuzz(func(t *testing.T, input string) {
		s := NewStream("m")
		for _, ev := range strings.Split(input, "\n") {
			chunks, _, _ := s.Event([]byte(ev))
			for _, c := range chunks {
				var ch chunk
				if err := json.Unmarshal(c, &ch); err != nil || ch.Object != "chat.completion.chunk" || ch.ID == "" || len(ch.Choices) != 1 {
					t.Fatalf("bad chunk %s: %v", c, err)
				}
			}
		}
	})
}

func FuzzErrorBody(f *testing.F) {
	f.Add(429, []byte(`{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`))
	f.Add(502, []byte("<html>bad gateway</html>"))
	f.Add(0, []byte(""))
	f.Add(529, []byte(`{"error":"plain"}`))
	f.Add(400, []byte("\xff\xfe"+strings.Repeat("x", 2000)))
	f.Fuzz(func(t *testing.T, status int, body []byte) {
		var e struct {
			Error errorObject `json:"error"`
		}
		out := ErrorBody(status, body)
		if err := json.Unmarshal(out, &e); err != nil || e.Error.Message == "" || e.Error.Type == "" || e.Error.Code != strconv.Itoa(status) {
			t.Fatalf("bad error body %s: %v", out, err)
		}
	})
}
