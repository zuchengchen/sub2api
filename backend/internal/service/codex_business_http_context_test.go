package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/stretchr/testify/require"
)

type codexBusinessMarkerUpstream struct{ requests []*http.Request }

func (u *codexBusinessMarkerUpstream) Do(r *http.Request, _ string, _ int64, _ int) (*http.Response, error) {
	u.requests = append(u.requests, r)
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
}
func (u *codexBusinessMarkerUpstream) DoWithTLS(r *http.Request, p string, a int64, c int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	return u.Do(r, p, a, c)
}

func TestCodexBusinessHTTPMarkerClonesAndClearsForOtherAccounts(t *testing.T) {
	upstream := &codexBusinessMarkerUpstream{}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	oauth := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	apiKey := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://example.invalid/models", nil)
	resp, err := svc.doOpenAIHTTPTransport(req, "", oauth, oauth)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.False(t, CodexBusinessHTTPFromContext(req.Context()), "the shared input request must not be mutated")
	require.NotSame(t, req, upstream.requests[0])
	require.True(t, CodexBusinessHTTPFromContext(upstream.requests[0].Context()))
	inherited := upstream.requests[0]
	resp, err = svc.doOpenAIHTTPTransport(inherited, "", apiKey, apiKey)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.False(t, CodexBusinessHTTPFromContext(upstream.requests[1].Context()), "later non-OAuth account clears inherited marker")
	require.True(t, CodexBusinessHTTPFromContext(inherited.Context()), "clearing applies only to a new clone")
}
