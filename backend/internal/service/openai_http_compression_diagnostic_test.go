package service

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHTTPCompressionPolicyValidationDiagnostic(t *testing.T) {
	const body = `{"model":"gpt-6-astra","input":"PRIVATE_BODY_MARKER"}`
	for _, tc := range []struct {
		name, policy, encoding string
		enabled, eligible      bool
	}{
		{name: "default plain", eligible: true, policy: "disabled", encoding: "identity"},
		{name: "explicit compression", eligible: true, enabled: true, policy: "zstd", encoding: "zstd"},
		{name: "API key ineligible", enabled: true, policy: "ineligible", encoding: "identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
			cfg.Gateway.OpenAIRequestCompressionEnabled = tc.enabled
			svc := &OpenAIGatewayService{cfg: cfg}
			account := newOpenAIOAuthNamespaceTestAccount()
			if !tc.eligible {
				account.Type = AccountTypeAPIKey
			}
			request := httptest.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", bytes.NewBufferString(body))
			request.Header.Set("Authorization", "Bearer PRIVATE_AUTH_MARKER")
			require.NoError(t, svc.maybeCompressCodexUpstreamRequest(request, account))
			// The transport may clone the request (including during 401 recovery).
			response := newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, "")
			response.Request = request.Clone(request.Context())
			event := validationDiagnosticEvent(t, newOpenAIRejectedFieldTestContext([]byte(body)), body, response)
			require.Equal(t, tc.policy, gjson.Get(event.Detail, "http_request_compression_policy").String())
			require.Equal(t, tc.encoding, gjson.Get(event.Detail, "http_content_encoding").String())
			require.Equal(t, request.ContentLength, gjson.Get(event.Detail, "http_request_content_length").Int())
			require.NotContains(t, event.Detail, "PRIVATE_")
			require.Empty(t, request.Header.Get("X-OpenAI-Request-Compression-Policy"))
		})
	}
}

func TestHTTPCompressionDiagnosticMissingAndUnknownPolicy(t *testing.T) {
	const body = `{"input":"PRIVATE_BODY_MARKER"}`
	for _, tc := range []struct {
		name string
		req  *http.Request
	}{
		{name: "missing transport request"},
		{name: "unannotated request", req: httptest.NewRequest(http.MethodPost, "http://example.test/", nil)},
		{name: "unknown context value", req: httptest.NewRequest(http.MethodPost, "http://example.test/", nil).WithContext(context.WithValue(context.Background(), openAIRequestCompressionPolicyKey{}, "PRIVATE_POLICY_MARKER"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, "")
			response.Request = tc.req
			event := validationDiagnosticEvent(t, newOpenAIRejectedFieldTestContext([]byte(body)), body, response)
			require.Equal(t, "unknown", gjson.Get(event.Detail, "http_request_compression_policy").String())
			require.NotContains(t, event.Detail, "PRIVATE_")
		})
	}
}
