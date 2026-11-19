//go:build unit

package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestGPTAccountThreadAffinityScopeAndPrecedence(t *testing.T) {
	svc := &OpenAIGatewayService{}
	const base = `{"model":"public-alias","messages":[{"role":"user","content":"First question"}]`
	const model = "gpt-5.1"
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	key := &APIKey{ID: 101, UserID: 100, Group: &Group{Platform: PlatformOpenAI}}
	c.Set("api_key", key)
	bare := []byte(base + `}`)
	without := svc.GenerateChatCompletionsSessionHash(c, bare, model)
	c.Request.Header.Set("thread-id", "thread-A")
	got := svc.GenerateChatCompletionsSessionHash(c, bare, model)
	require.NotEqual(t, without, got, "channel-mapped GPT Chat")
	require.Equal(t, got, svc.GenerateResponsesAccountSessionHash(c, []byte(`{"model":"gpt-5.1","input":"A summary"}`), model))
	require.Empty(t, svc.ExtractSessionID(c, bare), "account selection cannot grant upstream state")
	require.Empty(t, svc.GenerateExplicitSessionHash(c, bare), "image routing keeps the original explicit session signals")
	require.Equal(t, DeriveSessionHashFromSeed(deriveOpenAIContentSessionSeed(bare)), svc.GenerateSessionHash(c, bare), "other session and lineage callers retain their original scope")
	key.ID++
	require.NotEqual(t, got, svc.GenerateChatCompletionsSessionHash(c, bare, model))
	key.ID--
	key.UserID++
	require.NotEqual(t, got, svc.GenerateChatCompletionsSessionHash(c, bare, model))
	key.UserID = 0
	require.Equal(t, without, svc.GenerateChatCompletionsSessionHash(c, bare, model))
	key.UserID = 100
	for _, platform := range []string{PlatformGrok, PlatformDeepseek} {
		c.Request = c.Request.WithContext(WithResolvedTargetPlatform(c.Request.Context(), platform))
		withThread := svc.GenerateChatCompletionsSessionHash(c, bare, model)
		c.Request.Header.Del("thread-id")
		require.Equal(t, withThread, svc.GenerateChatCompletionsSessionHash(c, bare, model), platform)
		c.Request.Header.Set("thread-id", "thread-A")
	}
	c.Request = c.Request.WithContext(WithResolvedTargetPlatform(c.Request.Context(), PlatformOpenAI))
	const nonGPT = "third-party-model"
	thirdParty := svc.GenerateChatCompletionsSessionHash(c, bare, nonGPT)
	c.Request.Header.Del("thread-id")
	require.Equal(t, thirdParty, svc.GenerateChatCompletionsSessionHash(c, bare, nonGPT))
	for _, tc := range []struct{ name, fields, header, value string }{
		{"turn only", `,"client_metadata":{"turn_id":"thread-A"}`, "", ""},
		{"embedded turn only", `,"client_metadata":{"x-codex-turn-metadata":"{\"turn_id\":\"thread-A\"}"}`, "", ""},
		{"header turn only", "", openAIWSTurnMetadataHeader, `{"turn_id":"thread-A"}`},
		{"request only", "", "X-Client-Request-Id", "thread-A"},
		{"unscoped metadata", `,"metadata":{"thread_id":"thread-A"}`, "", ""},
		{"nested tool metadata", `,"metadata":{"client_metadata":{"thread_id":"thread-A"}}`, "", ""},
		{"invalid number", `,"client_metadata":{"session_id":121}`, "", ""},
		{"duplicate metadata", `,"client_metadata":{"thread_id":"A","thread_id":"B"}`, "", ""},
		{"malformed embedded metadata", `,"client_metadata":{"session_id":"A","x-codex-turn-metadata":"{"}`, "", ""},
		{"child lacks a thread", `,"client_metadata":{"session_id":"A","parent_thread_id":"parent","turn_id":"turn"}`, "", ""},
		{"unsupported nested session", `,"metadata":{"user_id":"{\"session_id\":\"thread-A\"}"}`, "", ""},
		{"invalid thread header", "", "thread-id", "bad\tvalue"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.header != "" {
				c.Request.Header.Set(tc.header, tc.value)
				defer c.Request.Header.Del(tc.header)
			}
			require.Equal(t, without, svc.GenerateChatCompletionsSessionHash(c, []byte(base+tc.fields+`}`), model))
		})
	}
	c.Request.Header.Add("thread-id", "first")
	c.Request.Header.Add("thread-id", "second")
	require.Equal(t, without, svc.GenerateChatCompletionsSessionHash(c, bare, model))
	c.Request.Header.Del("thread-id")
	c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"thread_id":"thread-A"}`)
	require.Equal(t, got, svc.GenerateChatCompletionsSessionHash(c, bare, model))
	c.Request.Header.Del(openAIWSTurnMetadataHeader)
	require.Equal(t, got, svc.GenerateChatCompletionsSessionHash(c, []byte(base+`,"client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"thread-A\"}"}}`), model))
	c.Request.Header.Set("thread-id", "thread-A")
	withKey := []byte(base + `,"prompt_cache_key":"cache-first"}`)
	require.Equal(t, DeriveSessionHashFromSeed("cache-first"), svc.GenerateChatCompletionsSessionHash(c, withKey, model))
	c.Request.Header.Set("session-id", "session-first")
	require.Equal(t, DeriveSessionHashFromSeed("session-first"), svc.GenerateResponsesAccountSessionHash(c, withKey, model))
}
