package e2e

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/rcondron/Morpheus-Core-Node/internal/apikeys"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/provider"
)

// The test binary doubles as an MCP server for agent tests.
func TestMain(m *testing.M) {
	if os.Getenv("MORCORE_FAKE_MCP") == "1" {
		fakeMCP()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeMCP() {
	in := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]int `json:"arguments"`
			} `json:"params"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil || len(req.ID) == 0 {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "add", "description": "Add two numbers",
				"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"a": map[string]any{"type": "number"}, "b": map[string]any{"type": "number"}}}}}}
		case "tools/call":
			sum := req.Params.Arguments["a"] + req.Params.Arguments["b"]
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(sum)}}, "isError": false}
		}
		_ = out.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	}
}

// mediaBackend imitates OpenAI audio endpoints, Prodia v2, Hyperbolic and a
// Decisions API.
func mediaBackend(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/audio/transcriptions":
			if err := r.ParseMultipartForm(64 << 20); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			f, fh, err := r.FormFile("file")
			if err != nil || r.FormValue("model") != "whisper-backend" {
				http.Error(w, `{"error":{"message":"bad transcription request"}}`, 400)
				return
			}
			data, _ := io.ReadAll(f)
			sum := sha256.Sum256(data)
			text := fmt.Sprintf("heard %d bytes %s via %s lang=%s", len(data), hex.EncodeToString(sum[:4]), fh.Filename[strings.LastIndex(fh.Filename, "."):], r.FormValue("language"))
			switch {
			case r.FormValue("stream") == "true":
				w.Header().Set("Content-Type", "text/event-stream")
				for _, word := range strings.Fields(text) {
					fmt.Fprintf(w, "data: {\"type\":\"transcript.text.delta\",\"delta\":%q}\n\n", word+" ")
				}
				fmt.Fprintf(w, "data: {\"type\":\"transcript.text.done\",\"text\":%q}\n\n", text)
			case r.FormValue("response_format") == "text":
				w.Header().Set("Content-Type", "text/plain")
				io.WriteString(w, text)
			default:
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]string{"text": text})
			}
		case "/v1/audio/speech":
			var req struct{ Model, Input, Voice string }
			json.NewDecoder(r.Body).Decode(&req)
			if req.Model != "tts-backend" || req.Input == "" {
				http.Error(w, `{"error":{"message":"bad speech request"}}`, 400)
				return
			}
			w.Header().Set("Content-Type", "audio/mpeg")
			w.Write(speechBytes(req.Input))
		case "/prodia/v2/job":
			var req struct {
				Type   string         `json:"type"`
				Config map[string]any `json:"config"`
			}
			json.NewDecoder(r.Body).Decode(&req)
			if r.Header.Get("Authorization") != "Bearer prodia-key" || req.Type != "inference.flux.schnell.txt2img.v2" || req.Config["steps"] != float64(4) {
				http.Error(w, `{"error":"bad prodia request"}`, 400)
				return
			}
			w.Header().Set("Content-Type", "image/png")
			fmt.Fprintf(w, "PNG:%s", req.Config["prompt"])
		case "/hyperbolic/image":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			json.NewEncoder(w).Encode(map[string]any{"images": []any{map[string]string{"image": base64.StdEncoding.EncodeToString([]byte("IMG:" + req["prompt"].(string)))}}})
		case "/decisions":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			if req["model"] != "jev-1.13.0" || req["type"] != nil {
				http.Error(w, `{"detail":"secret case data echoed back"}`, 422)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"model": req["model"], "answers": map[string]any{"approve": true}, "usage": map[string]int{"input_tokens": 12, "output_tokens": 3}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func speechBytes(input string) []byte {
	return bytes.Repeat([]byte(input), 200<<10/len(input)+1)[:200<<10] // 200 KiB: several chunks
}

func mediaHarness(t *testing.T) (*harness, string) {
	t.Helper()
	media := mediaBackend(t)
	agent := &provider.Agent{Command: os.Args[0], Args: []string{"-test.run=^$"}, Env: map[string]string{"MORCORE_FAKE_MCP": "1"}}
	h := newHarness(t, 2, 0,
		offering{name: "whisper", model: &provider.Model{APIURL: media.URL + "/v1/audio/transcriptions", Backend: "whisper-backend"}},
		offering{name: "kokoro", model: &provider.Model{APIURL: media.URL + "/v1", Backend: "tts-backend"}},
		offering{name: "flux", model: &provider.Model{APIType: config.APIProdiaV2, APIURL: media.URL + "/prodia/v2/job", APIKey: "prodia-key",
			Backend: "inference.flux.schnell.txt2img.v2", Parameters: map[string]string{"steps": "4"}}},
		offering{name: "sdxl", model: &provider.Model{APIType: config.APIHyperbolicSD, APIURL: media.URL + "/hyperbolic/image", Backend: "SDXL1.0-base"}},
		offering{name: "jev", model: &provider.Model{APIType: config.APIDecisions, APIURL: media.URL + "/decisions", Backend: "jev-1.13.0"}},
		offering{name: "calculator", agent: agent},
	)
	return h, h.newKey(apikeys.CreateOptions{Name: "media"})
}

func (h *harness) multipart(path, key string, fields map[string]string, file []byte, filename string) (*http.Response, []byte) {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	fw, _ := mw.CreateFormFile("file", filename)
	fw.Write(file)
	mw.Close()
	req, _ := http.NewRequest("POST", h.gw.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func TestAudioTranscriptionUploadsInChunks(t *testing.T) {
	h, key := mediaHarness(t)
	audio := bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x44}, (5<<20)/8) // 2.5 MiB: three 1 MiB chunks
	sum := sha256.Sum256(audio)
	want := fmt.Sprintf("heard %d bytes %s via .mp3 lang=en", len(audio), hex.EncodeToString(sum[:4]))

	resp, body := h.multipart("/v1/audio/transcriptions", key, map[string]string{"model": "whisper", "language": "en"}, audio, "memo.mp3")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"text":"`+want+`"`) {
		t.Fatalf("json transcription: %d %s", resp.StatusCode, body)
	}
	resp, body = h.multipart("/v1/audio/transcriptions", key, map[string]string{"model": "whisper", "language": "en", "response_format": "text"}, audio, "memo.mp3")
	if resp.StatusCode != 200 || string(body) != want || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("text transcription: %d %q", resp.StatusCode, body)
	}
	resp, body = h.multipart("/v1/audio/transcriptions", key, map[string]string{"model": "whisper", "language": "en", "stream": "true"}, audio[:1000], "memo.wav")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"type":"transcript.text.delta"`) ||
		!strings.Contains(string(body), `"type":"transcript.text.done","text":"heard 1000 bytes`) || !strings.Contains(string(body), "via .wav") {
		t.Fatalf("streamed transcription: %d %s", resp.StatusCode, body)
	}
}

