package service

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserCacheDiagnosticStreamingRequestWithoutReliableContentType(t *testing.T) {
	for _, clientStream := range []bool{false, true} {
		for _, contentType := range []string{"", "application/json", "text/event-stream"} {
			name := strconvBoolForUserCacheTest(clientStream) + "/" + strings.ReplaceAll(contentType, "/", "_")
			t.Run(name, func(t *testing.T) {
				path := userCacheDiagnosticTestEnv(t)
				request := []byte(`{"model":"gpt-6-astra","messages":[{"role":"user","content":"PRIVATE_PROMPT"}],"stream":` + strconvBoolForUserCacheTest(clientStream) + `}`)
				terminal := `{"type":"response.completed","response":{"id":"PRIVATE_RESPONSE","model":"gpt-6-astra","status":"completed","output":[{"type":"reasoning","id":"PRIVATE_REASONING","summary":[],"encrypted_content":"PRIVATE_CIPHER"},{"type":"message","content":[{"type":"output_text","text":"PRIVATE_ANSWER"}]}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":64},"output_tokens":7}}}`
				header := http.Header{}
				if contentType != "" {
					header.Set("Content-Type", contentType)
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader("data: " + terminal + "\n\n"))}}
				svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
				c := userCacheDiagnosticTestContext(428)
				result, err := svc.ForwardAsChatCompletions(context.Background(), c, newCodexSynthTestAccount(23157, AccountTypeOAuth), request, "", "")
				require.NoError(t, err)
				require.Equal(t, 100, result.Usage.InputTokens)
				require.Equal(t, 64, result.Usage.CacheReadInputTokens)
				require.Equal(t, contentType, upstream.resp.Header.Get("Content-Type"), "diagnostics must not rewrite the upstream headers")
				events := userCacheDiagnosticReadEvents(t, path)
				require.Len(t, events, 3)
				response := events[2]
				require.Equal(t, "response.completed", response["terminal"])
				require.Equal(t, false, response["partial_event"])
				require.Equal(t, false, response["read_error"])
				require.EqualValues(t, 0, response["invalid_json_events"])
				require.Len(t, response["output_items"], 2)
				usage := response["usage"].(map[string]any)["response.usage"].(map[string]any)
				require.EqualValues(t, 100, usage["input_tokens"])
				require.EqualValues(t, 64, usage["input_tokens_details.cached_tokens"])
				raw, err := os.ReadFile(path)
				require.NoError(t, err)
				for _, value := range []string{"PRIVATE_PROMPT", "PRIVATE_RESPONSE", "PRIVATE_REASONING", "PRIVATE_CIPHER", "PRIVATE_ANSWER"} {
					require.NotContains(t, string(raw), value)
				}
			})
		}
	}
}

func TestUserCacheDiagnosticJSONResponseModesPreserved(t *testing.T) {
	for _, tc := range []struct {
		name, request string
		status        int
	}{
		{"nonstream_success", `{"stream":false}`, http.StatusOK},
		{"string_is_not_boolean", `{"stream":"true"}`, http.StatusOK},
		{"stream_json_error", `{"stream":true}`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			d := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(`{}`))
			request, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", strings.NewReader(tc.request))
			require.NoError(t, err)
			attempt := d.upstreamRequest(&Account{ID: 1}, request)
			wire := `{"status":"completed","output":[],"usage":{"input_tokens":19,"output_tokens":2}}`
			terminal := "response.completed"
			if tc.status >= http.StatusBadRequest {
				wire = `{"error":{"code":"invalid_encrypted_content","message":"PRIVATE_ERROR"},"usage":{"input_tokens":19,"output_tokens":2}}`
				terminal = ""
			}
			response := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(wire))}
			attempt.upstreamResponse(response, nil)
			received, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, wire, string(received))
			events := userCacheDiagnosticReadEvents(t, path)
			require.Equal(t, terminal, events[2]["terminal"])
			require.EqualValues(t, 1, events[2]["observed_events"])
			require.EqualValues(t, 19, events[2]["usage"].(map[string]any)["usage"].(map[string]any)["input_tokens"])
			require.Equal(t, false, events[2]["partial_event"])
			require.EqualValues(t, tc.status, events[2]["status"])
		})
	}
}
