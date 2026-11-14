package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIMessagesEncryptedRejectionStrictBoundary(t *testing.T) {
	message := "The encrypted content CAQS...GAE= could not be verified. Reason: Encrypted content could not be decrypted or parsed."
	messageJSON, err := json.Marshal(message)
	require.NoError(t, err)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"explicit code", 400, `{"error":{"code":"invalid_encrypted_content"}}`, true},
		{"strict message", 400, `{"type":"error","error":{"type":"invalid_request_error","message":` + string(messageJSON) + `}}`, true},
		{"hydration message", 400, `{"error":{"code":"thinking_signature_invalid","message":"The encrypted content for item rs_1 could not be verified. Reason: encrypted content hydration failed: Encrypted content could not be decrypted or parsed."}}`, true},
		{"wrong status", 500, `{"error":{"code":"invalid_encrypted_content"}}`, false},
		{"unrelated code", 400, `{"error":{"code":"model_not_found","message":` + string(messageJSON) + `}}`, false},
		{"generic signature", 400, `{"error":{"code":"thinking_signature_invalid","message":"Invalid signature"}}`, false},
		{"verification only", 400, `{"error":{"message":"The encrypted content CAQS could not be verified."}}`, false},
		{"echo only", 400, `{"error":{"message":"Bad Request"},"echo":` + string(messageJSON) + `}`, false},
		{"nested message JSON", 400, `{"error":{"message":"{\"error\":{\"code\":\"invalid_encrypted_content\"}}"}}`, false},
		{"wrong error type", 400, `{"error":{"code":"invalid_encrypted_content","type":"authentication_error"}}`, false},
		{"stream terminal", 400, `{"type":"response.failed","error":{"code":"invalid_encrypted_content"}}`, false},
		{"duplicate error", 400, `{"error":{"code":"invalid_encrypted_content"},"error":{"code":"server_error"}}`, false},
		{"duplicate code", 400, `{"error":{"code":"invalid_encrypted_content","code":"server_error"}}`, false},
		{"escaped duplicate code", 400, `{"error":{"code":"invalid_encrypted_content","\u0063ode":"server_error"}}`, false},
		{"duplicate usage", 400, `{"error":{"code":"invalid_encrypted_content"},"usage":{"input_tokens":0,"input_tokens":10}}`, false},
		{"total tokens only", 400, `{"error":{"code":"invalid_encrypted_content"},"usage":{"total_tokens":10}}`, false},
		{"unrecognized usage", 400, `{"error":{"code":"invalid_encrypted_content"},"response":{"usage":{"cost":1}}}`, false},
		{"output", 400, `{"error":{"code":"invalid_encrypted_content"},"response":{"output":[{"type":"message"}]}}`, false},
		{"tool usage", 400, `{"error":{"code":"invalid_encrypted_content"},"tool_usage":{"web_search":1}}`, false},
		{"incomplete JSON", 400, `{"error":{"code":"invalid_encrypted_content"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, openAIMessagesEncryptedRejection(tc.status, []byte(tc.body)))
		})
	}
}

func TestStripOpenAIMessagesRejectedReasoningPreservesWireItems(t *testing.T) {
	toolCall := `{ "type":"function_call", "id":"fc_exact", "call_id":"call_exact", "name":"tool", "arguments":"{\"n\":9007199254740993}" }`
	toolOutput := `{"type":"function_call_output","call_id":"call_exact","output":{"n":9007199254740993,"text":"do not edit"}}`
	message := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"preserve \\ escapes"}],"encrypted_content":"message-cipher"}`
	compaction := `{"type":"compaction","encrypted_content":"compaction-cipher"}`
	body := []byte(`{"model":"gpt-6.1-sol","input":[{"type":"reasoning","encrypted_content":"rejected-only","summary":[]},` + toolCall + `,{"type":"reasoning","encrypted_content":"rejected-summary","summary":[{"type":"summary_text","text":"visible summary"}]},` + toolOutput + `,` + message + `,` + compaction + `],"client_metadata":{"turn_id":"turn-exact"}}`)
	retry, stripped, err := stripOpenAIMessagesRejectedReasoning(body)
	require.NoError(t, err)
	require.Equal(t, 2, stripped)
	require.Len(t, gjson.GetBytes(retry, "input").Array(), 5)
	require.Contains(t, string(retry), toolCall)
	require.Contains(t, string(retry), toolOutput)
	require.Contains(t, string(retry), message)
	require.Contains(t, string(retry), compaction)
	require.Equal(t, "visible summary", gjson.GetBytes(retry, "input.1.summary.0.text").String())
	require.Equal(t, "turn-exact", gjson.GetBytes(retry, "client_metadata.turn_id").String())
	require.NotContains(t, string(retry), "rejected-only")
	require.NotContains(t, string(retry), "rejected-summary")
	require.Contains(t, string(body), "rejected-only", "the original request is immutable")

	second, stripped, err := stripOpenAIMessagesRejectedReasoning(retry)
	require.NoError(t, err)
	require.Zero(t, stripped)
	require.Equal(t, retry, second)
}

