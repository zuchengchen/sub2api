//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestCodexSessionIdentityPinsVersionAcrossRequestsAndReconnect(t *testing.T) {
	codexCanonicalUAMu.RLock()
	previous := codexCanonicalUAResolver
	codexCanonicalUAMu.RUnlock()
	t.Cleanup(func() { SetCodexCanonicalUserAgentResolver(previous) })
	codexSessionIdentities.Lock()
	previousEntries := codexSessionIdentities.entries
	codexSessionIdentities.entries = make(map[string]codexSessionIdentitySnapshot)
	codexSessionIdentities.Unlock()
	t.Cleanup(func() {
		codexSessionIdentities.Lock()
		codexSessionIdentities.entries = previousEntries
		codexSessionIdentities.Unlock()
	})
	newContext := func(key int64, session, thread string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.Header.Set("session-id", session)
		c.Request.Header.Set("thread-id", thread)
		c.Set("api_key", &APIKey{ID: key})
		return c
	}
	svc := &OpenAIGatewayService{}
	account := newCodexSynthTestAccount(99173, AccountTypeOAuth)
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.163.0") })
	first := newContext(12, "session-original", "child-original")
	profile, ok := svc.codexWireProfileForRequest(first, account, nil)
	require.True(t, ok)
	require.Equal(t, "0.163", profile.Name)
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.164.0") })
	for _, c := range []*gin.Context{first, newContext(12, "session-original", "child-original")} {
		svc.codexWireProfileForRequest(c, account, nil)
		headers := http.Header{}
		svc.applyCodexSessionIdentity(c, account, []byte(`{"client_metadata":{"session_id":"already-rewritten"}}`), headers)
		require.Equal(t, "0.163.0", headers.Get("version"))
		require.Contains(t, headers.Get("User-Agent"), "/0.163.0")
		profile, _ = svc.codexWireProfileForRequest(c, account, nil)
		require.Equal(t, "0.163", profile.Name)
	}
	for _, c := range []*gin.Context{newContext(13, "session-original", "child-original"), newContext(12, "new-session", "new-thread")} {
		require.Equal(t, "0.164.0", svc.codexSessionClientVersion(c, account, nil))
	}
	otherAccount := newCodexSynthTestAccount(99174, AccountTypeOAuth)
	otherAccount.Credentials = map[string]any{"chatgpt_account_id": "other-credential"}
	require.Equal(t, "0.164.0", svc.codexSessionClientVersion(newContext(12, "session-original", "child-original"), otherAccount, nil))
	// Context snapshots stay pinned even if the bounded idle cache is cleaned.
	codexSessionIdentities.Lock()
	for key, entry := range codexSessionIdentities.entries {
		entry.lastUsed = time.Now().Add(-2 * codexSessionIdentityIdleTTL)
		codexSessionIdentities.entries[key] = entry
	}
	codexSessionIdentities.Unlock()
	require.Equal(t, "0.163.0", svc.codexSessionClientVersion(first, account, nil))
	// A temporary HTTP session and a shared cache key must not create cache entries.
	for _, session := range []string{"", "anonymous-http"} {
		c := newContext(12, session, "")
		if session != "" {
			c.Set("codex_anonymous_http_session", session)
		}
		codexSessionIdentities.Lock()
		count := len(codexSessionIdentities.entries)
		codexSessionIdentities.Unlock()
		SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.163.0") })
		require.Equal(t, "0.163.0", svc.codexSessionClientVersion(c, account, []byte(`{"prompt_cache_key":"shared"}`)))
		SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent("0.164.0") })
		require.Equal(t, "0.163.0", svc.codexSessionClientVersion(c, account, nil))
		codexSessionIdentities.Lock()
		after := len(codexSessionIdentities.entries)
		codexSessionIdentities.Unlock()
		require.Equal(t, count, after)
	}
}
