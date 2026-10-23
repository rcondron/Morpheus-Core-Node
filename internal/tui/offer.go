package tui

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/ethereum/go-ethereum/common"

	"github.com/rcondron/Morpheus-Core-Node/internal/chain"
	"github.com/rcondron/Morpheus-Core-Node/internal/config"
	"github.com/rcondron/Morpheus-Core-Node/internal/logx"
	"github.com/rcondron/Morpheus-Core-Node/internal/marketplace"
	"github.com/rcondron/Morpheus-Core-Node/internal/provider"
)

// AskOffering walks a provider through adding one model to sell: pick a
// hosted API or local server, paste its key, pick the model from the
// backend's own list, and join the matching marketplace listing.
func AskOffering(ctx context.Context, cfg *config.Config, nw config.Network) (*config.ProviderModel, error) {
	// 1. Backend.
	presetKey := "venice"
	var opts []huh.Option[string]
	for _, p := range config.Presets {
		kind := "hosted API — resell your access"
		if p.Local {
			kind = "your own server"
		}
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-26s %s", p.Label, Subtle.Render(kind)), p.Key))
	}
	opts = append(opts, huh.NewOption("Other OpenAI-compatible or custom endpoint", "custom"))
	if err := run(huh.NewSelect[string]().Title("Where does the model run?").
		Description("Resell a hosted API you already pay for, or serve a model from your own machine.").
		Options(opts...).Height(14).Filtering(true).Value(&presetKey)); err != nil {
		return nil, err
	}
	pm := &config.ProviderModel{MaxSessions: 4}
	preset, known := config.LookupPreset(presetKey)
	base := preset.BaseURL
	if !known {
		preset = config.Preset{Label: "Custom", APIType: config.APIOpenAI, Modalities: []string{config.ModChat, config.ModEmbeddings, config.ModSpeech, config.ModTranscription}}
		var typeOpts []huh.Option[string]
		for _, t := range config.APITypes {
			typeOpts = append(typeOpts, huh.NewOption(t, t))
		}
		if err := run(
			huh.NewSelect[string]().Title("API type").Options(typeOpts...).Value(&preset.APIType),
			huh.NewInput().Title("Endpoint URL").Description("An OpenAI-style base (…/v1) or the full endpoint.").
				Placeholder("https://my-server.example.com/v1").Value(&base).Validate(validURL),
		); err != nil {
			return nil, err
		}
		preset.NeedsKey = true
	}
	pm.APIType = preset.APIType
	if preset.APIType == config.APIOpenAI {
		pm.APIType = "" // the default; keeps config.toml short
	}

	// 2. Where and how to reach it.
	var fields []huh.Field
	if preset.Local {
		fields = append(fields, huh.NewInput().Title(preset.Label+" address").
			Description("Change the host if the server runs on another machine.").Value(&base).Validate(validURL))
	}
	if preset.NeedsKey || preset.Local {
		title := preset.Label + " API key"
		desc := "Stored in config.toml (owner-only). Consumers never see it."
		if preset.Local {
			title, desc = "API key (optional)", "Only if your server requires one."
		}
		fields = append(fields, huh.NewInput().Title(title).Description(desc).EchoMode(huh.EchoModePassword).
			Value(&pm.APIKey).Validate(func(v string) error {
			if preset.NeedsKey && !preset.Local && strings.TrimSpace(v) == "" {
				return errors.New("required")
			}
			return nil
		}))
	}
	modality := preset.Modalities[0]
	if len(preset.Modalities) > 1 {
		var mods []huh.Option[string]
		for _, m := range preset.Modalities {
			mods = append(mods, huh.NewOption(config.ModalityLabels[m], m))
		}
		fields = append(fields, huh.NewSelect[string]().Title("What will you sell?").Options(mods...).Value(&modality))
	}
	if len(fields) > 0 {
		if err := run(fields...); err != nil {
			return nil, err
		}
	}
	pm.APIKey = strings.TrimSpace(pm.APIKey)
	pm.APIURL = config.EndpointFor(preset.APIType, base, modality)

	// 3. The backend's model.
	var backendModels []string
	var listErr error
	_ = Spin("Asking "+preset.Label+" for its models", func() error {
		backendModels, listErr = provider.ListBackendModels(ctx, preset.APIType, pm.APIURL, pm.APIKey)
		return nil
	})
	if listErr != nil && preset.NeedsKey && strings.Contains(listErr.Error(), "rejected the API key") {
		return nil, fmt.Errorf("%s rejected the API key; check it and try again", preset.Label)
	}
	if len(backendModels) > 0 {
		var mopts []huh.Option[string]
		for _, m := range backendModels {
			mopts = append(mopts, huh.NewOption(m, m))
		}
		mopts = append(mopts, huh.NewOption("Enter a model name manually", ""))
		pm.BackendModel = backendModels[0]
		if err := run(huh.NewSelect[string]().Title("Model to sell").
			Description(fmt.Sprintf("%d models available from %s (type / to search).", len(backendModels), preset.Label)).
			Options(mopts...).Filtering(true).Height(12).Value(&pm.BackendModel)); err != nil {
			return nil, err
		}
	}
	if pm.BackendModel == "" {
		if err := run(huh.NewInput().Title("Model name the backend expects").
			Placeholder("e.g. llama-3.3-70b").Value(&pm.BackendModel).Validate(nonEmpty)); err != nil {
			return nil, err
		}
	}
	pm.BackendModel = strings.TrimSpace(pm.BackendModel)

	// 4. The marketplace listing it is sold under.
	if err := pickListing(ctx, cfg, nw, pm); err != nil {
		return nil, err
	}

	// 5. Capacity.
	slots := strconv.Itoa(pm.MaxSessions)
	if err := run(huh.NewSelect[string]().Title("Concurrent sessions").
		Description("How many consumers can hold a session at once. Keep it within your upstream rate limits.").
		Options(huh.NewOption("1", "1"), huh.NewOption("2", "2"), huh.NewOption("4", "4"), huh.NewOption("8", "8"),
			huh.NewOption("16", "16"), huh.NewOption("Unlimited", "0")).Value(&slots)); err != nil {
		return nil, err
	}
	pm.MaxSessions, _ = strconv.Atoi(slots)
	return pm, nil
}