func TestAudioSpeechStreamsAudio(t *testing.T) {
	h, key := mediaHarness(t)
	resp, body := h.do("POST", "/v1/audio/speech", key, map[string]any{"model": "kokoro", "input": "hello marketplace", "voice": "af_sky"})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "audio/mpeg" {
		t.Fatalf("speech: %d %s", resp.StatusCode, body)
	}
	if !bytes.Equal(body, speechBytes("hello marketplace")) {
		t.Fatalf("speech audio differs: got %d bytes", len(body))
	}
}

func TestImageModels(t *testing.T) {
	h, key := mediaHarness(t)
	// OpenAI images API over a Prodia v2 model.
	resp, body := h.do("POST", "/v1/images/generations", key, map[string]any{"model": "flux", "prompt": "a lighthouse", "response_format": "b64_json"})
	var img struct {
		Data []struct {
			B64 string `json:"b64_json"`
			URL string `json:"url"`
		} `json:"data"`
	}
	if resp.StatusCode != 200 || json.Unmarshal(body, &img) != nil || len(img.Data) != 1 {
		t.Fatalf("images: %d %s", resp.StatusCode, body)
	}
	if raw, _ := base64.StdEncoding.DecodeString(img.Data[0].B64); string(raw) != "PNG:a lighthouse" {
		t.Fatalf("image content %q", raw)
	}
	resp, body = h.do("POST", "/v1/images/generations", key, map[string]any{"model": "flux", "prompt": "a lighthouse"})
	if resp.StatusCode != 200 || json.Unmarshal(body, &img) != nil || !strings.HasPrefix(img.Data[0].URL, "data:image/png;base64,") {
		t.Fatalf("image url: %d %s", resp.StatusCode, body)
	}
	// Image models also answer chat completions in the reference format.
	resp, body = h.do("POST", "/v1/chat/completions", key, map[string]any{"model": "sdxl", "messages": []any{map[string]any{"role": "user", "content": "a cat"}}})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"imageRawContent":"data:image/png;base64,`+base64.StdEncoding.EncodeToString([]byte("IMG:a cat"))) {
		t.Fatalf("chat to image model: %d %s", resp.StatusCode, body)
	}
	// Asking an image model for embeddings is the client's mistake: a 400, not a failover.
	resp, body = h.do("POST", "/v1/embeddings", key, map[string]any{"model": "sdxl", "input": "x"})
	if resp.StatusCode != 400 || !strings.Contains(string(body), "not supported") {
		t.Fatalf("unsupported request: %d %s", resp.StatusCode, body)
	}
}

func TestDecisions(t *testing.T) {
	h, key := mediaHarness(t)
	resp, body := h.do("POST", "/v1/decisions", key, map[string]any{"model": "jev", "state": "case 42", "questions": map[string]any{"approve": "Approve?"}})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"approve":true`) {
		t.Fatalf("decisions: %d %s", resp.StatusCode, body)
	}
	resp, body = h.do("POST", "/v1/decisions", key, map[string]any{"model": "jev", "state": "", "questions": map[string]any{"q": "?"}})
	if resp.StatusCode != 400 || !strings.Contains(string(body), "needs a state") {
		t.Fatalf("invalid decisions: %d %s", resp.StatusCode, body)
	}
}

func TestAgentTools(t *testing.T) {
	h, key := mediaHarness(t)
	resp, body := h.do("GET", "/v1/agents/calculator/tools", key, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"name":"add"`) {
		t.Fatalf("agent tools: %d %s", resp.StatusCode, body)
	}
	resp, body = h.do("POST", "/v1/agents/calculator/tools/add", key, map[string]any{"a": 2, "b": 40})
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"text":"42"`) {
		t.Fatalf("agent call: %d %s", resp.StatusCode, body)
	}
}
