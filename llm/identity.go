package llm

import "strings"

// SelectionSeparator joins provider and model in a provider-scoped model id.
const SelectionSeparator = "::"

// ModelRef is the part of a catalog entry that model selection looks at. ID is
// the provider-scoped selection id ("openai::gpt-5"), ModelID the raw id.
type ModelRef struct {
	ID       string
	ModelID  string
	Provider string
}

// RawID is the id a provider API knows the model by.
func (m ModelRef) RawID() string {
	if id := strings.TrimSpace(m.ModelID); id != "" {
		return id
	}
	return strings.TrimSpace(m.ID)
}

// SelectionID builds "provider::modelId" (provider lower-cased).
func SelectionID(provider, modelID string) string {
	return strings.ToLower(strings.TrimSpace(provider)) + SelectionSeparator + strings.TrimSpace(modelID)
}

// ParseSelectionID splits a selection id; ok is false for plain ids and "auto".
func ParseSelectionID(value string) (provider, modelID string, ok bool) {
	raw := strings.TrimSpace(value)
	sep := strings.Index(raw, SelectionSeparator)
	if sep <= 0 {
		return "", "", false
	}
	provider = strings.ToLower(strings.TrimSpace(raw[:sep]))
	modelID = strings.TrimSpace(raw[sep+len(SelectionSeparator):])
	if provider == "" || modelID == "" || modelID == "auto" {
		return "", "", false
	}
	return provider, modelID, true
}

// Selectable gives a model its provider-scoped selection id.
func Selectable(m ModelRef) ModelRef {
	provider := strings.ToLower(strings.TrimSpace(m.Provider))
	modelID := m.RawID()
	return ModelRef{ID: SelectionID(provider, modelID), ModelID: modelID, Provider: provider}
}

// ResolveSelection finds the catalog entry a configured value names: first by
// exact selection id, then by raw id (legacy unscoped settings). When several
// providers serve the raw id, preferredProvider wins, else catalog order.
func ResolveSelection(models []ModelRef, value, preferredProvider string) (ModelRef, bool) {
	requested := strings.TrimSpace(value)
	if requested == "" {
		return ModelRef{}, false
	}
	for _, m := range models {
		if m.ID == requested {
			return m, true
		}
	}
	var legacy []ModelRef
	for _, m := range models {
		if m.RawID() == requested {
			legacy = append(legacy, m)
		}
	}
	if len(legacy) == 0 {
		return ModelRef{}, false
	}
	if preferred := strings.ToLower(strings.TrimSpace(preferredProvider)); preferred != "" && len(legacy) > 1 {
		for _, m := range legacy {
			if m.Provider == preferred {
				return m, true
			}
		}
	}
	return legacy[0], true
}

// NormalizeSelections resolves configured values to unique selection ids,
// dropping unknown ones.
func NormalizeSelections(models []ModelRef, values []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, value := range values {
		m, ok := ResolveSelection(models, value, "")
		if !ok || seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		out = append(out, m.ID)
	}
	return out
}

// MatchesConfiguredID reports whether the model is named by selection id or raw id.
func MatchesConfiguredID(m ModelRef, configured map[string]bool) bool {
	return configured != nil && (configured[m.ID] || configured[m.RawID()])
}
