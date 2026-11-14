package service

import (
	"encoding/json"
	"strings"
)

const (
	TiboWelfareGroupName = "GPT-福利"
	TiboHealthyGroupName = "GPT-不降智"
)

// EffectiveModelAllowlist applies the customized policy to the request group,
// never to an account shared with other groups. Keep stored admin settings
// intact so renaming the group or reverting the policy restores those settings.
func (g *Group) EffectiveModelAllowlist() GroupModelAllowlist {
	if g == nil {
		return GroupModelAllowlist{}
	}
	if g.Platform == PlatformOpenAI && g.Name == TiboHealthyGroupName {
		models := []string{"*"}
		if g.ModelAllowlist.Enabled {
			// Exact selections also enumerate aliases behind wildcard account
			// mappings and opt in to Codex automatic modes.
			models = append(models, g.ModelAllowlist.Models...)
		}
		return GroupModelAllowlist{Enabled: true, Models: models, excludeLuna: true}
	}
	return g.ModelAllowlist
}

// Match public spellings and the identity used by OAuth forwarding. Keep the
// group's historical GPT-5.6 Luna prefix exclusion here: legacy spellings such
// as gpt-5.6-lunatic stay blocked without globally rewriting unknown models.
// Image requests keep their public image model; their internal Luna driver does
// not pass through this client-facing group policy.
func isTiboLunaModel(model string) bool {
	name := normalizeVIPModelCandidate(model)
	return name == "luna" || strings.HasPrefix(name, "luna-") ||
		strings.HasPrefix(name, VipExclusiveModelName) ||
		isLunaMinRateModel(model) || isLunaMinRateModel(normalizeKnownOpenAICodexModel(model))
}

// Apply to a private response body, before computing the client ETag. The raw
// upstream cache is shared by groups and must retain its unfiltered catalog.
func filterTiboGroupModelsBody(body []byte, group *Group) ([]byte, bool, error) {
	if !group.EffectiveModelAllowlist().excludeLuna {
		return body, false, nil
	}
	envelope, entries, err := modelCatalogEntries(body, "models")
	if err != nil {
		return nil, false, err
	}
	filtered := make([]json.RawMessage, 0, len(entries))
	for _, raw := range entries {
		var entry struct {
			Slug string `json:"slug"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return nil, false, err
		}
		if !isTiboLunaModel(entry.Slug) {
			filtered = append(filtered, raw)
		}
	}
	if len(filtered) == len(entries) {
		return body, false, nil
	}
	envelope["models"], err = json.Marshal(filtered)
	if err != nil {
		return nil, false, err
	}
	result, err := json.Marshal(envelope)
	return result, true, err
}
