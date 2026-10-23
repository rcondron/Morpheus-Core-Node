package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/consumer"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// maxUpload bounds audio uploads (the provider-side limit is 256 MiB).
const maxUpload = 256<<20 + 1<<20

// failure writes err as the HTTP response when nothing has been sent yet.
func failure(w http.ResponseWriter, sess *consumer.Session, err error) int {
	if sess != nil {
		w.Header().Set("X-Morpheus-Session", sess.ID.Hex())
		w.Header().Set("X-Morpheus-Provider", sess.Provider.Hex())
	}
	var ee *morrpc.EngineError
	if errors.As(err, &ee) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ee.HTTPStatus())
		_, _ = w.Write(engineErrorBody(ee))
		return ee.HTTPStatus()
	}
	var pe *consumer.ProviderError
	if errors.As(err, &pe) {
		writeError(w, http.StatusBadGateway, "api_error", "provider_error", err.Error())
		return http.StatusBadGateway
	}
	writeError(w, http.StatusBadGateway, "api_error", "upstream_error", err.Error())
	return http.StatusBadGateway
}

// jsonOutput relays the provider's last JSON payload as the response body.
type jsonOutput struct {
	w    http.ResponseWriter
	sess *consumer.Session
	body []byte
	wrap func([]byte) ([]byte, error) // optional transformation
}

func (o *jsonOutput) chunk(payload []byte) error {
	o.body = append([]byte(nil), payload...)
	return nil
}

func (o *jsonOutput) finish(err error) int {
	if err == nil && o.wrap != nil && o.body != nil {
		o.body, err = o.wrap(o.body)
	}
	if err == nil && o.body == nil {
		err = errors.New("provider returned no result")
	}
	if err != nil {
		return failure(o.w, o.sess, err)
	}
	o.w.Header().Set("X-Morpheus-Session", o.sess.ID.Hex())
	o.w.Header().Set("X-Morpheus-Provider", o.sess.Provider.Hex())
	o.w.Header().Set("Content-Type", "application/json")
	o.w.WriteHeader(http.StatusOK)
	_, _ = o.w.Write(o.body)
	return http.StatusOK
}

// ---------------------------------------------------------------------------
// POST /v1/audio/transcriptions (multipart, OpenAI-compatible)
// ---------------------------------------------------------------------------

func (s *Server) transcriptions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_form", "expected multipart/form-data with a file: "+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["file"]
	if len(files) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "file_required", "\"file\" is required")
		return
	}
	fh := files[0]
	form := r.MultipartForm.Value
	get := func(k string) string {
		if v := form[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	format := get("response_format")
	stream, _ := strconv.ParseBool(get("stream"))
	req := map[string]any{"type": morrpc.TypeTranscription, "Model": get("model")}
	// Field names follow the reference consumer (Go field names) so every
	// provider maps them; response_format is repeated for OpenAI backends.
	if v := get("prompt"); v != "" {
		req["Prompt"] = v
	}
	if v := get("language"); v != "" {
		req["Language"] = v
	}
	if v, err := strconv.ParseFloat(get("temperature"), 64); err == nil && v != 0 {
		req["Temperature"] = v
	}
	if format != "" {
		req["Format"], req["response_format"] = format, format
	}
	if g := append(form["timestamp_granularities[]"], form["timestamp_granularities"]...); len(g) > 0 {
		req["TimestampGranularities"] = g
	}
	if stream {
		req["Stream"] = true
	}
	known := map[string]bool{"model": true, "prompt": true, "language": true, "temperature": true, "response_format": true,
		"stream": true, "timestamp_granularities[]": true, "timestamp_granularities": true}
	for k, v := range form {
		if !known[k] && len(v) > 0 {
			req[k] = v[0]
		}
	}
	body, _ := json.Marshal(req)
	contentType := fh.Header.Get("Content-Type")
	if contentType == "" || contentType == "application/octet-stream" {
		contentType = audioContentType(fh.Filename)
	}

	s.route(w, r, &call{
		kind: "transcription", modelName: get("model"), stream: stream,
		do: func(ctx context.Context, l *consumer.Lease, onChunk func([]byte) error) (*consumer.Result, error) {
			f, err := fh.Open()
			if err != nil {
				return nil, err
			}
			defer f.Close()
			return s.d.Sessions.Upload(ctx, l, f, fh.Size, contentType, body, onChunk)
		},
		output: func(sess *consumer.Session) output {
			return &transcriptOutput{w: w, sess: sess, stream: stream, format: format}
		},
	})
}

func audioContentType(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".wav", ".wave":
		return "audio/wav"
	case ".ogg":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".m4a":
		return "audio/mp4"
	case ".webm":
		return "audio/webm"
	case ".opus":
		return "audio/opus"
	case ".aac":
		return "audio/aac"
	case ".mp4":
		return "video/mp4"
	}
	return "audio/mpeg"
}

