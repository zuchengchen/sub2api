package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGatewayCompatPoolMode429AllowsSameAccountRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name string
		path string
		body []byte
		call func(*GatewayService, context.Context, *gin.Context, *Account, []byte) (*ForwardResult, error)
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`),
			call: func(svc *GatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
			},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"claude-sonnet-4-5","input":"hello"}`),
			call: func(svc *GatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsResponses(ctx, c, account, body, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
				StatusCode: http.StatusTooManyRequests,
				Header:     http.Header{"X-Request-Id": []string{"pool-429"}},
				Body:       io.NopCloser(http.NoBody),
			}}}
			svc := &GatewayService{
				cfg:                 &config.Config{},
				httpUpstream:        upstream,
				tlsFPProfileService: &TLSFingerprintProfileService{},
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			account := &Account{
				ID:       1,
				Name:     "pool-account",
				Platform: PlatformAnthropic,
				Type:     AccountTypeAPIKey,
				Credentials: map[string]any{
					"api_key":   "test-key",
					"pool_mode": true,
				},
			}

			result, err := tt.call(svc, context.Background(), c, account, tt.body)
			require.Nil(t, result)
			var failoverErr *UpstreamFailoverError
			require.ErrorAs(t, err, &failoverErr)
			require.Equal(t, http.StatusTooManyRequests, failoverErr.StatusCode)
			require.True(t, failoverErr.RetryableOnSameAccount)
			require.Equal(t, 1, upstream.callCount)
			require.Empty(t, recorder.Body.String())
		})
	}
}

// queuedHTTPUpstreamStub replays a queue of canned responses/errors per call.
type queuedHTTPUpstreamStub struct {
	responses     []*http.Response
	errors        []error
	requestBodies [][]byte
	callCount     int
	onCall        func(*http.Request, *queuedHTTPUpstreamStub)
}

func (s *queuedHTTPUpstreamStub) Do(req *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	if req != nil && req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		s.requestBodies = append(s.requestBodies, body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	} else {
		s.requestBodies = append(s.requestBodies, nil)
	}

	idx := s.callCount
	s.callCount++
	if s.onCall != nil {
		s.onCall(req, s)
	}

	var resp *http.Response
	if idx < len(s.responses) {
		resp = s.responses[idx]
	}
	var err error
	if idx < len(s.errors) {
		err = s.errors[idx]
	}
	if resp == nil && err == nil {
		return nil, errors.New("unexpected upstream call")
	}
	return resp, err
}

func (s *queuedHTTPUpstreamStub) DoWithTLS(req *http.Request, proxyURL string, accountID int64, concurrency int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return s.Do(req, proxyURL, accountID, concurrency)
}

// Anthropic 上游 400（内容策略 / 参数拒绝）经协议转换回给 Responses/Chat 客户端时：
// 必须带上 upstream 状态供 ops 归因为 provider（而非 platform/internal），
// 且错误码为 invalid_request_error，避免 Codex 把请求级拒绝当成可重试的 server_error。
func TestGatewayCompatUpstream400AttributedToProvider(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const upstreamBody = `{"type":"error","error":{"type":"invalid_request_error","message":"devin upstream error (invalid_argument): This request was flagged by Anthropic's cyber verification policy."}}`
	tests := []struct {
		name string
		path string
		body []byte
		call func(*GatewayService, context.Context, *gin.Context, *Account, []byte) (*ForwardResult, error)
	}{
		{
			name: "chat completions",
			path: "/v1/chat/completions",
			body: []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`),
			call: func(svc *GatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsChatCompletions(ctx, c, account, body, nil)
			},
		},
		{
			name: "responses",
			path: "/v1/responses",
			body: []byte(`{"model":"claude-sonnet-4-5","input":"hello"}`),
			call: func(svc *GatewayService, ctx context.Context, c *gin.Context, account *Account, body []byte) (*ForwardResult, error) {
				return svc.ForwardAsResponses(ctx, c, account, body, nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &queuedHTTPUpstreamStub{responses: []*http.Response{{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"X-Request-Id": []string{"up-400"}, "Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader([]byte(upstreamBody))),
			}}}
			svc := &GatewayService{
				cfg:                 &config.Config{},
				httpUpstream:        upstream,
				tlsFPProfileService: &TLSFingerprintProfileService{},
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, tt.path, nil)
			account := &Account{
				ID:          7,
				Name:        "anthropic-bridge",
				Platform:    PlatformAnthropic,
				Type:        AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "test-key"},
			}

			result, err := tt.call(svc, context.Background(), c, account, tt.body)
			require.Nil(t, result)
			require.Error(t, err)
			var failoverErr *UpstreamFailoverError
			require.False(t, errors.As(err, &failoverErr), "400 不应 failover")

			require.Equal(t, http.StatusBadRequest, recorder.Code)
			require.Contains(t, recorder.Body.String(), "invalid_request_error")
			require.NotContains(t, recorder.Body.String(), "server_error")

			status, ok := c.Get(OpsUpstreamStatusCodeKey)
			require.True(t, ok, "必须记录 upstream 状态，ops 才能归因为 provider")
			require.Equal(t, http.StatusBadRequest, status)
			events, ok := c.Get(OpsUpstreamErrorsKey)
			require.True(t, ok)
			list, ok := events.([]*OpsUpstreamErrorEvent)
			require.True(t, ok)
			require.Len(t, list, 1)
			require.Equal(t, int64(7), list[0].AccountID)
			require.Equal(t, "http_error", list[0].Kind)
		})
	}
}
