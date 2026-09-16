package openai

import (
	"context"
	"net/url"
	"testing"
	"time"
)

func TestSessionStoreCleanupExpiredSessionsPreservesTTLBoundary(t *testing.T) {
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

func TestBuildAuthorizationURLForPlatform_OpenAI(t *testing.T) {
	authURL := BuildAuthorizationURLForPlatform("state-1", "challenge-1", DefaultRedirectURI, OAuthPlatformOpenAI)
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("Parse URL failed: %v", err)
	}
	q := parsed.Query()
	if got := q.Get("client_id"); got != ClientID {
		t.Fatalf("client_id mismatch: got=%q want=%q", got, ClientID)
	}
	if got := q.Get("codex_cli_simplified_flow"); got != "true" {
		t.Fatalf("codex flow mismatch: got=%q want=true", got)
	}
	if got := q.Get("id_token_add_organizations"); got != "true" {
		t.Fatalf("id_token_add_organizations mismatch: got=%q want=true", got)
	}
}
