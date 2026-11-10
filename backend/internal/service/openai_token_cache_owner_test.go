//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCodexOAuthIdentityDecodeCacheBoundedAndConcurrent(t *testing.T) {
	cache := &openAIJWTIdentityCache{}
	for i := 0; i < openAICacheJWTMaxEntries+3; i++ {
		require.Nil(t, cache.get(fmt.Sprintf("opaque-%d", i)))
	}
	require.LessOrEqual(t, len(cache.claims), openAICacheJWTMaxEntries)
	tokens := []string{
		codexOAuthOwnerJWT(t, map[string]any{"chatgpt_user_id": "user-a"}, "snapshot"),
		codexOAuthOwnerJWT(t, map[string]any{"chatgpt_user_id": "user-b"}, "replacement"),
	}
	var workers sync.WaitGroup
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for iteration := 0; iteration < 50; iteration++ {
				for index, token := range tokens {
					got := cache.get(token).userID()
					if got != []string{"user-a", "user-b"}[index] {
						t.Errorf("cached identity crossed token revisions: %q", got)
					}
				}
			}
		}()
	}
	workers.Wait()
	oversized := codexOAuthOwnerJWT(t, map[string]any{"chatgpt_user_id": "large-user"}, strings.Repeat("x", openAICacheJWTMaxTokenBytes))
	before := len(cache.claims)
	require.Equal(t, "large-user", cache.get(oversized).userID())
	require.Len(t, cache.claims, before, "oversized tokens are decoded without retention")
	_, retained := cache.claims[oversized]
	require.False(t, retained)
}

type codexOAuthOwnerRepo struct {
	mockAccountRepoForTest
	stub *openAIAccountRepoStub
}

func (r *codexOAuthOwnerRepo) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.stub.GetByID(ctx, id)
}

func codexOAuthOwnerJWT(t *testing.T, claims any, revision string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"https://api.openai.com/auth": claims,
		"exp":                         time.Now().Add(time.Hour).Unix(),
		"aud":                         "aud-is-allowed-to-be-a-string",
		"jti":                         revision,
	})
	require.NoError(t, err)
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
}

func codexOAuthOwnerAccount(t *testing.T, user string) *Account {
	t.Helper()
	return &Account{ID: 9105, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive,
		Credentials: map[string]any{
			"chatgpt_account_id": "workspace-a",
			"chatgpt_user_id":    user,
			"refresh_token":      "synthetic-refresh",
			"access_token": codexOAuthOwnerJWT(t, map[string]any{
				"chatgpt_account_id": "workspace-a", "chatgpt_user_id": user,
			}, "snapshot"),
		}}
}

