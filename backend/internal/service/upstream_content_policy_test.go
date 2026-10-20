//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const devinContentPolicy403Body = `{"error":{"type":"permission_error","message":"devin upstream error (permission_denied): Your request was blocked by our content policy. Please remove sensitive or unsafe content from your prompt, memories, and other settings and try again. (trace ID: 00000000000000000000000000000000)"}}`

func TestIsUpstreamContentPolicy403(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"devin_content_policy", http.StatusForbidden, devinContentPolicy403Body, true},
		{"plain_text_content_policy", http.StatusForbidden, "Request blocked by content policy", true},
		{"not_403", http.StatusBadRequest, devinContentPolicy403Body, false},
		{"generic_forbidden", http.StatusForbidden, `{"error":{"message":"account may be suspended or lack permissions"}}`, false},
		{"account_suspended_mentions_policy", http.StatusForbidden, `{"error":{"message":"Your account has been suspended for violating our content policy"}}`, false},
		{"empty", http.StatusForbidden, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isUpstreamContentPolicy403(tc.status, []byte(tc.body)))
		})
	}
}

func TestHandleUpstreamError_ContentPolicy403SkipsAccountPenalty(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI} {
		t.Run(platform, func(t *testing.T) {
			repo := &rateLimitAccountRepoStub{}
			svc := NewRateLimitService(repo, nil, &config.Config{}, nil)
			account := &Account{ID: 701, Platform: platform, Type: AccountTypeAPIKey}

			shouldDisable := svc.HandleUpstreamError(
				context.Background(), account, http.StatusForbidden, http.Header{}, []byte(devinContentPolicy403Body),
			)

			require.False(t, shouldDisable, "content policy 403 must not fail over")
			require.Zero(t, repo.setErrorCalls)
			require.Zero(t, repo.tempCalls)
		})
	}
}

func TestGatewayService_Forward_ContentPolicy403ReturnedAs400WithoutFailover(t *testing.T) {
	gin.SetMode(gin.TestMode)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)

	body := []byte(`{"model":"claude-3-5-sonnet-latest","stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	parsed, err := ParseGatewayRequest(NewRequestBodyRef(body), PlatformAnthropic)
	require.NoError(t, err)

	upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(devinContentPolicy403Body)),
	}}
	repo := &rateLimitAccountRepoStub{}
	cfg := &config.Config{Gateway: config.GatewayConfig{MaxLineSize: defaultMaxLineSize}}
	svc := &GatewayService{
		cfg:                  cfg,
		responseHeaderFilter: compileResponseHeaderFilter(cfg),
		httpUpstream:         upstream,
		rateLimitService:     NewRateLimitService(repo, nil, cfg, nil),
		deferredService:      &DeferredService{},
	}
	account := newAnthropicAPIKeyAccountForTest()
	delete(account.Extra, "anthropic_passthrough")

	result, err := svc.Forward(context.Background(), c, account, parsed)
	require.Error(t, err)
	require.Nil(t, result)
	var failoverErr *UpstreamFailoverError
	require.False(t, errors.As(err, &failoverErr), "content policy rejection must not fail over to other accounts")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.JSONEq(t, devinContentPolicy403Body, rec.Body.String())
	require.Zero(t, repo.setErrorCalls)
	require.Zero(t, repo.tempCalls)
}
