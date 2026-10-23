package config

import "strings"

// Modalities a provider model can serve.
const (
	ModChat          = "chat"
	ModEmbeddings    = "embeddings"
	ModSpeech        = "speech"        // text-to-speech
	ModTranscription = "transcription" // speech-to-text
	ModImage         = "image"
	ModDecisions     = "decisions"
)

// ModalityLabels name modalities for people.
var ModalityLabels = map[string]string{
	ModChat: "Chat / text generation", ModEmbeddings: "Embeddings", ModSpeech: "Text to speech",
	ModTranscription: "Speech to text", ModImage: "Image generation", ModDecisions: "Decisions",
}

// Preset is a known backend a provider can serve or resell on the marketplace.
type Preset struct {
	Key        string
	Label      string
	APIType    string
	BaseURL    string // OpenAI-style base (…/v1), or the full endpoint for other API types
	Local      bool   // runs on the provider's own machine
	NeedsKey   bool
	Modalities []string // the first is the default
}

// Presets lists hosted APIs (resale) and local model servers.
var Presets = []Preset{
	{Key: "venice", Label: "Venice", APIType: APIOpenAI, BaseURL: "https://api.venice.ai/api/v1", NeedsKey: true,
		Modalities: []string{ModChat, ModEmbeddings, ModSpeech}},
	{Key: "openai", Label: "OpenAI", APIType: APIOpenAI, BaseURL: "https://api.openai.com/v1", NeedsKey: true,
		Modalities: []string{ModChat, ModEmbeddings, ModSpeech, ModTranscription}},
	{Key: "anthropic", Label: "Anthropic (Claude)", APIType: APIClaude, BaseURL: "https://api.anthropic.com/v1/messages", NeedsKey: true,
		Modalities: []string{ModChat}},
	{Key: "openrouter", Label: "OpenRouter", APIType: APIOpenAI, BaseURL: "https://openrouter.ai/api/v1", NeedsKey: true,
		Modalities: []string{ModChat}},
	{Key: "groq", Label: "Groq", APIType: APIOpenAI, BaseURL: "https://api.groq.com/openai/v1", NeedsKey: true,
		Modalities: []string{ModChat, ModTranscription}},
	{Key: "together", Label: "Together AI", APIType: APIOpenAI, BaseURL: "https://api.together.xyz/v1", NeedsKey: true,
		Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "deepinfra", Label: "DeepInfra", APIType: APIOpenAI, BaseURL: "https://api.deepinfra.com/v1/openai", NeedsKey: true,
		Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "fireworks", Label: "Fireworks AI", APIType: APIOpenAI, BaseURL: "https://api.fireworks.ai/inference/v1", NeedsKey: true,
		Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "mistral", Label: "Mistral", APIType: APIOpenAI, BaseURL: "https://api.mistral.ai/v1", NeedsKey: true,
		Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "hyperbolic", Label: "Hyperbolic (LLMs)", APIType: APIOpenAI, BaseURL: "https://api.hyperbolic.xyz/v1", NeedsKey: true,
		Modalities: []string{ModChat}},
	{Key: "hyperbolic-image", Label: "Hyperbolic (images)", APIType: APIHyperbolicSD, BaseURL: "https://api.hyperbolic.xyz/v1/image/generation", NeedsKey: true,
		Modalities: []string{ModImage}},
	{Key: "prodia", Label: "Prodia (images & video)", APIType: APIProdiaV2, BaseURL: "https://inference.prodia.com/v2/job", NeedsKey: true,
		Modalities: []string{ModImage}},
	{Key: "typesafe", Label: "TypeSafe Decisions", APIType: APIDecisions, BaseURL: "https://api.typesafe.ai/v1/systemone", NeedsKey: true,
		Modalities: []string{ModDecisions}},
	{Key: "ollama", Label: "Ollama (local)", APIType: APIOpenAI, BaseURL: "http://localhost:11434/v1", Local: true, Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "vllm", Label: "vLLM / SGLang (local)", APIType: APIOpenAI, BaseURL: "http://localhost:8000/v1", Local: true, Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "llamacpp", Label: "llama.cpp server (local)", APIType: APIOpenAI, BaseURL: "http://localhost:8080/v1", Local: true, Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "lmstudio", Label: "LM Studio (local)", APIType: APIOpenAI, BaseURL: "http://localhost:1234/v1", Local: true, Modalities: []string{ModChat, ModEmbeddings}},
	{Key: "litellm", Label: "LiteLLM proxy", APIType: APIOpenAI, BaseURL: "http://localhost:4000/v1", Local: true,
		Modalities: []string{ModChat, ModEmbeddings, ModSpeech, ModTranscription}},
}

// LookupPreset finds a preset by key.
func LookupPreset(key string) (Preset, bool) {
	for _, p := range Presets {
		if p.Key == strings.ToLower(strings.TrimSpace(key)) {
			return p, true
		}
	}
	return Preset{}, false
}

// modalityPaths are the OpenAI endpoints per modality.
var modalityPaths = map[string]string{
	ModChat: "/chat/completions", ModEmbeddings: "/embeddings",
	ModSpeech: "/audio/speech", ModTranscription: "/audio/transcriptions",
}

// EndpointFor returns the api_url for a modality: OpenAI-compatible bases
// get the modality's path, other API types use their full endpoint.
func EndpointFor(apiType, base, modality string) string {
	base = strings.TrimRight(base, "/")
	if apiType != APIOpenAI {
		return base
	}
	path := modalityPaths[modality]
	for _, p := range modalityPaths {
		if strings.HasSuffix(base, p) {
			base = strings.TrimSuffix(base, p)
			break
		}
	}
	return base + path
}

// ModalityOf infers what a configured model serves.
func ModalityOf(m ProviderModel) string {
	switch m.Type() {
	case APIProdiaSD, APIProdiaSDXL, APIProdiaV2, APIHyperbolicSD:
		return ModImage
	case APIDecisions:
		return ModDecisions
	case APIClaude:
		return ModChat
	}
	u := strings.ToLower(strings.TrimRight(m.APIURL, "/"))
	switch {
	case strings.HasSuffix(u, "/embeddings"):
		return ModEmbeddings
	case strings.HasSuffix(u, "/audio/speech"):
		return ModSpeech
	case strings.HasSuffix(u, "/audio/transcriptions"):
		return ModTranscription
	}
	return ModChat
}
