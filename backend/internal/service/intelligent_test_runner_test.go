package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveIntelligentTestModelDefaultsChatGPTToGpt52(t *testing.T) {
	t.Parallel()
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.Equal(t, "gpt-5.2", resolveIntelligentTestModel(oauth, ""))
	require.Equal(t, "gpt-5.2", resolveIntelligentTestModel(oauth, "  "))
	require.Equal(t, "gpt-5.3-codex-spark", resolveIntelligentTestModel(oauth, "gpt-5.3-codex-spark"))
	require.Equal(t, "", resolveIntelligentTestModel(&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, ""))
	require.Equal(t, "", resolveIntelligentTestModel(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, ""))
}

func TestClassifyIntelligentErrorTreatsCodexPlanGateAsModelError(t *testing.T) {
	t.Parallel()
	msg := `API returned 400: {"detail":"The 'gpt-5.3-codex' model is not supported when using Codex with a ChatGPT account."}`
	require.Equal(t, "model_error", classifyIntelligentError(400, msg))
	require.Equal(t, "model_error", classifyIntelligentError(0, msg))
	require.Equal(t, "request_error", classifyIntelligentError(400, "API returned 400: invalid json"))
}
