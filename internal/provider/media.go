package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/morrpc"
)

// Default endpoints for the image adapters (the reference node's defaults).
const (
	prodiaV1URL   = "https://api.prodia.com/v1"
	prodiaV2URL   = "https://inference.prodia.com/v2/job"
	hyperbolicURL = "https://api.hyperbolic.xyz/v1/image/generation"
)

// Image results, shaped as reference consumers expect them.
type imageURLResult struct {
	Job      string `json:"job"`
	Status   string `json:"status"`
	ImageURL string `json:"imageUrl"`
}

type imageRawResult struct {
	ImageRawContent string `json:"imageRawContent"`
}

type videoRawResult struct {
	VideoRawContent string `json:"videoRawContent"`
}

// imagePrompt is the text of the last chat message: image models are sold
// through chat completions on the marketplace.
func imagePrompt(r *request) (string, error) {
	if r.kind != morrpc.TypeChat {
		return "", fmt.Errorf("%w: %s", errUnsupported, r.kind)
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(r.body["messages"], &msgs); err != nil || len(msgs) == 0 {
		return "", invalid("image generation needs messages; the last one is the prompt")
	}
	c := msgs[len(msgs)-1].Content
	var s string
	if json.Unmarshal(c, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(c, &parts) == nil {
		var texts []string
		for _, p := range parts {
			if p.Type == "text" && p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) > 0 {
			return strings.Join(texts, "\n"), nil
		}
	}
	return "", invalid("the last message has no text prompt")
}

// typedParams converts string parameters to numbers or booleans where they
// look like one, since image APIs expect typed JSON.
func typedParams(params map[string]string) map[string]any {
	out := make(map[string]any, len(params))
	for k, v := range params {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			out[k] = n
		} else if b, err := strconv.ParseBool(v); err == nil {
			out[k] = b
		} else {
			out[k] = v
		}
	}
	return out
}

func (p *Provider) postJSON(ctx context.Context, url string, body any, headers map[string]string) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := p.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model backend unreachable: %w", err)
	}
	return resp, nil
}

// prodiaV1 generates with Prodia's v1 job API and polls the job until done.
func (p *Provider) prodiaV1(ctx context.Context, m *Model, r *request, e *emitter) error {
	prompt, err := imagePrompt(r)
	if err != nil {
		return err
	}
	url := strings.TrimRight(m.APIURL, "/")
	if url == "" {
		url = prodiaV1URL
	}
	base := url
	if i := strings.Index(url, "/v1"); i >= 0 {
		base = url[:i+3]
	}
	if strings.HasSuffix(url, "/v1") {
		if m.Type() == config.APIProdiaSDXL {
			url += "/sdxl/generate"
		} else {
			url += "/sd/generate"
		}
	}
	body := typedParams(m.Parameters)
	body["model"], body["prompt"] = m.Backend, prompt
	headers := map[string]string{"X-Prodia-Key": m.APIKey, "Accept": "application/json"}
	resp, err := p.postJSON(ctx, url, body, headers)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	e.st.status = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return e.upstreamError(resp.StatusCode, raw)
	}
	var job imageURLResult
	if err := json.Unmarshal(raw, &job); err != nil || job.Job == "" {
		return fmt.Errorf("unexpected Prodia response: %s", strings.TrimSpace(string(raw)))
	}
	for {
		switch job.Status {
		case "succeeded":
			e.st.out = 1
			return e.json(job)
		case "failed":
			return e.engineError(http.StatusBadGateway, map[string]any{"error": map[string]any{"message": "image generation job failed", "type": "upstream_error", "job": job.Job}})
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/job/"+job.Job, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Prodia-Key", m.APIKey)
		req.Header.Set("Accept", "application/json")
		res, err := p.http.Do(req)
		if err != nil {
			return fmt.Errorf("poll Prodia job: %w", err)
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
		res.Body.Close()
		if res.StatusCode != http.StatusOK || json.Unmarshal(raw, &job) != nil {
			return fmt.Errorf("poll Prodia job: status %d", res.StatusCode)
		}
	}
}

