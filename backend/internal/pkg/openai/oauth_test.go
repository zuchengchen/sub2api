package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

func TestIDTokenSubscriptionExpiry(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{"valid", "2026-11-20T15:01:57+00:00", "2026-11-20T15:01:57+00:00"},
		{"trimmed", " 2026-11-20T15:01:57Z ", "2026-11-20T15:01:57Z"},
		{"invalid", "not-a-date", ""},
		{"empty", "", ""},
		{"null", nil, ""},
		{"number", 12345, ""},
		{"boolean", true, ""},
		{"object", map[string]any{"unexpected": true}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"exp": 1,
				"https://api.openai.com/auth": map[string]any{
					"chatgpt_plan_type":                 "prolite",
					"chatgpt_subscription_active_until": tc.value,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := DecodeIDToken("e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature")
			if err != nil {
				t.Fatal(err)
			}
			info := claims.GetUserInfo()
			if info.SubscriptionExpiresAt != tc.want || info.PlanType != "prolite" {
				t.Fatalf("unexpected subscription metadata: %+v", info)
			}
		})
	}
}
