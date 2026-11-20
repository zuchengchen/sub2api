//go:build unit

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func responsesCacheSampleFile(t *testing.T, userID, keyID int64) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0700))
	path := filepath.Join(dir, "native.jsonl")
	t.Setenv("SUB2_RESPONSES_CACHE_DIAGNOSTIC_USER_ID", strconv.FormatInt(userID, 10))
	t.Setenv("SUB2_RESPONSES_CACHE_DIAGNOSTIC_API_KEY_ID", strconv.FormatInt(keyID, 10))
	t.Setenv("SUB2_RESPONSES_CACHE_DIAGNOSTIC_MODELS", "gpt-5.6-sol")
	t.Setenv("SUB2_RESPONSES_CACHE_DIAGNOSTIC_PATH", path)
	t.Setenv("SUB2_RESPONSES_CACHE_DIAGNOSTIC_UNTIL", time.Now().Add(10*time.Minute).Format(time.RFC3339))
	return path
}

func readResponsesCacheSamples(t *testing.T, path string) []gjson.Result {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	var samples []gjson.Result
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		require.True(t, gjson.Valid(line))
		samples = append(samples, gjson.Parse(line))
	}
	return samples
}

func TestResponsesCacheDiagnosticHTTPHandler(t *testing.T) {
	path := responsesCacheSampleFile(t, 100, 99)
	r := oauthReliabilityRunLiteChannelMappedHandler(t, "responses", "public-alias", "gpt-5.6-sol", true, false, false)
	require.Equal(t, http.StatusOK, r.recorder.Code, r.diagnostic())
	samples := readResponsesCacheSamples(t, path)
	require.Len(t, samples, 1)
	require.Equal(t, "http", samples[0].Get("transport").String())
	require.Equal(t, "gpt-5.6-sol", samples[0].Get("sent_model").String())
	require.EqualValues(t, 7, samples[0].Get("usage.input_tokens").Int())
	require.False(t, samples[0].Get("http_ws_incremental").Exists())
	require.NotContains(t, samples[0].Raw, "retain declared tool")
}

func TestResponsesCacheDiagnosticHTTPIncrementalEvidence(t *testing.T) {
	path := responsesCacheSampleFile(t, 8, 9)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	key := &service.APIKey{ID: 9, UserID: 8, Group: &service.Group{Platform: service.PlatformOpenAI}}
	c.Set("api_key", key)
	const model = "gpt-5.6-sol"
	obs := service.NewResponsesCacheDiagnosticObserver(c, key)
	result := &service.OpenAIForwardResult{Model: model, OpenAIWSMode: true}
	for _, reason := range []string{"input_mismatch", "private unexpected value"} {
		sample := obs.Begin([]byte(`{"model":"gpt-5.6-sol","input":"private prompt"}`), model, "http")
		require.NotNil(t, sample)
		c.Set("openai_ws_auto_incremental_reason", reason)
		sample.ObserveHTTPIncremental(c, result)
		sample.Finish(11, result, nil)
	}
	samples := readResponsesCacheSamples(t, path)
	require.Len(t, samples, 2)
	require.Equal(t, "input_mismatch", samples[0].Get("http_ws_incremental.reason").String())
	require.Equal(t, "other", samples[1].Get("http_ws_incremental.reason").String())
	require.NotContains(t, samples[1].Raw, "private")
}