// prodiaV2 runs a Prodia v2 job whose response body is the image or video.
func (p *Provider) prodiaV2(ctx context.Context, m *Model, r *request, e *emitter) error {
	prompt, err := imagePrompt(r)
	if err != nil {
		return err
	}
	url := m.APIURL
	if url == "" {
		url = prodiaV2URL
	}
	cfg := typedParams(m.Parameters)
	cfg["prompt"] = prompt
	resp, err := p.postJSON(ctx, url, map[string]any{"type": m.Backend, "config": cfg}, map[string]string{"Authorization": "Bearer " + m.APIKey})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read Prodia result: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return e.upstreamError(resp.StatusCode, raw)
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.Index(ct, ";"); i >= 0 {
		ct = ct[:i]
	}
	data := "data:" + strings.TrimSpace(ct) + ";base64," + base64.StdEncoding.EncodeToString(raw)
	e.st.out = 1
	if strings.HasPrefix(ct, "video/") {
		return e.json(videoRawResult{VideoRawContent: data})
	}
	return e.json(imageRawResult{ImageRawContent: data})
}

// hyperbolic generates an image with Hyperbolic's API (base64 PNG).
func (p *Provider) hyperbolic(ctx context.Context, m *Model, r *request, e *emitter) error {
	prompt, err := imagePrompt(r)
	if err != nil {
		return err
	}
	url := m.APIURL
	if url == "" {
		url = hyperbolicURL
	}
	body := map[string]any{"model_name": m.Backend, "prompt": prompt, "height": 512, "width": 512, "backend": "auto"}
	for k, v := range typedParams(m.Parameters) {
		body[k] = v
	}
	resp, err := p.postJSON(ctx, url, body, map[string]string{"Authorization": "Bearer " + m.APIKey, "Accept": "application/json"})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return fmt.Errorf("read Hyperbolic result: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return e.upstreamError(resp.StatusCode, raw)
	}
	var res struct {
		Images []struct {
			Image string `json:"image"`
		} `json:"images"`
	}
	if err := json.Unmarshal(raw, &res); err != nil || len(res.Images) == 0 || res.Images[0].Image == "" {
		return errors.New("Hyperbolic returned no image")
	}
	e.st.out = 1
	return e.json(imageRawResult{ImageRawContent: "data:image/png;base64," + res.Images[0].Image})
}

// decisions forwards a Decisions / System One request to the full api_url.
// Upstream error bodies are summarized, not relayed: they can echo case data.
func (p *Provider) decisions(ctx context.Context, m *Model, r *request, e *emitter) error {
	if r.kind != morrpc.TypeDecisions {
		return invalid("%s requests are not supported: this model answers decisions requests", r.kind)
	}
	if err := validDecisions(r.body); err != nil {
		return err
	}
	body, err := r.withModel(m.Backend)
	if err != nil {
		return err
	}
	e.st.in = estimatePromptTokens(body)
	resp, err := p.post(ctx, m, m.APIURL, "application/json", bytes.NewReader(body), "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	e.st.status = resp.StatusCode
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("read decisions response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, typ := "upstream decisions request failed", "upstream_error"
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
			msg, typ = "upstream rejected the decisions request", "validation_error"
		}
		return e.engineError(resp.StatusCode, map[string]any{"error": map[string]any{"message": msg, "type": typ, "code": resp.StatusCode}})
	}
	var out struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(raw, &out) == nil && out.Usage.InputTokens+out.Usage.OutputTokens > 0 {
		e.st.in, e.st.out = out.Usage.InputTokens, out.Usage.OutputTokens
	}
	return e.raw(bytes.TrimSpace(raw))
}

// validDecisions requires a non-empty state and questions, so an empty
// request is never forwarded (and billed) upstream.
func validDecisions(body map[string]json.RawMessage) error {
	state := bytes.TrimSpace(body["state"])
	if len(state) == 0 || string(state) == "null" || string(state) == `""` {
		return invalid("decisions request needs a state")
	}
	var questions map[string]json.RawMessage
	if json.Unmarshal(body["questions"], &questions) != nil || len(questions) == 0 {
		return invalid("decisions request needs questions")
	}
	return nil
}
