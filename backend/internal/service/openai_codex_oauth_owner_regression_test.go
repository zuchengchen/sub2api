//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

// Two completed attempts may deliver different blobs under one client session.
// A provenance table that stores only session -> latest account confuses both
// older in-flight turns and child threads with the newer blob's owner.
func TestCodexOAuthIsolationTurnStateTracksActualBlob(t *testing.T) {
	for _, sameThread := range []bool{false, true} {
		name := "distinct_threads"
		if sameThread {
			name = "older_blob_same_thread"
		}
		t.Run(name, func(t *testing.T) {
			svc := &OpenAIGatewayService{}
			a := &Account{ID: 101}
			b := &Account{ID: 102}
			ca, _ := newTurnStateTestContext(t, 7, "shared-client-session")
			cb, _ := newTurnStateTestContext(t, 7, "shared-client-session")
			ca.Request.Header.Set("thread-id", "thread-a")
			cb.Request.Header.Set("thread-id", "thread-b")
			if sameThread {
				cb.Request.Header.Set("thread-id", "thread-a")
			}
			headerA := http.Header{}
			headerA.Set(openAICodexTurnStateHeader, "synthetic-blob-a")
			headerB := http.Header{}
			headerB.Set(openAICodexTurnStateHeader, "synthetic-blob-b")
			svc.relayOpenAICodexTurnState(ca, a, headerA)
			svc.relayOpenAICodexTurnState(cb, b, headerB)

			toA := headerA.Clone()
			svc.guardOpenAICodexTurnStateEcho(ca, a, toA)
			require.Equal(t, "synthetic-blob-a", toA.Get(openAICodexTurnStateHeader), "another committed blob must not replace the provenance of blob-a")
			toB := headerA.Clone()
			svc.guardOpenAICodexTurnStateEcho(ca, b, toB)
			require.Empty(t, toB.Get(openAICodexTurnStateHeader), "blob-a must remain bound to account A after B commits blob-b")
		})
	}
}

func auditChangedOAuthUserFixture() (*Account, *openAIRecoveryRepo, *openAIRecoveryExecutor, *OpenAITokenProvider) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	account.Credentials["chatgpt_user_id"] = "synthetic-user-a"
	latest := snapshotOAuthRefreshAccount(account)
	latest.Credentials["chatgpt_user_id"] = "synthetic-user-b"
	latest.Credentials["access_token"] = "synthetic-new-owner-access"
	latest.Credentials["refresh_token"] = "synthetic-new-owner-refresh"
	repo.account = latest
	return account, repo, executor, provider
}

