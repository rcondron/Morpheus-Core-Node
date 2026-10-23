package anthropic

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// anthropicReply builds a Messages response with the given content blocks and stop reason.
func anthropicReply(content, stop string) string {
	return `{"id":"msg_01XFDUDYJgAACzvnptvVoYEL","type":"message","role":"assistant","model":"claude-opus-5-5",
		"content":` + content + `,"stop_reason":` + stop + `,"stop_sequence":null,
		"usage":{"input_tokens":12,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":34}}`
}

func TestResponse(t *testing.T) {
	cases := []struct {
		name    string
		content string
		stop    string
		message string
		finish  string
	}{
		{
			name:    "text",
			content: `[{"type":"text","text":"Hello"},{"type":"text","text":" world"}]`,
			stop:    `"end_turn"`,
			message: `{"role":"assistant","content":"Hello world"}`,
			finish:  "stop",
		},
		{
			name:    "tool use only",
			content: `[{"type":"tool_use","id":"toolu_01A09q90qw90lq917835lq9","name":"get_weather","input":{"city":"Paris","unit":"celsius"}}]`,
			stop:    `"tool_use"`,
			message: `{"role":"assistant","content":null,"tool_calls":[{"id":"toolu_01A09q90qw90lq917835lq9","type":"function",
				"function":{"name":"get_weather","arguments":"{\"city\":\"Paris\",\"unit\":\"celsius\"}"}}]}`,
			finish: "tool_calls",
		},
		{
			name: "text and two tool calls",
			content: `[{"type":"text","text":"Checking both."},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"Paris"}},
				{"type":"tool_use","id":"toolu_2","name":"get_time","input":{}}]`,
			stop: `"tool_use"`,
			message: `{"role":"assistant","content":"Checking both.","tool_calls":[
				{"id":"toolu_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
				{"id":"toolu_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]}`,
			finish: "tool_calls",
		},
		{
			name: "thinking",
			content: `[{"type":"thinking","thinking":"Two plus two is four.","signature":"EqQBCgIYAh"},
				{"type":"redacted_thinking","data":"EmwKAhgBEgy3va3pzix"},
				{"type":"text","text":"4"}]`,
			stop:    `"end_turn"`,
			message: `{"role":"assistant","content":"4","reasoning_content":"Two plus two is four."}`,
			finish:  "stop",
		},
		{
			name:    "omitted thinking and no text",
			content: `[{"type":"thinking","thinking":"","signature":"sig"}]`,
			stop:    `"max_tokens"`,
			message: `{"role":"assistant","content":""}`,
			finish:  "length",
		},
		{
			name: "server tool blocks are not client tool calls",
			content: `[{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"news"}},
				{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://example.com","title":"x"}]},
				{"type":"text","text":"Here is the news.","citations":[{"type":"web_search_result_location","url":"https://example.com"}]}]`,
			stop:    `"end_turn"`,
			message: `{"role":"assistant","content":"Here is the news."}`,
			finish:  "stop",
		},
		{
			name:    "markup stays unescaped",
			content: `[{"type":"text","text":"<b>a & b</b>"}]`,
			stop:    `"end_turn"`,
			message: `{"role":"assistant","content":"<b>a & b</b>"}`,
			finish:  "stop",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := time.Now().Unix()
			out, err := Response([]byte(anthropicReply(c.content, c.stop)))
			if err != nil {
				t.Fatal(err)
			}
			var got struct {
				ID      string `json:"id"`
				Object  string `json:"object"`
				Created int64  `json:"created"`
				Model   string `json:"model"`
				Choices []struct {
					Index        int             `json:"index"`
					Message      json.RawMessage `json:"message"`
					FinishReason string          `json:"finish_reason"`
				} `json:"choices"`
				Usage json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if got.ID != "chatcmpl-msg_01XFDUDYJgAACzvnptvVoYEL" || got.Object != "chat.completion" || got.Model != "claude-opus-5-5" {
				t.Errorf("id/object/model = %q %q %q", got.ID, got.Object, got.Model)
			}
			if got.Created < before || got.Created > time.Now().Unix() {
				t.Errorf("created = %d", got.Created)
			}
			if len(got.Choices) != 1 || got.Choices[0].Index != 0 {
				t.Fatalf("choices: %s", out)
			}
			if !sameJSON(t, got.Choices[0].Message, c.message) {
				t.Errorf("message =\n\t%s\nwant\n\t%s", got.Choices[0].Message, c.message)
			}
			if got.Choices[0].FinishReason != c.finish {
				t.Errorf("finish_reason = %q, want %q", got.Choices[0].FinishReason, c.finish)
			}
			if !sameJSON(t, got.Usage, `{"prompt_tokens":12,"completion_tokens":34,"total_tokens":46}`) {
				t.Errorf("usage = %s", got.Usage)
			}
			if strings.Contains(string(out), "\\"+"u003c") {
				t.Errorf("HTML-escaped output: %s", out)
			}
		})
	}
}

