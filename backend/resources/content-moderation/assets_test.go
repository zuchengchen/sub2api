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
	require.Len(t, asset.Layer1, 1055)
	require.Len(t, asset.Layer2, 222)
	require.Len(t, asset.Manifest.CandidateEndpoints, 1)
	require.False(t, asset.Manifest.CandidateEndpoints[0].Enabled)
	require.Empty(t, asset.Manifest.CandidateEndpoints[0].Token)
}

func TestLegacyPromptAuditCandidateAssetIncludesRequestedLayer1Keywords(t *testing.T) {
	asset, err := Load(LegacyPromptAuditV1)
	require.NoError(t, err)

	for _, keyword := range []string{
		"制作病毒",
		"mcp__ida",
		"write a virus",
		"remote access trojan",
		"ransomware-as-a-service",
		"production ready exploit",
		"ignore all previous instructions",
		"Forge 可执行见证",
	} {
		require.Contains(t, asset.Layer1, keyword)
	}
}
