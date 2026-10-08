package service

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	convTestSession      = "01a07c73-e312-76e1-9054-e4665b8ee0a7"
	convTestThread       = "01a07c73-e312-76e1-9054-e4665b8ee0a7"
	convTestTurn         = "01a07c73-e3a0-7ae1-baf0-ce1c532f019c"
	convTestWindow       = convTestThread + ":1"
	convTestInstallation = "7f582abd-05d2-4a59-b4e5-ec1b733b4edc"
	convTestParentThread = "01a07c60-1111-7aaa-8bbb-cccccccccccc"
)

func convTestAccount(enabled bool) *Account {
	extra := map[string]any{codexFingerprintConvergenceExtraKey: enabled}
	return &Account{
		ID:          11,
		Platform:    PlatformOpenAI,
		Type:        AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11", "chatgpt_user_id": "user-11"},
		Extra:       extra,
	}
}

func requireV7SameTimestamp(t *testing.T, raw, derived, label string) {
	t.Helper()
	parsed, err := uuid.Parse(derived)
	require.NoError(t, err, label)
	require.Equal(t, uuid.Version(7), parsed.Version(), "%s 应保持 v7", label)
	require.NotEqual(t, raw, derived, "%s 必须被命名空间化", label)
	require.Equal(t, raw[:13], derived[:13], "%s 应保留 48 位时间戳", label)
}

func TestCodexFingerprintConvergence_SwitchParsing(t *testing.T) {
	cases := []struct {
		name  string
		extra map[string]any
		typ   string
		want  bool
	}{
		{"bool true", map[string]any{codexFingerprintConvergenceExtraKey: true}, AccountTypeOAuth, true},
		{"string true", map[string]any{codexFingerprintConvergenceExtraKey: "true"}, AccountTypeOAuth, true},
		{"string 1", map[string]any{codexFingerprintConvergenceExtraKey: "1"}, AccountTypeOAuth, true},
		{"bool false", map[string]any{codexFingerprintConvergenceExtraKey: false}, AccountTypeOAuth, false},
		{"string false", map[string]any{codexFingerprintConvergenceExtraKey: "false"}, AccountTypeOAuth, false},
		{"string 0", map[string]any{codexFingerprintConvergenceExtraKey: "0"}, AccountTypeOAuth, false},
		{"missing", map[string]any{}, AccountTypeOAuth, true},
		{"nil extra", nil, AccountTypeOAuth, true},
		{"empty string", map[string]any{codexFingerprintConvergenceExtraKey: ""}, AccountTypeOAuth, true},
		{"api key account", map[string]any{codexFingerprintConvergenceExtraKey: true}, AccountTypeAPIKey, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account := &Account{Platform: PlatformOpenAI, Type: tc.typ, Extra: tc.extra}
			require.Equal(t, tc.want, codexFingerprintConvergenceEnabled(account))
		})
	}
	require.False(t, codexFingerprintConvergenceEnabled(nil))
}

func TestCodexFingerprintConvergence_DeriveKeepsVersionAndTimestamp(t *testing.T) {
	on, off := convTestAccount(true), convTestAccount(false)
	v7 := scopeCodexAccountIdentityValue(on, 77, "thread", convTestThread)
	requireV7SameTimestamp(t, convTestThread, v7, "v7 派生")
	require.Equal(t, v7, scopeCodexAccountIdentityValue(on, 77, "thread", convTestThread), "确定性")
	require.NotEqual(t, v7, scopeCodexAccountIdentityValue(on, 78, "thread", convTestThread), "不同 API key 不同值")
	require.NotEqual(t, v7, scopeCodexAccountIdentityValue(on, 77, "turn", convTestThread), "不同 kind 不同值")

	v4 := scopeCodexAccountIdentityValue(on, 77, "installation", convTestInstallation)
	parsed, err := uuid.Parse(v4)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(4), parsed.Version())
	require.Equal(t, scopeCodexAccountIdentityValue(off, 77, "installation", convTestInstallation), v4, "v4 原始值的派生不随开关变化")

	nonUUID := scopeCodexAccountIdentityValue(on, 77, "thread", "not-a-uuid")
	parsed, err = uuid.Parse(nonUUID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(4), parsed.Version(), "非 UUID 原始值仍走 v4 哈希")
}

