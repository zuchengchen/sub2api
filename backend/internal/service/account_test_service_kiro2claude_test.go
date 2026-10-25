//go:build unit

package service

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCreateTestPayloadOmitsClaudeCodeIdentityForKiro2Claude(t *testing.T) {
	t.Parallel()

	withIdentity, err := createTestPayload("claude-opus-5-5")
	require.NoError(t, err)
	require.Equal(t, claudeCodeSystemPrompt, systemTextFromTestPayload(t, withIdentity))

	otherGroup, err := createTestPayloadForAccount("claude-opus-5-5", &Account{
		Groups: []*Group{{Name: "Claude"}},
	})
	require.NoError(t, err)
	require.Equal(t, claudeCodeSystemPrompt, systemTextFromTestPayload(t, otherGroup))

	kiroAccount := &Account{
		Groups: []*Group{{Name: "Kiro2Claude"}},
	}
	omitted, err := createTestPayloadForAccount("claude-opus-5-5", kiroAccount)
	require.NoError(t, err)
	_, hasSystem := omitted["system"]
	require.False(t, hasSystem)
	raw, err := json.Marshal(omitted)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "You are Claude Code")
	require.Equal(t, "hi", gjson.GetBytes(raw, "messages.0.content.0.text").String())
}

func TestAccountInNamedGroupMatchesGroupsAndAccountGroups(t *testing.T) {
	t.Parallel()

	require.False(t, accountInNamedGroup(nil, kiro2ClaudeGroupName))
	require.False(t, accountInNamedGroup(&Account{}, kiro2ClaudeGroupName))
	require.False(t, accountInNamedGroup(&Account{Groups: []*Group{{Name: "Claude"}}}, kiro2ClaudeGroupName))
	require.True(t, omitClaudeCodeIdentityInAccountTest(&Account{
		Groups: []*Group{{Name: "Kiro2Claude"}},
	}))
	require.True(t, omitClaudeCodeIdentityInAccountTest(&Account{
		Groups: []*Group{{Name: "  kiro2claude  "}},
	}))
	require.True(t, omitClaudeCodeIdentityInAccountTest(&Account{
		AccountGroups: []AccountGroup{{Group: &Group{Name: "Kiro2Claude"}}},
	}))
	require.True(t, omitClaudeCodeIdentityInAccountTest(&Account{
		Groups: []*Group{{Name: "Claude"}, {Name: "Kiro2Claude"}},
	}))
}

func TestClaudeAccountConnectionOmitsClaudeCodeIdentityForKiro2ClaudeGroup(t *testing.T) {
	kiro := claudeAPIKeyTestAccount(41, []*Group{{Name: "Kiro2Claude"}})
	other := claudeAPIKeyTestAccount(42, []*Group{{Name: "Claude"}})

	kiroReq := mustClaudeAccountTestRequest(t, kiro)
	otherReq := mustClaudeAccountTestRequest(t, other)

	kiroBody := mustReadRequestBody(t, kiroReq)
	otherBody := mustReadRequestBody(t, otherReq)

	require.False(t, gjson.GetBytes(kiroBody, "system").Exists())
	require.NotContains(t, string(kiroBody), "You are Claude Code")
	require.Equal(t, "", kiroReq.Header.Get("User-Agent"))
	require.Equal(t, "", kiroReq.Header.Get("X-App"))
	require.Equal(t, "", kiroReq.Header.Get("anthropic-beta"))
	require.Equal(t, "sk-kiro-test", kiroReq.Header.Get("x-api-key"))

	require.Equal(t, claudeCodeSystemPrompt, gjson.GetBytes(otherBody, "system.0.text").String())
	require.Contains(t, otherReq.Header.Get("User-Agent"), "claude-cli/")
	require.Equal(t, "cli", otherReq.Header.Get("X-App"))
	require.Equal(t, claude.APIKeyBetaHeader, otherReq.Header.Get("anthropic-beta"))
	require.Contains(t, otherReq.Header.Get("anthropic-beta"), claude.BetaClaudeCode)
}

func claudeAPIKeyTestAccount(id int64, groups []*Group) *Account {
	return &Account{
		ID:          id,
		Name:        "kiro-rs-ip",
		Platform:    PlatformAnthropic,
		Type:        AccountTypeAPIKey,
		Concurrency: 1,
		Credentials: map[string]any{
			"api_key":  "sk-kiro-test",
			"base_url": "https://kiro.example",
		},
		Groups: groups,
	}
}

func mustClaudeAccountTestRequest(t *testing.T, account *Account) *http.Request {
	t.Helper()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(`data: {"type":"content_block_delta","delta":{"text":"Hi"}}

data: {"type":"message_stop"}

`)),
	}
	upstream := &queuedHTTPUpstream{responses: []*http.Response{resp}}
	svc := &AccountTestService{
		httpUpstream: upstream,
		cfg:          &config.Config{Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{Enabled: false}}},
	}
	c, recorder := newTestContext()
	err := svc.testClaudeAccountConnection(c, account, "claude-opus-5-5")
	require.NoError(t, err)
	require.Contains(t, recorder.Body.String(), "test_complete")
	require.Len(t, upstream.requests, 1)
	return upstream.requests[0]
}

func mustReadRequestBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	return body
}

func systemTextFromTestPayload(t *testing.T, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	return gjson.GetBytes(raw, "system.0.text").String()
}
