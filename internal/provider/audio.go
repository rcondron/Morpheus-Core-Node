package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Form names for the transcription request fields. The reference consumer
// sends its request struct with Go field names as JSON keys (it only has
// form tags), so both spellings are accepted.
var transcriptionFields = map[string]string{
	"model":                   "",
	"s3presignedurl":          "s3_presigned_url",
	"s3_presigned_url":        "s3_presigned_url",
	"prompt":                  "prompt",
	"temperature":             "temperature",
	"language":                "language",
	"format":                  "response_format",
	"response_format":         "response_format",
	"timestampgranularities":  "timestamp_granularities[]",
	"timestamp_granularities": "timestamp_granularities[]",
	"stream":                  "stream",
}

// transcribe sends the uploaded audio to the backend as multipart form data
// (OpenAI /audio/transcriptions) and relays the transcript.
func (p *Provider) transcribe(ctx context.Context, m *Model, r *request, e *emitter) error {
	if r.file == "" && !hasField(r.body, "s3presignedurl", "s3_presigned_url") {
		return invalid("audio transcription needs an uploaded file (session.prompt.stream.*) or s3_presigned_url")
	}
	url := endpointFor(m.APIURL, "/audio/transcriptions")
	stream := r.stream()

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := writeTranscriptionForm(mw, m.Backend, r)
		if err == nil {
			err = mw.Close()
		}
		pw.CloseWithError(err)
	}()
	accept := ""
	if stream {
		accept = "text/event-stream"
	}
	resp, err := p.post(ctx, m, url, mw.FormDataContentType(), pr, accept)
	pr.Close()
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return e.upstreamError(resp.StatusCode, raw)
	}
	if stream && isEventStream(resp.Header) {
		return relayTranscriptStream(resp.Body, e)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read transcription: %w", err)
	}
	raw = bytes.TrimSpace(raw)
	e.st.out = (len(raw) + 3) / 4
	if json.Valid(raw) && len(raw) > 0 && raw[0] == '{' {
		return e.raw(raw) // json / verbose_json
	}
	return e.json(string(raw)) // text, srt, vtt: a JSON string
}

func hasField(body map[string]json.RawMessage, names ...string) bool {
	for k, v := range body {
		for _, n := range names {
			if strings.EqualFold(k, n) && len(v) > 0 && string(v) != `""` && string(v) != "null" {
				return true
			}
		}
	}
	return false
}

func writeTranscriptionForm(mw *multipart.Writer, backend string, r *request) error {
	if err := mw.WriteField("model", backend); err != nil {
		return err
	}
	keys := make([]string, 0, len(r.body))
	for k := range r.body {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	written := map[string]bool{}
	for _, k := range keys {
		raw := r.body[k]
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		name, known := transcriptionFields[strings.ToLower(k)]
		if known && name == "" {
			continue // model: always the backend's name
		}
		if !known {
			name = k // vendor extras pass through unchanged
		}
		switch {
		case name == "timestamp_granularities[]":
			var list []string
			if json.Unmarshal(raw, &list) != nil {
				continue
			}
			for _, g := range list {
				if err := mw.WriteField(name, g); err != nil {
					return err
				}
			}
			continue
		case name == "stream" || name == "temperature":
			if s := string(raw); s == "false" || s == "0" {
				continue // defaults: omit, as the reference's omitempty does
			}
		}
		val := formValue(raw)
		if val == "" || written[name] {
			continue // "Format" and "response_format" may both be present
		}
		written[name] = true
		if err := mw.WriteField(name, val); err != nil {
			return err
		}
	}
	if r.file == "" {
		return nil
	}
	f, err := os.Open(r.file)
	if err != nil {
		return err
	}
	defer f.Close()
	part, err := mw.CreateFormFile("file", filepath.Base(r.file))
	if err != nil {
		return err
	}
	_, err = io.Copy(part, f)
	return err
}

// formValue renders a JSON value as a form field: strings unquoted, anything
// else as its JSON text.
func formValue(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// transcriptDelta is the reference chunk for streamed transcripts.
type transcriptDelta struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Delta string `json:"delta"`
}

func relayTranscriptStream(r io.Reader, e *emitter) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		data, ok := sseData(sc.Bytes())
		if !ok {
			continue
		}
		if string(data) == "[DONE]" {
			return nil
		}
		var ev struct {
			Type  string          `json:"type"`
			Delta string          `json:"delta"`
			Text  string          `json:"text"`
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "transcript.text.delta":
			e.st.out += (len(ev.Delta) + 3) / 4
			if err := e.json(transcriptDelta{Type: ev.Type, Delta: ev.Delta}); err != nil {
				return err
			}
		case "transcript.text.done":
			return e.json(transcriptDelta{Type: ev.Type, Text: ev.Text})
		case "error":
			return e.upstreamError(http.StatusBadGateway, data)
		}
	}
	return sc.Err()
}

// speechChunk is how much synthesized audio each response carries.
const speechChunk = 64 << 10

// speech asks the backend to synthesize audio and relays it as base64 chunks
// (JSON strings), ending with null — the reference's audio_speech framing.
func (p *Provider) speech(ctx context.Context, m *Model, r *request, e *emitter) error {
	body, err := r.withModel(m.Backend)
	if err != nil {
		return err
	}
	var in struct {
		Input string `json:"input"`
	}
	_ = json.Unmarshal(body, &in)
	e.st.in = (len(in.Input) + 3) / 4
	resp, err := p.post(ctx, m, endpointFor(m.APIURL, "/audio/speech"), "application/json", bytes.NewReader(body), "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return e.upstreamError(resp.StatusCode, raw)
	}
	buf := make([]byte, speechChunk)
	for {
		n, err := io.ReadFull(resp.Body, buf)
		if n > 0 {
			e.st.out += n
			if err := e.json(buf[:n]); err != nil { // []byte marshals as base64
				return err
			}
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read synthesized audio: %w", err)
		}
	}
	return e.raw([]byte("null"))
}
