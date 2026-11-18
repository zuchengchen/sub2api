package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/httpclient"
)

// Endpoints advertised by https://auth.openai.com/.well-known/openid-configuration.
const (
	RegisteredIssuer           = "https://auth.openai.com"
	RegisteredAuthorizeURL     = RegisteredIssuer + "/api/accounts/authorize"
	RegisteredTokenURL         = RegisteredIssuer + "/api/accounts/oauth/token"
	RegisteredJWKSURL          = RegisteredIssuer + "/.well-known/jwks.json"
	KeyPouchClientRegistration = "keypouch"
)

// KeyPouchRegistration is a deployment-owned public client. Its callback must
// be the exact registered address of Ant's dedicated loopback receiver.
type KeyPouchRegistration struct {
	ClientID    string
	RedirectURI string
}

func (r KeyPouchRegistration) Valid() bool {
	if r.ClientID == "" || r.ClientID == ClientID || len(r.ClientID) > 255 || strings.ContainsAny(r.ClientID, " \r\n\t") {
		return false
	}
	u, err := url.Parse(r.RedirectURI)
	if err != nil || u.Scheme != "http" || u.User != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "::1") {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	return r.RedirectURI == "http://"+net.JoinHostPort(u.Hostname(), strconv.Itoa(port))+"/keypouch/oauth/callback"
}

func (r KeyPouchRegistration) AuthorizationURL(state, verifier, nonce string) string {
	q := url.Values{
		"client_id": {r.ClientID}, "redirect_uri": {r.RedirectURI}, "response_type": {"code"},
		"scope": {DefaultScopes}, "state": {state}, "nonce": {nonce},
		"code_challenge": {GenerateCodeChallenge(verifier)}, "code_challenge_method": {"S256"},
		"prompt": {"consent"},
	}
	return RegisteredAuthorizeURL + "?" + q.Encode()
}

func (r KeyPouchRegistration) Exchange(ctx context.Context, code, verifier string) (*TokenResponse, error) {
	if !r.Valid() {
		return nil, errors.New("OAuth registration unavailable")
	}
	return registeredTokenGrant(ctx, "", url.Values{"grant_type": {"authorization_code"}, "client_id": {r.ClientID},
		"redirect_uri": {r.RedirectURI}, "code": {code}, "code_verifier": {verifier}})
}

// Refresh uses the exact public client that obtained this account's grant.
func RefreshKeyPouchGrant(ctx context.Context, clientID, refreshToken, proxyURL string) (*TokenResponse, error) {
	if clientID == "" || clientID == ClientID || refreshToken == "" {
		return nil, errors.New("missing registered OAuth grant")
	}
	return registeredTokenGrant(ctx, proxyURL, url.Values{"grant_type": {"refresh_token"}, "client_id": {clientID}, "refresh_token": {refreshToken}})
}

// KeyPouchCredentialClaims describes the provider-specific identity on a grant.
// Decode only after verifying a fresh ID token, or to veto inconsistent
// metadata on an already trusted account. This function does not verify a JWT.
type KeyPouchCredentialClaims struct {
	Email   string            `json:"email"`
	Auth    *OpenAIAuthClaims `json:"https://api.openai.com/auth"`
	Profile struct {
		Email string `json:"email"`
	} `json:"https://api.openai.com/profile"`
}

func DecodeKeyPouchCredentialClaims(token string) (*KeyPouchCredentialClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid JWT")
	}
	return DecodeKeyPouchCredentialPayload(parts[1])
}

// DecodeKeyPouchCredentialPayload accepts just a payload segment, so a
// legacy lookup does not need to retrieve usable credentials from storage.
func DecodeKeyPouchCredentialPayload(segment string) (*KeyPouchCredentialClaims, error) {
	payload, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return nil, errors.New("invalid JWT payload")
	}
	var claims KeyPouchCredentialClaims
	if json.Unmarshal(payload, &claims) != nil {
		return nil, errors.New("invalid JWT claims")
	}
	return &claims, nil
}

func (c *KeyPouchCredentialClaims) AgreesWith(email, workspace, user string) bool {
	for _, value := range []string{c.Email, c.Profile.Email} {
		if value != "" && email != "" && !strings.EqualFold(strings.TrimSpace(value), email) {
			return false
		}
	}
	if c.Auth == nil {
		return true
	}
	tokenUser := c.Auth.ChatGPTUserID
	if tokenUser == "" {
		tokenUser = c.Auth.UserID
	}
	return (c.Auth.ChatGPTAccountID == "" || workspace == "" || workspace == c.Auth.ChatGPTAccountID) &&
		(tokenUser == "" || user == "" || user == tokenUser)
}

func registeredTokenGrant(ctx context.Context, proxyURL string, form url.Values) (*TokenResponse, error) {
	shared, err := httpclient.GetClient(httpclient.Options{Timeout: 30 * time.Second, ProxyURL: proxyURL})
	if err != nil {
		return nil, errors.New("OAuth transport unavailable")
	}
	return sendRegisteredTokenGrant(ctx, form, shared)
}

func sendRegisteredTokenGrant(ctx context.Context, form url.Values, shared *http.Client) (*TokenResponse, error) {
	// A 307/308 must never carry a code, verifier or refresh token elsewhere.
	client := *shared
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, RegisteredTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("OAuth request unavailable")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	response, err := client.Do(req)
	if err != nil {
		return nil, errors.New("OAuth provider unavailable")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, errors.New("OAuth response unavailable")
	}
	if response.StatusCode != http.StatusOK {
		return nil, NewRefreshTokenError(response.StatusCode, body)
	}
	var token TokenResponse
	if err := json.Unmarshal(body, &token); err != nil || token.AccessToken == "" || !strings.EqualFold(token.TokenType, "Bearer") {
		return nil, errors.New("invalid OAuth token response")
	}
	return &token, nil
}
