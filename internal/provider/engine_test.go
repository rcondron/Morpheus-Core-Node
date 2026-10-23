package provider

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEndpointFor(t *testing.T) {
	cases := []struct{ url, path, want string }{
		{"https://api.openai.com/v1/chat/completions", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/chat/completions", "/embeddings", "https://api.openai.com/v1/embeddings"},
		{"https://api.venice.ai/api/v1/embeddings", "/embeddings", "https://api.venice.ai/api/v1/embeddings"},
		{"https://api.openai.com/v1/audio/speech", "/models", "https://api.openai.com/v1/models"},
		{"http://localhost:8000/v1", "/audio/transcriptions", "http://localhost:8000/v1/audio/transcriptions"},
		{"http://localhost:8080/completion", "/chat/completions", "http://localhost:8080/completion"}, // custom: as is
	}
	for _, c := range cases {
		if got := endpointFor(c.url, c.path); got != c.want {
			t.Errorf("endpointFor(%s, %s) = %s, want %s", c.url, c.path, got, c.want)
		}
	}
}

func TestParseRequestStripsServerFields(t *testing.T) {
	r, err := parseRequest([]byte(`{"type":"audio_transcription","FilePath":"/etc/passwd","file_path":"/x","Model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.kind != "audio_transcription" || len(r.body) != 1 {
		t.Fatalf("unexpected request %+v", r)
	}
	if _, err := parseRequest([]byte(`[1,2]`)); err == nil {
		t.Fatal("a non-object request was accepted")
	}
}

func TestTranscriptionForm(t *testing.T) {
	dir := t.TempDir()
	audio := filepath.Join(dir, "a.mp3")
	os.WriteFile(audio, []byte("ID3 audio"), 0o600)
	// The reference consumer's shape: Go field names plus extras.
	r, _ := parseRequest([]byte(`{"type":"audio_transcription","Model":"whisper","Language":"en","Format":"verbose_json",
		"response_format":"verbose_json","TimestampGranularities":["word","segment"],"Temperature":0,"Stream":false,"enable_diarization":true}`))
	r.file = audio
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := writeTranscriptionForm(mw, "whisper-1", r); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	_, params, _ := mime.ParseMediaType(mw.FormDataContentType())
	form, err := multipart.NewReader(&buf, params["boundary"]).ReadForm(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	v := form.Value
	if v["model"][0] != "whisper-1" || v["language"][0] != "en" || len(v["response_format"]) != 1 || v["response_format"][0] != "verbose_json" {
		t.Fatalf("form values %v", v)
	}
	if strings.Join(v["timestamp_granularities[]"], ",") != "word,segment" || v["enable_diarization"][0] != "true" {
		t.Fatalf("form values %v", v)
	}
	if _, ok := v["temperature"]; ok {
		t.Fatal("zero temperature should be omitted")
	}
	if _, ok := v["stream"]; ok {
		t.Fatal("stream=false should be omitted")
	}
	f, _ := form.File["file"][0].Open()
	data, _ := io.ReadAll(f)
	if string(data) != "ID3 audio" || form.File["file"][0].Filename != "a.mp3" {
		t.Fatalf("file part %q %s", data, form.File["file"][0].Filename)
	}
}

func TestFinishChatAddsProviderUsage(t *testing.T) {
	out := finishChat([]byte(`{"id":"x","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`), "chat.completion", 3, 1)
	var v map[string]json.RawMessage
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if string(v["object"]) != `"chat.completion"` || !strings.Contains(string(v["usage_from_provider"]), `"total_tokens":4`) || !strings.Contains(string(v["usage"]), `"prompt_tokens":3`) {
		t.Fatalf("finished payload %s", out)
	}
	if !isFinalChunk([]byte(`{"choices":[{"delta":{},"finish_reason":"stop"}]}`)) || isFinalChunk([]byte(`{"choices":[{"delta":{"content":"a"},"finish_reason":null}]}`)) {
		t.Fatal("final chunk detection")
	}
}
