package service

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResolveIntelligentTestModelDefaultsChatGPTToGpt6Astra(t *testing.T) {
	t.Parallel()
	oauth := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	require.Equal(t, "gpt-6-astra", resolveIntelligentTestModel(oauth, ""))
	require.Equal(t, "gpt-6-astra", resolveIntelligentTestModel(oauth, "  "))
	require.Equal(t, "gpt-5.3-codex-spark", resolveIntelligentTestModel(oauth, "gpt-5.3-codex-spark"))
	require.Equal(t, "", resolveIntelligentTestModel(&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, ""))
	require.Equal(t, "", resolveIntelligentTestModel(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, ""))
}

func TestApplyIntelligentPayloadPromptPinsAstraLowReasoning(t *testing.T) {
	t.Parallel()
	ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "candy"})

	responses := createOpenAITestPayload("gpt-6-astra", true)
	applyIntelligentPayloadPrompt(ctx, responses)
	raw, err := json.Marshal(responses)
	require.NoError(t, err)
	reasoningJSON, err := json.Marshal(responses["reasoning"])
	require.NoError(t, err)
	require.JSONEq(t, `{"effort":"low"}`, string(reasoningJSON))
	require.Contains(t, string(raw), `"text":"candy"`)

	chat := createOpenAIChatCompletionsTestPayload("gpt-6-astra", "hi")
	applyIntelligentPayloadPrompt(ctx, chat)
	require.Equal(t, "low", chat["reasoning_effort"])
	require.Nil(t, chat["reasoning"])

	claude, err := createTestPayload("claude-sonnet-4-5-20250929")
	require.NoError(t, err)
	applyIntelligentPayloadPrompt(ctx, claude)
	_, hasReasoning := claude["reasoning"]
	_, hasEffort := claude["reasoning_effort"]
	require.False(t, hasReasoning)
	require.False(t, hasEffort)

	untouched := createOpenAITestPayload("gpt-6-astra", true)
	applyIntelligentPayloadPrompt(context.Background(), untouched)
	_, hasReasoning = untouched["reasoning"]
	require.False(t, hasReasoning)
	require.Equal(t, "hi", untouched["input"].([]map[string]any)[0]["content"].([]map[string]any)[0]["text"])
}

func TestClassifyIntelligentErrorTreatsCodexPlanGateAsModelError(t *testing.T) {
	t.Parallel()
	msg := `API returned 400: {"detail":"The 'gpt-5.3-codex' model is not supported when using Codex with a ChatGPT account."}`
	require.Equal(t, "model_error", classifyIntelligentError(400, msg))
	require.Equal(t, "model_error", classifyIntelligentError(0, msg))
	require.Equal(t, "request_error", classifyIntelligentError(400, "API returned 400: invalid json"))
}
