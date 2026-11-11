package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTiboHealthyGroupModelPolicy(t *testing.T) {
	group := &Group{Name: TiboHealthyGroupName, Platform: PlatformOpenAI,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"gpt-6-luna", "public-one", "codex-auto-review"}}}
	policy := group.EffectiveModelAllowlist()
	luna := []string{"luna", "openai/LUNA-max", "gpt-5.6-luna", "openai/GPT5.6LUNA-max", "gpt-6-luna", "provider/gpt6luna-high", "gpt-6-luna-2026-09-01", "gpt-5.6-lunatic", "gpt-5.6-lunaevil"}
	for _, model := range luna {
		require.True(t, policy.Allows(model), model)
	}
	allowed := []string{"gpt-6-astra", "gpt-6-sol", "gpt-6.1-sol", "gpt-6.1-terra", "gpt-image-2", "gpt-6-lunatic", "public-one"}
	for _, model := range allowed {
		require.True(t, policy.Allows(model), model)
	}
	all := append(append([]string{}, luna...), allowed...)
	require.ElementsMatch(t, all, policy.FilterForListing(all))
	require.Equal(t, []string{"gpt-6-luna", "public-one", "codex-auto-review"}, group.ModelAllowlist.Models, "stored settings remain intact")
	require.Equal(t, []string{"codex-auto-review", "gpt-6-luna"}, FilterCodexModelIDsForGroup([]string{"codex-auto-review", "gpt-6-luna"}, group))
	group.Name = TiboWelfareGroupName
	require.True(t, group.EffectiveModelAllowlist().Allows("gpt-6-luna"))
	require.False(t, group.EffectiveModelAllowlist().Allows("gpt-6-astra"), "other groups retain their allowlist")
	group.Name, group.Platform = TiboHealthyGroupName, PlatformAnthropic
	require.True(t, group.EffectiveModelAllowlist().Allows("gpt-6-luna"))
}

func TestTiboHealthyGroupModelManifestAndETag(t *testing.T) {
	group := &Group{ID: 991, Name: TiboHealthyGroupName, Platform: PlatformOpenAI,
		ModelAllowlist: GroupModelAllowlist{Enabled: true, Models: []string{"public-one"}}}
	account := Account{ID: 992, Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Credentials: map[string]any{
		"model_mapping": map[string]any{"gpt-6-sol": "gpt-6-sol", "gpt-6-luna": "gpt-6-luna", "public-*": "gpt-6-sol"},
	}}
	svc := &OpenAIGatewayService{accountRepo: codexModelsVisibilityAccountRepo{byGroup: map[int64][]Account{group.ID: {account}}}}
	manifest, configured, err := svc.BuildGroupConfiguredCodexModelsManifest(context.Background(), group, "")
	require.NoError(t, err)
	require.True(t, configured)
	require.ElementsMatch(t, []string{"gpt-6-sol", "gpt-6-luna", "public-one"}, codexManifestModelSlugs(t, manifest.Body))
	next, _, err := svc.BuildGroupConfiguredCodexModelsManifest(context.Background(), group, manifest.ETag)
	require.NoError(t, err)
	require.True(t, next.NotModified)
	// A catalog cached while Luna was excluded must be replaced with the full catalog.
	source := []byte(`{"models":[{"slug":"gpt-6-luna"},{"slug":"gpt-6-sol","custom":"preserved"}],"extra":true}`)
	oldTag := codexModelsManifestBodyETag([]byte(`{"models":[{"slug":"gpt-6-sol","custom":"preserved"}],"extra":true}`))
	group.CodexModelsManifestConfig.Enabled = true
	response := &OpenAIModelsResponse{Body: append([]byte(nil), source...), ETag: oldTag}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, response, oldTag))
	require.False(t, response.NotModified)
	require.Equal(t, []string{"gpt-6-luna", "gpt-6-sol"}, codexManifestModelSlugs(t, response.Body))
	require.Contains(t, string(response.Body), `"custom":"preserved"`)
	require.Contains(t, string(response.Body), `"extra":true`)
	require.NotEqual(t, oldTag, response.ETag)
	require.Contains(t, string(source), "gpt-6-luna")
	second := &OpenAIModelsResponse{Body: append([]byte(nil), source...), ETag: oldTag}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, second, response.ETag))
	require.True(t, second.NotModified)
	group.Name, group.ModelAllowlist = TiboWelfareGroupName, GroupModelAllowlist{}
	other := &OpenAIModelsResponse{Body: append([]byte(nil), source...), ETag: oldTag}
	require.NoError(t, svc.MergeGroupConfiguredCodexModels(context.Background(), group, other, ""))
	require.Contains(t, codexManifestModelSlugs(t, other.Body), "gpt-6-luna")
}
