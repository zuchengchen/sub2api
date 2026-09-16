package oauth

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestAuthorizeURLMatchesClaudeCodeCLI(t *testing.T) {
	const want = "https://claude.com/cai/oauth/authorize"
	if AuthorizeURL != want {
		t.Fatalf("AuthorizeURL = %q, want %q", AuthorizeURL, want)
	}

	authURL := BuildAuthorizationURL("state-value", "challenge-value", ScopeOAuth)
	if !strings.HasPrefix(authURL, want+"?") {
		t.Fatalf("BuildAuthorizationURL() = %q, want prefix %q", authURL, want+"?")
	}
	for _, part := range []string{
		"code=true",
		"client_id=" + ClientID,
		"response_type=code",
		"code_challenge=challenge-value",
		"code_challenge_method=S256",
		"state=state-value",
	} {
		if !strings.Contains(authURL, part) {
			t.Fatalf("BuildAuthorizationURL() missing %q\nURL: %s", part, authURL)
		}
	}
}

func TestSessionStoreCleanupExpiredPreservesTTLBoundary(t *testing.T) {
	now := time.Date(2026, time.August, 4, 8, 0, 0, 0, time.UTC)
	store := NewSessionStore()
	store.Set("expired", &OAuthSession{CreatedAt: now.Add(-SessionTTL - time.Nanosecond)})
	store.Set("boundary", &OAuthSession{CreatedAt: now.Add(-SessionTTL)})
	store.Set("fresh", &OAuthSession{CreatedAt: now.Add(-SessionTTL + time.Nanosecond)})

	if err := store.cleanupExpiredAt(context.Background(), now); err != nil {
		t.Fatalf("cleanupExpiredAt: %v", err)
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if _, ok := store.sessions["expired"]; ok {
		t.Fatal("expired session should be removed")
	}
	if _, ok := store.sessions["boundary"]; !ok {
		t.Fatal("TTL boundary session should be kept")
	}
	if _, ok := store.sessions["fresh"]; !ok {
		t.Fatal("fresh session should be kept")
	}
}

func TestSessionStoreCleanupExpiredHonorsCancellation(t *testing.T) {
	store := NewSessionStore()
	store.Set("expired", &OAuthSession{CreatedAt: time.Now().Add(-SessionTTL - time.Second)})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.CleanupExpired(ctx); err != context.Canceled {
		t.Fatalf("CleanupExpired error = %v, want context.Canceled", err)
	}
	store.mu.RLock()
	_, ok := store.sessions["expired"]
	store.mu.RUnlock()
	if !ok {
		t.Fatal("pre-canceled cleanup must leave the expired entry")
	}
}
