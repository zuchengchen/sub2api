//go:build unit

package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func responsesAffinityTestContext(keyID, userID int64, platform string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set("api_key", &APIKey{ID: keyID, UserID: userID, Group: &Group{Platform: platform}})
	return c
}

func TestNativeResponsesCacheAffinityScope(t *testing.T) {
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"public-alias","input":"compressed history"}`)
	const textModel = "gpt-6.1-sol"
	for _, tc := range []struct {
		scope, model, platform string
		key, user              int64
		enabled                bool
	}{
		{"", textModel, PlatformOpenAI, 8101, 701, false},
		{"key:8101", textModel, PlatformOpenAI, 8101, 701, true},
		{"key:8101", textModel, PlatformOpenAI, 8102, 701, false},
		{"all", textModel, PlatformOpenAI, 8102, 701, true},
		{"all", textModel, PlatformGrok, 8101, 701, false},
		{"all", "gpt-image-2", PlatformOpenAI, 8101, 701, false},
		{"all", "external-model", PlatformOpenAI, 8101, 701, false},
		{"all", "", PlatformOpenAI, 8101, 701, false},
		{"all", textModel, PlatformOpenAI, 8101, 0, false},
		{"all", textModel, PlatformOpenAI, 0, 701, false},
		{"key:+8101", textModel, PlatformOpenAI, 8101, 701, false},
		{"key:08101", textModel, PlatformOpenAI, 8101, 701, false},
		{"key:8101,8102", textModel, PlatformOpenAI, 8101, 701, false},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s/%d/%d", tc.scope, tc.model, tc.platform, tc.key, tc.user), func(t *testing.T) {
			t.Setenv(responsesCacheAffinityScopeEnv, tc.scope)
			c := responsesAffinityTestContext(tc.key, tc.user, tc.platform)
			c.Request.Header.Set("thread-id", "thread-a")
			got := svc.GenerateResponsesWSAccountSessionHash(c, body, tc.model, "last-resort")
			legacy := svc.GenerateSessionHash(c, body)
			if tc.enabled {
				require.NotEqual(t, legacy, got)
				require.Equal(t, svc.GenerateResponsesAccountSessionHash(c, body, tc.model), got)
			} else {
				require.Equal(t, legacy, got)
			}
			require.Equal(t, tc.enabled, c.GetBool(responsesCacheAffinityContextKey))
		})
	}
	for _, scope := range []string{"", "all"} {
		t.Setenv(responsesCacheAffinityScopeEnv, scope)
		c := responsesAffinityTestContext(8101, 701, PlatformOpenAI)
		require.Equal(t, DeriveSessionHashFromSeed("last-resort"), svc.GenerateResponsesWSAccountSessionHash(c, []byte("{}"), textModel, "last-resort"))
		c.Request.Header.Set("thread-id", "existing-thread")
		require.NotEqual(t, svc.GenerateSessionHash(c, body), svc.GenerateResponsesAccountSessionHash(c, body, textModel), "deployed HTTP Codex affinity survives rollback")
	}
}

func TestNativeResponsesCacheAffinityClaudeCarriers(t *testing.T) {
	t.Setenv(responsesCacheAffinityScopeEnv, "all")
	svc := &OpenAIGatewayService{}
	account := &Account{ID: 12, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	const id = "9ed40161-35bb-44b6-919b-f242a4213723"
	const model = "gpt-6.1-sol"
	legacy := FormatMetadataUserID(strings.Repeat("a", 64), "", id, "2.0.1")
	current := FormatMetadataUserID("device", "", id, "2.1.78")
	claudeBody := func(metadata string) []byte {
		raw, err := json.Marshal(map[string]any{"model": model, "input": "rewritten history", "metadata": map[string]string{"user_id": metadata}})
		require.NoError(t, err)
		return raw
	}
	for _, tc := range []struct {
		name    string
		body    []byte
		headers []string
		want    string
	}{
		{"dedicated header", []byte(`{"model":"gpt-6.1-sol","input":"hello"}`), []string{id}, id},
		{"legacy", claudeBody(legacy), nil, id},
		{"structured", claudeBody(current), nil, id},
		{"matching", claudeBody(current), []string{id}, id},
		{"conflict", claudeBody(legacy), []string{"other-session"}, ""},
		{"duplicate header", claudeBody(current), []string{id, id}, ""},
		{"combined header", claudeBody(""), []string{id + ",another"}, ""},
		{"quoted header", claudeBody(""), []string{"\"quoted-value\""}, ""},
		{"nil uuid", claudeBody(`{"session_id":"00000000-0000-0000-0000-000000000000"}`), nil, ""},
		{"padded nil uuid", claudeBody(`{"session_id":" 00000000-0000-0000-0000-000000000000 "}`), nil, ""},
		{"oversize", claudeBody(""), []string{strings.Repeat("a", 256)}, ""},
		{"ambiguous metadata", claudeBody(`{"session_id":"other","session_id":"` + id + `"}`), nil, ""},
		{"arbitrary suffix", claudeBody("account-hint_session_" + id), nil, ""},
		{"user only", claudeBody("user-701"), nil, ""},
		{"unqualified session in user field", claudeBody(`{"session_id":"short"}`), nil, ""},
		{"invalid metadata type", []byte(`{"model":"gpt-6.1-sol","input":"a","metadata":{"user_id":23}}`), []string{id}, ""},
		{"parent only", []byte(`{"model":"gpt-6.1-sol","input":"a","client_metadata":{"parent_thread_id":"parent"}}`), []string{id}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := responsesAffinityTestContext(8101, 701, PlatformOpenAI)
			for _, h := range tc.headers {
				c.Request.Header.Add(claudeCodeSessionHeader, h)
			}
			got := svc.GenerateResponsesAccountSessionHash(c, tc.body, model)
			require.Equal(t, got, svc.GenerateResponsesWSAccountSessionHash(c, tc.body, model, "fallback"))
			restore := stageCodexHTTPClientSession(c, account, tc.body)
			if tc.want != "" {
				key := getAPIKeyFromContext(c)
				require.Equal(t, DeriveSessionHashFromSeed(gptAccountThreadSeed(key, id, id)), got)
				require.Equal(t, tc.want, c.GetHeader("session-id"))
			} else {
				require.Equal(t, DeriveSessionHashFromSeed(deriveOpenAIContentSessionSeed(tc.body)), got)
				require.Equal(t, codexAnonymousRequestSession(c), c.GetHeader("session-id"))
				scope, _ := resolveOpenAIWSExecutionScope(c, tc.body, 8101)
				require.Empty(t, scope)
			}
			restore()
			require.Empty(t, c.GetHeader("session-id"))
		})
	}

	body := claudeBody(current)
	hash := func(user, key int64) string {
		c := responsesAffinityTestContext(key, user, PlatformOpenAI)
		return svc.GenerateResponsesAccountSessionHash(c, body, model)
	}
	require.NotEqual(t, hash(701, 8101), hash(702, 8101))
	require.NotEqual(t, hash(701, 8101), hash(701, 8102))
	for _, scope := range []string{"", "key:8102", "key:8101"} {
		t.Setenv(responsesCacheAffinityScopeEnv, scope)
		c := responsesAffinityTestContext(8101, 701, PlatformOpenAI)
		c.Request.Header.Set("turn-id", "only-this-turn")
		got := svc.GenerateResponsesAccountSessionHash(c, body, model)
		restore := stageCodexHTTPClientSession(c, account, body)
		if scope == "key:8101" {
			require.Equal(t, id, c.GetHeader("session-id"))
			require.NotEqual(t, svc.GenerateSessionHash(c, body), got)
		} else {
			require.Empty(t, c.GetHeader("session-id"))
			require.Equal(t, svc.GenerateSessionHash(c, body), got)
		}
		require.Equal(t, "only-this-turn", c.GetHeader("turn-id"))
		restore()
	}
	c := responsesAffinityTestContext(8101, 701, PlatformOpenAI)
	c.Request.Header.Set(claudeCodeSessionHeader, id)
	keyBody := []byte(`{"model":"gpt-6.1-sol","input":"hello","prompt_cache_key":"client-cache"}`)
	require.Equal(t, DeriveSessionHashFromSeed("client-cache"), svc.GenerateResponsesWSAccountSessionHash(c, keyBody, model, "fallback"))
	c.Request.Header.Set("session-id", "canonical-session")
	require.Equal(t, DeriveSessionHashFromSeed("canonical-session"), svc.GenerateResponsesAccountSessionHash(c, keyBody, model))
}

func TestNativeResponsesCacheAffinityProjectionUsesActualHTTPModel(t *testing.T) {
	t.Setenv(responsesCacheAffinityScopeEnv, "all")
	const id = "9ed40161-35bb-44b6-919b-f242a4213723"
	const model = "gpt-6.1-sol"
	const target = "gpt-5.1"
	body := []byte(`{"model":"gpt-6.1-sol","input":"rewrite","client_metadata":{"turn_id":"real-turn"}}`)
	svc := &OpenAIGatewayService{}
	for _, tc := range []struct {
		name, platform, target, accountType string
		ws, wantSession                     bool
	}{
		{"OpenAI OAuth ignores stored model mapping", PlatformOpenAI, "external-model", AccountTypeOAuth, false, true},
		{"legacy OAuth maps to GPT", "", target, AccountTypeOAuth, false, true},
		{"legacy OAuth maps to non-GPT", "", "external-model", AccountTypeOAuth, false, false},
		{"legacy OAuth maps to image", "", "gpt-image-2", AccountTypeOAuth, false, false},
		{"setup token preserves prior contract", PlatformOpenAI, target, AccountTypeSetupToken, false, false},
		{"native WS connection can switch models", PlatformOpenAI, target, AccountTypeOAuth, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := responsesAffinityTestContext(8101, 701, PlatformOpenAI)
			c.Request.Header.Set(claudeCodeSessionHeader, id)
			account := &Account{ID: 55, Platform: tc.platform, Type: tc.accountType, Credentials: map[string]any{"model_mapping": map[string]any{model: tc.target}}}
			if tc.ws {
				c.Request.Method = http.MethodGet
				SetOpenAIClientTransport(c, OpenAIClientTransportWS)
				svc.GenerateResponsesWSAccountSessionHash(c, body, model, "fallback")
			} else {
				svc.GenerateResponsesAccountSessionHash(c, body, model)
			}
			restore := stageCodexHTTPClientSession(c, account, body)
			if tc.wantSession {
				require.Equal(t, id, c.GetHeader("session-id"))
			} else {
				require.Empty(t, c.GetHeader("session-id"), "original turn-only projection must be preserved")
			}
			restore()
			if tc.ws {
				// Real Claude-only sessions already span frames under the legacy WS
				// projection, including after reconnect. There is no new private scope.
				defer stageCodexHTTPClientSession(c, account, []byte(`{"model":"gpt-6.1-sol","input":"hi"}`))()
				require.Equal(t, id, c.GetHeader("session-id"))
			}
		})
	}
}
