package service

const (
	TiboWelfareGroupName = "GPT-福利"
	TiboHealthyGroupName = "GPT-不降智"
)

// EffectiveModelAllowlist preserves full model access for the healthy group,
// including Luna. Stored selections still enumerate aliases and automatic modes;
// other groups retain their configured allowlists.
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
		return GroupModelAllowlist{Enabled: true, Models: models}
	}
	return g.ModelAllowlist
}
