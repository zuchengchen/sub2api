//go:build unit

package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestOpenAIWS401HTTPAccountFailoverBoundary(t *testing.T) {
	for _, written := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_output", true: "after_output"}[written], func(t *testing.T) {
			var dials atomic.Int32
			wsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				dials.Add(1)
				w.Header().Set("X-Request-Id", "synthetic-ws401")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"type":"authentication_error","message":"synthetic unauthorized"}}`))
			}))
			defer wsServer.Close()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			c.Request.Header.Set("User-Agent", "synthetic-client")
			if written {
				_, _ = c.Writer.WriteString("already committed output")
			}
			before := c.Writer.Size()
			upstream := &httpUpstreamRecorder{}
			cfg := &config.Config{}
			cfg.Security.URLAllowlist.Enabled = false
			cfg.Security.URLAllowlist.AllowInsecureHTTP = true
			cfg.Gateway.OpenAIWS.Enabled = true
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			cfg.Gateway.OpenAIWS.APIKeyEnabled = true
			cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
			cfg.Gateway.OpenAIWS.SessionDisableEnabled = true
			cfg.Gateway.OpenAIWS.SessionDisableRetryTTLMS = 900000
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg)}
			defer svc.CloseOpenAIWSPool()
			account := &Account{ID: 9081, Name: "synthetic", Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Concurrency: 1, Credentials: map[string]any{"api_key": "synthetic-key", "base_url": wsServer.URL}, Extra: map[string]any{"responses_websockets_v2_enabled": true}}
			body := []byte(`{"model":"gpt-5.1","stream":false,"input":[{"type":"input_text","text":"synthetic"}]}`)
			result, err := svc.Forward(context.Background(), c, account, body)
			require.Error(t, err)
			require.Nil(t, result)
			require.EqualValues(t, 1, dials.Load(), "do not retry rejected API key on this account")
			require.Empty(t, upstream.requests, "401 must not fall back to HTTP on the same account")
			require.Empty(t, svc.openAIWSSessionDisabledReason(resolveOpenAIWSSessionScope(c, account, body)))
			var failure *UpstreamFailoverError
			if written {
				require.False(t, errors.As(err, &failure), "never replay after semantic output")
				require.Equal(t, before, c.Writer.Size())
			} else {
				require.ErrorAs(t, err, &failure, "handler needs an uncommitted bounded account failover signal")
				require.Equal(t, http.StatusUnauthorized, failure.StatusCode)
				require.True(t, failure.ShouldRetryNextAccount())
				require.False(t, failure.RetryableOnSameAccount)
				require.Equal(t, "synthetic-ws401", failure.ResponseHeaders.Get("X-Request-Id"))
				require.False(t, c.Writer.Written())
			}
		})
	}
}
