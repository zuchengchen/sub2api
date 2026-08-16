package contentmoderationassets

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLegacyPromptAuditCandidateAsset(t *testing.T) {
	asset, err := Load(LegacyPromptAuditV1)
	require.NoError(t, err)
	require.False(t, asset.Manifest.EnabledByDefault)
	require.Equal(t, "99c8e4bf7564823bafbab369acab6539e734c1bb", asset.Manifest.SourceCommit)
	require.Equal(t, "layer1-high-confidence-keywords.json", asset.Manifest.Layer1.File)
	require.Equal(t, "layer2-candidate-keywords.json", asset.Manifest.Layer2.File)
	require.Len(t, asset.Layer1, 964)
	require.Len(t, asset.Layer2, 254)
	require.Len(t, asset.Layer1Demotions, 63)
	require.Len(t, asset.Layer1Suppressions, 28)
	require.Len(t, asset.Manifest.CandidateEndpoints, 1)
	require.False(t, asset.Manifest.CandidateEndpoints[0].Enabled)
	require.Empty(t, asset.Manifest.CandidateEndpoints[0].Token)
}

func TestLegacyPromptAuditCandidateAssetAppliesLayerAssignments(t *testing.T) {
	asset, err := Load(LegacyPromptAuditV1)
	require.NoError(t, err)

	for _, keyword := range []string{
		"制作病毒",
		"write a virus",
		"ransomware-as-a-service",
		"production ready exploit",
		"ignore all previous instructions",
		"Forge 可执行见证",
	} {
		require.Contains(t, asset.Layer1, keyword)
	}

	layer2 := make(map[string]struct{}, len(asset.Layer2))
	for _, keyword := range asset.Layer2 {
		layer2[candidatePrefilterKey(keyword)] = struct{}{}
	}
	demotions := make(map[string]struct{}, len(asset.Layer1Demotions))
	for _, keyword := range asset.Layer1Demotions {
		demotions[normalizedCandidateTerm(keyword)] = struct{}{}
	}
	for _, keyword := range []string{
		"mcp__ida",
		"remote access trojan",
		"降级攻击",
		"obfuscator",
		"firmware reverse engineering",
		"pass-the-hash",
		"zero_day",
		"destructive payload",
		"metamorphic",
		"do anything now",
		"make a bomb",
		"malicious payload",
		"session hijacking",
		"opposite mode",
		"bypass content filter",
	} {
		require.NotContains(t, asset.Layer1, keyword)
		require.Contains(t, layer2, candidatePrefilterKey(keyword))
	}
	for _, keyword := range []string{
		"metamorphic",
		"do anything now",
		"make a bomb",
		"malicious payload",
		"session hijacking",
		"opposite mode",
		"bypass content filter",
	} {
		require.Contains(t, demotions, normalizedCandidateTerm(keyword))
	}

	for _, keyword := range []string{"hax0r", "sh3ll", "sp00f", "zer0day"} {
		require.NotContains(t, asset.Layer1, keyword)
		require.NotContains(t, layer2, candidatePrefilterKey(keyword))
	}
}
