//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const validationForwardPrivateMarker = "private-validation-forward-fixture"
const validationForwardGeneric400 = `{"error":{"type":"invalid_request_error","message":"Bad Request"}}`

func validationForwardEvents(t *testing.T, c *gin.Context) []*OpsUpstreamErrorEvent {
	t.Helper()
	value, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events, ok := value.([]*OpsUpstreamErrorEvent)
	require.True(t, ok)
	var validation []*OpsUpstreamErrorEvent
	for _, event := range events {
		if event.Kind == "request_validation" {
			validation = append(validation, event)
		}
	}
	return validation
}

func validationForwardSummary(t *testing.T, event *OpsUpstreamErrorEvent) openAIResponsesValidationSummary {
	t.Helper()
	var summary openAIResponsesValidationSummary
	require.NoError(t, json.Unmarshal([]byte(event.Detail), &summary))
	require.Equal(t, "responses_json", summary.RequestBytesBasis)
	require.Equal(t, "http", summary.Transport)
	require.Equal(t, http.StatusBadRequest, event.UpstreamStatusCode)
	require.Equal(t, "forward", event.Stage)
	require.NotContains(t, event.Detail+event.Message, validationForwardPrivateMarker)
	require.LessOrEqual(t, len(event.Detail), openAIResponsesValidationMaxDetailBytes)
	return summary
}

func TestOpenAIValidationDiagnosticForwardSources(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	for _, tc := range []struct {
		name, path, body, format, source string
		lite, search, history            bool
		conflict                         bool
	}{
		{
			name: "responses JSON omits implicit search", path: "/v1/responses",
			body:   `{"model":"gpt-6-luna","stream":false,"input":[{"role":"user","content":"` + validationForwardPrivateMarker + `"}],"text":{"format":{"type":"json_object"}}}`,
			format: "json_object",
		},
		{
			name: "responses text records synthesis", path: "/v1/responses",
			body:   `{"model":"gpt-6-luna","stream":false,"input":[{"role":"user","content":"` + validationForwardPrivateMarker + `"}],"text":{"format":{"type":"text"}}}`,
			format: "text", source: "synthesis", search: true,
		},
		{
			name: "responses JSON history records repair", path: "/v1/responses",
			body:   `{"model":"gpt-6-luna","stream":false,"input":[{"role":"user","content":"` + validationForwardPrivateMarker + `"},{"type":"web_search_call","id":"ws_fixture","status":"completed","action":{"type":"search","query":"` + validationForwardPrivateMarker + `"}}],"tools":[],"text":{"format":{"type":"json_object"}}}`,
			format: "json_object", source: "history", search: true, history: true,
		},
		{
			name: "lite JSON records client additional tools", path: "/v1/responses", lite: true,
			body:   `{"model":"gpt-6-luna","stream":false,"input":[{"role":"user","content":"` + validationForwardPrivateMarker + `"},{"type":"additional_tools","role":"developer","tools":[{"type":"web_search"}]}],"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}}}}`,
			format: "json_schema", source: "client", search: true,
		},
		{
			name: "chat JSON records explicit search conflict", path: "/v1/chat/completions",
			body:   `{"model":"gpt-6-luna","stream":false,"messages":[{"role":"user","content":"` + validationForwardPrivateMarker + ` JSON"}],"response_format":{"type":"json_object"},"tools":[{"type":"web_search"}]}`,
			format: "json_object", source: "client", search: true, conflict: true,
		},
		{
			name: "messages records synthesis", path: "/v1/messages",
			body:   `{"model":"gpt-6-luna","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"` + validationForwardPrivateMarker + `"}]}`,
			format: "absent", source: "synthesis", search: true,
		},
		{
			name: "messages records client search", path: "/v1/messages",
			body:   `{"model":"gpt-6-luna","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"` + validationForwardPrivateMarker + `"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`,
			format: "absent", source: "client", search: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, tc.path, bytes.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Request.Header.Set("User-Agent", "AsyncOpenAI/Python 3.42.1")
			if tc.lite {
				c.Request.Header.Set(responsesLiteHeader, "true")
			}
			rejection, reason := validationForwardGeneric400, "unspecified_bad_request"
			if tc.conflict {
				rejection = `{"error":{"type":"invalid_request_error","message":"Web Search cannot be used with JSON mode."}}`
				reason = "json_search_conflict"
			}
			upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, rejection)}
			cfg := codexSynthEnabledConfig()
			cfg.Security.URLAllowlist.Enabled = false
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tiboRouteDisabled: true}
			account := newCodexSynthTestAccount(7811, AccountTypeOAuth)
			var err error
			switch tc.path {
			case "/v1/chat/completions":
				_, err = svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-6-luna")
			case "/v1/messages":
				_, err = svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-6-luna")
			default:
				_, err = svc.Forward(context.Background(), c, account, body)
			}
			require.Error(t, err)
			if tc.lite {
				require.Empty(t, upstream.bodies, "known unsupported Lite declarations fail locally")
				require.Equal(t, http.StatusBadRequest, rec.Code)
				value, ok := c.Get(openAIFinalProtocolDiagnosticKey)
				require.True(t, ok)
				diagnostic := value.(openAIFinalProtocolDiagnostic)
				require.True(t, diagnostic.FinalLite)
				require.Equal(t, []string{"client"}, diagnostic.ToolSources)
				require.Equal(t, []string{"additional_tools"}, diagnostic.ToolCarriers)
				return
			}
			require.Len(t, upstream.bodies, 1, "validation diagnostics must not add an upstream retry")
			require.Equal(t, http.StatusBadRequest, rec.Code)
			events := validationForwardEvents(t, c)
			require.Len(t, events, 1, "one diagnostic per rejected upstream attempt")
			require.Equal(t, reason, events[0].Reason)
			require.Equal(t, account.ID, events[0].AccountID)
			summary := validationForwardSummary(t, events[0])
			require.Equal(t, 1, summary.Attempt)
			require.Equal(t, len(rejection), summary.ResponseBytes)
			require.Equal(t, tc.format, summary.TextFormat)
			require.Equal(t, tc.search, summary.SearchPresent)
			require.Equal(t, tc.history, summary.SearchHistoryPresent)
			require.Equal(t, tc.search && !tc.lite, summary.SearchTopLevel)
			require.Equal(t, tc.search && tc.lite, summary.SearchAdditionalTools)
			expectedSources := []string{}
			if tc.source != "" {
				expectedSources = append(expectedSources, tc.source)
			}
			require.Equal(t, expectedSources, summary.SearchSources)
			require.Equal(t, len(gjson.GetBytes(upstream.bodies[0], "input").Array()), summary.InputItems)
		})
	}
}