// transcriptOutput renders transcripts: JSON objects as JSON, plain strings
// as text (or {"text"} for JSON formats), deltas as SSE when streaming.
type transcriptOutput struct {
	w       http.ResponseWriter
	sess    *consumer.Session
	stream  bool
	format  string
	started bool
	body    []byte
	text    strings.Builder
}

func (o *transcriptOutput) chunk(payload []byte) error {
	var delta struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
		Text  string `json:"text"`
	}
	if json.Unmarshal(payload, &delta) == nil && strings.HasPrefix(delta.Type, "transcript.text.") {
		if !o.stream {
			if delta.Type == "transcript.text.done" {
				o.text.Reset()
				o.text.WriteString(delta.Text)
			} else {
				o.text.WriteString(delta.Delta)
			}
			return nil
		}
		if !o.started {
			o.started = true
			startSSE(o.w, o.sess)
		}
		// OpenAI's event shape, "type" first.
		ev := struct {
			Type  string `json:"type"`
			Delta string `json:"delta,omitempty"`
			Text  string `json:"text,omitempty"`
		}{Type: delta.Type, Delta: delta.Delta}
		if delta.Type == "transcript.text.done" {
			ev.Delta, ev.Text = "", delta.Text
		}
		b, _ := json.Marshal(ev)
		return writeSSE(o.w, b)
	}
	o.body = append([]byte(nil), payload...)
	return nil
}

func (o *transcriptOutput) finish(err error) int {
	if o.started {
		if err != nil {
			msg, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]any{"message": err.Error()}})
			_ = writeSSE(o.w, msg)
		}
		return http.StatusOK
	}
	if err != nil {
		return failure(o.w, o.sess, err)
	}
	o.w.Header().Set("X-Morpheus-Session", o.sess.ID.Hex())
	o.w.Header().Set("X-Morpheus-Provider", o.sess.Provider.Hex())
	jsonFormat := o.format == "" || o.format == "json" || o.format == "verbose_json"
	var text string
	switch {
	case o.body != nil && o.body[0] == '{':
		o.w.Header().Set("Content-Type", "application/json")
		o.w.WriteHeader(http.StatusOK)
		_, _ = o.w.Write(o.body)
		return http.StatusOK
	case o.body != nil:
		if json.Unmarshal(o.body, &text) != nil {
			text = string(o.body)
		}
	default:
		text = o.text.String()
	}
	if jsonFormat {
		writeJSON(o.w, http.StatusOK, map[string]string{"text": text})
		return http.StatusOK
	}
	o.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	o.w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(o.w, text)
	return http.StatusOK
}

func startSSE(w http.ResponseWriter, sess *consumer.Session) {
	h := w.Header()
	h.Set("X-Morpheus-Session", sess.ID.Hex())
	h.Set("X-Morpheus-Provider", sess.Provider.Hex())
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

func writeSSE(w http.ResponseWriter, data []byte) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// ---------------------------------------------------------------------------
// POST /v1/audio/speech
// ---------------------------------------------------------------------------

var speechTypes = map[string]string{
	"mp3": "audio/mpeg", "opus": "audio/ogg", "aac": "audio/aac", "flac": "audio/flac",
	"wav": "audio/wav", "pcm": "audio/pcm",
}

func (s *Server) speech(w http.ResponseWriter, r *http.Request) {
	req, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	if _, ok := req["input"]; !ok {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "input_required", "\"input\" is required")
		return
	}
	var format string
	_ = json.Unmarshal(req["response_format"], &format)
	ct := speechTypes[strings.ToLower(format)]
	if ct == "" {
		ct = "audio/mpeg"
	}
	req["type"] = json.RawMessage(`"` + morrpc.TypeSpeech + `"`)
	body, _ := json.Marshal(req)
	s.route(w, r, &call{
		kind: "speech", modelName: modelOf(req), stream: true, body: body,
		output: func(sess *consumer.Session) output { return &speechOutput{w: w, sess: sess, contentType: ct} },
	})
}

// speechOutput streams decoded audio bytes to the client as they arrive.
type speechOutput struct {
	w           http.ResponseWriter
	sess        *consumer.Session
	contentType string
	started     bool
}

