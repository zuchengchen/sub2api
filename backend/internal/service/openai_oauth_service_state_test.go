package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

type openaiOAuthClientStateStub struct {
	exchangeCalled int32
	lastClientID   string
	idToken        string
}

func (s *openaiOAuthClientStateStub) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	atomic.AddInt32(&s.exchangeCalled, 1)
	s.lastClientID = clientID
	return &openai.TokenResponse{
		AccessToken:  "at",
		RefreshToken: "rt",
		IDToken:      s.idToken,
		ExpiresIn:    3600,
	}, nil
}

func (s *openaiOAuthClientStateStub) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	if s.idToken != "" {
		return &openai.TokenResponse{AccessToken: "at", IDToken: s.idToken, ExpiresIn: 3600}, nil
	}
	return nil, errors.New("not implemented")
}

func (s *openaiOAuthClientStateStub) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	return s.RefreshToken(ctx, refreshToken, proxyURL)
}

func TestOpenAIOAuthService_ExchangeCode_StateRequired(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("sid", &openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "oauth state is required")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMismatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("sid", &openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	})

	_, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "wrong-state",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid oauth state")
	require.Equal(t, int32(0), atomic.LoadInt32(&client.exchangeCalled))
}

func TestOpenAIOAuthService_ExchangeCode_StateMatch(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()

	svc.sessionStore.Set("sid", &openai.OAuthSession{
		State:        "expected-state",
		CodeVerifier: "verifier",
		RedirectURI:  openai.DefaultRedirectURI,
		CreatedAt:    time.Now(),
	})

	info, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{
		SessionID: "sid",
		Code:      "auth-code",
		State:     "expected-state",
	})
	require.NoError(t, err)
	require.NotNil(t, info)
	require.Equal(t, "at", info.AccessToken)
	require.Equal(t, openai.ClientID, info.ClientID)
	require.Equal(t, openai.ClientID, client.lastClientID)
	require.Equal(t, int32(1), atomic.LoadInt32(&client.exchangeCalled))

	_, ok := svc.sessionStore.Get("sid")
	require.False(t, ok)
}

func TestOpenAIOAuthService_SubscriptionExpiryFromIDToken(t *testing.T) {
	const expiry = "2026-11-20T15:01:57+00:00"
	payload, err := json.Marshal(map[string]any{
		"exp": time.Now().Add(time.Hour).Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                "subscription-account",
			"chatgpt_user_id":                   "subscription-user",
			"chatgpt_plan_type":                 "prolite",
			"chatgpt_subscription_active_until": expiry,
		},
	})
	require.NoError(t, err)
	idToken := "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
	for _, mode := range []string{"exchange", "refresh", "throttled refresh", "access token only", "saved subscription", "saved subscription throttled"} {
		t.Run(mode, func(t *testing.T) {
			const key = "subscription-account|subscription-user"
			openAIRefreshEnrichLast.Delete(key)
			t.Cleanup(func() { openAIRefreshEnrichLast.Delete(key) })
			if mode == "throttled refresh" || mode == "saved subscription throttled" {
				openAIRefreshEnrichLast.Store(key, time.Now())
			}
			svc := NewOpenAIOAuthService(nil, &openaiOAuthClientStateStub{idToken: idToken})
			defer svc.Stop()
			svc.SetPrivacyClientFactory(func(string) (*req.Client, error) { return nil, errors.New("upstream unavailable") })
			var info *OpenAITokenInfo
			var err error
			want := expiry
			switch mode {
			case "exchange":
				svc.sessionStore.Set("sid", &openai.OAuthSession{State: "state", CreatedAt: time.Now()})
				info, err = svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{SessionID: "sid", State: "state", Code: "code"})
			case "access token only":
				info, err = svc.RefreshAccountToken(context.Background(), &Account{
					Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Credentials: map[string]any{"access_token": "at", "id_token": idToken, "plan_type": "prolite"},
				})
			case "saved subscription", "saved subscription throttled":
				want = "2026-12-20T00:00:00Z"
				info, err = svc.RefreshAccountToken(context.Background(), &Account{
					Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Credentials: map[string]any{
						"refresh_token": "rt", "plan_type": "prolite", "chatgpt_account_id": "subscription-account",
						"subscription_expires_at": want,
					},
				})
			default:
				info, err = svc.RefreshToken(context.Background(), "rt", "")
			}
			require.NoError(t, err)
			require.Equal(t, want, info.SubscriptionExpiresAt)
			require.Equal(t, want, svc.BuildAccountCredentials(info)["subscription_expires_at"])
		})
	}
}