func TestStripOpenAIMessagesRejectedReasoningDeclinesAmbiguousReferences(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},{"type":"item_reference","id":"rs_1"}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"message","type":"item_reference","id":"rs_1"}]}`,
		`{"input":[{"type":"reasoning","encrypted_content":"opaque","encrypted_content":"other"}]}`,
		`{"input":[],"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`,
	} {
		retry, stripped, err := stripOpenAIMessagesRejectedReasoning([]byte(body))
		require.NoError(t, err)
		require.Zero(t, stripped)
		require.Equal(t, body, string(retry))
	}

	// An ID or extension is not an empty skeleton and must survive removal.
	retry, stripped, err := stripOpenAIMessagesRejectedReasoning([]byte(`{"input":[{"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque","extension":{"keep":true}}]}`))
	require.NoError(t, err)
	require.Equal(t, 1, stripped)
	require.Equal(t, "rs_1", gjson.GetBytes(retry, "input.0.id").String())
	require.True(t, gjson.GetBytes(retry, "input.0.extension.keep").Bool())
}

func TestOpenAIMessagesEncryptedRecoveryDiagnosticIsContentFree(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Set(openAIMessagesEncryptedRecoveryUsedKey, true)
	account := &Account{ID: 7, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	resp := &http.Response{StatusCode: 400, Header: http.Header{"X-Request-Id": []string{"upstream-request"}}}
	recordOpenAIMessagesEncryptedRecovery(c, account, resp, "request_recovery", 3, 1200, 1000)
	value, exists := c.Get(OpsUpstreamErrorsKey)
	require.True(t, exists)
	events := value.([]*OpsUpstreamErrorEvent)
	require.Len(t, events, 1)
	require.Equal(t, "invalid_encrypted_reasoning", events[0].Reason)
	require.Equal(t, "upstream-request", events[0].UpstreamRequestID)
	require.JSONEq(t, `{"recovery_attempts":1,"stripped_reasoning_items":3,"request_bytes":1200,"retry_bytes":1000,"transport":"http"}`, events[0].Detail)
	require.Empty(t, events[0].UpstreamResponseBody)
	_, err := handleOpenAIMessagesEncryptedRejection(context.Background(), c, account, resp, "The encrypted content CAQS-sensitive-content could not be verified")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "CAQS-sensitive-content")
	value, _ = c.Get(OpsUpstreamErrorsKey)
	raw, marshalErr := json.Marshal(value)
	require.NoError(t, marshalErr)
	require.NotContains(t, string(raw), "CAQS-sensitive-content")
}

func TestOpenAIMessagesEncryptedRejectionDoesNotRewriteCommittedOrCanceledResponse(t *testing.T) {
	for _, state := range []string{"written", "committed", "canceled", "request_canceled"} {
		t.Run(state, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			ctx := context.Background()
			switch state {
			case "written":
				_, _ = c.Writer.Write([]byte("already written"))
			case "committed":
				MarkResponseCommitted(c)
			case "canceled", "request_canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				if state == "canceled" {
					ctx = canceled
				} else {
					c.Request = c.Request.WithContext(canceled)
				}
			}
			original := recorder.Body.String()
			_, err := handleOpenAIMessagesEncryptedRejection(ctx, c, &Account{ID: 1, Platform: PlatformOpenAI}, &http.Response{StatusCode: 400}, "rejected")
			require.Error(t, err)
			require.Equal(t, original, recorder.Body.String())
		})
	}
}
