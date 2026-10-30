package service

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestSubscriptionFixtureCannotSendExternalRequests(t *testing.T) {
	client, err := newTestPrivacyClientFactory()("")
	require.NoError(t, err)
	dials := 0
	client.SetDial(func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("network must not be reached")
	})
	resp, err := client.R().SetQueryParam("feature", "training_allowed").SetQueryParam("value", "false").Patch(openAISettingsURL)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_, err = client.R().Get("https://example.invalid/forbidden")
	require.ErrorContains(t, err, "test denied non-loopback request")
	require.Zero(t, dials, "privacy and unknown external targets must be intercepted before dialing")
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://example.invalid/redirected", http.StatusFound)
	}))
	defer redirect.Close()
	client, err = newTestPrivacyClientFactory()("")
	require.NoError(t, err)
	externalDials := 0
	client.SetDial(func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			externalDials++
			return nil, errors.New("redirect must be intercepted before dialing")
		}
		return (&net.Dialer{}).DialContext(ctx, network, address)
	})
	_, err = client.R().Get(redirect.URL)
	require.ErrorContains(t, err, "test denied non-loopback request")
	require.Zero(t, externalDials)
}

func TestCodexRefreshWebRequestsRemainCompatible(t *testing.T) {
	codexCanonicalUAMu.Lock()
	oldWeb := codexWebUAResolver
	codexWebUAResolver = func() string { return buildCodexCLIUserAgent("0.163.0") }
	codexCanonicalUAMu.Unlock()
	t.Cleanup(func() {
		codexCanonicalUAMu.Lock()
		codexWebUAResolver = oldWeb
		codexCanonicalUAMu.Unlock()
	})
	server := newChatGPTBackendTestServer(t, chatGPTBackendTestServerConfig{
		accountsCheck: map[string]any{"accounts": map[string]any{"personal": map[string]any{"account": map[string]any{"account_id": "personal", "plan_type": "pro", "is_default": true}}}},
		onSubscription: func(id string) map[string]any {
			require.Equal(t, "personal", id)
			return map[string]any{"plan_type": "pro", "active_until": "2027-01-01T00:00:00Z", "will_renew": true}
		},
	})
	defer server.Close()
	var requests []*http.Request
	factory := func(proxy string) (*req.Client, error) {
		client, err := newTestPrivacyClientFactory()(proxy)
		if err != nil {
			return nil, err
		}
		client.Transport.WrapRoundTripFunc(func(next http.RoundTripper) req.HttpRoundTripFunc {
			return func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.Clone(r.Context()))
				if r.Body != nil {
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					require.Empty(t, body)
					r.Body = io.NopCloser(strings.NewReader(string(body)))
				}
				return next.RoundTrip(r)
			}
		})
		return client, nil
	}
	svc := &OpenAIOAuthService{privacyClientFactory: factory}
	info := &OpenAITokenInfo{AccessToken: "synthetic-web-token", ChatGPTAccountID: "personal", PlanType: "pro"}
	svc.enrichTokenInfo(context.Background(), info, "")
	require.Equal(t, "2027-01-01T00:00:00Z", info.SubscriptionExpiresAt)
	require.Equal(t, PrivacyModeTrainingOff, info.PrivacyMode)
	require.Len(t, requests, 3, "one existing enrichment retains accounts, subscription and privacy calls")
	for _, r := range requests {
		require.Equal(t, "Bearer synthetic-web-token", r.Header.Get("Authorization"))
		require.Equal(t, buildCodexCLIUserAgent("0.163.0"), r.Header.Get("User-Agent"))
		require.Empty(t, r.Header.Get("OpenAI-Beta"))
	}
	require.Equal(t, "GET /backend-api/accounts/check/v4-2023-04-27", requests[0].Method+" "+requests[0].URL.Path)
	require.Equal(t, "GET /backend-api/subscriptions", requests[1].Method+" "+requests[1].URL.Path)
	require.Equal(t, "personal", requests[1].URL.Query().Get("account_id"))
	require.Equal(t, "PATCH /backend-api/settings/account_user_setting", requests[2].Method+" "+requests[2].URL.Path)
	require.Equal(t, "feature=training_allowed&value=false", requests[2].URL.RawQuery)
}
