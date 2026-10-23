package anthropic

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// translate runs Request and splits the Anthropic body into top-level fields.
func translate(t *testing.T, model, body string, defaultMaxTokens int) map[string]json.RawMessage {
	t.Helper()
	out, _, err := Request([]byte(body), model, defaultMaxTokens)
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("output is not a JSON object: %v\n%s", err, out)
	}
	return fields
}

// sameJSON compares two JSON documents, ignoring formatting and key order.
func sameJSON(t *testing.T, got []byte, want string) bool {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("invalid JSON %s: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		t.Fatalf("invalid expected JSON %s: %v", want, err)
	}
	return reflect.DeepEqual(g, w)
}

// checkFields compares top-level fields; an empty want means the field must be absent.
func checkFields(t *testing.T, got map[string]json.RawMessage, want map[string]string) {
	t.Helper()
	for k, w := range want {
		g, ok := got[k]
		switch {
		case w == "" && ok:
			t.Errorf("%s = %s, want it absent", k, g)
		case w != "" && !ok:
			t.Errorf("%s missing, want %s", k, w)
		case w != "" && !sameJSON(t, g, w):
			t.Errorf("%s =\n\t%s\nwant\n\t%s", k, g, w)
		}
	}
}

func TestRequestMessages(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]string
	}{
		{
			name: "system and developer messages become the system prompt",
			body: `{"messages":[
				{"role":"system","content":"You are terse."},
				{"role":"developer","content":[{"type":"text","text":"Answer in French."},{"type":"text","text":" "}]},
				{"role":"user","content":"Hi"},
				{"role":"system","content":"Never use emoji."}]}`,
			want: map[string]string{
				"system":   `"You are terse.\n\nAnswer in French.\n\nNever use emoji."`,
				"messages": `[{"role":"user","content":[{"type":"text","text":"Hi"}]}]`,
			},
		},
		{
			name: "no system prompt",
			body: `{"messages":[{"role":"user","content":"Hi"}]}`,
			want: map[string]string{"system": "", "messages": `[{"role":"user","content":[{"type":"text","text":"Hi"}]}]`},
		},
		{
			name: "consecutive same-role messages merge and blank text is dropped",
			body: `{"messages":[
				{"role":"user","content":"a"},
				{"role":"user","content":[{"type":"text","text":"b"},{"type":"text","text":""}]},
				{"role":"user","content":"   "},
				{"role":"assistant","content":"c"},
				{"role":"assistant","content":null},
				{"role":"assistant","content":[{"type":"text","text":"d"},{"type":"refusal","refusal":"e"}]},
				{"role":"user","content":"f"}]}`,
			want: map[string]string{"messages": `[
				{"role":"user","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]},
				{"role":"assistant","content":[{"type":"text","text":"c"},{"type":"text","text":"d"},{"type":"text","text":"e"}]},
				{"role":"user","content":[{"type":"text","text":"f"}]}]`},
		},
		{
			name: "a leading assistant greeting gets a placeholder user turn",
			body: `{"messages":[
				{"role":"system","content":"Be kind."},
				{"role":"assistant","content":"Hello! How can I help?"},
				{"role":"user","content":"Tell me a joke."}]}`,
			want: map[string]string{"messages": `[
				{"role":"user","content":[{"type":"text","text":"."}]},
				{"role":"assistant","content":[{"type":"text","text":"Hello! How can I help?"}]},
				{"role":"user","content":[{"type":"text","text":"Tell me a joke."}]}]`},
		},
		{
			name: "images from data and http URLs",
			body: `{"messages":[{"role":"user","content":[
				{"type":"text","text":"Compare these"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo=","detail":"high"}},
				{"type":"image_url","image_url":{"url":"data:image/jpg;base64,/9j/4AAQ"}},
				{"type":"image_url","image_url":{"url":"https://example.com/cat.webp"}},
				{"type":"image_url","image_url":"http://example.com/dog.gif"}]}]}`,
			want: map[string]string{"messages": `[{"role":"user","content":[
				{"type":"text","text":"Compare these"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw0KGgo="}},
				{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"/9j/4AAQ"}},
				{"type":"image","source":{"type":"url","url":"https://example.com/cat.webp"}},
				{"type":"image","source":{"type":"url","url":"http://example.com/dog.gif"}}]}]`},
		},
		{
			name: "assistant tool calls and tool results round trip",
			body: `{"messages":[
				{"role":"user","content":"Weather in Paris and Rome?"},
				{"role":"assistant","content":null,"tool_calls":[
					{"id":"call_1","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}},
					{"id":"call:2","type":"function","function":{"name":"get_weather","arguments":"{\"city\": \"Ro"}}]},
				{"role":"tool","tool_call_id":"call_1","content":"18°C"},
				{"role":"tool","tool_call_id":"call:2","content":[{"type":"text","text":"21°C"}]},
				{"role":"user","content":"Which is warmer?"}],
				"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
			want: map[string]string{"messages": `[
				{"role":"user","content":[{"type":"text","text":"Weather in Paris and Rome?"}]},
				{"role":"assistant","content":[
					{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"Paris"}},
					{"type":"tool_use","id":"call_2","name":"get_weather","input":{}}]},
				{"role":"user","content":[
					{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"18°C"}]},
					{"type":"tool_result","tool_use_id":"call_2","content":[{"type":"text","text":"21°C"}]},
					{"type":"text","text":"Which is warmer?"}]}]`},
		},
		{
			name: "assistant text with a call whose arguments are an object, and an empty result",
			body: `{"messages":[
				{"role":"user","content":"Oslo?"},
				{"role":"assistant","content":"Checking.","tool_calls":[{"id":"call_9","type":"function","function":{"name":"lookup","arguments":{"city":"Oslo"}}}]},
				{"role":"tool","tool_call_id":"call_9","content":""}],
				"tools":[{"type":"function","function":{"name":"lookup"}}]}`,
			want: map[string]string{"messages": `[
				{"role":"user","content":[{"type":"text","text":"Oslo?"}]},
				{"role":"assistant","content":[{"type":"text","text":"Checking."},{"type":"tool_use","id":"call_9","name":"lookup","input":{"city":"Oslo"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_9"}]}]`},
		},
		{
			name: "tool results move ahead of user text in the same turn",
			body: `{"messages":[
				{"role":"user","content":"Time?"},
				{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"now","arguments":""}}]},
				{"role":"user","content":"hurry"},
				{"role":"tool","tool_call_id":"c1","content":"noon"}],
				"tools":[{"type":"function","function":{"name":"now"}}]}`,
			want: map[string]string{"messages": `[
				{"role":"user","content":[{"type":"text","text":"Time?"}]},
				{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"now","input":{}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":[{"type":"text","text":"noon"}]},{"type":"text","text":"hurry"}]}]`},
		},
		{
			name: "legacy function messages are dropped",
			body: `{"messages":[
				{"role":"user","content":"q"},
				{"role":"assistant","content":"ok"},
				{"role":"function","name":"f","content":"x"},
				{"role":"user","content":"next"}]}`,
			want: map[string]string{"messages": `[
				{"role":"user","content":[{"type":"text","text":"q"}]},
				{"role":"assistant","content":[{"type":"text","text":"ok"}]},
				{"role":"user","content":[{"type":"text","text":"next"}]}]`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFields(t, translate(t, "claude-sonnet-4-5", c.body, 1000), c.want)
		})
	}
}

func TestRequestTools(t *testing.T) {
	const weather = `{"type":"function","function":{"name":"get_weather","description":"Current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}`
	withTools := func(extra string) string {
		return `{"messages":[{"role":"user","content":"Weather in Oslo?"}],"tools":[` + weather + `]` + extra + `}`
	}
	cases := []struct {
		name  string
		model string
		body  string
		want  map[string]string
	}{
		{
			name:  "definitions",
			model: "claude-sonnet-4-5",
			body: `{"messages":[{"role":"user","content":"hi"}],"tools":[` + weather + `,
				{"type":"function","function":{"name":"ping"}},
				{"type":"function","function":{"name":"echo","parameters":{"properties":{"s":{"type":"string"}}}}},
				{"type":"custom","custom":{"name":"grammar"}}]}`,
			want: map[string]string{
				"tools": `[
					{"name":"get_weather","description":"Current weather","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}},
					{"name":"ping","input_schema":{"type":"object"}},
					{"name":"echo","input_schema":{"type":"object","properties":{"s":{"type":"string"}}}}]`,
				"tool_choice": "",
			},
		},
		{name: "auto", model: "claude-sonnet-4-5", body: withTools(`,"tool_choice":"auto"`), want: map[string]string{"tool_choice": `{"type":"auto"}`}},
		{name: "required", model: "claude-sonnet-4-5", body: withTools(`,"tool_choice":"required"`), want: map[string]string{"tool_choice": `{"type":"any"}`}},
		{name: "none", model: "claude-sonnet-4-5", body: withTools(`,"tool_choice":"none"`), want: map[string]string{"tool_choice": `{"type":"none"}`}},
		{
			name:  "named function",
			model: "claude-sonnet-4-5",
			body:  withTools(`,"tool_choice":{"type":"function","function":{"name":"get_weather"}}`),
			want:  map[string]string{"tool_choice": `{"type":"tool","name":"get_weather"}`},
		},
		{
			name:  "allowed_tools falls back to auto",
			model: "claude-sonnet-4-5",
			body:  withTools(`,"tool_choice":{"type":"allowed_tools","allowed_tools":{"mode":"auto","tools":[]}}`),
			want:  map[string]string{"tool_choice": `{"type":"auto"}`},
		},
		{
			name:  "parallel tool calls off",
			model: "claude-sonnet-4-5",
			body:  withTools(`,"parallel_tool_calls":false`),
			want:  map[string]string{"tool_choice": `{"type":"auto","disable_parallel_tool_use":true}`},
		},
		{
			name:  "parallel tool calls off with required",
			model: "claude-sonnet-4-5",
			body:  withTools(`,"tool_choice":"required","parallel_tool_calls":false`),
			want:  map[string]string{"tool_choice": `{"type":"any","disable_parallel_tool_use":true}`},
		},
		{
			name:  "parallel tool calls off with none",
			model: "claude-sonnet-4-5",
			body:  withTools(`,"tool_choice":"none","parallel_tool_calls":false`),
			want:  map[string]string{"tool_choice": `{"type":"none"}`},
		},
		{
			name:  "parallel tool calls on",
			model: "claude-sonnet-4-5",
			body:  withTools(`,"parallel_tool_calls":true`),
			want:  map[string]string{"tool_choice": ""},
		},
		{
			name:  "tool_choice without tools",
			model: "claude-sonnet-4-5",
			body:  `{"messages":[{"role":"user","content":"hi"}],"tool_choice":"required","parallel_tool_calls":false}`,
			want:  map[string]string{"tools": "", "tool_choice": ""},
		},
		{
			name:  "forced tool use on a model that rejects it",
			model: "claude-opus-5-5",
			body:  withTools(`,"tool_choice":"required"`),
			want:  map[string]string{"tool_choice": `{"type":"auto"}`, "system": `"Use the provided tools to answer."`},
		},
		{
			name:  "named tool on a model that rejects forcing",
			model: "claude-sonnet-5-5",
			body: `{"messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Oslo?"}],"tools":[` + weather + `],
				"tool_choice":{"type":"function","function":{"name":"get_weather"}},"parallel_tool_calls":false}`,
			want: map[string]string{
				"tool_choice": `{"type":"auto","disable_parallel_tool_use":true}`,
				"system":      `"Be brief.\n\nUse the get_weather tool to answer."`,
			},
		},
		{name: "forced tool use where supported", model: "claude-haiku-5-5", body: withTools(`,"tool_choice":"required"`), want: map[string]string{"tool_choice": `{"type":"any"}`, "system": ""}},
		{name: "forced tool use on fable 5.1", model: "claude-fable-5-1", body: withTools(`,"tool_choice":"required"`), want: map[string]string{"tool_choice": `{"type":"auto"}`}},
		{name: "forced tool use on opus 5", model: "claude-opus-5", body: withTools(`,"tool_choice":"required"`), want: map[string]string{"tool_choice": `{"type":"any"}`}},
		{
			name:  "tool history without tool definitions",
			model: "claude-sonnet-4-5",
			body: `{"messages":[
				{"role":"user","content":"Summarize the weather."},
				{"role":"assistant","tool_calls":[{"id":"a","type":"function","function":{"name":"get_weather","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
				{"role":"tool","tool_call_id":"a","content":"sunny"},
				{"role":"tool","tool_call_id":"b","content":"rain"}],"tool_choice":"required"}`,
			want: map[string]string{
				"tools":       `[{"name":"get_weather","input_schema":{"type":"object"}}]`,
				"tool_choice": `{"type":"none"}`,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFields(t, translate(t, c.model, c.body, 1000), c.want)
		})
	}
}

func TestRequestLimitsAndMetadata(t *testing.T) {
	const msgs = `"messages":[{"role":"user","content":"hi"}]`
	cases := []struct {
		name string
		body string // request fields, without the enclosing braces
		def  int
		want map[string]string
	}{
		{name: "caller default", body: msgs, def: 1000, want: map[string]string{"max_tokens": "1000"}},
		{name: "package fallback", body: msgs, def: 0, want: map[string]string{"max_tokens": "8192"}},
		{name: "max_tokens", body: msgs + `,"max_tokens":300`, def: 1000, want: map[string]string{"max_tokens": "300"}},
		{name: "max_completion_tokens wins", body: msgs + `,"max_tokens":300,"max_completion_tokens":500`, def: 1000, want: map[string]string{"max_tokens": "500"}},
		{name: "zero is unset", body: msgs + `,"max_tokens":0`, def: 1000, want: map[string]string{"max_tokens": "1000"}},
		{name: "stop string", body: msgs + `,"stop":"END"`, def: 1000, want: map[string]string{"stop_sequences": `["END"]`}},
		{name: "stop array drops whitespace", body: msgs + `,"stop":["\n","###"," ","Human:"]`, def: 1000, want: map[string]string{"stop_sequences": `["###","Human:"]`}},
		{name: "whitespace-only stop", body: msgs + `,"stop":["\n"]`, def: 1000, want: map[string]string{"stop_sequences": ""}},
		{name: "null stop", body: msgs + `,"stop":null`, def: 1000, want: map[string]string{"stop_sequences": ""}},
		{name: "user", body: msgs + `,"user":"u-42"`, def: 1000, want: map[string]string{"metadata": `{"user_id":"u-42"}`}},
		{name: "safety_identifier wins", body: msgs + `,"user":"u-42","safety_identifier":"hash-9"`, def: 1000, want: map[string]string{"metadata": `{"user_id":"hash-9"}`}},
		{name: "overlong user", body: msgs + `,"user":"` + strings.Repeat("x", 257) + `"`, def: 1000, want: map[string]string{"metadata": ""}},
		{name: "streaming", body: msgs + `,"stream":true,"stream_options":{"include_usage":true}`, def: 1000, want: map[string]string{"stream": "true"}},
		{name: "not streaming", body: msgs + `,"stream":false`, def: 1000, want: map[string]string{"stream": ""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFields(t, translate(t, "claude-sonnet-4-5", "{"+c.body+"}", c.def), c.want)
		})
	}
}

func TestRequestSampling(t *testing.T) {
	cases := []struct {
		model string
		body  string
		want  map[string]string
	}{
		{"claude-3-5-sonnet-20241022", `"temperature":1.6,"top_p":0.9,"top_k":40`, map[string]string{"temperature": "1", "top_p": "0.9", "top_k": "40"}},
		{"claude-sonnet-4-5-20250929", `"temperature":0.2,"top_p":0.9,"top_k":40`, map[string]string{"temperature": "0.2", "top_p": "", "top_k": "40"}},
		{"claude-opus-4-6", `"top_p":0.8`, map[string]string{"temperature": "", "top_p": "0.8"}},
		{"claude-haiku-4-5", `"temperature":-1,"top_k":0`, map[string]string{"temperature": "0", "top_k": ""}},
		{"claude-opus-4-7", `"temperature":0.2,"top_p":0.9,"top_k":40`, map[string]string{"temperature": "", "top_p": "", "top_k": ""}},
		{"claude-opus-5-5", `"temperature":1,"top_p":1`, map[string]string{"temperature": "", "top_p": ""}},
		{"claude-sonnet-5", `"temperature":0.7`, map[string]string{"temperature": ""}},
		{"my-finetune", `"temperature":0.7`, map[string]string{"temperature": ""}},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			body := `{"messages":[{"role":"user","content":"hi"}],` + c.body + `}`
			checkFields(t, translate(t, c.model, body, 1000), c.want)
		})
	}
}

