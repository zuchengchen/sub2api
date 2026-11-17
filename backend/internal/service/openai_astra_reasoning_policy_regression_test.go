//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCanonicalRequestedReasoningEffortPreservesDisabled(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"nested none", `{"model":"gpt-6-astra","reasoning":{"effort":"none"}}`, "none"},
		{"flat none", `{"model":"gpt-6-astra","reasoning_effort":" NONE "}`, "none"},
		{"messages disabled", `{"model":"gpt-6-astra","thinking":{"type":"disabled"}}`, "none"},
		{"messages disabled takes precedence", `{"model":"gpt-6-astra","thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, "none"},
		{"messages explicit none", `{"model":"gpt-6-astra","output_config":{"effort":"none"}}`, "none"},
		{"suffix none", `{"model":"gpt-6-astra-none"}`, "none"},
		{"explicit high", `{"model":"gpt-6-astra-none","reasoning":{"effort":"high"}}`, "high"},
		{"missing effort", `{"model":"gpt-6-astra"}`, ""},
		{"enabled without effort", `{"model":"gpt-6-astra","thinking":{"type":"enabled"}}`, ""},
		{"unknown effort", `{"model":"gpt-6-astra-high","reasoning":{"effort":"future"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := []byte(tc.body)
			got := CanonicalRequestedReasoningEffort(original)
			if tc.want == "" {
				require.Nil(t, got)
			} else {
				require.NotNil(t, got)
				require.Equal(t, tc.want, *got)
			}
			require.Equal(t, tc.body, string(original))
		})
	}
	// Capturing a client's explicit disable must not make "none" a valid ceiling.
	require.Empty(t, NormalizeMaxReasoningEffort("none"))
}

func TestMessagesAstraReasoningPolicyForward(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, model, options, want string
			configured                 bool
		}{
			{"disabled mapped", "gpt-6-astra", `,"thinking":{"type":"disabled"}`, "low", true},
			{"disabled overrides high", "gpt-6-astra", `,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}`, "low", true},
			{"legal high preserved", "gpt-6-astra", `,"output_config":{"effort":"high"}`, "high", true},
			{"mapping must be configured", "gpt-6-astra", `,"thinking":{"type":"disabled"}`, "none", false},
			{"other model unchanged", "gpt-6-luna", `,"thinking":{"type":"disabled"}`, "none", true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":%q,"max_tokens":32,"stream":%t,"system":"Keep the fixture intact.","messages":[{"role":"user","content":"Read the fixture."},{"role":"assistant","content":[{"type":"tool_use","id":"toolu_astra_fixture","name":"Read","input":{"path":"fixture.txt"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_astra_fixture","content":"Original tool output."}]}],"tools":[{"name":"Read","description":"Read fixture","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}]%s}`, tc.model, stream, tc.options))
				original := bytes.Clone(body)
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				ctx := context.Background()
				requested := CanonicalRequestedReasoningEffort(body)
				require.NotNil(t, requested)
				ctx = WithRequestedReasoningEffort(ctx, *requested)
				if tc.configured {
					ctx = WithOpenAIReasoningEffortPolicy(ctx, "", []ReasoningEffortMapping{
						{From: "none", To: "low", MatchType: "exact", Model: "gpt-6-astra"},
					}, "")
				}
				c.Request = c.Request.WithContext(ctx)
				upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_astra_policy_fixture", tc.model)}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
				result, err := svc.ForwardAsAnthropic(ctx, c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
				if !tc.configured {
					require.Error(t, err)
					require.Nil(t, result)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "reasoning.effort", gjson.Get(recorder.Body.String(), "error.param").String())
					require.Empty(t, upstream.bodies)
					require.Equal(t, original, body)
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1, "mapping happens before sending and must not add a retry")
				forwarded := upstream.bodies[0]
				require.Equal(t, tc.model, gjson.GetBytes(forwarded, "model").String())
				require.Equal(t, tc.want, gjson.GetBytes(forwarded, "reasoning.effort").String())
				require.NotNil(t, result.ReasoningEffort)
				require.Equal(t, tc.want, *result.ReasoningEffort)
				require.Equal(t, *requested, *RequestedReasoningEffortFromContext(c.Request.Context()), "the handler must still be able to record the original preference")
				require.Equal(t, original, body)
				require.Equal(t, "toolu_astra_fixture", gjson.GetBytes(forwarded, `input.#(type=="function_call").call_id`).String())
				require.Equal(t, "toolu_astra_fixture", gjson.GetBytes(forwarded, `input.#(type=="function_call_output").call_id`).String())
				require.Equal(t, "Read", gjson.GetBytes(forwarded, `tools.#(type=="function").name`).String())
				require.Contains(t, string(forwarded), "Original tool output.")
				require.Contains(t, recorder.Body.String(), "ok")
			})
		}
	}
}

func TestMessagesAstraMappingDoesNotBypassOtherModelValidation(t *testing.T) {
	body := []byte(`{"model":"gpt-6.1-sol","max_tokens":32,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"fixture"}]}`)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	ctx := WithOpenAIReasoningEffortPolicy(context.Background(), "", []ReasoningEffortMapping{
		{From: "none", To: "low", MatchType: "exact", Model: "gpt-6-astra"},
	}, "")
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
	_, err := svc.ForwardAsAnthropic(ctx, c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
	require.ErrorContains(t, err, `does not support reasoning effort "none"`)
	require.Empty(t, upstream.bodies)
}

func TestCanonicalRequestedReasoningEffortMapPreservesNone(t *testing.T) {
	body := []byte(`{"model":"gpt-6-astra","reasoning":{"effort":"none"}}`)
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	got := CanonicalRequestedReasoningEffortFromReqBody(decoded)
	require.NotNil(t, got)
	require.Equal(t, "none", *got)
}
