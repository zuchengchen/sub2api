package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
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
	require.Equal(t, "account_error", classifyIntelligentError(0, ErrOpenAICodexTicketUnavailable.Error()))
}

func TestApplyIntelligentTestOpenAICodexTicketMatchesGatewayInjection(t *testing.T) {
	t.Parallel()
	state := fakeCodexTicketState(292)
	gateway := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		TTLSeconds:   3600,
		FailClosed:   true,
	}, nil)
	account := ticketTestAccount(41)
	gateway.storeOpenAICodexTicket(context.Background(), account, &openAICodexTicket{
		AccountID:  41,
		Model:      "gpt-6-astra",
		State:      state,
		Length:     292,
		CapturedAt: time.Now(),
		ExpiresAt:  time.Now().Add(time.Hour),
	})
	svc := &AccountTestService{openaiGatewayService: gateway}
	body := []byte(`{"model":"gpt-6-astra"}`)
	intelligentCtx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "pelican"})

	injected := http.Header{}
	require.NoError(t, svc.applyIntelligentTestOpenAICodexTicket(intelligentCtx, account, body, injected))
	require.Equal(t, state, injected.Get(openAICodexTurnStateHeader))
	require.Equal(t, 292, len(injected.Get(openAICodexTurnStateHeader)))

	plain := http.Header{}
	require.NoError(t, svc.applyIntelligentTestOpenAICodexTicket(context.Background(), account, body, plain))
	require.Empty(t, plain.Get(openAICodexTurnStateHeader))
}

func TestApplyIntelligentTestOpenAICodexTicketFailClosedWithoutTicket(t *testing.T) {
	t.Parallel()
	gateway := ticketTestService(t, config.OpenAICodexTicketConfig{
		Enabled:      true,
		TargetLength: 292,
		FailClosed:   true,
	}, nil)
	svc := &AccountTestService{openaiGatewayService: gateway}
	ctx := context.WithValue(context.Background(), intelligentRunKey{}, &intelligentRunContext{prompt: "pelican"})
	err := svc.applyIntelligentTestOpenAICodexTicket(ctx, ticketTestAccount(41), []byte(`{"model":"gpt-6-astra"}`), http.Header{})
	require.ErrorIs(t, err, ErrOpenAICodexTicketUnavailable)
}