func TestRequestReasoning(t *testing.T) {
	const pendingTools = `"messages":[
		{"role":"user","content":"Weather?"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"sunny"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}]`
	const finishedTools = `"messages":[
		{"role":"user","content":"Weather?"},
		{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c1","content":"sunny"},
		{"role":"assistant","content":"It is sunny."},
		{"role":"user","content":"And tomorrow?"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}]`
	const hi = `"messages":[{"role":"user","content":"hi"}]`
	cases := []struct {
		name  string
		model string
		body  string // request fields, without the enclosing braces
		want  map[string]string
	}{
		{
			name: "adaptive thinking from 4.6 drops temperature", model: "claude-sonnet-4-6",
			body: hi + `,"reasoning_effort":"medium","temperature":0.3`,
			want: map[string]string{"thinking": `{"type":"adaptive"}`, "output_config": `{"effort":"medium"}`, "temperature": ""},
		},
		{
			name: "high on opus 5.5", model: "claude-opus-5-5", body: hi + `,"reasoning_effort":"high"`,
			want: map[string]string{"thinking": `{"type":"adaptive"}`, "output_config": `{"effort":"high"}`},
		},
		{
			name: "xhigh before 4.7", model: "claude-sonnet-4-6", body: hi + `,"reasoning_effort":"xhigh"`,
			want: map[string]string{"output_config": `{"effort":"high"}`},
		},
		{
			name: "xhigh from 4.7", model: "claude-opus-4-7", body: hi + `,"reasoning_effort":"xhigh"`,
			want: map[string]string{"output_config": `{"effort":"xhigh"}`},
		},
		{
			name: "minimal only lowers effort", model: "claude-opus-5-5", body: hi + `,"reasoning_effort":"minimal"`,
			want: map[string]string{"thinking": "", "output_config": `{"effort":"low"}`},
		},
		{
			name: "none keeps sampling on 4.6", model: "claude-sonnet-4-6", body: hi + `,"reasoning_effort":"none","temperature":0.3`,
			want: map[string]string{"thinking": "", "output_config": `{"effort":"low"}`, "temperature": "0.3"},
		},
		{
			name: "unknown effort", model: "claude-opus-5-5", body: hi + `,"reasoning_effort":"turbo"`,
			want: map[string]string{"thinking": "", "output_config": ""},
		},
		{
			name: "unrecognized model gets adaptive thinking", model: "my-finetune", body: hi + `,"reasoning_effort":"low"`,
			want: map[string]string{"thinking": `{"type":"adaptive"}`, "output_config": `{"effort":"low"}`},
		},
		{
			name: "budget before 4.6", model: "claude-sonnet-4-5", body: hi + `,"reasoning_effort":"medium","max_tokens":16000,"temperature":0.5`,
			want: map[string]string{"thinking": `{"type":"enabled","budget_tokens":4096}`, "output_config": "", "temperature": ""},
		},
		{
			name: "budget capped at half of max_tokens", model: "claude-3-7-sonnet-latest", body: hi + `,"reasoning_effort":"high","max_tokens":10000`,
			want: map[string]string{"thinking": `{"type":"enabled","budget_tokens":5000}`},
		},
		{
			name: "budget below the minimum", model: "claude-opus-4-1", body: hi + `,"reasoning_effort":"low","max_tokens":1500,"temperature":0.5`,
			want: map[string]string{"thinking": "", "temperature": "0.5"},
		},
		{
			name: "pending tool round trip blocks budget thinking", model: "claude-sonnet-4-5",
			body: pendingTools + `,"reasoning_effort":"high","max_tokens":20000`, want: map[string]string{"thinking": ""},
		},
		{
			name: "finished tool round trip allows budget thinking", model: "claude-sonnet-4-5",
			body: finishedTools + `,"reasoning_effort":"high","max_tokens":20000`,
			want: map[string]string{"thinking": `{"type":"enabled","budget_tokens":10000}`},
		},
		{
			name: "pending tool round trip with adaptive thinking", model: "claude-opus-4-6",
			body: pendingTools + `,"reasoning_effort":"high"`, want: map[string]string{"thinking": `{"type":"adaptive"}`},
		},
		{
			name: "assistant prefill blocks budget thinking", model: "claude-sonnet-4-5",
			body: `"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"Sure:"}],"reasoning_effort":"low","max_tokens":20000`,
			want: map[string]string{"thinking": ""},
		},
		{
			name: "no thinking before 3.7", model: "claude-3-5-haiku-latest", body: hi + `,"reasoning_effort":"high"`,
			want: map[string]string{"thinking": "", "output_config": ""},
		},
		{
			name: "forced tool use blocks thinking", model: "claude-sonnet-4-6",
			body: finishedTools + `,"reasoning_effort":"high","tool_choice":"required"`,
			want: map[string]string{"thinking": "", "output_config": "", "tool_choice": `{"type":"any"}`},
		},
		{
			name: "downgraded forced tool use keeps thinking", model: "claude-opus-5-5",
			body: finishedTools + `,"reasoning_effort":"high","tool_choice":"required"`,
			want: map[string]string{"thinking": `{"type":"adaptive"}`, "tool_choice": `{"type":"auto"}`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkFields(t, translate(t, c.model, "{"+c.body+"}", 1000), c.want)
		})
	}
}