func TestCodexFingerprintConvergence_CompositeWindowAndPromptCache(t *testing.T) {
	account := convTestAccount(false)
	window := scopeCodexAccountIdentityValue(account, 77, "window", convTestWindow)
	thread := scopeCodexAccountIdentityValue(account, 77, "thread", convTestThread)
	require.Equal(t, thread+":1", window)

	pck := scopeCodexAccountIdentityValue(account, 77, "prompt-cache", "guardian:"+convTestParentThread)
	parent := scopeCodexAccountIdentityValue(account, 77, "thread", convTestParentThread)
	require.Equal(t, "guardian:"+parent, pck)
}

func TestCodexFingerprintConvergence_RootSessionAndThreadShareSeed(t *testing.T) {
	account := convTestAccount(false)
	session := scopeCodexAccountIdentityValue(account, 77, "session", convTestSession)
	thread := scopeCodexAccountIdentityValue(account, 77, "thread", convTestThread)
	require.Equal(t, session, thread, "根会话 session 与 thread 原始值相同时派生必须相同")
}

func TestCodexFingerprintConvergence_HeadersFillAndDropAliases(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := convTestAccount(true)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	require.NoError(t, err)
	c.Request = req
	c.Set("api_key_id", int64(77))
	body := map[string]any{
		"client_metadata": map[string]any{
			"session_id":               scopeCodexAccountIdentityValue(account, 77, "session", convTestSession),
			"thread_id":                scopeCodexAccountIdentityValue(account, 77, "thread", convTestThread),
			"x-codex-parent-thread-id": scopeCodexAccountIdentityValue(account, 77, "thread", convTestParentThread),
			"x-openai-subagent":        "explorer",
		},
	}
	stageCodexConvergenceBodyIdentityMap(c, account, body)

	headers := http.Header{}
	headers.Set("session_id", "legacy-session")
	headers.Set("conversation_id", "legacy-conversation")
	applyCodexFingerprintConvergenceHeaders(c, account, headers)

	sid := headers.Get("session-id")
	tid := headers.Get("thread-id")
	requireV7SameTimestamp(t, convTestSession, sid, "session-id")
	require.Equal(t, sid, tid)
	require.Equal(t, tid, headers.Get("x-client-request-id"))
	require.Empty(t, headers.Get("session_id"))
	require.Empty(t, headers.Get("conversation_id"))
	require.Equal(t, "explorer", headers.Get("x-openai-subagent"))
}

func TestCodexFingerprintConvergence_OffLeavesLegacyAliases(t *testing.T) {
	account := convTestAccount(false)
	headers := http.Header{}
	headers.Set("session_id", "legacy-session")
	headers.Set("conversation_id", "legacy-conversation")
	applyCodexFingerprintConvergenceHeaders(nil, account, headers)
	require.Equal(t, "legacy-session", headers.Get("session_id"))
	require.Equal(t, "legacy-conversation", headers.Get("conversation_id"))
	require.Empty(t, headers.Get("session-id"))
}

func TestCodexDeviceWireProfileRequiresDualOpen(t *testing.T) {
	deviceOnly := convTestAccount(false)
	deviceOnly.Extra[codexFingerprintModeExtraKey] = string(codexFingerprintDevice)
	require.False(t, codexDeviceWireProfileEnabledFor(deviceOnly, deviceOnly))

	dual := convTestAccount(true)
	dual.Extra[codexFingerprintModeExtraKey] = string(codexFingerprintDevice)
	require.True(t, codexDeviceWireProfileEnabledFor(dual, dual))
}
