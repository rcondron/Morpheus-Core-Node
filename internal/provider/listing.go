package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
)

// ListBackendModels asks a backend which models it serves, so setup can
// offer a list instead of a blank field. Only OpenAI-compatible and Anthropic
// backends have a listing endpoint; others return an error.
func ListBackendModels(ctx context.Context, apiType, apiURL, apiKey string) ([]string, error) {
	var url string
	header := http.Header{}
	switch apiType {
	case config.APIOpenAI, "":
		url = endpointFor(apiURL, "/models")
		if apiKey != "" {
			header.Set("Authorization", "Bearer "+apiKey)
		}
	case config.APIClaude:
		u := apiURL
		if u == "" {
			u = anthropicURL
		}
		url = strings.TrimSuffix(strings.TrimRight(u, "/"), "/messages") + "/models?limit=1000"
		header.Set("x-api-key", apiKey)
		header.Set("anthropic-version", anthropicVersion)
	default:
		return nil, fmt.Errorf("%s backends have no model listing", apiType)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, errors.New("the backend rejected the API key")
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("model listing returned HTTP %d", resp.StatusCode)
	}
	// OpenAI and Anthropic use {"data":[{"id":…}]}; Ollama's native list uses
	// {"models":[{"name":…}]}; some servers return a bare array.
	var v struct {
		Data   []struct{ ID string }          `json:"data"`
		Models []struct{ Name, Model string } `json:"models"`
	}
	var ids []string
	if json.Unmarshal(body, &v) == nil {
		for _, d := range v.Data {
			ids = append(ids, d.ID)
		}
		for _, m := range v.Models {
			ids = append(ids, firstNonEmpty(m.Name, m.Model))
		}
	} else {
		var arr []struct{ ID string }
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, errors.New("unrecognized model listing")
		}
		for _, d := range arr {
			ids = append(ids, d.ID)
		}
	}
	seen := map[string]bool{}
	out := ids[:0]
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
