package service

import (
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
	for _, tt := range []struct {
		name     string
		run      func(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context)
		reason   string
		degraded *bool
	}{
		{
			name: "healthy HTTP",
			run: func(t *testing.T) (*OpenAIForwardResult, *httptest.ResponseRecorder, *gin.Context) {
				tc := newTiboRouteCase(t, false, false, cookieWSHTTPResponse("http ok"))
				tc.seed(openAITiboRouteHTTP, openAITiboHealthy)
				result, rec, c, err := tc.forwardWith(t, tiboRouteAstraBody, nil)
				require.NoError(t, err)
				require.Equal(t, []string{"chatgpt.com"}, tc.hosts())
				return result, rec, c
			},
			reason:   "",
			degraded: new(bool),
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
