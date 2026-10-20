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

	"github.com/Wei-Shaw/sub2api/internal/config"
)

// A self-hosted inference upstream is paid in GPU time, not per token: when the
// client is gone, the in-flight upstream request must be cancelled instead of
// running to completion (a 150K-token round kept the GPU busy for minutes after
// Cloudflare had dropped the client). Metered upstreams keep the default
// detached context so their usage still settles.
func TestForwardResponsesUpstreamContextFollowsClientOnlyWhenFlagEnabled(t *testing.T) {
	for _, tc := range []struct {
		name         string
		extra        map[string]any
		wantCanceled bool
	}{
		{name: "default detaches from client", extra: nil, wantCanceled: false},
		{name: "flag false detaches from client", extra: map[string]any{"cancel_upstream_on_client_disconnect": false}, wantCanceled: false},
		{name: "flag follows client", extra: map[string]any{"cancel_upstream_on_client_disconnect": true}, wantCanceled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			body := `{"model":"duckgpt","instructions":"Reply OK","input":[],"stream":false}`
			upstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"resp_1","object":"response","model":"duckgpt","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
				)),
			}}
			svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream, tiboRouteDisabled: true}
			account := &Account{
				ID:          6,
				Name:        "self-hosted-inference",
				Platform:    PlatformOpenAI,
				Type:        AccountTypeAPIKey,
				Status:      StatusActive,
				Schedulable: true,
				Concurrency: 1,
				Credentials: map[string]any{"api_key": "sk-test", "base_url": "https://inference.example.com"},
				Extra:       tc.extra,
			}
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			clientCtx, clientDisconnect := context.WithCancel(context.Background())
			defer clientDisconnect()

			_, err := svc.Forward(clientCtx, c, account, []byte(body))
			require.NoError(t, err)
			require.NotNil(t, upstream.lastReq)

			clientDisconnect()
			require.Equal(t, tc.wantCanceled, upstream.lastReq.Context().Err() != nil)
		})
	}
}