func TestResponseStopReasons(t *testing.T) {
	cases := map[string]string{
		`"end_turn"`:                      "stop",
		`"stop_sequence"`:                 "stop",
		`"pause_turn"`:                    "stop",
		`"max_tokens"`:                    "length",
		`"model_context_window_exceeded"`: "length",
		`"tool_use"`:                      "tool_calls",
		`"refusal"`:                       "content_filter",
		`null`:                            "stop",
		`"a_future_reason"`:               "stop",
	}
	for stop, want := range cases {
		out, err := Response([]byte(anthropicReply(`[{"type":"text","text":"x"}]`, stop)))
		if err != nil {
			t.Fatalf("%s: %v", stop, err)
		}
		var got struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(out, &got); err != nil || len(got.Choices) != 1 {
			t.Fatalf("%s: %s", stop, out)
		}
		if got.Choices[0].FinishReason != want {
			t.Errorf("stop_reason %s → %q, want %q", stop, got.Choices[0].FinishReason, want)
		}
	}
}

func TestResponseUsageCountsCachedTokens(t *testing.T) {
	body := `{"id":"msg_1","type":"message","model":"claude-sonnet-4-5","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"cache_creation_input_tokens":200,"cache_read_input_tokens":3000,"output_tokens":5}}`
	out, err := Response([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Usage json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(out, &got)
	want := `{"prompt_tokens":3210,"completion_tokens":5,"total_tokens":3215,"prompt_tokens_details":{"cached_tokens":3000}}`
	if !sameJSON(t, got.Usage, want) {
		t.Errorf("usage = %s, want %s", got.Usage, want)
	}
}

func TestResponseErrors(t *testing.T) {
	_, err := Response([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"},"request_id":"req_1"}`))
	var ae *Error
	if !errors.As(err, &ae) || ae.Type != "overloaded_error" || ae.Message != "Overloaded" || ae.Status() != 529 {
		t.Errorf("error body: got %v", err)
	}
	for name, body := range map[string]string{
		"malformed":          `{"type":"message","content":[`,
		"empty":              ``,
		"not a message":      `{"object":"chat.completion","choices":[]}`,
		"array":              `[1,2]`,
		"bad text block":     `{"type":"message","content":[{"type":"text","text":5}]}`,
		"bad tool_use":       `{"type":"message","content":[{"type":"tool_use","id":"t","name":7,"input":{}}]}`,
		"content of strings": `{"type":"message","content":"hello"}`,
	} {
		if out, err := Response([]byte(body)); err == nil {
			t.Errorf("%s: accepted, produced %s", name, out)
		}
	}
}

func TestErrorBody(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			name:   "rate limit",
			status: 429,
			body:   `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your per-minute rate limit"},"request_id":"req_011CSHoEeqs5C35K2UUqR7Fy"}`,
			want:   `{"error":{"message":"Number of request tokens has exceeded your per-minute rate limit","type":"rate_limit_error","param":null,"code":"429"}}`,
		},
		{
			name:   "overloaded",
			status: 529,
			body:   `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			want:   `{"error":{"message":"Overloaded","type":"overloaded_error","param":null,"code":"529"}}`,
		},
		{
			name:   "invalid request",
			status: 400,
			body:   `{"type":"error","error":{"type":"invalid_request_error","message":"messages: roles must alternate"}}`,
			want:   `{"error":{"message":"messages: roles must alternate","type":"invalid_request_error","param":null,"code":"400"}}`,
		},
		{
			name:   "proxy page",
			status: 502,
			body:   "<html><body>Bad gateway</body></html>\n",
			want:   `{"error":{"message":"<html><body>Bad gateway</body></html>","type":"api_error","param":null,"code":"502"}}`,
		},
		{
			name:   "empty body",
			status: 503,
			body:   ``,
			want:   `{"error":{"message":"Service Unavailable","type":"api_error","param":null,"code":"503"}}`,
		},
		{
			name:   "JSON without a message",
			status: 401,
			body:   `{}`,
			want:   `{"error":{"message":"Unauthorized","type":"authentication_error","param":null,"code":"401"}}`,
		},
		{
			name:   "already OpenAI-shaped",
			status: 404,
			body:   `{"error":{"message":"model: claude-x not found","type":"not_found_error","code":"model_not_found"}}`,
			want:   `{"error":{"message":"model: claude-x not found","type":"not_found_error","param":null,"code":"404"}}`,
		},
		{
			name:   "unknown status",
			status: 418,
			body:   `oops`,
			want:   `{"error":{"message":"oops","type":"invalid_request_error","param":null,"code":"418"}}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ErrorBody(c.status, []byte(c.body)); !sameJSON(t, got, c.want) {
				t.Errorf("got  %s\nwant %s", got, c.want)
			}
		})
	}
	long := ErrorBody(500, []byte(strings.Repeat("é", 2000)))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(long, &e); err != nil || len(e.Error.Message) > maxErrorText+len("…") || !strings.HasSuffix(e.Error.Message, "…") {
		t.Errorf("long body not truncated cleanly: %v %q", err, e.Error.Message)
	}
	if strings.ContainsRune(e.Error.Message, utf8.RuneError) {
		t.Error("truncation split a character")
	}
}

func TestErrorStatus(t *testing.T) {
	cases := map[string]int{
		"invalid_request_error": 400,
		"authentication_error":  401,
		"billing_error":         402,
		"permission_error":      403,
		"not_found_error":       404,
		"request_too_large":     413,
		"rate_limit_error":      429,
		"api_error":             500,
		"timeout_error":         504,
		"overloaded_error":      529,
		"brand_new_error":       500,
	}
	for typ, want := range cases {
		e := &Error{Type: typ, Message: "m"}
		if got := e.Status(); got != want {
			t.Errorf("%s: status %d, want %d", typ, got, want)
		}
		if !strings.Contains(e.Error(), typ) {
			t.Errorf("%s: message %q", typ, e.Error())
		}
	}
}
