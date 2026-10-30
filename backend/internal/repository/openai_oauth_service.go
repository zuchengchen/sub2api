package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/imroc/req/v3"
)

// NewOpenAIOAuthClient creates a new OpenAI OAuth client
func NewOpenAIOAuthClient() service.OpenAIOAuthClient {
	return &openaiOAuthService{tokenURL: openai.TokenURL}
}

type openaiOAuthService struct {
	tokenURL string
}

func (s *openaiOAuthService) ExchangeCode(ctx context.Context, code, codeVerifier, redirectURI, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	if redirectURI == "" {
		redirectURI = openai.DefaultRedirectURI
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}

	formData := url.Values{}
	formData.Set("grant_type", "authorization_code")
	formData.Set("client_id", clientID)
	formData.Set("code", code)
	formData.Set("redirect_uri", redirectURI)
	formData.Set("code_verifier", codeVerifier)

	var tokenResp openai.TokenResponse

	authUA, authOriginator := service.CodexAuthIdentityForContext(ctx)
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", authUA).
		SetHeader("originator", authOriginator).
		SetFormDataFromValues(formData).
		SetSuccessResult(&tokenResp).
		Post(s.tokenURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_TOKEN_EXCHANGE_FAILED", "token exchange failed: status %d, body: %s", resp.StatusCode, resp.String())
	}

	return &tokenResp, nil
}

func (s *openaiOAuthService) RefreshToken(ctx context.Context, refreshToken, proxyURL string) (*openai.TokenResponse, error) {
	return s.RefreshTokenWithClientID(ctx, refreshToken, proxyURL, "")
}

func (s *openaiOAuthService) RefreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL string, clientID string) (*openai.TokenResponse, error) {
	// 调用方应始终传入正确的 client_id；为兼容旧数据，未指定时默认使用 OpenAI ClientID
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}
	return s.refreshTokenWithClientID(ctx, refreshToken, proxyURL, clientID)
}

func (s *openaiOAuthService) refreshTokenWithClientID(ctx context.Context, refreshToken, proxyURL, clientID string) (*openai.TokenResponse, error) {
	client, err := createOpenAIReqClient(proxyURL)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_CLIENT_INIT_FAILED", "create HTTP client: %v", err)
	}

	refreshReason := openai.CodexRefreshReasonFromContext(ctx)
	if refreshReason == "" {
		refreshReason = "forced"
	}
	refreshRequest := struct {
		ClientID      string `json:"client_id"`
		GrantType     string `json:"grant_type"`
		RefreshReason string `json:"refresh_reason"`
		RefreshToken  string `json:"refresh_token"`
	}{ClientID: clientID, GrantType: "refresh_token", RefreshReason: refreshReason, RefreshToken: refreshToken}
	refreshBody, err := json.Marshal(refreshRequest)
	if err != nil {
		return nil, err
	}

	var tokenResp openai.TokenResponse

	authUA, authOriginator := service.CodexAuthIdentityForContext(ctx)
	resp, err := client.R().
		SetContext(ctx).
		SetHeader("User-Agent", authUA).
		SetHeader("originator", authOriginator).
		SetHeader("Content-Type", "application/json").
		SetBodyBytes(refreshBody).
		SetSuccessResult(&tokenResp).
		Post(s.tokenURL)

	if err != nil {
		if shouldReturnOpenAINoProxyHint(ctx, proxyURL, err) {
			return nil, newOpenAINoProxyHintError(err)
		}
		return nil, infraerrors.Newf(http.StatusBadGateway, "OPENAI_OAUTH_REQUEST_FAILED", "request failed: %v", err)
	}

	if !resp.IsSuccessState() {
		return nil, openai.NewRefreshTokenError(resp.StatusCode, resp.Bytes())
	}
	return &tokenResp, nil
}

// RetireAuthClient replaces this account's saved auth transport after a
// successful guarded reload, including a reload of unchanged credentials.
// Retire drains active streams; it never interrupts another in-flight refresh.
func (s *openaiOAuthService) RetireAuthClient(ctx context.Context, refreshToken, clientID string) {
	if id := openai.CodexAuthAccountFromContext(ctx); id > 0 {
		// A successful reload replaces the account's auth client, including
		// snapshots loaded concurrently with a different token or device.
		codexAuthH2Pool.Retire(fmt.Sprintf("auth-account-%d", id))
		return
	}
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		clientID = openai.ClientID
	}
	body, _ := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": refreshToken})
	ua, originator := service.CodexAuthIdentityForContext(ctx)
	codexAuthH2Pool.Retire(codexAuthPoolKey(ctx, s.tokenURL, http.Header{"User-Agent": {ua}, "Originator": {originator}}, body))
}

func createOpenAIReqClient(proxyURL string) (*req.Client, error) {
	if !codexDeviceClientDisabled() {
		// auth.openai.com token calls use the Codex CLI device lane.
		return newCodexDeviceReqClientWithAuthCA(proxyURL, 120*time.Second, true)
	}
	return getSharedReqClient(reqClientOptions{
		ProxyURL: proxyURL,
		Timeout:  120 * time.Second,
	})
}

func shouldReturnOpenAINoProxyHint(ctx context.Context, proxyURL string, err error) bool {
	if strings.TrimSpace(proxyURL) != "" || err == nil {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return false
	}
	return !errors.Is(err, context.Canceled)
}

func newOpenAINoProxyHintError(cause error) error {
	return infraerrors.New(
		http.StatusBadGateway,
		"OPENAI_OAUTH_PROXY_REQUIRED",
		"OpenAI OAuth request failed: no proxy is configured and this server could not reach OpenAI directly. Select a proxy that can access OpenAI, then retry; if the authorization code has expired, regenerate the authorization URL.",
	).WithCause(cause)
}
