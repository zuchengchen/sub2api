package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestInjectAnthropicAPIKeyCacheMetadata_IsStableAndAccountScoped(t *testing.T) {
	account := &Account{ID: 22094, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
		AnthropicAPIKeyCacheControlRewriteExtraKey: true,
	}}
	parsed := &ParsedRequest{SessionContext: &SessionContext{
		ClientIP: "192.0.2.10", UserAgent: "Go-http-client/2.0", APIKeyID: 408, ClientSessionID: "session-42",
	}}
	body := []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hello"}]}`)

	first := injectAnthropicAPIKeyCacheMetadata(body, parsed, account)
	second := injectAnthropicAPIKeyCacheMetadata(body, parsed, account)
	require.Equal(t, string(first), string(second))

	parsedUserID := ParseMetadataUserID(gjson.GetBytes(first, "metadata.user_id").String())
	require.NotNil(t, parsedUserID)
	require.Len(t, parsedUserID.DeviceID, 64)
	require.NotEmpty(t, parsedUserID.SessionID)
}

func TestInjectAnthropicAPIKeyCacheMetadata_PreservesClientIdentity(t *testing.T) {
	account := &Account{ID: 22172, Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{
		AnthropicAPIKeyCacheControlRewriteExtraKey: true,
	}}
	body := []byte(`{"metadata":{"user_id":"client-provided","other":"kept"},"messages":[{"role":"user","content":"hello"}]}`)

	got := injectAnthropicAPIKeyCacheMetadata(body, &ParsedRequest{}, account)
	require.Equal(t, "client-provided", gjson.GetBytes(got, "metadata.user_id").String())
	require.Equal(t, "kept", gjson.GetBytes(got, "metadata.other").String())
}

func TestAnthropicAPIKeyCacheControlRewrite_DefaultOffAndStrict(t *testing.T) {
	account := &Account{Platform: PlatformAnthropic, Type: AccountTypeAPIKey, Extra: map[string]any{}}
	require.False(t, account.IsAnthropicAPIKeyCacheControlRewriteEnabled())
	account.Extra[AnthropicAPIKeyCacheControlRewriteExtraKey] = "true"
	require.False(t, account.IsAnthropicAPIKeyCacheControlRewriteEnabled())
	account.Extra[AnthropicAPIKeyCacheControlRewriteExtraKey] = true
	require.True(t, account.IsAnthropicAPIKeyCacheControlRewriteEnabled())
}

func TestRewriteMessageCacheControlBody_PreservesLongestClientTTL(t *testing.T) {
	body := []byte(`{"messages":[
		{"role":"user","content":[{"type":"text","text":"q1","cache_control":{"type":"ephemeral","ttl":"1h"}}]},
		{"role":"assistant","content":[{"type":"text","text":"a1"}]},
		{"role":"user","content":[{"type":"text","text":"q2","cache_control":{"type":"ephemeral","ttl":"5m"}}]},
		{"role":"assistant","content":[{"type":"text","text":"a2"}]}
	]}`)

	out := rewriteMessageCacheControlBody(body)
	require.Equal(t, "1h", gjson.GetBytes(out, "messages.3.content.0.cache_control.ttl").String())
	require.Equal(t, "1h", gjson.GetBytes(out, "messages.0.content.0.cache_control.ttl").String())

	defaulted := rewriteMessageCacheControlBody([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	require.Equal(t, claude.DefaultCacheControlTTL, gjson.GetBytes(defaulted, "messages.0.content.0.cache_control.ttl").String())
}
