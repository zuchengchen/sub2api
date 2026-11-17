package service

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func oauthReliabilityErrorEnvelopes(errorObject string) map[string][]byte {
	return map[string][]byte{
		"http":          []byte(`{"error":` + errorObject + `}`),
		"ws_nested":     []byte(`{"type":"error","error":` + errorObject + `}`),
		"bare_object":   []byte(errorObject),
		"response_root": []byte(`{"type":"response.failed","error":` + errorObject + `}`),
		"response":      []byte(`{"type":"response.failed","response":{"status":"failed","error":` + errorObject + `}}`),
	}
}

func TestOAuthReliabilityErrorNormalizationShapes(t *testing.T) {
	object := `{"type":"invalid_request_error","code":"invalid_value","param":"input[15].id","message":"Synthetic invalid value","status_code":400,"provider_extension":{"value":9007199254740993}}`
	envelopes := oauthReliabilityErrorEnvelopes(object)
	envelopes["ws_bare"] = []byte(`{"type":"error","code":"invalid_value","param":"input[15].id","message":"Synthetic invalid value","status":400}`)
	for name, payload := range envelopes {
		t.Run(name, func(t *testing.T) {
			got, ok := normalizeOpenAIUpstreamError(payload)
			require.True(t, ok)
			require.Equal(t, "invalid_value", got.Code)
			require.Equal(t, "input[15].id", got.Param)
			require.Equal(t, "Synthetic invalid value", got.Message)
			require.Equal(t, http.StatusBadRequest, got.StatusCode)
			require.Equal(t, "Synthetic invalid value", gjson.GetBytes(got.ErrorBody, "error.message").String())
			if name == "ws_bare" {
				require.Empty(t, got.Type, "the WS discriminator is not an error type")
			} else {
				require.Equal(t, "invalid_request_error", got.Type)
				require.Equal(t, "9007199254740993", gjson.GetBytes(got.ErrorBody, "error.provider_extension.value").Raw)
			}
			require.Equal(t, http.StatusBadRequest, openAIStreamFailedEventSemanticStatus(payload, ""))
			require.Equal(t, http.StatusBadRequest, openAIStreamFailureStatus(payload, ""))
			require.False(t, openAIStreamFailedEventShouldFailover(payload, ""))
			require.False(t, openAIStreamErrorEventShouldFailover(payload, ""))
		})
	}
}

func TestOAuthReliabilityErrorNormalizationRejectsAmbiguity(t *testing.T) {
	tests := map[string]string{
		"duplicate root":       `{"error":{"code":"invalid_value"},"error":{"code":"server_error"}}`,
		"duplicate error":      `{"error":{"code":"invalid_value","code":"server_error"}}`,
		"escaped duplicate":    `{"error":{"code":"invalid_value","\u0063ode":"server_error"}}`,
		"nested duplicate":     `{"error":{"code":"invalid_value","extra":{"x":1,"x":2}}}`,
		"response duplicate":   `{"type":"response.failed","response":{"error":{"message":"first"},"error":{"message":"second"}}}`,
		"two envelopes":        `{"error":{"message":"first"},"response":{"error":{"message":"second"}}}`,
		"conflicting code":     `{"type":"error","code":"server_error","error":{"code":"invalid_value"}}`,
		"conflicting message":  `{"type":"error","message":"temporary failure","error":{"message":"bad request"}}`,
		"conflicting status":   `{"status":429,"error":{"status_code":400}}`,
		"conflicting aliases":  `{"error":{"status_code":400,"status":500}}`,
		"conflicting terminal": `{"type":"response.completed","response":{"error":{"code":"invalid_value"}}}`,
		"conflicting response": `{"type":"response.failed","response":{"status":"completed","error":{"code":"invalid_value"}}}`,
		"numeric type":         `{"error":{"type":400}}`,
		"numeric code":         `{"error":{"code":400}}`,
		"array param":          `{"error":{"param":["input[0].id"]}}`,
		"object message":       `{"error":{"message":{"code":"invalid_value"}}}`,
		"float status":         `{"error":{"status":400.5}}`,
		"boolean status":       `{"error":{"status":true}}`,
		"success status":       `{"error":{"status_code":200}}`,
		"text error":           `{"error":"Bad Request"}`,
		"trailing JSON":        `{"error":{"code":"invalid_value"}} {}`,
		"non-object":           `[{"error":{"code":"invalid_value"}}]`,
		"malformed":            `{"error":`,
		"invalid UTF-8":        "{\"error\":{\"message\":\"\xff\"}}",
		"too deep":             `{"error":{"extra":` + strings.Repeat("[", maxOpenAIErrorJSONDepth) + `0` + strings.Repeat("]", maxOpenAIErrorJSONDepth) + `}}`,
		"too large":            `{"error":{"message":"` + strings.Repeat("x", maxOpenAIErrorNormalizationBytes) + `"}}`,
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			_, ok := normalizeOpenAIUpstreamError([]byte(payload))
			require.False(t, ok)
			require.Equal(t, http.StatusBadGateway, openAIStreamFailureStatus([]byte(payload), ""))
			require.False(t, openAIStreamErrorEventShouldFailover([]byte(payload), "please retry"))
			require.False(t, openAIStreamFailedEventShouldFailover([]byte(payload), "please retry"))
		})
	}
}

