package cli

import (
	"flag"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/tui"
)

const providerAddHelp = `
Adds a model you sell: a hosted API you resell (Venice, OpenAI, Anthropic,
OpenRouter, Groq, Together, Prodia, Hyperbolic…) or your own server (vLLM,
llama.cpp, Ollama, LM Studio, LiteLLM). Without flags it asks interactively,
listing the backend's models and matching marketplace listings.

Scripted:
  VENICE_API_KEY=… morcore provider add --preset venice --model llama-3.3-70b \
      --listing llama-3.3-70b --key-env VENICE_API_KEY --slots 4

Flags:
  --preset KEY        venice, openai, anthropic, openrouter, groq, together, deepinfra,
                      fireworks, mistral, hyperbolic, hyperbolic-image, prodia, typesafe,
                      ollama, vllm, llamacpp, lmstudio, litellm (morcore provider presets)
  --url URL           backend base or full endpoint (overrides the preset's)
  --type TYPE         api type for custom endpoints: openai, claudeai, prodia-sd,
                      prodia-sdxl, prodia-v2, hyperbolic-sd, decisions
  --modality M        chat (default), embeddings, speech, transcription, image, decisions
  --model NAME        model name the backend expects
  --listing NAME|ID   marketplace listing to sell it under (default: --model)
  --key-env VAR       read the backend API key from environment variable VAR
  --slots N           concurrent sessions (default 4, 0 = unlimited)
  --policy P          simple (default) or idle_timeout
  --endpoint HOST:PORT  public provider address, if not configured yet`

func providerAdd(a *App, args []string) error {
	fs := flag.NewFlagSet("provider add", flag.ContinueOnError)
	presetKey := fs.String("preset", "", "")
	rawURL := fs.String("url", "", "")
	apiType := fs.String("type", "", "")
	modality := fs.String("modality", "", "")
	model := fs.String("model", "", "")
	listing := fs.String("listing", "", "")
	keyEnv := fs.String("key-env", "", "")
	slots := fs.Int("slots", 4, "")
	policy := fs.String("policy", "", "")
	endpoint := fs.String("endpoint", "", "")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	nw := cfg.NetworkInfo()

	var pm *config.ProviderModel
	if *model == "" {
		if !tui.Interactive() {
			return usagef("Pass --preset/--url and --model to add a model without prompts.", "provider add needs a terminal or flags")
		}
		ctx, cancel := timeout(30 * time.Minute)
		defer cancel()
		if pm, err = tui.AskOffering(ctx, cfg, nw); err != nil {
			return err
		}
	} else {
		if pm, err = offeringFromFlags(a, cfg, *presetKey, *rawURL, *apiType, *modality, *model, *listing, *keyEnv); err != nil {
			return err
		}
		pm.MaxSessions = *slots
	}
	if *policy != "" {
		pm.CapacityPolicy = *policy
	}

	if !cfg.ProviderEnabled() {
		if cfg.Role == config.RoleConsumer {
			cfg.Role = config.RoleBoth
		} else {
			cfg.Role = config.RoleProvider
		}
		a.info("Selling requires the provider role: this node becomes %s.", tui.Strong.Render(cfg.Role))
	}
	if *endpoint != "" {
		cfg.Provider.PublicEndpoint = *endpoint
	}
	if cfg.Provider.PublicEndpoint == "" {
		return usagef("Pass --endpoint your.host:3333 (the address consumers reach, registered on-chain).", "no public provider endpoint configured")
	}
	cfg.Provider.Enabled = true
	var replaced int
	cfg.Provider.Models, _, replaced = config.MergeModels(cfg.Provider.Models, []config.ProviderModel{*pm})
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(a.Home); err != nil {
		return err
	}
	verb := "added"
	if replaced > 0 {
		verb = "updated"
	}
	a.ok("%s %s %s", verb, tui.Strong.Render(pm.Name), tui.Subtle.Render(fmt.Sprintf("(%s via %s, %s)", config.ModalityLabels[config.ModalityOf(*pm)], pm.BackendModel, hostOf(pm.APIURL))))
	a.applyReload()
	a.info("\nNext: %s  then  %s",
		tui.Code.Render("morcore provider register"),
		tui.Code.Render(fmt.Sprintf("morcore provider bid %s --price <MOR/hour>", pm.Name)))
	return nil
}