// pickListing chooses the on-chain model id, preferring existing listings
// whose name matches the backend model.
func pickListing(ctx context.Context, cfg *config.Config, nw config.Network, pm *config.ProviderModel) error {
	var models []marketplace.Model
	_ = Spin("Loading marketplace listings", func() error {
		c, err := chain.NewClient(cfg.Chain.RPCURLs, logx.Discard())
		if err != nil {
			return err
		}
		defer c.Close()
		k := chain.NewContracts(c, common.HexToAddress(nw.DiamondAddress), common.HexToAddress(nw.MORTokenAddress), big.NewInt(nw.ChainID))
		cctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		all, err := marketplace.New(k, nil, nil, logx.Discard()).Models(cctx)
		for _, m := range all {
			if !m.IsStorage() { // storage listings are sold with provider storage
				models = append(models, m)
			}
		}
		return err
	})
	sort.SliceStable(models, func(i, j int) bool {
		si, sj := nameMatch(models[i].Name, pm.BackendModel), nameMatch(models[j].Name, pm.BackendModel)
		if si != sj {
			return si > sj
		}
		return models[i].Bids > models[j].Bids
	})
	choice := "manual"
	var opts []huh.Option[string]
	for _, m := range models {
		label := fmt.Sprintf("%s  %s", m.Alias, Subtle.Render(fmt.Sprintf("(%d bids)", m.Bids)))
		if nameMatch(m.Name, pm.BackendModel) >= 2 {
			label += Good.Render("  ✓ matches")
		}
		opts = append(opts, huh.NewOption(label, m.ID.Hex()))
	}
	if len(models) > 0 && nameMatch(models[0].Name, pm.BackendModel) > 0 {
		choice = models[0].ID.Hex()
	}
	opts = append(opts, huh.NewOption("Enter a listing id / register a new listing later", "manual"))
	if err := run(huh.NewSelect[string]().Title("Marketplace listing").
		Description("Join an existing listing so consumers already looking for this model find you.").
		Options(opts...).Filtering(true).Height(12).Value(&choice)); err != nil {
		return err
	}
	if choice != "manual" {
		pm.ModelID = strings.ToLower(choice)
		for _, m := range models {
			if strings.EqualFold(m.ID.Hex(), choice) {
				pm.Name = m.Alias
			}
		}
		return nil
	}
	if err := run(huh.NewInput().Title("Listing id (0x…, 32 bytes)").
		Description("Leave empty to create one with `morcore provider model-register`.").
		Value(&pm.ModelID).Validate(func(v string) error {
		v = strings.TrimSpace(v)
		if v != "" && (!strings.HasPrefix(v, "0x") || len(v) != 66) {
			return errors.New("expected 0x followed by 64 hex characters")
		}
		return nil
	})); err != nil {
		return err
	}
	pm.ModelID = strings.ToLower(strings.TrimSpace(pm.ModelID))
	pm.Name = pm.BackendModel
	if pm.ModelID == "" {
		return errors.New("no listing chosen: register one with `morcore provider model-register <name>`, then add the model again")
	}
	return nil
}

// nameMatch scores how well a listing name matches a backend model name:
// 3 identical, 2 one contains the other, 1 sharing a distinctive token.
func nameMatch(listing, backend string) int {
	a, b := normName(listing), normName(backend)
	if a == "" || b == "" {
		return 0
	}
	if a == b {
		return 3
	}
	if strings.Contains(a, b) || strings.Contains(b, a) {
		return 2
	}
	ta := strings.FieldsFunc(strings.ToLower(listing), splitName)
	for _, t := range strings.FieldsFunc(strings.ToLower(backend), splitName) {
		if len(t) < 3 {
			continue
		}
		for _, u := range ta {
			if t == u {
				return 1
			}
		}
	}
	return 0
}

func splitName(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.') }

func normName(s string) string {
	s = strings.ToLower(s)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:] // "meta-llama/llama-3.3-70b" → "llama-3.3-70b"
	}
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '.' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