func TestOAuthReliabilitySemantic400DoesNotCreateRetries(t *testing.T) {
	objects := []string{
		`{"type":"invalid_request_error","message":"Bad Request"}`,
		`{"type":"invalid_request_error","message":"Unsupported parameter; please retry with a valid request"}`,
		`{"type":"invalid_request_error","code":"invalid_value","message":"The supplied text contains rate_limit, authentication, permission and temporary"}`,
		`{"type":"invalid_request_error","code":"invalid_value","message":"Invalid value 'Selected model is at capacity'"}`,
		`{"type":"invalid_request_error","code":"invalid_value","message":"Invalid value 'An error occurred while processing your request'"}`,
		`{"type":"invalid_request_error","code":"unknown_parameter","message":"Unknown parameter foo"}`,
		`{"type":"error","status":400,"message":"Bad Request"}`,
		`{"type":"error","status":400,"message":"temporary error, please retry"}`,
	}
	for _, object := range objects {
		for name, payload := range oauthReliabilityErrorEnvelopes(object) {
			t.Run(fmt.Sprintf("%d/%s", len(object), name), func(t *testing.T) {
				require.Equal(t, http.StatusBadRequest, openAIStreamFailureStatus(payload, ""))
				require.False(t, openAIStreamFailedEventShouldFailover(payload, ""))
				require.False(t, openAIStreamErrorEventShouldFailover(payload, ""))
				_, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest,
					[]byte(`{"input":[{"id":"item_BAD","role":"user","content":"keep"}]}`), payload)
				require.NoError(t, err)
				require.False(t, changed)
			})
		}
	}
}

