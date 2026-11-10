package service

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
)

// These claims are local cache-continuity hints, not authentication evidence.
// A narrow decoder deliberately ignores unrelated claims such as aud, whose
// wire type may differ between access tokens and ID tokens.
type openAICacheJWTAuthClaims struct {
	ChatGPTAccountID *string `json:"chatgpt_account_id"`
	ChatGPTUserID    *string `json:"chatgpt_user_id"`
	UserID           *string `json:"user_id"`
}

const openAICacheJWTMaxEntries = 1024
const openAICacheJWTMaxTokenBytes = 16 << 10

type openAIJWTIdentityCache struct {
	mu     sync.RWMutex
	claims map[string]*openAICacheJWTAuthClaims
}

var openAIJWTIdentities = openAIJWTIdentityCache{claims: make(map[string]*openAICacheJWTAuthClaims)}

func openAICacheJWTClaims(token string) *openAICacheJWTAuthClaims {
	return openAIJWTIdentities.get(token)
}

// Token text determines these immutable hints. Cache decoding only, never a
// decision about whether a token belongs to the current account or is valid.
// Bound entries and token sizes so rotations cannot retain unlimited secrets.
func (cache *openAIJWTIdentityCache) get(token string) *openAICacheJWTAuthClaims {
	if len(token) > openAICacheJWTMaxTokenBytes {
		return decodeOpenAICacheJWTClaims(token)
	}
	cache.mu.RLock()
	claims, found := cache.claims[token]
	cache.mu.RUnlock()
	if found {
		return claims
	}
	claims = decodeOpenAICacheJWTClaims(token)
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if existing, ok := cache.claims[token]; ok {
		return existing
	}
	if cache.claims == nil {
		cache.claims = make(map[string]*openAICacheJWTAuthClaims)
	} else if len(cache.claims) >= openAICacheJWTMaxEntries {
		clear(cache.claims)
	}
	cache.claims[token] = claims
	return claims
}

func decodeOpenAICacheJWTClaims(token string) *openAICacheJWTAuthClaims {
	_, remaining, ok := strings.Cut(token, ".")
	if !ok {
		return nil
	}
	payload, signature, ok := strings.Cut(remaining, ".")
	if !ok || payload == "" || strings.Contains(signature, ".") {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		decoded, err = base64.URLEncoding.DecodeString(payload)
		if err != nil {
			return nil
		}
	}
	var claims struct {
		Auth *openAICacheJWTAuthClaims `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(decoded, &claims) != nil {
		return nil
	}
	return claims.Auth
}

func openAICacheClaimValue(value *string) string {
	if value == nil || *value == "" || strings.TrimSpace(*value) != *value {
		return ""
	}
	return *value
}

func (claims *openAICacheJWTAuthClaims) userID() string {
	if claims == nil {
		return ""
	}
	// Official token_data.rs uses Option::or: an explicitly empty primary
	// claim is unknown identity, not permission to use a conflicting fallback.
	if claims.ChatGPTUserID != nil {
		return openAICacheClaimValue(claims.ChatGPTUserID)
	}
	return openAICacheClaimValue(claims.UserID)
}

// openAICachedAccessTokenMatchesAccount keeps the row-scoped cache and refresh
// lock key intact. Reusing a different cached token requires a complete owner.
// Unknown or opaque claims are a miss, never a guessed cross-owner hit.
func openAICachedAccessTokenMatchesAccount(account *Account, token string) bool {
	if account == nil || !account.IsOpenAIOAuth() || strings.TrimSpace(token) == "" {
		return false
	}
	if token == account.GetOpenAIAccessToken() {
		return true
	}
	// A different PAT/agent credential cannot establish an ordinary OAuth
	// refresh relationship. Access-only imported tokens also use exact matching.
	if !canRecoverOpenAIOAuth(account) {
		return false
	}
	expectedWorkspace := account.GetChatGPTAccountID()
	expectedUser := account.GetChatGPTUserID()
	if strings.TrimSpace(expectedWorkspace) == "" || strings.TrimSpace(expectedUser) == "" {
		return false
	}
	claims := openAICacheJWTClaims(token)
	return claims != nil &&
		openAICacheClaimValue(claims.ChatGPTAccountID) == expectedWorkspace &&
		claims.userID() == expectedUser
}

// Managed OAuth reads its current ID-token identity; token-only imports can
// use access-token claims. Persisted display metadata may survive a reauth
// merge, so it must not override a different user in the current token. PAT,
// agent, and opaque imports retain their explicitly persisted metadata.
// An existing malformed/incomplete ID token does not select an access-token
// identity or revive stale metadata.
func openAIOAuthAccountUserID(account *Account) string {
	if account == nil || !account.IsOpenAIOAuth() {
		return ""
	}
	if account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() {
		return account.GetCredential("chatgpt_user_id")
	}
	if idToken := account.GetCredential("id_token"); idToken != "" {
		return openAICacheJWTClaims(idToken).userID()
	}
	if claims := openAICacheJWTClaims(account.GetOpenAIAccessToken()); claims != nil {
		return claims.userID()
	}
	return account.GetCredential("chatgpt_user_id")
}
