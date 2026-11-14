package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesValidationDiagnosticContainsOnlyStructuralMetadata(t *testing.T) {
	const secret = "private-conversation-and-credential-marker"
	body := []byte(`{"input":[{"type":"message","role":"user","content":"` + secret + `"},
		{"type":"reasoning","encrypted_content":"` + secret + `"},
		{"type":"function_call","name":"` + secret + `","arguments":"` + secret + `"},
		{"type":"function_call_output","output":"` + secret + `"},
		{"type":"item_reference","id":"` + secret + `"},
		{"type":"` + secret + `","content":"` + secret + `"}],"previous_response_id":"` + secret + `","` + secret + `":"` + secret + `"}`)
	c := newOpenAIRejectedFieldTestContext(body)
	c.Request.Header.Set("Authorization", "Bearer "+secret)
	c.Set("openai_ws_session_fallback_reason", "payload_too_large")
	response := newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, "")
	response.Header.Set("x-request-id", "req_upstream_validation")
	appendOpenAIResponsesValidationDiagnostic(c, newOpenAIOAuthNamespaceTestAccount(), body,
		[]byte(`{"error":{"message":"Bad Request","type":"invalid_request_error","unknown":"`+secret+`"}}`), response, 2)
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	event := events[0]
	require.Equal(t, "req_upstream_validation", event.UpstreamRequestID)
	require.Equal(t, "unspecified_bad_request", event.Reason)
	require.NotContains(t, event.Detail+event.Message+event.UpstreamResponseBody, secret)
	require.Equal(t, int64(len(body)), gjson.Get(event.Detail, "request_bytes").Int())
	require.Equal(t, int64(6), gjson.Get(event.Detail, "input_items").Int())
	for _, field := range []string{"message_items", "reasoning_items", "tool_call_items", "tool_output_items", "reference_items", "other_items"} {
		require.Equal(t, int64(1), gjson.Get(event.Detail, field).Int(), field)
	}
	require.True(t, gjson.Get(event.Detail, "has_previous_response_id").Bool())
	require.Equal(t, "payload_too_large", gjson.Get(event.Detail, "ws_fallback_reason").String())
	require.Less(t, len(event.Detail), 1024)
}

func TestResponsesValidationDiagnosticGeneric400DoesNotRetry(t *testing.T) {
	body := rejectedMessageIDHistory(t, 1, 0)
	upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusBadRequest,
		`{"error":{"type":"invalid_request_error","message":"Bad Request"}}`)}
	c := newOpenAIRejectedFieldTestContext(body)
	_, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), body)
	require.Error(t, err)
	require.Len(t, upstream.bodies, 1)
	require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.1.id").String())
	events := c.MustGet(OpsUpstreamErrorsKey).([]*OpsUpstreamErrorEvent)
	require.GreaterOrEqual(t, len(events), 1)
	require.Equal(t, "request_validation", events[0].Kind)
	require.Equal(t, "unspecified_bad_request", events[0].Reason)
	require.NotContains(t, events[0].Detail, "Keep the conversation")
	require.False(t, strings.Contains(events[0].Detail, "item_BAD"))
}