func TestCodexOAuthTokenCacheOwnerClaims(t *testing.T) {
	account := codexOAuthOwnerAccount(t, "user-a")
	for _, tc := range []struct {
		name   string
		claims any
		want   bool
	}{
		{"same_owner", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": "user-a"}, true},
		{"fallback_user_id", map[string]any{"chatgpt_account_id": "workspace-a", "user_id": "user-a"}, true},
		{"null_primary_falls_back", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": nil, "user_id": "user-a"}, true},
		{"different_user", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": "user-b"}, false},
		{"different_workspace", map[string]any{"chatgpt_account_id": "workspace-b", "chatgpt_user_id": "user-a"}, false},
		{"primary_wins", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": "user-b", "user_id": "user-a"}, false},
		{"empty_primary_is_unknown", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": "", "user_id": "user-a"}, false},
		{"unknown_user", map[string]any{"chatgpt_account_id": "workspace-a"}, false},
		{"unknown_workspace", map[string]any{"chatgpt_user_id": "user-a"}, false},
		{"membership_not_user", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_account_user_id": "user-a"}, false},
		{"invalid_type", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": 42}, false},
		{"trimmed_collision", map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": " user-a "}, false},
		{"null_auth", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := codexOAuthOwnerJWT(t, tc.claims, "cached")
			require.Equal(t, tc.want, openAICachedAccessTokenMatchesAccount(account, token))
		})
	}
	for _, malformed := range []string{"", "opaque-token", "e30.!.sig", "e30.e30.sig.extra", "e30.W10.sig"} {
		require.False(t, openAICachedAccessTokenMatchesAccount(account, malformed))
	}
	unknown := &Account{ID: 9105, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "opaque-current", "refresh_token": "synthetic-refresh"}}
	require.True(t, openAICachedAccessTokenMatchesAccount(unknown, "opaque-current"))
	require.False(t, openAICachedAccessTokenMatchesAccount(unknown, "opaque-different"))
}

func TestCodexOAuthTokenCacheUsesOwnerWithoutHotPathDBRead(t *testing.T) {
	for _, changedOwner := range []bool{false, true} {
		name := "same_owner_cache_hit"
		if changedOwner {
			name = "different_owner_cache_miss"
		}
		t.Run(name, func(t *testing.T) {
			account := codexOAuthOwnerAccount(t, "user-a")
			cachedUser := "user-a"
			if changedOwner {
				cachedUser = "user-b"
			}
			cachedToken := codexOAuthOwnerJWT(t, map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": cachedUser}, "cached")
			cache := newOpenAITokenCacheStub()
			cache.tokens[OpenAITokenCacheKey(account)] = cachedToken
			repo := &openAIAccountRepoStub{account: snapshotOAuthRefreshAccount(account)}
			provider := NewOpenAITokenProvider(&codexOAuthOwnerRepo{stub: repo}, cache, nil)
			token, err := provider.GetAccessToken(context.Background(), account)
			require.NoError(t, err)
			if changedOwner {
				require.Equal(t, account.GetOpenAIAccessToken(), token)
				require.Equal(t, int32(1), atomic.LoadInt32(&repo.getCalled), "only the ordinary miss/version-check path reads DB")
			} else {
				require.Equal(t, cachedToken, token)
				require.Zero(t, atomic.LoadInt32(&repo.getCalled), "valid same-owner hit must not add a DB read")
			}
		})
	}
}

func TestCodexOAuthTokenCacheMismatchDoesNotDeleteNewOwnerEntry(t *testing.T) {
	account := codexOAuthOwnerAccount(t, "user-a")
	newOwner := codexOAuthOwnerAccount(t, "user-b")
	cache := newOpenAITokenCacheStub()
	cache.tokens[OpenAITokenCacheKey(account)] = newOwner.GetOpenAIAccessToken()
	repo := &openAIAccountRepoStub{account: newOwner}
	provider := NewOpenAITokenProvider(&codexOAuthOwnerRepo{stub: repo}, cache, nil)
	token, err := provider.GetAccessToken(context.Background(), account)
	require.Error(t, err, "latest-account owner guard must reject even when both token versions are zero")
	require.Empty(t, token)
	require.Equal(t, newOwner.GetOpenAIAccessToken(), cache.tokens[OpenAITokenCacheKey(account)], "stale reader must not evict the new owner's valid cache")
}

type codexOAuthOwnerWaitCache struct {
	TokenCache
	token  string
	cancel context.CancelFunc
}

func (cache codexOAuthOwnerWaitCache) GetAccessToken(context.Context, string) (string, error) {
	cache.cancel()
	return cache.token, nil
}

func TestCodexOAuthTokenCacheWaitRejectsForeignOwner(t *testing.T) {
	account := codexOAuthOwnerAccount(t, "user-a")
	foreign := codexOAuthOwnerAccount(t, "user-b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cache := codexOAuthOwnerWaitCache{token: foreign.GetOpenAIAccessToken(), cancel: cancel}
	provider := NewOpenAITokenProvider(nil, cache, nil)
	token, err := provider.waitForTokenAfterLockRace(ctx, account, OpenAITokenCacheKey(account))
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, token)
}

func TestCodexOAuthTokenCachePreservesLockAndInvalidationKey(t *testing.T) {
	a := codexOAuthOwnerAccount(t, "user-a")
	b := codexOAuthOwnerAccount(t, "user-b")
	executor := &OpenAITokenRefresher{}
	require.Equal(t, "openai:account:9105", executor.CacheKey(a))
	require.Equal(t, executor.CacheKey(a), executor.CacheKey(b), "owner changes must not split cross-process refresh locks")
	cache := newOpenAITokenCacheStub()
	cache.tokens[OpenAITokenCacheKey(a)] = b.GetOpenAIAccessToken()
	require.NoError(t, NewCompositeTokenCacheInvalidator(cache).InvalidateToken(context.Background(), b))
	require.Empty(t, cache.tokens)
}

func TestCodexOAuthTokenOwnerUserIDFallback(t *testing.T) {
	a := codexOAuthOwnerAccount(t, "user-a")
	delete(a.Credentials, "chatgpt_user_id")
	a.Credentials["id_token"] = codexOAuthOwnerJWT(t, map[string]any{"user_id": "id-user"}, "id")
	require.Equal(t, "id-user", a.GetChatGPTUserID())
	a.Credentials["chatgpt_user_id"] = "saved-user"
	require.Equal(t, "id-user", a.GetChatGPTUserID(), "current ID-token user takes priority over stale metadata retained by a reauth merge")
	delete(a.Credentials, "chatgpt_user_id")
	a.Credentials["id_token"] = "invalid-id-token"
	require.Empty(t, a.GetChatGPTUserID(), "a malformed managed ID token cannot silently select access-token identity")
	delete(a.Credentials, "id_token")
	require.Equal(t, "user-a", a.GetChatGPTUserID(), "token-only import can use access-token claims")
}

func TestCodexOAuthUnknownOAuthOwnerUsesCredentialFingerprint(t *testing.T) {
	unknown := &Account{ID: 9105, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "opaque-current", "refresh_token": "synthetic-refresh"}}
	unchanged := snapshotOAuthRefreshAccount(unknown)
	require.True(t, sameOpenAIOAuthOwner(unknown, unchanged))
	for _, field := range []string{"access_token", "refresh_token", "id_token"} {
		changed := snapshotOAuthRefreshAccount(unknown)
		changed.Credentials[field] = "synthetic-replacement"
		require.False(t, sameOpenAIOAuthOwner(unknown, changed), field)
	}
	known := codexOAuthOwnerAccount(t, "user-a")
	rotated := snapshotOAuthRefreshAccount(known)
	rotated.Credentials["access_token"] = codexOAuthOwnerJWT(t, map[string]any{"chatgpt_account_id": "workspace-a", "chatgpt_user_id": "user-a"}, "rotated")
	rotated.Credentials["refresh_token"] = "synthetic-rotated-refresh"
	require.True(t, sameOpenAIOAuthOwner(known, rotated), "complete same-owner identity must survive normal refresh")
}