func TestResponsesCacheDiagnosticHTTPFailoverRecordsOneRequest(t *testing.T) {
	path := responsesCacheSampleFile(t, 100, 99)
	t.Setenv("SUB2_RESPONSES_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
	upstream := &openAIResponsesFailoverCancelUpstream{}
	h := newOpenAIResponsesFailoverTestHandler(t, upstream)
	c, _ := newOpenAIResponsesFailoverTestContext(t, context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","stream":false,"input":"hello"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	key, ok := middleware.GetAPIKeyFromContext(c)
	require.True(t, ok)
	key.UserID = key.User.ID
	h.Responses(c)
	require.Equal(t, []int64{1, 2}, upstream.calls())
	samples := readResponsesCacheSamples(t, path)
	require.Len(t, samples, 1)
	require.Equal(t, "error", samples[0].Get("status").String())
	require.EqualValues(t, 2, samples[0].Get("account_id").Int())
	require.Equal(t, "[1,2]", samples[0].Get("attempt_accounts").Raw)
	require.False(t, samples[0].Get("usage").Exists())
}

func TestResponsesCacheDiagnosticWebSocketHandler(t *testing.T) {
	for _, mode := range []string{service.OpenAIWSIngressModePassthrough, service.OpenAIWSIngressModeCtxPool} {
		t.Run(mode, func(t *testing.T) {
			path := responsesCacheSampleFile(t, 1701, 1801)
			r := runOpenAIResponsesWebSocketUsageLogCase(t, openAIResponsesWSUsageLogCase{
				ingressMode:    mode,
				group:          wsAllowlistGroup(false),
				channelMapping: map[string]string{"public-alias": "gpt-5.6-sol"},
				firstPayload:   `{"type":"response.create","model":"public-alias","input":[{"role":"user","content":"private test prefix"}]}`,
				secondPayload:  `{"type":"response.create","previous_response_id":"resp_usage_e2e_1","input":[{"role":"user","content":"private test prefix"},{"role":"user","content":"private test suffix"}]}`,
			})
			require.Len(t, r.logs, 2)
			samples := readResponsesCacheSamples(t, path)
			require.Len(t, samples, 2)
			for i, sample := range samples {
				require.Equal(t, "ws", sample.Get("transport").String())
				require.EqualValues(t, i+1, sample.Get("ws_client_turn").Int())
				require.Equal(t, "gpt-5.6-sol", sample.Get("sent_model").String())
				require.EqualValues(t, 9901, sample.Get("account_id").Int())
				require.EqualValues(t, 2, sample.Get("usage.input_tokens").Int())
				require.Len(t, sample.Get("input.items").Array(), i+1)
				require.NotContains(t, sample.Raw, "private test")
			}
			require.Equal(t, samples[0].Get("input.items.0").Raw, samples[1].Get("input.items.0").Raw)
			require.NotEmpty(t, samples[0].Get("response_id_hmac").String())
			require.Equal(t, samples[0].Get("response_id_hmac").String(), samples[1].Get("identity.body:previous_response_id.hmac").String())
		})
	}
}

func TestResponsesCacheDiagnosticWSFailoverKeepsOriginalTurn(t *testing.T) {
	path := responsesCacheSampleFile(t, 8, 9)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	key := &service.APIKey{ID: 9, UserID: 8, Group: &service.Group{Platform: service.PlatformOpenAI}}
	c.Set("api_key", key)
	const model = "gpt-5.6-sol"
	first := []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"first"}`)
	turns := newResponsesWSCacheDiagnostics(service.NewResponsesCacheDiagnosticObserver(c, key), first)
	result := &service.OpenAIForwardResult{Model: model, Usage: service.OpenAIUsage{InputTokens: 500, CacheReadInputTokens: 400}}
	turns.startAttempt(11)
	turns.observeTurn(1, first, model)
	turns.finishTurn(1, result, nil)
	turns.observeTurn(2, []byte(`{"type":"response.create","input":"followup"}`), model)
	turns.finishTurn(2, nil, &service.UpstreamFailoverError{StatusCode: 429})
	turns.startAttempt(12)
	turns.observeTurn(1, []byte(`{"type":"response.create","input":"reconstructed"}`), model)
	turns.finishTurn(1, result, nil) // the second proxy attempt starts from one
	turns.close()
	samples := readResponsesCacheSamples(t, path)
	require.Len(t, samples, 2)
	require.EqualValues(t, 11, samples[0].Get("account_id").Int())
	require.EqualValues(t, 12, samples[1].Get("account_id").Int())
	require.EqualValues(t, 2, samples[1].Get("ws_client_turn").Int())
	require.EqualValues(t, 1, samples[1].Get("account_switches").Int())
	require.EqualValues(t, 400, samples[1].Get("usage.cached_tokens").Int())
}