func (o *speechOutput) chunk(payload []byte) error {
	var data []byte // base64 JSON string; null ends the stream
	if err := json.Unmarshal(payload, &data); err != nil {
		return fmt.Errorf("unexpected speech payload: %w", err)
	}
	if len(data) == 0 {
		return nil
	}
	if !o.started {
		o.started = true
		h := o.w.Header()
		h.Set("X-Morpheus-Session", o.sess.ID.Hex())
		h.Set("X-Morpheus-Provider", o.sess.Provider.Hex())
		h.Set("Content-Type", o.contentType)
		o.w.WriteHeader(http.StatusOK)
	}
	if _, err := o.w.Write(data); err != nil {
		return err
	}
	if f, ok := o.w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func (o *speechOutput) finish(err error) int {
	if o.started {
		return http.StatusOK // a truncated stream is all we can signal now
	}
	if err == nil {
		err = errors.New("provider returned no audio")
	}
	return failure(o.w, o.sess, err)
}

// ---------------------------------------------------------------------------
// POST /v1/images/generations
// ---------------------------------------------------------------------------

// imageGenerations maps the OpenAI images API onto marketplace image models,
// which are prompted through chat completions.
func (s *Server) imageGenerations(w http.ResponseWriter, r *http.Request) {
	req, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	var prompt, format string
	_ = json.Unmarshal(req["prompt"], &prompt)
	_ = json.Unmarshal(req["response_format"], &format)
	if strings.TrimSpace(prompt) == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "prompt_required", "\"prompt\" is required")
		return
	}
	body, _ := json.Marshal(map[string]any{
		"model":    modelOf(req),
		"messages": []any{map[string]any{"role": "user", "content": prompt}},
	})
	s.route(w, r, &call{
		kind: "image", modelName: modelOf(req), body: body,
		output: func(sess *consumer.Session) output {
			return &jsonOutput{w: w, sess: sess, wrap: func(b []byte) ([]byte, error) { return openAIImage(b, format) }}
		},
	})
}

// openAIImage converts a provider image result into an OpenAI images response.
func openAIImage(payload []byte, format string) ([]byte, error) {
	var v struct {
		ImageURL        string `json:"imageUrl"`
		ImageRawContent string `json:"imageRawContent"`
		VideoRawContent string `json:"videoRawContent"`
		Error           any    `json:"error"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		return nil, fmt.Errorf("unexpected image payload: %w", err)
	}
	item := map[string]string{}
	raw := v.ImageRawContent
	if raw == "" {
		raw = v.VideoRawContent
	}
	switch {
	case raw != "" && format == "b64_json":
		if i := strings.Index(raw, ";base64,"); i >= 0 {
			raw = raw[i+len(";base64,"):]
		}
		item["b64_json"] = raw
	case raw != "":
		item["url"] = raw // a data: URL
	case v.ImageURL != "":
		item["url"] = v.ImageURL
	default:
		return nil, errors.New("the model did not return an image (is it an image model?)")
	}
	return json.Marshal(map[string]any{"created": time.Now().Unix(), "data": []any{item}})
}

// ---------------------------------------------------------------------------
// POST /v1/decisions
// ---------------------------------------------------------------------------

func (s *Server) decisions(w http.ResponseWriter, r *http.Request) {
	req, ok := s.readJSON(w, r)
	if !ok {
		return
	}
	req["type"] = json.RawMessage(`"` + morrpc.TypeDecisions + `"`)
	body, _ := json.Marshal(req)
	s.route(w, r, &call{
		kind: "decisions", modelName: modelOf(req), body: body,
		output: func(sess *consumer.Session) output { return &jsonOutput{w: w, sess: sess} },
	})
}

// ---------------------------------------------------------------------------
// MCP agents: GET /v1/agents/{model}/tools, POST /v1/agents/{model}/tools/{tool}
// ---------------------------------------------------------------------------

func (s *Server) agentTools(w http.ResponseWriter, r *http.Request) {
	if s.d.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "consumer_disabled", "this node runs in provider-only mode")
		return
	}
	s.route(w, r, &call{
		kind: "agent", modelName: r.PathValue("model"),
		do: func(ctx context.Context, l *consumer.Lease, onChunk func([]byte) error) (*consumer.Result, error) {
			out, err := s.d.Sessions.Agent(ctx, l, "", nil)
			if err != nil {
				return nil, err
			}
			return &consumer.Result{Chunks: 1}, onChunk(out)
		},
		output: func(sess *consumer.Session) output {
			return &jsonOutput{w: w, sess: sess, wrap: func(b []byte) ([]byte, error) {
				return json.Marshal(map[string]json.RawMessage{"tools": b})
			}}
		},
	})
}

func (s *Server) agentCall(w http.ResponseWriter, r *http.Request) {
	if s.d.Sessions == nil {
		writeError(w, http.StatusServiceUnavailable, "api_error", "consumer_disabled", "this node runs in provider-only mode")
		return
	}
	args, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "body_too_large", "request body too large or unreadable")
		return
	}
	if len(strings.TrimSpace(string(args))) == 0 {
		args = []byte(`{}`)
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(args, &probe) != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "the body must be a JSON object of tool arguments")
		return
	}
	tool := r.PathValue("tool")
	s.route(w, r, &call{
		kind: "agent", modelName: r.PathValue("model"),
		do: func(ctx context.Context, l *consumer.Lease, onChunk func([]byte) error) (*consumer.Result, error) {
			out, err := s.d.Sessions.Agent(ctx, l, tool, args)
			if err != nil {
				return nil, err
			}
			return &consumer.Result{Chunks: 1}, onChunk(out)
		},
		output: func(sess *consumer.Session) output { return &jsonOutput{w: w, sess: sess} },
	})
}
