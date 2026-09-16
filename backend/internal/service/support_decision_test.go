//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSupportDecisionCoversAllLocalPlatforms(t *testing.T) {
	table := BuildSupportDecisionTable(time.Now())
	for _, platform := range supportDecisionPlatforms() {
		_, ok := table.Platforms[platform]
		require.True(t, ok, platform)
	}
	require.Contains(t, table.Platforms, PlatformAnthropic)
	require.Contains(t, table.Platforms, PlatformOpenAI)
	require.Contains(t, table.Platforms, PlatformGrok)
	require.Contains(t, table.Platforms, PlatformKimi)
	require.Contains(t, table.Platforms, PlatformZhipu)
	require.Contains(t, table.Platforms, PlatformDeepseek)
	require.Contains(t, table.Platforms, PlatformMiniMax)
	require.Contains(t, table.Platforms, PlatformOpenCodeGo)
	require.Contains(t, table.Platforms, PlatformComposite)
}

func TestSupportDecisionUnknownFallbackForUnmodeledConstraints(t *testing.T) {
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	reader.Install(BuildSupportDecisionTable(time.Now()))

	for _, query := range []SupportDecisionQuery{
		{Scope: SupportDecisionScope{Platform: PlatformOpenAI}, RequestedModel: "gpt-5.4", RequireCompact: true},
		{Scope: SupportDecisionScope{Platform: PlatformGrok}, RequestedModel: "grok-4.6"},
		{Scope: SupportDecisionScope{Platform: PlatformComposite}, RequestedModel: "routed-model"},
		{Scope: SupportDecisionScope{Platform: PlatformAnthropic}, RequestedModel: "claude-opus-4-6"},
		{Scope: SupportDecisionScope{Platform: PlatformKimi}, RequestedModel: "kimi-k2"},
		{Scope: SupportDecisionScope{Platform: PlatformZhipu}, RequestedModel: "glm-4.6"},
		{Scope: SupportDecisionScope{Platform: PlatformDeepseek}, RequestedModel: "deepseek-chat"},
		{Scope: SupportDecisionScope{Platform: PlatformMiniMax}, RequestedModel: "MiniMax-M2"},
		{Scope: SupportDecisionScope{Platform: PlatformOpenCodeGo}, RequestedModel: "opencode-go"},
		{Scope: SupportDecisionScope{Platform: PlatformOpenAI}, RequestedModel: "gpt-5.4", Transport: OpenAIUpstreamTransportAny},
	} {
		require.Equal(t, SupportDecisionUnknown, reader.Lookup(query), query)
		require.NotEqual(t, SupportDecisionPureMiss, reader.Lookup(query))
	}
}

func TestSupportDecisionStaleReplicaIsUnknown(t *testing.T) {
	reader := NewSupportDecisionAtomicReader(time.Millisecond)
	reader.Install(BuildSupportDecisionTable(time.Now().Add(-time.Second)))
	require.Equal(t, SupportDecisionUnknown, reader.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI},
		RequestedModel: "gpt-5.4",
	}))
}

func TestSupportDecisionMissingTableIsUnknown(t *testing.T) {
	reader := NewSupportDecisionAtomicReader(30 * time.Second)
	require.Equal(t, SupportDecisionUnknown, reader.Lookup(SupportDecisionQuery{
		Scope:          SupportDecisionScope{Platform: PlatformOpenAI},
		RequestedModel: "gpt-5.4",
	}))
}
