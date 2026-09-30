package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// requireNoOpenAIRoutingHeaders asserts the response tells the client nothing
// about which channel served it. Header keys are canonicalized, so the prefix
// is compared case-insensitively.
func requireNoOpenAIRoutingHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for key := range header {
		require.False(t, strings.HasPrefix(strings.ToLower(key), "x-codex2api-"), "routing header leaked to client: %s", key)
	}
}

func TestForwardNeverSendsRoutingHeaders(t *testing.T) {
	forbidden := func() *http.Response { return tiboRouteStatusResponse(http.StatusForbidden) }
	for _, tt := range []struct {
		name     string
		run      func(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context)
		reason   string
		degraded *bool
	}{
		{
			name: "healthy HTTP with BPS enabled",
			run: func(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context) {
				tc := newTiboRouteCase(t, true, false, cookieWSHTTPResponse("http ok"))
				tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
				result, rec, c, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
				require.NoError(t, err)
				require.Equal(t, []string{"chatgpt.com"}, tc.hosts())
				return result, rec, c
			},
			reason:   openAITiboHTTPOKReason,
			degraded: new(bool),
		},
		{
			name: "BPS 403 falls back to HTTP",
			run: func(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context) {
				tc := newTiboRouteCase(t, true, false, forbidden(), cookieWSHTTPResponse("http ok"))
				tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
				result, rec, c, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
				require.NoError(t, err)
				require.Equal(t, []string{"bps.openai.com", "chatgpt.com"}, tc.hosts())
				return result, rec, c
			},
			reason:   excelBPSHTTPFallbackReason,
			degraded: boolPtrForRoutingHeadersTest(true),
		},
		{
			name: "everything degraded serves HTTP",
			run: func(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context) {
				tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("http ok"))
				tc.seed(openAITiboRouteHTTP, openAITiboDegraded)
				result, rec, c, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
				require.NoError(t, err)
				require.Equal(t, []string{"chatgpt.com"}, tc.hosts())
				return result, rec, c
			},
			reason:   "",
			degraded: boolPtrForRoutingHeadersTest(true),
		},
		{
			name: "non-Tibo BPS account falls back",
			run: func(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context) {
				upstream := &httpUpstreamRecorder{responses: []*http.Response{forbidden(), excelBPSCodexHTTPSuccessResponse()}}
				svc := openAIClientToolsTestService(upstream)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				result, err := svc.Forward(context.Background(), c, excelAccount(), []byte(`{"model":"gpt-5.6-sol","input":"x"}`))
				require.NoError(t, err)
				require.Len(t, upstream.requests, 2)
				require.Equal(t, "chatgpt.com", upstream.requests[1].URL.Host)
				return result, rec, c
			},
			reason: excelBPSHTTPFallbackReason,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			result, rec, c := tt.run(t)
			require.NotNil(t, result)
			body, err := io.ReadAll(rec.Result().Body)
			require.NoError(t, err)
			require.Contains(t, string(body), "ok")
			requireNoOpenAIRoutingHeaders(t, rec.Header())
			requireNoOpenAIRoutingHeaders(t, rec.Result().Header)
			require.Equal(t, tt.reason, c.GetString(openAIBPSBypassReasonKey))
			if tt.degraded != nil {
				require.NotNil(t, result.RouteDegraded)
				require.Equal(t, *tt.degraded, *result.RouteDegraded, "degraded state still reaches usage logs")
			}
		})
	}
}

func boolPtrForRoutingHeadersTest(v bool) *bool { return &v }