// Official AuthOwner includes both workspace and user, even without experimental
// runtime agent identity. Request retry state must not cross a user replacement.
func TestCodexOAuthIsolationOAuthRecoveryRejectsChangedUser(t *testing.T) {
	t.Run("guarded_reload", func(t *testing.T) {
		account, _, executor, provider := auditChangedOAuthUserFixture()
		svc := &OpenAIGatewayService{openAITokenProvider: provider}
		headers := http.Header{"Authorization": {"Bearer rejected-access"}}
		_, _, recovered := svc.reloadOpenAIOAuthHeaders(context.Background(), account, headers)
		require.False(t, recovered, "same workspace does not mean same authenticated user")
		require.Zero(t, executor.calls.Load())
	})
	t.Run("delayed_dial", func(t *testing.T) {
		account, _, executor, provider := auditChangedOAuthUserFixture()
		svc := &OpenAIGatewayService{openAITokenProvider: provider}
		headers := http.Header{"Authorization": {"Bearer rejected-access"}}
		_, err := svc.refreshOpenAIWSAuthHeaders(context.Background(), account, headers)
		require.Error(t, err, "a delayed dial must rebuild its identity and request state after an owner change")
		require.Zero(t, executor.calls.Load())
	})
	t.Run("unauthorized_refresh_race", func(t *testing.T) {
		account, _, executor, provider := auditChangedOAuthUserFixture()
		token, err := provider.RefreshAfterUnauthorized(context.Background(), account, "rejected-access")
		require.Error(t, err, "a replacement user's durable token is not a concurrent same-owner refresh")
		require.Empty(t, token)
		require.Zero(t, executor.calls.Load())
	})
	t.Run("proactive_refresh_reread", func(t *testing.T) {
		account, _, executor, provider := auditChangedOAuthUserFixture()
		account.Credentials["access_token"] = codexTestJWTExpiry(time.Now().Add(-time.Minute).Unix())
		account.Credentials["expires_at"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		token, err := provider.GetAccessToken(context.Background(), account)
		require.Error(t, err, "reread must not return a different owner's token for the old request snapshot")
		require.Empty(t, token)
		require.Zero(t, executor.calls.Load())
	})
}

// Device/off configurations are supported. Owner isolation must not depend on
// full fingerprint synthesis accidentally changing session headers.
func TestCodexOAuthIsolationWSPoolDoesNotReuseDifferentOAuthOwner(t *testing.T) {
	for _, mode := range []string{"off", "device"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 2
			cfg.Gateway.OpenAIWS.MinIdlePerAccount = 0
			cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 2
			pool := newOpenAIWSConnPool(cfg)
			t.Cleanup(pool.Close)
			dialer := &openAIWSCountingDialer{}
			pool.setClientDialerForTest(dialer)
			account, _, _, _ := auditChangedOAuthUserFixture()
			account.Extra = map[string]any{
				codexFingerprintModeExtraKey: mode,
				codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
			}
			headers := stableOpenAIWSIdentityHeadersForTest()
			headers.Set("Authorization", "Bearer rejected-access")
			first, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
				Account: account,
				WSURL:   "wss://example.invalid/responses",
				Headers: headers,
			})
			require.NoError(t, err)
			firstID := first.ConnID()
			first.Release()

			replacement := snapshotOAuthRefreshAccount(account)
			replacement.Credentials["chatgpt_user_id"] = "synthetic-user-b"
			replacement.Credentials["access_token"] = "synthetic-new-owner-access"
			nextHeaders := headers.Clone()
			nextHeaders.Set("Authorization", "Bearer synthetic-new-owner-access")
			second, err := pool.Acquire(context.Background(), openAIWSAcquireRequest{
				Account: replacement,
				WSURL:   "wss://example.invalid/responses",
				Headers: nextHeaders,
			})
			require.NoError(t, err)
			defer second.Release()
			require.False(t, second.Reused(), "a websocket authenticated as user A cannot execute user B's request")
			require.NotEqual(t, firstID, second.ConnID())
			require.Equal(t, 2, dialer.DialCount())
		})
	}
}

// A cold models request is an authenticated first-party request too. It must
// not use the stale Account snapshot while the ordinary token provider would
// proactively refresh it. This uses only a loopback httptest server.
func TestCodexOAuthIsolationModelsUsesFreshOAuthToken(t *testing.T) {
	account, repo, executor, provider := newOpenAIRecoveryFixture()
	account.Credentials["chatgpt_user_id"] = "synthetic-user-a"
	account.Credentials["access_token"] = codexTestJWTExpiry(time.Now().Add(4 * time.Minute).Unix())
	account.Credentials["expires_at"] = time.Now().Add(4 * time.Minute).UTC().Format(time.RFC3339)
	repo.account = snapshotOAuthRefreshAccount(account)
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.5","display_name":"GPT-5.5"}]}`))
	}))
	defer server.Close()
	previousURL := chatgptCodexModelsURL
	chatgptCodexModelsURL = server.URL
	defer func() { chatgptCodexModelsURL = previousURL }()
	svc := &OpenAIGatewayService{openAITokenProvider: provider}
	_, err := svc.FetchCodexModelsManifest(context.Background(), account, "0.166.0", "")
	require.NoError(t, err)
	require.Equal(t, int32(1), executor.calls.Load(), "GET models must share ordinary proactive token freshness")
	require.Equal(t, "Bearer repaired-access", gotAuth)
}
