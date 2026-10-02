package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestEnsureMessageCacheBreakpointsIfMissing_ToolsOnlyGetsMessageBreakpoints(t *testing.T) {
	body := []byte(`{"tools":[{"name":"bash","cache_control":{"type":"ephemeral"}}],"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"q1"}]},{"role":"assistant","content":[{"type":"text","text":"a1"}]},{"role":"user","content":[{"type":"text","text":"q2"}]},{"role":"assistant","content":[{"type":"text","text":"a2"}]},{"role":"user","content":[{"type":"text","text":"q3"}]}]}`)

	out := ensureMessageCacheBreakpointsIfMissing(body)
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.4.content.0.cache_control.type").String())
	require.False(t, gjson.GetBytes(out, "messages.4.content.0.cache_control.ttl").Exists())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "messages.2.content.0.cache_control.type").String())
	require.False(t, gjson.GetBytes(out, "messages.2.content.0.cache_control.ttl").Exists())
	require.False(t, gjson.GetBytes(out, "messages.0.content.0.cache_control").Exists())
	require.False(t, gjson.GetBytes(out, "messages.1.content.0.cache_control").Exists())
	require.False(t, gjson.GetBytes(out, "messages.3.content.0.cache_control").Exists())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "tools.0.cache_control.type").String())
	require.False(t, gjson.GetBytes(out, "tools.0.cache_control.ttl").Exists())
	require.Equal(t, "sys", gjson.GetBytes(out, "system.0.text").String())
	require.Equal(t, "ephemeral", gjson.GetBytes(out, "system.0.cache_control.type").String())
}

func TestEnsureMessageCacheBreakpointsIfMissing_PreservesClaudeCodeBody(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"1h"}}]}]}`)
	require.Equal(t, string(body), string(ensureMessageCacheBreakpointsIfMissing(body)))
}

func TestEnsureMessageCacheBreakpointsIfMissing_EmptyMessagesNoop(t *testing.T) {
	empty := []byte(`{"messages":[]}`)
	require.Equal(t, string(empty), string(ensureMessageCacheBreakpointsIfMissing(empty)))
	missing := []byte(`{}`)
	require.Equal(t, string(missing), string(ensureMessageCacheBreakpointsIfMissing(missing)))
}
