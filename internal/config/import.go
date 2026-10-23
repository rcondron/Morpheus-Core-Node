package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// lumerinModel is one entry of the reference node's models-config.json.
type lumerinModel struct {
	ModelID         string            `json:"modelId"`
	ModelName       string            `json:"modelName"`
	APIType         string            `json:"apiType"`
	APIURL          string            `json:"apiUrl"`
	APIKey          string            `json:"apiKey"`
	ConcurrentSlots int               `json:"concurrentSlots"`
	CapacityPolicy  string            `json:"capacityPolicy"`
	Parameters      map[string]string `json:"parameters"`
}

func (m lumerinModel) toProvider() ProviderModel {
	return ProviderModel{
		ModelID:        normalizeID(m.ModelID),
		Name:           m.ModelName,
		APIType:        strings.TrimSpace(m.APIType),
		APIURL:         strings.TrimSpace(m.APIURL),
		APIKey:         m.APIKey,
		BackendModel:   m.ModelName,
		MaxSessions:    m.ConcurrentSlots,
		CapacityPolicy: m.CapacityPolicy,
		Parameters:     m.Parameters,
	}
}

// ImportModelsConfig converts a reference node models-config.json (the
// current {"models":[…]} shape or the legacy {"0x…":{…}} map) into provider
// models. The reference modelName is what the backend expects, so it becomes
// both the backend model and the initial display name.
func ImportModelsConfig(data []byte) ([]ProviderModel, error) {
	var v2 struct {
		Models []lumerinModel `json:"models"`
	}
	if err := json.Unmarshal(data, &v2); err == nil && len(v2.Models) > 0 {
		out := make([]ProviderModel, 0, len(v2.Models))
		for _, m := range v2.Models {
			out = append(out, m.toProvider())
		}
		return out, checkImported(out)
	}
	var legacy map[string]json.RawMessage
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, fmt.Errorf("not a models-config.json: %w", err)
	}
	ids := make([]string, 0, len(legacy))
	for id := range legacy {
		if strings.HasPrefix(id, "$") { // "$schema"
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []ProviderModel
	for _, id := range ids {
		var m lumerinModel
		if err := json.Unmarshal(legacy[id], &m); err != nil {
			return nil, fmt.Errorf("model %s: %w", id, err)
		}
		m.ModelID = id
		out = append(out, m.toProvider())
	}
	if len(out) == 0 {
		return nil, errors.New("no models found")
	}
	return out, checkImported(out)
}

// ImportAgentsConfig converts a reference node agents-config.json.
func ImportAgentsConfig(data []byte) ([]ProviderAgent, error) {
	var f struct {
		Agents []struct {
			AgentID         string            `json:"agentId"`
			AgentName       string            `json:"agentName"`
			Command         string            `json:"command"`
			Args            []string          `json:"args"`
			Env             map[string]string `json:"env"`
			ConcurrentSlots int               `json:"concurrentSlots"`
			CapacityPolicy  string            `json:"capacityPolicy"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("not an agents-config.json: %w", err)
	}
	if len(f.Agents) == 0 {
		return nil, errors.New("no agents found")
	}
	out := make([]ProviderAgent, 0, len(f.Agents))
	for i, a := range f.Agents {
		pa := ProviderAgent{
			AgentID: normalizeID(a.AgentID), Name: a.AgentName, Command: a.Command, Args: a.Args,
			Env: a.Env, MaxSessions: a.ConcurrentSlots, CapacityPolicy: a.CapacityPolicy,
		}
		if err := pa.validate(); err != nil {
			return nil, fmt.Errorf("agents[%d] (%s): %w", i, a.AgentName, err)
		}
		out = append(out, pa)
	}
	return out, nil
}

func checkImported(models []ProviderModel) error {
	var errs []error
	for i, m := range models {
		if err := m.validate(); err != nil {
			errs = append(errs, fmt.Errorf("models[%d] (%s): %w", i, m.Name, err))
		}
	}
	return errors.Join(errs...)
}

func normalizeID(id string) string {
	id = strings.TrimSpace(id)
	if id != "" && !strings.HasPrefix(strings.ToLower(id), "0x") {
		id = "0x" + id
	}
	return strings.ToLower(id)
}

// MergeModels adds or replaces models by model id, keeping the order of the
// existing list and appending new ids. It reports how many were added and replaced.
func MergeModels(existing, incoming []ProviderModel) (merged []ProviderModel, added, replaced int) {
	merged = append([]ProviderModel(nil), existing...)
	for _, in := range incoming {
		found := false
		for i := range merged {
			if strings.EqualFold(merged[i].ModelID, in.ModelID) {
				merged[i], found = in, true
				replaced++
				break
			}
		}
		if !found {
			merged = append(merged, in)
			added++
		}
	}
	return merged, added, replaced
}
