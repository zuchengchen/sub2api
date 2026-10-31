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
	require.Equal(t, "gpt-5.3-codex", resolveIntelligentTestModel(oauth, "gpt-5.3-codex"))
	require.Equal(t, "", resolveIntelligentTestModel(&Account{Platform: PlatformAnthropic, Type: AccountTypeOAuth}, ""))
	require.Equal(t, "", resolveIntelligentTestModel(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}, ""))
}

func TestPelicanUsesHTTPTransport(t *testing.T) {
	t.Parallel()
	pelican := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{testType: "pelican"})
	candy := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{testType: "candy"})
	require.True(t, pelicanUsesHTTPTransport(pelican, true))
	require.False(t, pelicanUsesHTTPTransport(pelican, false))
	require.False(t, pelicanUsesHTTPTransport(candy, true))
	require.False(t, pelicanUsesHTTPTransport(context.Background(), true))
}

func TestApplyIntelligentPayloadPromptPinsReasoningByTestType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		testType string
		effort   string
	}{
		{"tibo", "xhigh"},
		{"candy", "low"},
		{"pelican", "low"},
		{"custom", "low"},
		{"", "low"},
	} {
		t.Run(tc.testType, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "test prompt", testType: tc.testType})

			responses := createOpenAITestPayload("gpt-6-astra", true)
			applyIntelligentPayloadPrompt(ctx, responses)
			raw, err := json.Marshal(responses)
			require.NoError(t, err)
			require.Equal(t, "gpt-6-astra", responses["model"])
			require.Equal(t, map[string]any{"effort": tc.effort}, responses["reasoning"])
			require.Contains(t, string(raw), `"text":"test prompt"`)

			chat := createOpenAIChatCompletionsTestPayload("gpt-6-astra", "hi")
			applyIntelligentPayloadPrompt(ctx, chat)
			require.Equal(t, "gpt-6-astra", chat["model"])
			require.Equal(t, tc.effort, chat["reasoning_effort"])
			require.Nil(t, chat["reasoning"])

			claude, err := createTestPayload("claude-sonnet-4-5-20250929")
			require.NoError(t, err)
			applyIntelligentPayloadPrompt(ctx, claude)
			require.NotContains(t, claude, "reasoning")
			require.NotContains(t, claude, "reasoning_effort")
		})
	}

	untouched := createOpenAITestPayload("gpt-6-astra", true)
	applyIntelligentPayloadPrompt(context.Background(), untouched)
	_, hasReasoning := untouched["reasoning"]
	require.False(t, hasReasoning)
	input := mustTestValue[[]map[string]any](t, untouched["input"])
	content := mustTestValue[[]map[string]any](t, input[0]["content"])
	require.Equal(t, "hi", content[0]["text"])
}

func TestClassifyIntelligentErrorTreatsCodexPlanGateAsModelError(t *testing.T) {
	t.Parallel()
	msg := `API returned 400: {"detail":"The 'gpt-5.3-codex' model is not supported when using Codex with a ChatGPT account."}`
	require.Equal(t, "model_error", classifyIntelligentError(400, msg))
	require.Equal(t, "model_error", classifyIntelligentError(0, msg))
	require.Equal(t, "request_error", classifyIntelligentError(400, "API returned 400: invalid json"))
	require.Equal(t, "account_error", classifyIntelligentError(0, "account not found"))
}
