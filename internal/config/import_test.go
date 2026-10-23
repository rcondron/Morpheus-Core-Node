package config

import (
	"strings"
	"testing"
)

const lumerinModels = `{
  "$schema": "./internal/config/models-config-schema.json",
  "models": [
    {"modelId": "0x0000000000000000000000000000000000000000000000000000000000000003", "modelName": "claude-3-5-sonnet-20241022",
     "apiType": "claudeai", "apiUrl": "https://api.anthropic.com/v1/messages", "apiKey": "sk-ant", "concurrentSlots": 4, "capacityPolicy": "idle_timeout"},
    {"modelId": "0x0000000000000000000000000000000000000000000000000000000000000002", "modelName": "SDXL1.0-base",
     "apiType": "hyperbolic-sd", "apiUrl": "https://api.hyperbolic.xyz/v1/image/generation", "apiKey": "k", "parameters": {"cfg_scale": "5", "steps": "30"}}
  ]
}`

func TestImportModelsConfig(t *testing.T) {
	models, err := ImportModelsConfig([]byte(lumerinModels))
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models", len(models))
	}
	c := models[0]
	if c.Type() != APIClaude || c.BackendModel != "claude-3-5-sonnet-20241022" || c.MaxSessions != 4 || c.CapacityPolicy != CapacityIdleTimeout || c.APIKey != "sk-ant" {
		t.Fatalf("claude model %+v", c)
	}
	if models[1].Parameters["steps"] != "30" || ModalityOf(models[1]) != ModImage {
		t.Fatalf("image model %+v", models[1])
	}

	legacy := `{"0x0000000000000000000000000000000000000000000000000000000000000001": {"modelName": "llama2", "apiType": "openai", "apiUrl": "http://localhost:8080/v1/chat/completions"}}`
	models, err = ImportModelsConfig([]byte(legacy))
	if err != nil || len(models) != 1 || models[0].BackendModel != "llama2" {
		t.Fatalf("legacy import: %v %+v", err, models)
	}
	if _, err := ImportModelsConfig([]byte(`{"models":[{"modelId":"0x01","modelName":"x","apiType":"openai","apiUrl":"http://h"}]}`)); err == nil {
		t.Fatal("a short model id was accepted")
	}
	if _, err := ImportModelsConfig([]byte(`{"models":[{"modelId":"0x0000000000000000000000000000000000000000000000000000000000000001","modelName":"x","apiType":"gpt-magic","apiUrl":"http://h"}]}`)); err == nil || !strings.Contains(err.Error(), "api_type") {
		t.Fatalf("an unknown api type was accepted: %v", err)
	}
}

func TestImportAgentsConfig(t *testing.T) {
	agents, err := ImportAgentsConfig([]byte(`{"agents":[{"agentId":"0x00000000000000000000000000000000000000000000000000000000000000aa","agentName":"search","command":"npx","args":["-y","server"],"env":{"KEY":"v"},"concurrentSlots":2}]}`))
	if err != nil || len(agents) != 1 || agents[0].Command != "npx" || agents[0].Env["KEY"] != "v" || agents[0].MaxSessions != 2 {
		t.Fatalf("agents: %v %+v", err, agents)
	}
}

func TestMergeModels(t *testing.T) {
	a := ProviderModel{ModelID: "0xaa", Name: "a"}
	b := ProviderModel{ModelID: "0xbb", Name: "b"}
	merged, added, replaced := MergeModels([]ProviderModel{a, b}, []ProviderModel{{ModelID: "0xBB", Name: "b2"}, {ModelID: "0xcc", Name: "c"}})
	if added != 1 || replaced != 1 || len(merged) != 3 || merged[1].Name != "b2" || merged[2].Name != "c" {
		t.Fatalf("merge: %d %d %+v", added, replaced, merged)
	}
}

func TestEndpointsAndModalities(t *testing.T) {
	cases := []struct{ apiType, base, modality, want string }{
		{APIOpenAI, "https://api.venice.ai/api/v1", ModEmbeddings, "https://api.venice.ai/api/v1/embeddings"},
		{APIOpenAI, "https://api.openai.com/v1/chat/completions", ModSpeech, "https://api.openai.com/v1/audio/speech"},
		{APIOpenAI, "http://localhost:11434/v1/", ModChat, "http://localhost:11434/v1/chat/completions"},
		{APIClaude, "https://api.anthropic.com/v1/messages", ModChat, "https://api.anthropic.com/v1/messages"},
		{APIProdiaV2, "https://inference.prodia.com/v2/job", ModImage, "https://inference.prodia.com/v2/job"},
	}
	for _, c := range cases {
		got := EndpointFor(c.apiType, c.base, c.modality)
		if got != c.want {
			t.Errorf("EndpointFor(%s, %s, %s) = %s, want %s", c.apiType, c.base, c.modality, got, c.want)
		}
		if m := ModalityOf(ProviderModel{APIType: c.apiType, APIURL: got}); m != c.modality {
			t.Errorf("ModalityOf(%s) = %s, want %s", got, m, c.modality)
		}
	}
	for _, p := range Presets {
		if len(p.Modalities) == 0 || !strings.HasPrefix(p.BaseURL, "http") {
			t.Errorf("preset %s is incomplete", p.Key)
		}
	}
}

func TestProviderEnvOverride(t *testing.T) {
	home := t.TempDir()
	cfg := Default("mainnet")
	cfg.Role = RoleProvider
	cfg.Gateway.Enabled = true
	if err := cfg.Save(home); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MORCORE_PROVIDER_MODELS", lumerinModels)
	got, err := Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Provider.Models) != 2 || got.Provider.Models[0].Type() != APIClaude {
		t.Fatalf("env models not applied: %+v", got.Provider.Models)
	}
	t.Setenv("MORCORE_PROVIDER_MODELS", `{"models":[{"modelId":"nope"}]}`)
	if _, err := Load(home); err == nil {
		t.Fatal("invalid MORCORE_PROVIDER_MODELS was accepted")
	}
}