func TestOAuthReliabilitySemanticTransientAndCredentialPolicyPreserved(t *testing.T) {
	tests := []struct {
		name   string
		object string
		status int
		retry  bool
		auth   bool
	}{
		{"capacity", `{"type":"invalid_request_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}`, 503, true, false},
		{"rate_limit", `{"type":"invalid_request_error","code":"rate_limit_exceeded","message":"rate limited"}`, 429, true, false},
		{"authentication", `{"type":"authentication_error","code":"invalid_api_key","message":"key rejected"}`, 401, true, true},
		{"authentication_wrapped", `{"type":"invalid_request_error","code":"invalid_api_key","message":"key rejected"}`, 401, true, true},
		{"request_permission", `{"type":"permission_error","code":"forbidden","message":"request forbidden"}`, 403, false, false},
		{"access_state", `{"type":"invalid_request_error","code":"workspace_suspended","message":"workspace unavailable"}`, 403, true, false},
		{"credential_403", `{"type":"permission_error","code":"invalid_api_key","status":403,"message":"key rejected"}`, 403, true, true},
		{"overload_529", `{"type":"error","status":529,"message":"busy"}`, 529, true, false},
		{"processing", `{"type":"invalid_request_error","message":"An error occurred while processing your request."}`, 502, true, false},
		{"context", `{"type":"invalid_request_error","code":"context_length_exceeded","message":"input exceeds the context window"}`, 400, false, false},
		{"encrypted", `{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"Encrypted content could not be decrypted"}`, 400, false, false},
	}
	for _, tt := range tests {
		for name, payload := range oauthReliabilityErrorEnvelopes(tt.object) {
			t.Run(tt.name+"/"+name, func(t *testing.T) {
				require.Equal(t, tt.status, openAIStreamFailureStatus(payload, ""))
				require.Equal(t, tt.retry, openAIStreamFailedEventShouldFailover(payload, ""))
				require.Equal(t, tt.retry, openAIStreamErrorEventShouldFailover(payload, ""))
				require.Equal(t, tt.auth, openAIStreamCredentialAuthFailure(payload))
			})
		}
	}
}

func TestOAuthReliabilityErrorPassthroughPreservesStructuredFields(t *testing.T) {
	payload := []byte(`{"type":"response.failed","status_code":400,"response":{"error":{"type":"invalid_request_error","code":"invalid_id_prefix","param":"input[15].id","message":"precise rejection","extension":{"limit":9007199254740993}}}}`)
	body := openAIStreamFailedEventPassthroughBody(payload, "fallback")
	require.Equal(t, "invalid_request_error", gjson.GetBytes(body, "error.type").String())
	require.Equal(t, "invalid_id_prefix", gjson.GetBytes(body, "error.code").String())
	require.Equal(t, "input[15].id", gjson.GetBytes(body, "error.param").String())
	require.Equal(t, "precise rejection", gjson.GetBytes(body, "error.message").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(body, "error.extension.limit").Raw)
	require.Equal(t, int64(400), gjson.GetBytes(body, "error.status_code").Int())
	_, ok := normalizeOpenAIUpstreamError([]byte(`{"type":"error","status":400,"error":{"status_code":"400","code":"invalid_value"}}`))
	require.True(t, ok, "equal numeric status aliases are consistent")
}

func TestOAuthReliabilityConcurrentRejectedFieldBudget(t *testing.T) {
	budget := &openAIResponsesRejectedFieldRetryBudget{}
	states := make([]*openAIResponsesRejectedFieldRetryState, 32)
	for i := range states {
		states[i] = newOpenAIResponsesRejectedFieldRetryStateWithBudget([]byte(`{"initial":true}`), budget)
	}
	var allowed atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, state := range states {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if state.Allow([]byte(fmt.Sprintf(`{"repair":%d}`, i))) {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(maxOpenAIResponsesRejectedFieldRetries), allowed.Load())
	require.Equal(t, maxOpenAIResponsesRejectedFieldRetries, budget.attempts)
}

func TestOAuthReliabilityConcurrentRejectedFieldDuplicateState(t *testing.T) {
	state := newOpenAIResponsesRejectedFieldRetryState([]byte(`{"initial":true}`))
	var allowed atomic.Int32
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if state.Allow([]byte(`{"same":"repair"}`)) {
				allowed.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, int32(1), allowed.Load())
	require.Equal(t, 1, state.budget.attempts)
	require.Len(t, state.seenBodyHashes, 2)
	for i := 0; i < maxOpenAIResponsesRejectedFieldRetries-1; i++ {
		require.True(t, state.Allow([]byte(fmt.Sprintf(`{"new":%d}`, i))))
	}
	require.False(t, state.Allow([]byte(`{"overflow":true}`)))
}