func TestOpenAIValidationDiagnosticResetsSearchSourceAcrossFailover(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	body := []byte(`{"model":"gpt-6-luna","stream":false,"input":[{"role":"user","content":"` + validationForwardPrivateMarker + `"}],"text":{"format":{"type":"text"}}}`)
	c := newOpenAIRejectedFieldTestContext(body)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusServiceUnavailable, `{"error":{"message":"Service unavailable"}}`),
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, validationForwardGeneric400),
	}}
	cfg := codexSynthEnabledConfig()
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, tiboRouteDisabled: true}
	first := newCodexSynthTestAccount(7812, AccountTypeOAuth)
	second := newCodexSynthTestAccount(7813, AccountTypeOAuth)
	second.Extra[codexClientCapabilityExtraKey] = "compatible"

	_, err := svc.Forward(context.Background(), c, first, body)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.False(t, c.Writer.Written(), "account failover happens before client output")
	_, err = svc.Forward(context.Background(), c, second, body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 2)
	require.True(t, gjson.GetBytes(upstream.bodies[0], `tools.#(type=="web_search")`).Exists())
	require.False(t, gjson.GetBytes(upstream.bodies[1], `tools.#(type=="web_search")`).Exists())
	events := validationForwardEvents(t, c)
	require.Len(t, events, 1)
	require.Equal(t, second.ID, events[0].AccountID)
	summary := validationForwardSummary(t, events[0])
	require.Equal(t, 1, summary.Attempt, "attempt numbering restarts for the selected account")
	require.Equal(t, "text", summary.TextFormat)
	require.False(t, summary.SearchPresent)
	require.Empty(t, summary.SearchSources, "the first account's synthesis flag must not leak into the next account")
}

func TestOpenAIValidationDiagnosticMessagesRecoveryUsesRetriedBody(t *testing.T) {
	body := messagesEncryptedRecoveryBody(false, true)
	svc, account, c, upstream := messagesEncryptedRecoveryFixture(body, http.StatusBadRequest,
		messagesEncryptedRecoveryError("invalid_encrypted_content", messagesEncryptedRecoveryMessage))
	upstream.responses[1] = newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, validationForwardGeneric400)
	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "synthetic-stable-cache", "")
	require.Error(t, err)
	requireMessagesEncryptedReplayPreserved(t, upstream, account)
	events := validationForwardEvents(t, c)
	require.Len(t, events, 1, "the encrypted error has its recovery event; only the second generic 400 gets a validation summary")
	require.Equal(t, "unspecified_bad_request", events[0].Reason)
	summary := validationForwardSummary(t, events[0])
	first, retried := upstream.bodies[0], upstream.bodies[1]
	require.Equal(t, 2, summary.Attempt)
	require.Equal(t, len(retried), summary.RequestBytes)
	require.NotEqual(t, len(first), summary.RequestBytes)
	require.Equal(t, 0, summary.ReasoningItems)
	require.NotContains(t, summary.InputTypes, "reasoning")
	require.Equal(t, len(gjson.GetBytes(retried, "input").Array()), summary.InputItems)
	require.Equal(t, len(gjson.GetBytes(first, "input").Array())-1, summary.InputItems)
	require.Equal(t, 1, summary.ToolCallItems)
	require.Equal(t, 1, summary.ToolOutputItems)
	require.True(t, summary.ToolPairingComplete)
	require.Equal(t, 0, summary.UnmatchedToolCalls)
	require.Equal(t, 0, summary.UnmatchedToolOutputs)
	require.NotContains(t, events[0].Detail, messagesEncryptedRecoveryCipher)
}
