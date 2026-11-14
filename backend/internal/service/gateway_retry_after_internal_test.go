//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestRetryUpstreamResponseDelayBudget(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		remaining  time.Duration
		wantDelay  time.Duration
		wantRetry  bool
	}{
		{"no header preserves backoff", 429, "", time.Second, retryBaseDelay, true},
		{"short header preserves backoff", 429, "0.1", time.Second, retryBaseDelay, true},
		{"header extends wait", 429, "2", 3 * time.Second, 2 * time.Second, true},
		{"header exceeds remaining budget", 429, "2", time.Second, 0, false},
		{"header exceeds full budget", 429, "60", maxRetryElapsed, 0, false},
		{"ordinary backoff remains bounded", 429, "", 100 * time.Millisecond, 100 * time.Millisecond, true},
		{"invalid header", 429, "invalid", time.Second, retryBaseDelay, true},
		{"expired header", 429, "-2", time.Second, retryBaseDelay, true},
		{"other status preserves policy", 502, "60", time.Second, retryBaseDelay, true},
		{"budget exhausted", 429, "", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Retry-After": {tc.retryAfter}}}
			delay, retry := retryUpstreamResponseDelay(resp, 1, tc.remaining)
			require.Equal(t, tc.wantRetry, retry)
			require.Equal(t, tc.wantDelay, delay)
		})
	}
}

func TestAnthropicPoolExcluded429InternalRetryAfter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		path := "native"
		if passthrough {
			path = "passthrough"
		}
		for _, tc := range []struct {
			name       string
			retryAfter string
			minWait    time.Duration
			cancel     bool
			failover   bool
		}{
			{"short upstream wait", "1", time.Second, false, false},
			{"no header", "", retryBaseDelay, false, false},
			{"wait exceeds budget", "60", 0, false, true},
			{"canceled before wait", "1", 0, true, false},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var callTimes []time.Time
				upstream := &queuedHTTPUpstreamStub{
					responses: []*http.Response{
						{StatusCode: 429, Header: http.Header{"Retry-After": {tc.retryAfter}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`))},
						{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"msg_retry","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))},
					},
					onCall: func(_ *http.Request, _ *queuedHTTPUpstreamStub) {
						callTimes = append(callTimes, time.Now())
						if tc.cancel {
							cancel()
						}
					},
				}
				repo := &rateLimitAccountRepoStub{}
				cfg := &config.Config{}
				svc := &GatewayService{
					cfg: cfg, httpUpstream: upstream, tlsFPProfileService: &TLSFingerprintProfileService{},
					rateLimitService: NewRateLimitService(repo, nil, cfg, nil),
				}
				account := newAnthropicQuotaPoolAccount()
				account.Extra["anthropic_passthrough"] = passthrough
				account.Credentials["custom_error_codes_enabled"] = true
				account.Credentials["custom_error_codes"] = []any{float64(401)}
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
				body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"synthetic retry regression"}],"max_tokens":100}`)
				result, err := svc.Forward(ctx, c, account, &ParsedRequest{Model: "claude-sonnet-4-5", Body: NewRequestBodyRef(body)})
				switch {
				case tc.cancel:
					require.ErrorIs(t, err, context.Canceled)
					require.Nil(t, result)
					require.Equal(t, 1, upstream.callCount)
					require.False(t, c.Writer.Written())
				case tc.failover:
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result)
					require.Equal(t, 429, failover.StatusCode)
					require.False(t, failover.RetryableOnSameAccount)
					require.Equal(t, tc.retryAfter, failover.ResponseHeaders.Get("Retry-After"))
					require.Equal(t, 1, upstream.callCount)
					require.False(t, c.Writer.Written())
				default:
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, 2, upstream.callCount)
					require.GreaterOrEqual(t, callTimes[1].Sub(callTimes[0]), tc.minWait)
				}
				require.Zero(t, repo.rateLimitedCalls, "explicit error exclusions keep their scheduling policy")
			})
		}
	}
}