func TestRequestDropsUnsupportedFields(t *testing.T) {
	body := `{"model":"marketplace-alias","messages":[{"role":"user","content":"hi","name":"bob"}],"stream":true,
		"n":3,"logprobs":true,"top_logprobs":2,"presence_penalty":0.5,"frequency_penalty":0.5,"logit_bias":{"1":2},
		"response_format":{"type":"json_object"},"seed":7,"stream_options":{"include_usage":true},"store":true,
		"metadata":{"k":"v"},"service_tier":"auto","modalities":["text"],"verbosity":"low","prompt_cache_key":"x",
		"functions":[{"name":"f","parameters":{}}],"function_call":"auto","type":"chat_completion"}`
	out, stream, err := Request([]byte(body), "claude-opus-5-5", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !stream {
		t.Error("stream not reported")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"max_tokens", "messages", "model", "stream"}; !slices.Equal(keys, want) {
		t.Fatalf("fields %v, want %v\n%s", keys, want, out)
	}
	if string(fields["model"]) != `"claude-opus-5-5"` {
		t.Errorf("model = %s", fields["model"])
	}
}

func TestRequestKeepsMarkupUnescaped(t *testing.T) {
	out, _, err := Request([]byte(`{"messages":[{"role":"user","content":"if a < b && c > d {}"}]}`), "claude-opus-5-5", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"if a < b && c > d {}"`) {
		t.Fatalf("text was escaped: %s", out)
	}
}

func TestRequestErrors(t *testing.T) {
	cases := map[string]string{
		"malformed JSON":         `{"messages":[`,
		"not an object":          `[]`,
		"null":                   `null`,
		"no messages":            `{}`,
		"only a system message":  `{"messages":[{"role":"system","content":"hi"}]}`,
		"unknown role":           `{"messages":[{"role":"model","content":"hi"}]}`,
		"content of wrong type":  `{"messages":[{"role":"user","content":42}]}`,
		"unsupported part":       `{"messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{"data":"x","format":"wav"}}]}]}`,
		"image in system prompt": `{"messages":[{"role":"system","content":[{"type":"image_url","image_url":{"url":"https://x/y.png"}}]},{"role":"user","content":"hi"}]}`,
		"ftp image":              `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"ftp://x/y.png"}}]}]}`,
		"non-base64 data URL":    `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/svg+xml;utf8,<svg/>"}}]}]}`,
		"stop of wrong type":     `{"messages":[{"role":"user","content":"hi"}],"stop":5}`,
		"unknown tool_choice":    `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}],"tool_choice":"sometimes"}`,
		"parameters not object":  `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":[1]}}]}`,
		"temperature as string":  `{"messages":[{"role":"user","content":"hi"}],"temperature":"hot"}`,
		"messages not an array":  `{"messages":{"role":"user"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if out, _, err := Request([]byte(body), "claude-opus-5-5", 1000); err == nil {
				t.Fatalf("accepted, produced %s", out)
			}
		})
	}
	if _, _, err := Request([]byte(`{"messages":[{"role":"user","content":"hi"}]}`), "", 1000); err == nil {
		t.Error("empty model accepted")
	}
}

func TestParseModel(t *testing.T) {
	cases := []struct {
		id     string
		family string
		gen    int
	}{
		{"claude-3-haiku-20240307", "haiku", 300},
		{"claude-3-5-sonnet-20241022", "sonnet", 305},
		{"claude-3-5-sonnet-v2@20241022", "sonnet", 305},
		{"claude-3-7-sonnet-latest", "sonnet", 307},
		{"claude-sonnet-4-20250514", "sonnet", 400},
		{"claude-opus-4-1-20250805", "opus", 401},
		{"us.anthropic.claude-opus-4-1-20250805-v1:0", "opus", 401},
		{"claude-sonnet-4-5-20250929", "sonnet", 405},
		{"claude-haiku-4-5", "haiku", 405},
		{"claude-opus-4-6", "opus", 406},
		{"claude-opus-4-7", "opus", 407},
		{"claude-opus-5", "opus", 500},
		{"claude-fable-5-1", "fable", 501},
		{"anthropic.claude-opus-5-5", "opus", 505},
		{"Claude-Sonnet-5-5", "sonnet", 505},
		{"claude-2.1", "", 201},
		{"claude-instant-1.2", "instant", 102},
		{"claude-next", "next", latest},
		{"my-finetune", "", latest},
		{"", "", latest},
	}
	for _, c := range cases {
		if family, gen := parseModel(c.id); family != c.family || gen != c.gen {
			t.Errorf("parseModel(%q) = %q, %d; want %q, %d", c.id, family, gen, c.family, c.gen)
		}
	}
}
