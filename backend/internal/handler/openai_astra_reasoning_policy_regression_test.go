package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestMessagesAstraReasoningPolicyBindsExplicitDisable(t *testing.T) {
	for _, tc := range []struct {
		name, model, options, wantRequested, wantForwarded string
	}{
		{"disabled without output config", "gpt-6-astra", `,"thinking":{"type":"disabled"}`, "none", "low"},
		{"disabled takes precedence over effort", "gpt-6-astra", `,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`, "none", "low"},
		{"disabled with null effort", "gpt-6-astra", `,"thinking":{"type":"disabled"},"output_config":{"effort":null}`, "none", "low"},
		{"explicit none", "gpt-6-astra", `,"output_config":{"effort":"none"}`, "none", "low"},
		{"explicit high retained", "gpt-6-astra", `,"output_config":{"effort":"high"}`, "high", "high"},
		{"omitted does not bind synthesized default", "gpt-6-astra", ``, "", "medium"},
		{"enabled does not bind synthesized default", "gpt-6-astra", `,"thinking":{"type":"enabled","budget_tokens":1024}`, "", "medium"},
		{"unmatched model retains disabled", "gpt-6-luna", `,"thinking":{"type":"disabled"}`, "none", "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"` + tc.model + `","max_tokens":32,"messages":[{"role":"user","content":"fixture"}]` + tc.options + `}`)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			apiKey := &service.APIKey{Group: &service.Group{
				Platform: service.PlatformOpenAI,
				ReasoningEffortMappings: []service.ReasoningEffortMapping{
					{From: "none", To: "low", MatchType: "exact", Model: "gpt-6-astra"},
					// Detect accidental binding of the bridge's implicit medium default.
					{From: "medium", To: "low", MatchType: "exact", Model: "gpt-6-astra"},
				},
			}}
			bindOpenAIReasoningEffortPolicyForMessagesRequest(c, apiKey, body)

			var inbound apicompat.AnthropicRequest
			require.NoError(t, json.Unmarshal(body, &inbound))
			converted, err := apicompat.AnthropicToResponses(&inbound)
			require.NoError(t, err)
			convertedBody, err := json.Marshal(converted)
			require.NoError(t, err)
			forwarded, _, err := service.ApplyOpenAIReasoningEffortPolicyFromContext(c.Request.Context(), convertedBody)
			require.NoError(t, err)
			require.Equal(t, tc.wantForwarded, gjson.GetBytes(forwarded, "reasoning.effort").String())

			effective := tc.wantForwarded
			result := &service.OpenAIForwardResult{ReasoningEffort: &effective}
			stampOpenAIRequestedReasoningEffort(result, c)
			if tc.wantRequested == "" {
				require.Nil(t, result.RequestedReasoningEffort)
			} else {
				require.NotNil(t, result.RequestedReasoningEffort)
				require.Equal(t, tc.wantRequested, *result.RequestedReasoningEffort)
			}
			require.Equal(t, tc.wantForwarded, *result.ReasoningEffort, "recording requested effort must not replace effective effort")
		})
	}
}

func TestMessagesAstraReasoningPolicyRespectsCompositeTarget(t *testing.T) {
	for _, platform := range []string{service.PlatformOpenAI, service.PlatformGrok} {
		t.Run(platform, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Request = c.Request.WithContext(service.WithResolvedTargetPlatform(c.Request.Context(), platform))
			apiKey := &service.APIKey{Group: &service.Group{
				Platform: service.PlatformComposite,
				ReasoningEffortMappings: []service.ReasoningEffortMapping{
					{From: "none", To: "low", MatchType: "exact", Model: "gpt-6-astra"},
				},
			}}
			bindOpenAIReasoningEffortPolicyForMessagesRequest(c, apiKey, []byte(`{"model":"gpt-6-astra","thinking":{"type":"disabled"}}`))
			converted := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"none"}}`)
			forwarded, changed, err := service.ApplyOpenAIReasoningEffortPolicyFromContext(c.Request.Context(), converted)
			require.NoError(t, err)
			if platform == service.PlatformOpenAI {
				require.True(t, changed)
				require.Equal(t, "low", gjson.GetBytes(forwarded, "reasoning.effort").String())
			} else {
				require.False(t, changed)
				require.Equal(t, converted, forwarded)
			}
		})
	}
}