func offeringFromFlags(a *App, cfg *config.Config, presetKey, rawURL, apiType, modality, model, listing, keyEnv string) (*config.ProviderModel, error) {
	pm := &config.ProviderModel{BackendModel: strings.TrimSpace(model)}
	preset, ok := config.LookupPreset(presetKey)
	switch {
	case presetKey != "" && !ok:
		return nil, usagef("See `morcore provider presets`.", "unknown preset %q", presetKey)
	case !ok && rawURL == "":
		return nil, usagef("Pass --preset or --url.", "provider add needs a backend")
	}
	base := preset.BaseURL
	if rawURL != "" {
		base = rawURL
	}
	pm.APIType = preset.APIType
	if apiType != "" {
		pm.APIType = apiType
	}
	if pm.APIType == "" {
		pm.APIType = config.APIOpenAI
	}
	if modality == "" {
		modality = config.ModChat
		if len(preset.Modalities) > 0 {
			modality = preset.Modalities[0]
		}
	}
	if _, ok := config.ModalityLabels[modality]; !ok {
		return nil, usagef("", "unknown modality %q", modality)
	}
	pm.APIURL = config.EndpointFor(pm.APIType, base, modality)
	if pm.APIType == config.APIOpenAI {
		pm.APIType = ""
	}
	if keyEnv != "" {
		if pm.APIKey = strings.TrimSpace(os.Getenv(keyEnv)); pm.APIKey == "" {
			return nil, usagef("", "$%s is empty", keyEnv)
		}
	}
	if listing == "" {
		listing = model
	}
	if strings.HasPrefix(listing, "0x") && len(listing) == 66 {
		pm.ModelID, pm.Name = strings.ToLower(listing), model
		return pm, nil
	}
	ro, err := a.readOnly(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := timeout(60 * time.Second)
	defer cancel()
	m, err := ro.market.Resolve(ctx, listing)
	if err != nil {
		return nil, usagef("Pass --listing with an existing listing name or 0x id (see `morcore models --all`).", "marketplace listing %q: %v", listing, err)
	}
	pm.ModelID, pm.Name = strings.ToLower(m.ID.Hex()), m.Alias
	return pm, nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// applyReload tells a running node to pick up changed offerings now.
func (a *App) applyReload() {
	c, ok := a.running()
	if !ok {
		a.info("%s", tui.Subtle.Render("The node is not running: changes apply when it starts."))
		return
	}
	var out struct{ Added, Removed []string }
	if err := c.Do("POST", "/admin/provider/reload", nil, &out); err != nil {
		a.warn("the running node did not reload: %v", err)
		return
	}
	a.ok("running node reloaded (%d added, %d removed) — open sessions were not interrupted", len(out.Added), len(out.Removed))
}

func providerModels(a *App, args []string) error {
	cfg, err := a.config()
	if err != nil {
		return err
	}
	if a.JSON {
		type row struct {
			Name, ModelID, APIType, Modality, Backend, Endpoint string
			MaxSessions                                         int
		}
		var rows []row
		for _, m := range cfg.Provider.Models {
			rows = append(rows, row{m.Name, m.ModelID, m.Type(), config.ModalityOf(m), m.BackendModel, hostOf(m.APIURL), m.MaxSessions})
		}
		for _, ag := range cfg.Provider.Agents {
			rows = append(rows, row{ag.Name, ag.AgentID, "mcp", "agent", ag.Command, "local process", ag.MaxSessions})
		}
		return a.printJSON(rows)
	}
	if len(cfg.Provider.Models)+len(cfg.Provider.Agents) == 0 {
		a.info("No models configured. Add one with %s", tui.Code.Render("morcore provider add"))
		return nil
	}
	t := a.table()
	header(t, "NAME", "LISTING", "SELLS", "BACKEND", "VIA", "SLOTS")
	slots := func(n int) string {
		if n == 0 {
			return "∞"
		}
		return fmt.Sprint(n)
	}
	for _, m := range cfg.Provider.Models {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, shortHash(m.ModelID), config.ModalityLabels[config.ModalityOf(m)], m.BackendModel, hostOf(m.APIURL), slots(m.MaxSessions))
	}
	for _, ag := range cfg.Provider.Agents {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", ag.Name, shortHash(ag.AgentID), "MCP agent tools", ag.Command, "local process", slots(ag.MaxSessions))
	}
	return t.Flush()
}

func providerRemove(a *App, args []string) error {
	if len(args) != 1 {
		return usagef("See `morcore provider models`.", "provider remove needs a model name or id")
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	ref := args[0]
	var removedID string
	models := cfg.Provider.Models[:0]
	for _, m := range cfg.Provider.Models {
		if removedID == "" && (strings.EqualFold(m.Name, ref) || strings.EqualFold(m.ModelID, ref)) {
			removedID = m.ModelID
			continue
		}
		models = append(models, m)
	}
	cfg.Provider.Models = models
	agents := cfg.Provider.Agents[:0]
	for _, ag := range cfg.Provider.Agents {
		if removedID == "" && (strings.EqualFold(ag.Name, ref) || strings.EqualFold(ag.AgentID, ref)) {
			removedID = ag.AgentID
			continue
		}
		agents = append(agents, ag)
	}
	cfg.Provider.Agents = agents
	if removedID == "" {
		return fmt.Errorf("no configured model or agent named %q", ref)
	}
	if err := cfg.Save(a.Home); err != nil {
		return err
	}
	a.ok("removed %s", ref)
	if ro, err := a.readOnly(cfg); err == nil {
		ctx, cancel := timeout(30 * time.Second)
		defer cancel()
		if ids, err := ro.k.ProviderActiveBidIDs(ctx, commonAddr(cfg.Wallet.Address)); err == nil {
			if bids, err := ro.k.GetBids(ctx, ids); err == nil {
				for _, b := range bids {
					if strings.EqualFold(common.Hash(b.ModelId).Hex(), removedID) {
						a.warn("its bid %s is still live — consumers will be refused; withdraw it: %s", shortHash(b.ID.Hex()), tui.Code.Render("morcore provider unbid "+b.ID.Hex()))
					}
				}
			}
		}
	}
	a.applyReload()
	return nil
}

func providerImport(a *App, args []string) error {
	fs := flag.NewFlagSet("provider import", flag.ContinueOnError)
	agentsFile := fs.String("agents", "", "")
	replace := fs.Bool("replace", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 && *agentsFile == "" {
		return usagef("Example: morcore provider import models-config.json --agents agents-config.json", "provider import needs a models-config.json")
	}
	cfg, err := a.config()
	if err != nil {
		return err
	}
	var added, replaced int
	if len(pos) == 1 {
		data, err := os.ReadFile(pos[0])
		if err != nil {
			return err
		}
		models, err := config.ImportModelsConfig(data)
		if err != nil {
			return err
		}
		if *replace {
			cfg.Provider.Models, added = models, len(models)
		} else {
			cfg.Provider.Models, added, replaced = config.MergeModels(cfg.Provider.Models, models)
		}
	}
	if *agentsFile != "" {
		data, err := os.ReadFile(*agentsFile)
		if err != nil {
			return err
		}
		agents, err := config.ImportAgentsConfig(data)
		if err != nil {
			return err
		}
		if *replace {
			cfg.Provider.Agents = agents
		} else {
			for _, in := range agents {
				found := false
				for i := range cfg.Provider.Agents {
					if strings.EqualFold(cfg.Provider.Agents[i].AgentID, in.AgentID) {
						cfg.Provider.Agents[i], found = in, true
					}
				}
				if !found {
					cfg.Provider.Agents = append(cfg.Provider.Agents, in)
				}
			}
		}
		added += len(agents)
	}
	if cfg.Role == config.RoleConsumer {
		cfg.Role = config.RoleBoth
	}
	cfg.Provider.Enabled = true
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := cfg.Save(a.Home); err != nil {
		return err
	}
	a.ok("imported: %d added, %d updated", added-replaced, replaced)
	if cfg.Provider.PublicEndpoint == "" {
		a.warn("set provider.public_endpoint (host:3333) in %s or run `morcore provider register --endpoint …`", cfg.Path(config.FileName))
	}
	a.applyReload()
	return nil
}

func providerReload(a *App, args []string) error {
	if _, ok := a.running(); !ok {
		a.info("The node is not running; configuration is read when it starts.")
		return nil
	}
	a.applyReload()
	return nil
}

func providerPresets(a *App, args []string) error {
	if a.JSON {
		return a.printJSON(config.Presets)
	}
	t := a.table()
	header(t, "PRESET", "BACKEND", "API", "SELLS")
	for _, p := range config.Presets {
		fmt.Fprintf(t, "%s\t%s\t%s\t%s\n", p.Key, p.Label, p.APIType, strings.Join(p.Modalities, ", "))
	}
	return t.Flush()
}

// marketPrices summarizes the other offers for a model (per-second wei
// prices), to help price a bid.
func marketPrices(prices []*big.Int) string {
	if len(prices) == 0 {
		return "no other offers yet — you set the price"
	}
	sort.Slice(prices, func(i, j int) bool { return prices[i].Cmp(prices[j]) < 0 })
	return fmt.Sprintf("%d offer(s), %s – %s (median %s)", len(prices),
		perHour(prices[0].String()), perHour(prices[len(prices)-1].String()), perHour(prices[len(prices)/2].String()))
}
