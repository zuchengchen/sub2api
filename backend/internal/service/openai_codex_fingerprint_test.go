package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const testCodexFingerprintSeed = "11111111-1111-4111-8111-111111111111"

func newTestOAuthAccount(id int64, extra map[string]any) *Account {
	if codexFingerprintModeRequiresSeed(codexFingerprintModeFromExtra(extra)) {
		if extra == nil {
			extra = make(map[string]any)
		}
		if _, exists := extra[codexFingerprintSeedExtraKey]; !exists {
			extra[codexFingerprintSeedExtraKey] = testCodexFingerprintSeed
		}
	}
	return &Account{
		ID:       id,
		Platform: PlatformOpenAI,
		Type:     AccountTypeOAuth,
		Extra:    extra,
	}
}

// --- deriveStableUUIDv4 ---

func TestDeriveStableUUIDv4_Deterministic(t *testing.T) {
	a := deriveStableUUIDv4("test-seed-1")
	b := deriveStableUUIDv4("test-seed-1")
	assert.Equal(t, a, b, "同一种子应返回相同结果")
}

func TestDeriveStableUUIDv4_DifferentSeeds(t *testing.T) {
	a := deriveStableUUIDv4("seed-a")
	b := deriveStableUUIDv4("seed-b")
	assert.NotEqual(t, a, b, "不同种子应返回不同结果")
}

func TestDeriveStableUUIDv4_ValidFormat(t *testing.T) {
	result := deriveStableUUIDv4("test-seed")
	parsed, err := uuid.Parse(result)
	require.NoError(t, err, "应返回合法 UUID 格式")
	assert.Equal(t, uuid.Version(4), parsed.Version(), "应为 UUIDv4")
	assert.Equal(t, uuid.RFC4122, parsed.Variant(), "应为 RFC4122 变体")
}

// --- GetCodexFingerprintMode ---

func TestGetCodexFingerprintMode(t *testing.T) {
	tests := []struct {
		name     string
		account  *Account
		expected codexFingerprintMode
	}{
		{"nil 账号", nil, codexFingerprintOff},
		{"非 OAuth 账号", &Account{Platform: PlatformOpenAI, Type: "api_key"}, codexFingerprintOff},
		{"OpenAI setup token", &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Extra: map[string]any{codexFingerprintModeExtraKey: "session"}}, codexFingerprintSession},
		{"OpenAI setup token 默认 full", &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}, codexFingerprintFull},
		{"Anthropic setup token", &Account{Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Extra: map[string]any{codexFingerprintModeExtraKey: "session"}}, codexFingerprintOff},
		// 缺省完全收敛：缺省/空/非法一律 full。显式 off 才透传。
		{"无 extra 默认 full", newTestOAuthAccount(1, nil), codexFingerprintFull},
		{"空值默认 full", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: ""}), codexFingerprintFull},
		{"非法值默认 full", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "invalid"}), codexFingerprintFull},
		{"显式 off", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"}), codexFingerprintOff},
		{"device", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "device"}), codexFingerprintDevice},
		{"session", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "session"}), codexFingerprintSession},
		{"full", newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "full"}), codexFingerprintFull},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, tt.account.GetCodexFingerprintMode())
		})
	}
}

// --- resolveConvergedInstallationID ---

func TestResolveConvergedInstallationID_UsesDeviceID(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{"openai_device_id": "real-device-id"})
	assert.Equal(t, "real-device-id", resolveConvergedInstallationID(account, testCodexFingerprintSeed))
}

func TestResolveConvergedInstallationID_DerivesFromSeed(t *testing.T) {
	account := newTestOAuthAccount(42, nil)
	result := resolveConvergedInstallationID(account, testCodexFingerprintSeed)
	_, err := uuid.Parse(result)
	require.NoError(t, err, "派生值应为合法 UUID")
	assert.Equal(t, result, resolveConvergedInstallationID(account, testCodexFingerprintSeed), "确定性")
}

func TestResolveConvergedInstallationID_DifferentSeeds(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	a := resolveConvergedInstallationID(account, testCodexFingerprintSeed)
	b := resolveConvergedInstallationID(account, "22222222-2222-4222-8222-222222222222")
	assert.NotEqual(t, a, b)
}

// --- resolveConvergedThreadID ---

func TestResolveConvergedThreadID_PerClientSession(t *testing.T) {
	a := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	b := resolveConvergedThreadID(testCodexFingerprintSeed, "session-bbb")
	assert.NotEqual(t, a, b, "不同客户端 session 应得到不同 thread_id")
}

func TestResolveConvergedThreadID_Deterministic(t *testing.T) {
	a := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	b := resolveConvergedThreadID(testCodexFingerprintSeed, "session-aaa")
	assert.Equal(t, a, b, "同一客户端 session 应得到相同 thread_id")
}

func TestResolveConvergedThreadID_EmptySession(t *testing.T) {
	assert.Equal(t, "", resolveConvergedThreadID(testCodexFingerprintSeed, ""))
}

// --- off 模式：resolveCodexFingerprintIDsFromRequest 返回 nil ---

func TestResolveCodexFingerprintIDsFromRequest_ExplicitOff(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: "off"})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	assert.Nil(t, ids, "显式 off 模式应返回 nil")
}

// 未显式配置的存量账号默认完全收敛：有种子时必须改写出站身份。
func TestResolveCodexFingerprintIDsFromRequest_DefaultIsFull(t *testing.T) {
	account := newTestOAuthAccount(1, nil)
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids, "无 extra 应视为 full")
	assert.Equal(t, codexFingerprintFull, ids.mode)
	assert.NotEmpty(t, ids.installationID)
	assert.NotEmpty(t, ids.sessionID)
	assert.Equal(t, ids.sessionID, ids.threadID)
}

func TestShouldEnsureCodexFingerprintSeedForExtraUpdates_DefaultFull(t *testing.T) {
	assert.False(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(nil))
	assert.False(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{codexFingerprintModeExtraKey: "off"}))
	assert.True(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{}))
	assert.True(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{"codex_cli_only": true}))
	assert.True(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{codexFingerprintModeExtraKey: "full"}))
	assert.True(t, ShouldEnsureCodexFingerprintSeedForExtraUpdates(map[string]any{codexFingerprintModeExtraKey: "session"}))
}

func TestResolveCodexFingerprintIDsFromRequest_ExplicitModesHonored(t *testing.T) {
	for _, mode := range []string{"device", "session", "full"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(1, map[string]any{codexFingerprintModeExtraKey: mode})
			ids := resolveCodexFingerprintIDsFromRequest(account, nil)
			require.NotNil(t, ids, "显式配置必须生效")
			assert.Equal(t, codexFingerprintMode(mode), ids.mode)
			assert.NotEmpty(t, ids.installationID)
		})
	}
}

func TestResolveCodexFingerprintIDsFromRequest_EnabledModesRequireValidSeed(t *testing.T) {
	for _, tt := range []struct {
		name  string
		extra map[string]any
	}{
		{name: "missing seed with default full", extra: map[string]any{}},
		{name: "nil extra missing seed", extra: nil},
		{name: "missing", extra: map[string]any{codexFingerprintModeExtraKey: "device"}},
		{name: "missing with device override", extra: map[string]any{codexFingerprintModeExtraKey: "device", "openai_device_id": "real-device"}},
		{name: "blank", extra: map[string]any{codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: ""}},
		{name: "uppercase", extra: map[string]any{codexFingerprintModeExtraKey: "full", codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-AAAAAAAAAAAA"}},
		{name: "nil uuid", extra: map[string]any{codexFingerprintModeExtraKey: "device", codexFingerprintSeedExtraKey: "00000000-0000-0000-0000-000000000000"}},
		{name: "non string", extra: map[string]any{codexFingerprintModeExtraKey: "session", codexFingerprintSeedExtraKey: 123}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: tt.extra}
			require.Nil(t, resolveCodexFingerprintIDsFromRequest(account, nil))
		})
	}
}

// --- applyCodexFingerprintHeaders: off 模式 ---

func TestApplyCodexFingerprintHeaders_OffMode(t *testing.T) {
	h := http.Header{}
	h.Set("x-codex-installation-id", "original-install-id")
	h.Set("x-codex-window-id", "original-window-id")

	applyCodexFingerprintHeaders(h, nil)

	assert.Equal(t, "original-install-id", h.Get("x-codex-installation-id"), "nil ids 不改写")
	assert.Equal(t, "original-window-id", h.Get("x-codex-window-id"), "nil ids 不改写")
}

// --- applyCodexFingerprintHeaders: device 模式 ---

func TestApplyCodexFingerprintHeaders_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","sandbox":"seccomp"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-window:0")
	h.Set("x-codex-turn-metadata", turnMetadata)

	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	applyCodexFingerprintHeaders(h, ids)

	assert.Equal(t, "converged-device", h.Get("x-codex-installation-id"), "installation_id 应收敛")
	assert.Equal(t, "user-window:0", h.Get("x-codex-window-id"), "device 模式不改写 window_id")

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "user-session", meta["session_id"], "device 模式不改写 session_id")
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

// --- applyCodexFingerprintHeaders: session 模式 ---

func TestApplyCodexFingerprintHeaders_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	turnMetadata := `{"installation_id":"user-install","session_id":"user-session","thread_id":"user-thread","turn_id":"user-turn","window_id":"user-thread:0","sandbox":"seccomp","thread_source":"user"}`
	h := http.Header{}
	h.Set("x-codex-installation-id", "user-install")
	h.Set("x-codex-window-id", "user-thread:0")
	h.Set("x-codex-turn-metadata", turnMetadata)
	h.Set("x-client-request-id", "user-thread")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	applyCodexFingerprintHeaders(h, ids)

	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedInstall := resolveConvergedInstallationID(account, seed)
	convergedSession := resolveConvergedSessionID(codexAccountIdentityNamespace(account) + ":user:0")
	convergedThread := scopeCodexAccountIdentityValue(account, 0, "thread", "client-session-aaa")

	assert.Equal(t, convergedInstall, h.Get("x-codex-installation-id"))
	assert.Equal(t, convergedSession, h.Get("session-id"))
	assert.Equal(t, convergedSession, h.Get("session_id"), "下划线形式也应被改写")
	assert.Equal(t, convergedThread, h.Get("thread-id"))
	assert.Equal(t, convergedThread, h.Get("x-client-request-id"))
	assert.Equal(t, convergedThread+":0", h.Get("x-codex-window-id"))

	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, convergedThread, meta["thread_id"])
	assert.NotEqual(t, "user-turn", meta["turn_id"], "turn_id 应被新生成的值替换")
	assert.Equal(t, "seccomp", meta["sandbox"], "sandbox 保留原样")
	assert.Equal(t, "user", meta["thread_source"], "thread_source 保留原样")
}

// --- session 模式：不同客户端得到不同 thread ---

func TestApplyCodexFingerprintHeaders_SessionMode_DifferentClients(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})

	makeTurnMeta := func() string {
		return `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`
	}

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDsFromRequest(account, clientA)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDsFromRequest(account, clientB)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", makeTurnMeta())
	applyCodexFingerprintHeaders(hB, idsB)

	assert.Equal(t, hA.Get("session-id"), hB.Get("session-id"), "session_id 应相同")
	assert.NotEqual(t, hA.Get("thread-id"), hB.Get("thread-id"), "不同客户端 thread_id 应不同")
	assert.NotEqual(t, hA.Get("x-codex-window-id"), hB.Get("x-codex-window-id"), "不同客户端 window_id 应不同")
	assert.Equal(t, hA.Get("x-codex-installation-id"), hB.Get("x-codex-installation-id"))
}

// --- full 模式 ---

func TestApplyCodexFingerprintHeaders_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedSession := resolveConvergedSessionID(seed)

	clientA := http.Header{}
	clientA.Set("session-id", "client-A")
	idsA := resolveCodexFingerprintIDsFromRequest(account, clientA)
	hA := http.Header{}
	hA.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hA, idsA)

	clientB := http.Header{}
	clientB.Set("session-id", "client-B")
	idsB := resolveCodexFingerprintIDsFromRequest(account, clientB)
	hB := http.Header{}
	hB.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(hB, idsB)

	// 多用户中转：一个账号一台设备，每个下游会话一个独立线程（v7），根线程
	// thread_id == session_id；不同会话不再塌缩成同一线程。
	_ = convergedSession
	assert.NotEqual(t, hA.Get("thread-id"), hB.Get("thread-id"), "full 模式不同会话应是不同线程")
	assert.Equal(t, hA.Get("session-id"), hA.Get("thread-id"), "full 模式 thread_id 应等于 session_id")
	assert.Equal(t, hA.Get("thread-id")+":0", hA.Get("x-codex-window-id"))
	assert.Equal(t, idsA.installationID, idsB.installationID, "full 模式 installation_id 应相同")
	parsed, err := uuid.Parse(hA.Get("session-id"))
	require.NoError(t, err)
	assert.EqualValues(t, 7, parsed.Version(), "session-id 应为 UUIDv7")
}

// --- H1 修复验证：头和体的 turn_id 一致性 ---

func TestFingerprintIDs_HeaderAndBody_TurnID_Consistent(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-xyz")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	// 头改写
	h := http.Header{}
	h.Set("x-codex-turn-metadata", `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`)
	applyCodexFingerprintHeaders(h, ids)

	// 体改写（使用同一份 ids）
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "x",
			"session_id":              "x",
			"turn_id":                 "x",
			"x-codex-turn-metadata":   `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}
	applyCodexFingerprintClientMetadata(reqBody, ids)

	// 从头 turn-metadata JSON 提取 turn_id
	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	headerTurnID, ok := headerMeta["turn_id"].(string)
	require.True(t, ok, "头 turn-metadata 应包含 string 类型的 turn_id")

	// 从体 client_metadata 提取 turn_id
	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok, "请求体应包含 client_metadata")
	bodyTurnID, ok := cm["turn_id"].(string)
	require.True(t, ok, "体 client_metadata 应包含 string 类型的 turn_id")

	// 从体内嵌 turn-metadata JSON 提取 turn_id
	embeddedRaw, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok, "体 client_metadata 应包含 x-codex-turn-metadata 字符串")
	var bodyMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(embeddedRaw), &bodyMeta))
	bodyEmbeddedTurnID, ok := bodyMeta["turn_id"].(string)
	require.True(t, ok, "体内嵌 turn-metadata 应包含 string 类型的 turn_id")

	assert.Equal(t, headerTurnID, bodyTurnID, "头和体的 turn_id 必须一致")
	assert.Equal(t, headerTurnID, bodyEmbeddedTurnID, "头和体内嵌 turn-metadata 的 turn_id 必须一致")
	assert.Equal(t, ids.turnID, headerTurnID, "所有 turn_id 都应来自同一份 ids")
	assert.Equal(t, headerMeta["turn_started_at_unix_ms"], bodyMeta["turn_started_at_unix_ms"], "头和体的 timestamp 必须一致")
	assert.Equal(t, float64(ids.turnStartedAtUnixMs), headerMeta["turn_started_at_unix_ms"])
}

func TestFingerprintIDs_MalformedEmbeddedMetadataRebuiltConsistently(t *testing.T) {
	account := newTestOAuthAccount(2, map[string]any{codexFingerprintModeExtraKey: "session"})
	clientHeaders := make(http.Header)
	clientHeaders.Set("session-id", "client-session-malformed")
	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	h := make(http.Header)
	h.Set("x-codex-turn-metadata", "{malformed")
	applyCodexFingerprintHeaders(h, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "client-session-malformed",
			"x-codex-turn-metadata": "[malformed",
		},
	}
	require.True(t, applyCodexFingerprintClientMetadata(reqBody, ids))

	var headerMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(h.Get("x-codex-turn-metadata")), &headerMeta))
	clientMetadata, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	bodyRaw, ok := clientMetadata["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var bodyMeta map[string]any
	require.NoError(t, json.Unmarshal([]byte(bodyRaw), &bodyMeta))

	for _, key := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id", "turn_started_at_unix_ms"} {
		assert.Equal(t, headerMeta[key], bodyMeta[key], "rebuilt metadata field %s must match", key)
	}
}

// --- applyCodexFingerprintClientMetadata ---

func TestApplyCodexFingerprintClientMetadata_OffMode(t *testing.T) {
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original",
		},
	}
	modified := applyCodexFingerprintClientMetadata(reqBody, nil)
	assert.False(t, modified, "nil ids 不改写")
}

func TestApplyCodexFingerprintClientMetadata_DeviceMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "device",
		"openai_device_id":           "converged-device",
	})
	ids := resolveCodexFingerprintIDsFromRequest(account, nil)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"user-session","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "user-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "converged-device", cm["x-codex-installation-id"])
	assert.Equal(t, "user-session", cm["session_id"], "device 模式不改 session_id")

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, "converged-device", meta["installation_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_SessionMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "session",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "client-session-aaa")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	embeddedMeta := `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0","sandbox":"seccomp"}`
	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"x-codex-installation-id": "original-install",
			"session_id":              "original-session",
			"x-codex-turn-metadata":   embeddedMeta,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedInstall := resolveConvergedInstallationID(account, seed)
	convergedSession := resolveConvergedSessionID(codexAccountIdentityNamespace(account) + ":user:0")
	convergedThread := scopeCodexAccountIdentityValue(account, 0, "thread", "client-session-aaa")

	assert.Equal(t, convergedInstall, cm["x-codex-installation-id"])
	assert.Equal(t, convergedSession, cm["session_id"])
	assert.Equal(t, convergedThread, cm["thread_id"])
	assert.Equal(t, convergedThread+":0", cm["x-codex-window-id"])

	turnMetaStr, ok := cm["x-codex-turn-metadata"].(string)
	require.True(t, ok)
	var meta map[string]any
	require.NoError(t, json.Unmarshal([]byte(turnMetaStr), &meta))
	assert.Equal(t, convergedInstall, meta["installation_id"])
	assert.Equal(t, convergedSession, meta["session_id"])
	assert.Equal(t, "seccomp", meta["sandbox"], "非指纹字段保留原样")
}

func TestApplyCodexFingerprintClientMetadata_FullMode(t *testing.T) {
	account := newTestOAuthAccount(1, map[string]any{
		codexFingerprintModeExtraKey: "full",
	})
	clientHeaders := http.Header{}
	clientHeaders.Set("session-id", "any-client")

	ids := resolveCodexFingerprintIDsFromRequest(account, clientHeaders)
	require.NotNil(t, ids)

	reqBody := map[string]any{
		"client_metadata": map[string]any{
			"session_id":            "x",
			"thread_id":             "x",
			"x-codex-turn-metadata": `{"installation_id":"x","session_id":"x","thread_id":"x","turn_id":"x","window_id":"x:0"}`,
		},
	}

	modified := applyCodexFingerprintClientMetadata(reqBody, ids)
	require.True(t, modified)

	cm, ok := reqBody["client_metadata"].(map[string]any)
	require.True(t, ok)
	seed, ok := codexFingerprintSeed(account.Extra)
	require.True(t, ok)
	convergedSession := resolveConvergedSessionID(seed)

	// 客户端带了会话 id：派生该会话专属的 v7 线程，与头改写同一份 ids；
	// thread_id == session_id，且不再是账号级共享会话。
	assert.Equal(t, ids.sessionID, cm["session_id"])
	assert.Equal(t, ids.sessionID, cm["thread_id"], "full 模式 thread_id 应等于 session_id")
	assert.NotEqual(t, convergedSession, cm["session_id"], "有会话信号时不塌缩到账号级会话")
}

// --- extractClientSessionID ---

func TestExtractClientSessionID(t *testing.T) {
	tests := []struct {
		name     string
		headers  http.Header
		expected string
	}{
		{"连字符形式优先", func() http.Header {
			h := http.Header{}
			h.Set("session-id", "hyphen-form")
			h.Set("session_id", "underscore-form")
			return h
		}(), "hyphen-form"},
		{"回退到下划线形式", func() http.Header {
			h := http.Header{}
			h.Set("session_id", "underscore-form")
			return h
		}(), "underscore-form"},
		{"都没有", http.Header{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, extractClientSessionID(tt.headers))
		})
	}
}

// --- 透传路径：raw 字节版 client_metadata 改写 ---

// rawVsMapClientMetadata 用同一份 ids 分别跑 map 版与 raw 字节版，
// 返回两侧最终的 client_metadata 解码结果。
func rawVsMapClientMetadata(t *testing.T, body []byte, ids *codexFingerprintIDs) (map[string]any, map[string]any) {
	t.Helper()

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	applyCodexFingerprintClientMetadata(decoded, ids)
	mapCM, _ := decoded["client_metadata"].(map[string]any)

	rawBody, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)
	var rawDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
	rawCM, _ := rawDecoded["client_metadata"].(map[string]any)
	return mapCM, rawCM
}

func cloneCodexFingerprintIDsForTest(ids *codexFingerprintIDs) *codexFingerprintIDs {
	if ids == nil {
		return nil
	}
	cloned := *ids
	cloned.originalBodySessionID = ""
	cloned.originalBodySessionIDCaptured = false
	return &cloned
}

func applyMapAndRawFingerprintBodiesForTest(t *testing.T, body []byte, ids *codexFingerprintIDs) (map[string]any, map[string]any) {
	t.Helper()

	mapIDs := cloneCodexFingerprintIDsForTest(ids)
	rawIDs := cloneCodexFingerprintIDsForTest(ids)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(body, &decoded))
	applyCodexFingerprintClientMetadata(decoded, mapIDs)

	rawBody, _, err := applyCodexFingerprintClientMetadataRaw(body, rawIDs)
	require.NoError(t, err)
	var rawDecoded map[string]any
	require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
	return decoded, rawDecoded
}

func TestApplyCodexFingerprintPromptCacheKey_MapRawEquivalence(t *testing.T) {
	for _, mode := range []codexFingerprintMode{codexFingerprintSession, codexFingerprintFull} {
		t.Run(string(mode)+"/default", func(t *testing.T) {
			account := newTestOAuthAccount(4300, map[string]any{codexFingerprintModeExtraKey: string(mode)})
			ids := resolveCodexFingerprintIDs(account, "header-session", mode)
			require.NotNil(t, ids)

			body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"body-session","client_metadata":{"session_id":" body-session ","trace":"keep"},"input":[]}`)
			mapBody, rawBody := applyMapAndRawFingerprintBodiesForTest(t, body, ids)

			require.Equal(t, mapBody["prompt_cache_key"], rawBody["prompt_cache_key"])
			require.Equal(t, ids.sessionID, mapBody["prompt_cache_key"])
			mapCM, _ := mapBody["client_metadata"].(map[string]any)
			rawCM, _ := rawBody["client_metadata"].(map[string]any)
			require.Equal(t, ids.sessionID, mapCM["session_id"])
			require.Equal(t, mapCM["session_id"], rawCM["session_id"])
			require.Equal(t, "keep", rawCM["trace"])
		})
	}

	t.Run("explicit override", func(t *testing.T) {
		account := newTestOAuthAccount(4301, map[string]any{codexFingerprintModeExtraKey: "session"})
		ids := resolveCodexFingerprintIDs(account, "header-session", codexFingerprintSession)
		require.NotNil(t, ids)

		body := []byte(`{"model":"gpt-5.6-sol","prompt_cache_key":"explicit-cache","client_metadata":{"session_id":"body-session"},"input":[]}`)
		mapBody, rawBody := applyMapAndRawFingerprintBodiesForTest(t, body, ids)

		require.Equal(t, "explicit-cache", mapBody["prompt_cache_key"])
		require.Equal(t, "explicit-cache", rawBody["prompt_cache_key"])
		mapCM, _ := mapBody["client_metadata"].(map[string]any)
		rawCM, _ := rawBody["client_metadata"].(map[string]any)
		require.Equal(t, ids.sessionID, mapCM["session_id"])
		require.Equal(t, ids.sessionID, rawCM["session_id"])
	})
}

func TestApplyCodexFingerprintPromptCacheKey_Negatives(t *testing.T) {
	sessionAccount := newTestOAuthAccount(4310, map[string]any{codexFingerprintModeExtraKey: "session"})
	sessionIDs := resolveCodexFingerprintIDs(sessionAccount, "header-session", codexFingerprintSession)
	require.NotNil(t, sessionIDs)
	deviceAccount := newTestOAuthAccount(4311, map[string]any{codexFingerprintModeExtraKey: "device"})
	deviceIDs := resolveCodexFingerprintIDs(deviceAccount, "header-session", codexFingerprintDevice)
	require.NotNil(t, deviceIDs)

	tests := []struct {
		name          string
		body          []byte
		ids           *codexFingerprintIDs
		wantExists    bool
		wantCacheKey  any
		wantRawString string
	}{
		{
			name:         "missing key gets the session default",
			body:         []byte(`{"client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: sessionIDs.sessionID,
		},
		{
			name:         "empty key gets the session default",
			body:         []byte(`{"prompt_cache_key":"","client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: sessionIDs.sessionID,
		},
		{
			name:         "whitespace-different key is an explicit override",
			body:         []byte(`{"prompt_cache_key":" body-session ","client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: " body-session ",
		},
		{
			name:         "non-string key preserved",
			body:         []byte(`{"prompt_cache_key":123,"client_metadata":{"session_id":"body-session"}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: float64(123),
		},
		{
			name:         "missing source metadata preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session"}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:         "non-string source session preserves key",
			body:         []byte(`{"prompt_cache_key":"123","client_metadata":{"session_id":123}}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: "123",
		},
		{
			name:         "non-object source metadata preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session","client_metadata":"bad"}`),
			ids:          sessionIDs,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:         "device mode preserves key",
			body:         []byte(`{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`),
			ids:          deviceIDs,
			wantExists:   true,
			wantCacheKey: "body-session",
		},
		{
			name:          "off mode preserves body",
			body:          []byte(`{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`),
			ids:           nil,
			wantExists:    true,
			wantCacheKey:  "body-session",
			wantRawString: `{"prompt_cache_key":"body-session","client_metadata":{"session_id":"body-session"}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mapBody map[string]any
			require.NoError(t, json.Unmarshal(tt.body, &mapBody))
			changedMap := applyCodexFingerprintClientMetadata(mapBody, cloneCodexFingerprintIDsForTest(tt.ids))

			rawBody, changedRaw, err := applyCodexFingerprintClientMetadataRaw(tt.body, cloneCodexFingerprintIDsForTest(tt.ids))
			require.NoError(t, err)
			if tt.ids == nil {
				require.False(t, changedMap)
				require.False(t, changedRaw)
				require.JSONEq(t, tt.wantRawString, string(rawBody))
				return
			}
			require.True(t, changedMap)
			require.True(t, changedRaw)

			rawDecoded := map[string]any{}
			require.NoError(t, json.Unmarshal(rawBody, &rawDecoded))
			_, mapExists := mapBody["prompt_cache_key"]
			_, rawExists := rawDecoded["prompt_cache_key"]
			require.Equal(t, tt.wantExists, mapExists)
			require.Equal(t, tt.wantExists, rawExists)
			if tt.wantExists {
				require.Equal(t, tt.wantCacheKey, mapBody["prompt_cache_key"])
				require.Equal(t, tt.wantCacheKey, rawDecoded["prompt_cache_key"])
			}
		})
	}
}

func TestApplyCodexFingerprintClientMetadataRaw_MatchesMapVariant(t *testing.T) {
	embedded := `{\"installation_id\":\"real-install\",\"session_id\":\"real-session\",\"sandbox\":\"seatbelt\"}`
	bodies := map[string]string{
		"no_client_metadata": `{"model":"gpt-5.6-sol","input":[],"stream":true}`,
		"object_with_extras": `{"model":"gpt-5.6-sol","client_metadata":{"session_id":"client-session","traceparent":"00-abc-def-01","x-codex-turn-metadata":"` + embedded + `"},"stream":true}`,
		"non_object_value":   `{"model":"gpt-5.6-sol","client_metadata":"bogus","stream":true}`,
	}
	for _, mode := range []codexFingerprintMode{codexFingerprintDevice, codexFingerprintSession, codexFingerprintFull} {
		account := newTestOAuthAccount(4242, map[string]any{codexFingerprintModeExtraKey: string(mode)})
		ids := resolveCodexFingerprintIDs(account, "client-sess-raw", mode)
		require.NotNil(t, ids)
		for name, body := range bodies {
			t.Run(string(mode)+"/"+name, func(t *testing.T) {
				mapCM, rawCM := rawVsMapClientMetadata(t, []byte(body), ids)
				mapTurnRaw, hasMapTurn := mapCM["x-codex-turn-metadata"].(string)
				rawTurnRaw, hasRawTurn := rawCM["x-codex-turn-metadata"].(string)
				require.Equal(t, hasMapTurn, hasRawTurn)
				if hasMapTurn {
					var mapTurn, rawTurn map[string]any
					require.NoError(t, json.Unmarshal([]byte(mapTurnRaw), &mapTurn))
					require.NoError(t, json.Unmarshal([]byte(rawTurnRaw), &rawTurn))
					mapStarted, mapStartedOK := mapTurn["turn_started_at_unix_ms"].(float64)
					rawStarted, rawStartedOK := rawTurn["turn_started_at_unix_ms"].(float64)
					require.Equal(t, mapStartedOK, rawStartedOK)
					if mapStartedOK {
						require.InDelta(t, mapStarted, rawStarted, 100)
						mapTurn["turn_started_at_unix_ms"] = float64(0)
						rawTurn["turn_started_at_unix_ms"] = float64(0)
					}
					mapTurnNormalized, err := json.Marshal(mapTurn)
					require.NoError(t, err)
					rawTurnNormalized, err := json.Marshal(rawTurn)
					require.NoError(t, err)
					mapCM["x-codex-turn-metadata"] = string(mapTurnNormalized)
					rawCM["x-codex-turn-metadata"] = string(rawTurnNormalized)
				}
				assert.Equal(t, mapCM, rawCM, "raw 字节版与 map 版的 client_metadata 结果必须逐点一致")
			})
		}
	}
}

func TestApplyCodexFingerprintClientMetadataRaw_PreservesUnrelatedFields(t *testing.T) {
	account := newTestOAuthAccount(4243, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-sess-preserve", codexFingerprintSession)
	require.NotNil(t, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"message","role":"user","content":"hi"}],"stream":true,"prompt_cache_key":"pck-1"}`)
	out, changed, err := applyCodexFingerprintClientMetadataRaw(body, ids)
	require.NoError(t, err)
	require.True(t, changed)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(out, &decoded))
	assert.Equal(t, "gpt-5.6-sol", decoded["model"])
	assert.Equal(t, "pck-1", decoded["prompt_cache_key"])
	assert.Equal(t, true, decoded["stream"])
	cm, _ := decoded["client_metadata"].(map[string]any)
	require.NotNil(t, cm)
	assert.Equal(t, ids.sessionID, cm["session_id"])
	assert.Equal(t, ids.turnID, cm["turn_id"])
}

func TestApplyCodexFingerprintClientMetadataRaw_Noop(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol"}`)
	out, changed, err := applyCodexFingerprintClientMetadataRaw(body, nil)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, body, out)

	out, changed, err = applyCodexFingerprintClientMetadataRaw(nil, &codexFingerprintIDs{mode: codexFingerprintSession, installationID: "x"})
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Nil(t, out)
}

// --- context 暂存与出站头应用（透传/非透传共用 seam）---

func newFingerprintStageTestContext(t *testing.T) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	return c
}

func TestStageCodexFingerprintIDs_NilOverwritesPreviousAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	accountA := newTestOAuthAccount(1001, map[string]any{codexFingerprintModeExtraKey: "session"})
	idsA := resolveCodexFingerprintIDs(accountA, "sess-x", codexFingerprintSession)
	require.NotNil(t, idsA)
	stageCodexFingerprintIDs(c, idsA)

	// failover 切到 off 模式账号：无条件覆写为 nil，上一账号 IDs 不得残留
	stageCodexFingerprintIDs(c, nil)

	h := http.Header{}
	h.Set("session_id", "isolated-session")
	accountB := newTestOAuthAccount(1002, map[string]any{"codex_fingerprint_mode": "off"})
	applyStagedCodexFingerprintHeaders(c, accountB, h)
	assert.Equal(t, "isolated-session", h.Get("session_id"), "off 账号不得应用上一账号的收敛 ID")
	assert.Empty(t, h.Get("x-codex-installation-id"))
}

func TestApplyStagedCodexFingerprintRejectsDifferentOAuthAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	accountA := newTestOAuthAccount(1003, map[string]any{codexFingerprintModeExtraKey: "session"})
	idsA := resolveCodexFingerprintIDs(accountA, "sess-a", codexFingerprintSession)
	require.NotNil(t, idsA)
	stageCodexFingerprintIDs(c, idsA)

	accountB := newTestOAuthAccount(1004, map[string]any{codexFingerprintModeExtraKey: "session"})
	h := make(http.Header)
	h.Set("session-id", "account-b-session")
	applyStagedCodexFingerprintHeaders(c, accountB, h)
	assert.Equal(t, "account-b-session", h.Get("session-id"))
	assert.Empty(t, h.Get("x-codex-installation-id"))

	body := map[string]any{"client_metadata": map[string]any{"session_id": "account-b-session"}}
	assert.False(t, applyStagedCodexFingerprintClientMetadata(c, accountB, body))
	clientMetadata, ok := body["client_metadata"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "account-b-session", clientMetadata["session_id"])
}

func TestApplyStagedCodexFingerprintHeaders_SkipsNonOAuthAccount(t *testing.T) {
	c := newFingerprintStageTestContext(t)
	oauthIDs := resolveCodexFingerprintIDs(newTestOAuthAccount(1003, map[string]any{codexFingerprintModeExtraKey: "session"}), "sess-y", codexFingerprintSession)
	require.NotNil(t, oauthIDs)
	stageCodexFingerprintIDs(c, oauthIDs)

	h := http.Header{}
	apiKeyAccount := &Account{ID: 1004, Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	applyStagedCodexFingerprintHeaders(c, apiKeyAccount, h)
	assert.Empty(t, h.Get("x-codex-installation-id"), "stale 收敛 ID 不得应用到非 OAuth 账号")
}

func TestBuildUpstreamRequestOpenAIPassthrough_AppliesStagedFingerprint(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTestOAuthAccount(2001, map[string]any{
		"openai_oauth_passthrough": true,
		"codex_fingerprint_mode":   "session",
	})

	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session_id", "real-client-session")
	c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.1 (Ubuntu 22.4.0; x86_64) xterm-256color")
	c.Request.Header.Set("originator", "codex_cli_rs")
	c.Request.Header.Set("x-codex-turn-metadata", `{"installation_id":"real-install","session_id":"real-session","sandbox":"seatbelt"}`)

	// 复刻 forwardOpenAIPassthrough 的解析+暂存 seam（默认 session 模式）
	ids := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header)
	require.NotNil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[],"stream":true}`)
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
	require.NoError(t, err)

	assert.Equal(t, ids.sessionID, req.Header.Get("session-id"), "session 模式下出站 session-id 应为账号级收敛值")
	assert.Empty(t, req.Header.Get("session_id"), "实验投影默认开时不下发下划线会话别名")
	// x-codex-installation-id 官方 HTTP 头不发，仅 client_metadata 携带。
	assert.Empty(t, req.Header.Get("x-codex-installation-id"))
	assert.Equal(t, ids.windowID, req.Header.Get("x-codex-window-id"))
	assert.Equal(t, ids.threadID, req.Header.Get("thread-id"))
	assert.Equal(t, ids.threadID, req.Header.Get("x-client-request-id"))
	turnMetadata := req.Header.Get("x-codex-turn-metadata")
	require.NotEmpty(t, turnMetadata)
	assert.Contains(t, turnMetadata, ids.sessionID, "turn-metadata JSON 中的 session_id 应被收敛")
	assert.Contains(t, turnMetadata, `"sandbox":"seatbelt"`, "preserve the client's actual sandbox")
}

func TestBuildUpstreamRequestOpenAIPassthrough_OffModeKeepsIsolatedSession(t *testing.T) {
	svc := &OpenAIGatewayService{}
	account := newTestOAuthAccount(2002, map[string]any{
		"openai_oauth_passthrough": true,
		"codex_fingerprint_mode":   "off",
	})

	c := newFingerprintStageTestContext(t)
	c.Request.Header.Set("session_id", "real-client-session")
	c.Request.Header.Set("originator", "codex_cli_rs")

	ids := resolveCodexFingerprintIDsFromRequest(account, c.Request.Header)
	require.Nil(t, ids)
	stageCodexFingerprintIDs(c, ids)

	body := []byte(`{"model":"gpt-5.6-sol","input":[],"stream":true}`)
	req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, body, "test-token")
	require.NoError(t, err)

	// off 模式仍走隔离：会话值落在官方 session-id 头上，且不得等于账号级收敛值；
	// 官方从不发的 session_id / x-codex-installation-id 头由线序收口削平。
	assert.NotEmpty(t, req.Header.Get("session-id"))
	assert.NotEqual(t, resolveConvergedSessionID(testCodexFingerprintSeed), req.Header.Get("session-id"), "off 模式不得收敛 session-id")
	assert.Empty(t, req.Header.Get("session_id"))
	assert.Empty(t, req.Header.Get("x-codex-installation-id"))
	assert.NotEmpty(t, req.Header.Get("x-codex-window-id"), "Codex metadata defaults should include a context window")
}

func TestApplyCodexFingerprintClientMetadataRaw_NonObjectBodyUntouched(t *testing.T) {
	account := newTestOAuthAccount(4244, map[string]any{codexFingerprintModeExtraKey: "session"})
	ids := resolveCodexFingerprintIDs(account, "client-sess-nonobj", codexFingerprintSession)
	require.NotNil(t, ids)

	for _, body := range []string{`[1,2,3]`, `"plain string"`, `not json at all`} {
		out, changed, err := applyCodexFingerprintClientMetadataRaw([]byte(body), ids)
		require.NoError(t, err)
		assert.False(t, changed, "非 JSON 对象 body 不应被改写: %s", body)
		assert.Equal(t, []byte(body), out)
	}
}

func TestCodexFingerprintIdentity_AnonymousRequestsAndRetries(t *testing.T) {
	for _, mode := range []string{"session", "full"} {
		for _, payload := range []string{
			`{"input":[{"role":"user","content":"same prompt"}]}`,
			`{"prompt_cache_key":"shared-prefix","input":[{"role":"user","content":"same prompt"}]}`,
			`{"input":[]}`,
		} {
			t.Run(mode+"/"+payload, func(t *testing.T) {
				account := newTestOAuthAccount(51, map[string]any{codexFingerprintModeExtraKey: mode})
				c := newFingerprintStageTestContext(t)
				c.Set("api_key", &APIKey{ID: 7})
				first := resolveCodexFingerprintIDsForRawBody(c, account, nil, []byte(payload))
				retry := resolveCodexFingerprintIDsForRawBody(c, account, nil, []byte(payload))
				require.Equal(t, first.sessionID, retry.sessionID)
				require.Equal(t, first.turnID, retry.turnID)
				require.Equal(t, first.turnStartedAtUnixMs, retry.turnStartedAtUnixMs)
				require.Equal(t, first.sessionID, first.threadID)
				requireUUIDv7(t, first.sessionID)

				otherContext := newFingerprintStageTestContext(t)
				otherContext.Set("api_key", &APIKey{ID: 7})
				other := resolveCodexFingerprintIDsForRawBody(otherContext, account, nil, []byte(payload))
				require.NotEqual(t, first.sessionID, other.sessionID, "identical prompts/cache keys cannot join anonymous requests")
				require.NotEqual(t, first.turnID, other.turnID)
				require.Equal(t, first.installationID, other.installationID)

				failover := newTestOAuthAccount(52, map[string]any{codexFingerprintModeExtraKey: mode, codexFingerprintSeedExtraKey: "22222222-2222-4222-8222-222222222222"})
				next := resolveCodexFingerprintIDsForRawBody(c, failover, nil, []byte(payload))
				require.NotEqual(t, first.sessionID, next.sessionID)
				require.NotEqual(t, first.turnID, next.turnID)
			})
		}
	}
}

func TestCodexFingerprintIdentity_RelationshipsAndCarrierEquivalence(t *testing.T) {
	account := newTestOAuthAccount(53, nil)
	rootThread := convTestSession
	childThread := "01a07c73-e499-7aaa-8bbb-cccccccccccc"
	rootTurn := convTestTurn
	childTurn := "01a07c73-e500-7aaa-8bbb-cccccccccccc"
	rootMeta := map[string]any{"session_id": rootThread, "thread_id": rootThread, "turn_id": rootTurn, "root_turn_id": rootTurn}
	resolve := func(metadata map[string]any, headerOnly bool, apiKeyID int64) (*gin.Context, *codexFingerprintIDs, map[string]any) {
		t.Helper()
		c := newFingerprintStageTestContext(t)
		c.Set("api_key", &APIKey{ID: apiKeyID})
		raw, err := json.Marshal(metadata)
		require.NoError(t, err)
		body := map[string]any{"input": []any{map[string]any{"role": "user", "content": "same prompt"}}}
		if headerOnly {
			c.Request.Header.Set(openAIWSTurnMetadataHeader, string(raw))
		} else {
			body["client_metadata"] = map[string]any{openAIWSTurnMetadataHeader: string(raw)}
		}
		ids := resolveCodexFingerprintIDsForDecodedBody(c, account, nil, body)
		require.NotNil(t, ids)
		return c, ids, body
	}
	_, root, _ := resolve(rootMeta, false, 7)
	require.Equal(t, root.sessionID, root.threadID)
	require.Equal(t, root.turnID, root.rootTurnID)

	childMeta := map[string]any{
		"session_id": rootThread, "thread_id": childThread, "turn_id": childTurn,
		"parent_thread_id": rootThread, "forked_from_thread_id": rootThread,
		"root_turn_id": rootTurn, "parent_turn_id": rootTurn,
		"window_number": 3, "turn_started_at_unix_ms": int64(1792601171792),
		"traceparent": "00-11111111111111111111111111111111-2222222222222222-01",
		"sandbox":     "seatbelt", "opaque": map[string]any{"keep": true},
	}
	c, child, body := resolve(childMeta, false, 7)
	_, headerChild, _ := resolve(childMeta, true, 7)
	require.Equal(t, child.sessionID, headerChild.sessionID)
	require.Equal(t, child.threadID, headerChild.threadID)
	require.Equal(t, child.turnID, headerChild.turnID)
	require.Equal(t, root.sessionID, child.sessionID)
	require.NotEqual(t, child.sessionID, child.threadID)
	require.Equal(t, root.threadID, child.parentThreadID)
	require.Equal(t, root.threadID, child.forkedThreadID)
	require.Equal(t, root.turnID, child.parentTurnID)
	require.Equal(t, root.turnID, child.rootTurnID)
	require.Equal(t, codexUUIDv7Millis(childThread), codexUUIDv7Millis(child.threadID))
	require.Equal(t, codexUUIDv7Millis(childTurn), codexUUIDv7Millis(child.turnID))
	require.EqualValues(t, 1792601171792, child.turnStartedAtUnixMs)

	_, otherKey, _ := resolve(childMeta, false, 8)
	require.NotEqual(t, child.sessionID, otherKey.sessionID)
	require.NotEqual(t, child.threadID, otherKey.threadID)
	require.NotEqual(t, child.turnID, otherKey.turnID)
	require.NotEqual(t, child.parentThreadID, otherKey.parentThreadID)
	require.NotEqual(t, child.rootTurnID, otherKey.rootTurnID)

	// Production order: resolve raw evidence, scope the body once, then project
	// the precomputed IDs. The reference fields must match the parent's IDs.
	rawBody, err := json.Marshal(body)
	require.NoError(t, err)
	rawIDs := resolveCodexFingerprintIDsForRawBody(c, account, nil, rawBody)
	require.True(t, applyCodexAccountIdentityClientMetadataMap(body, account, 7))
	require.True(t, applyCodexFingerprintClientMetadata(body, child))
	scopedRaw, _, err := applyCodexAccountIdentityClientMetadataRaw(rawBody, account, 7)
	require.NoError(t, err)
	projectedRaw, _, err := applyCodexFingerprintClientMetadataRaw(scopedRaw, rawIDs)
	require.NoError(t, err)
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	require.JSONEq(t, string(encoded), string(projectedRaw))

	metadata := gjson.GetBytes(projectedRaw, "client_metadata")
	embedded := gjson.Parse(metadata.Get(openAIWSTurnMetadataHeader).String())
	require.Equal(t, root.threadID, metadata.Get("parent_thread_id").String())
	require.Equal(t, root.threadID, embedded.Get("forked_from_thread_id").String())
	require.Equal(t, root.turnID, embedded.Get("root_turn_id").String())
	require.Equal(t, root.turnID, embedded.Get("parent_turn_id").String())
	require.Equal(t, child.threadID+":3", metadata.Get("x-codex-window-id").String())
	require.Equal(t, childMeta["traceparent"], embedded.Get("traceparent").String())
	require.Equal(t, "seatbelt", embedded.Get("sandbox").String())
	require.True(t, embedded.Get("opaque.keep").Bool())

	headers := http.Header{}
	metaJSON, err := json.Marshal(childMeta)
	require.NoError(t, err)
	headers.Set(openAIWSTurnMetadataHeader, string(metaJSON))
	applyCodexAccountIdentityHeaders(headers, account, 7)
	applyCodexFingerprintHeaders(headers, child)
	require.Equal(t, root.threadID, headers.Get("x-codex-parent-thread-id"))
	require.Equal(t, child.threadID, headers.Get("thread-id"))
	headerMetadata := gjson.Parse(headers.Get(openAIWSTurnMetadataHeader))
	require.Equal(t, root.turnID, headerMetadata.Get("parent_turn_id").String())
	require.Equal(t, root.turnID, headerMetadata.Get("root_turn_id").String())
}

func TestCodexFingerprintIdentity_BodyEvidencePrecedesHeaders(t *testing.T) {
	account := newTestOAuthAccount(54, nil)
	c := newFingerprintStageTestContext(t)
	c.Set("api_key", &APIKey{ID: 9})
	c.Request.Header.Set("session-id", "stale-header-session")
	c.Request.Header.Set("thread-id", "stale-header-thread")
	c.Request.Header.Set(openAIWSTurnMetadataHeader, `{"turn_id":"stale-turn","turn_started_at_unix_ms":1}`)
	body := []byte(`{"client_metadata":{"session_id":"real-session","thread_id":"real-child","turn_id":"real-turn","turn_started_at_unix_ms":1792601171792}}`)
	ids := resolveCodexFingerprintIDsForRawBody(c, account, nil, body)
	require.Equal(t, scopeCodexAccountIdentityValue(account, 9, "session", "real-session"), ids.sessionID)
	require.Equal(t, scopeCodexAccountIdentityValue(account, 9, "thread", "real-child"), ids.threadID)
	require.Equal(t, scopeCodexAccountIdentityValue(account, 9, "turn", "real-turn"), ids.turnID)
	require.EqualValues(t, 1792601171792, ids.turnStartedAtUnixMs)
}

func TestCodexFingerprintPromptCachePolicy_MapRaw(t *testing.T) {
	for _, mode := range []string{"session", "full"} {
		for _, policy := range []codexPromptCacheKeyPolicy{codexPromptCacheKeySessionDerived, codexPromptCacheKeyCustom, codexPromptCacheKeyOmit} {
			for _, key := range []string{"", "real-session", "explicit-cache"} {
				t.Run(mode+"/"+string(policy)+"/"+key, func(t *testing.T) {
					account := newTestOAuthAccount(55, map[string]any{codexFingerprintModeExtraKey: mode, codexPromptCacheKeyExtraKey: string(policy)})
					c := newFingerprintStageTestContext(t)
					c.Set("api_key", &APIKey{ID: 9})
					body := map[string]any{"client_metadata": map[string]any{"session_id": "real-session", "turn_id": "real-turn"}}
					if key != "" {
						body["prompt_cache_key"] = key
					}
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					ids := resolveCodexFingerprintIDsForRawBody(c, account, nil, raw)
					scoped, _, err := applyCodexAccountIdentityClientMetadataRaw(raw, account, 9)
					require.NoError(t, err)
					mapped, projected := applyMapAndRawFingerprintBodiesForTest(t, scoped, ids)
					require.Equal(t, mapped, projected)
					if policy == codexPromptCacheKeyOmit || (policy == codexPromptCacheKeyCustom && key == "") {
						require.NotContains(t, mapped, "prompt_cache_key")
					} else if policy == codexPromptCacheKeySessionDerived && (key == "" || key == "real-session") {
						require.Equal(t, ids.sessionID, mapped["prompt_cache_key"])
					} else {
						require.Equal(t, scopeCodexAccountIdentityValue(account, 9, "prompt-cache", key), mapped["prompt_cache_key"])
					}
				})
			}
		}
	}
}

func TestCodexFingerprintIdentity_ThreadAndTurnOnlyEvidence(t *testing.T) {
	account := newTestOAuthAccount(56, nil)
	for _, body := range []string{
		`{"client_metadata":{"thread_id":"` + convTestThread + `","turn_id":"` + convTestTurn + `"}}`,
		`{"client_metadata":{"turn_id":"` + convTestTurn + `"}}`,
	} {
		first := resolveCodexFingerprintIDsForRawBody(newFingerprintStageTestContext(t), account, nil, []byte(body))
		second := resolveCodexFingerprintIDsForRawBody(newFingerprintStageTestContext(t), account, nil, []byte(body))
		require.Equal(t, first.sessionID, second.sessionID)
		require.Equal(t, first.threadID, second.threadID)
		require.Equal(t, first.turnID, second.turnID)
		require.Equal(t, first.sessionID, first.threadID)
		require.Equal(t, codexUUIDv7Millis(convTestTurn), first.turnStartedAtUnixMs)
	}
}

func TestCodexFingerprintIdentity_SessionModeIsolatesAPIKeys(t *testing.T) {
	account := newTestOAuthAccount(57, map[string]any{codexFingerprintModeExtraKey: "session"})
	c := newFingerprintStageTestContext(t)
	c.Set("api_key", &APIKey{ID: 1})
	c.Request.Header.Set("session-id", convTestSession)
	first := resolveCodexFingerprintIDsWithBody(c, account, nil, nil)
	c.Set("api_key", &APIKey{ID: 2})
	second := resolveCodexFingerprintIDsWithBody(c, account, nil, nil)
	require.NotEqual(t, first.sessionID, second.sessionID)
	require.NotEqual(t, first.threadID, second.threadID)
}

func TestCodexFingerprintIdentity_InapplicableAccounts(t *testing.T) {
	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey, Extra: map[string]any{codexFingerprintSeedExtraKey: testCodexFingerprintSeed}},
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth, Extra: map[string]any{codexFingerprintSeedExtraKey: testCodexFingerprintSeed}},
		{Platform: PlatformOpenAI, Type: AccountTypeOAuth},
	} {
		require.Nil(t, resolveCodexFingerprintIDsForRawBody(newFingerprintStageTestContext(t), account, nil, []byte(`{"input":[]}`)))
	}
}

func TestCodexFingerprintIdentity_PreservesUnknownMetadataNumbers(t *testing.T) {
	account := newTestOAuthAccount(59, nil)
	body := []byte(`{"input":[],"client_metadata":{"session_id":"client-session","unknown_counter":9007199254740993,"unknown":{"id":"opaque"},"x-codex-turn-metadata":"{\"turn_id\":\"client-turn\",\"unknown_counter\":9007199254740993,\"traceparent\":\"keep-trace\"}"}}`)
	ids := resolveCodexFingerprintIDsForRawBody(newFingerprintStageTestContext(t), account, nil, body)
	scoped, _, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 0)
	require.NoError(t, err)
	out, _, err := applyCodexFingerprintClientMetadataRaw(scoped, ids)
	require.NoError(t, err)
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "client_metadata.unknown_counter").Raw)
	require.Equal(t, "opaque", gjson.GetBytes(out, "client_metadata.unknown.id").String())
	embedded := gjson.Parse(gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata").String())
	require.Equal(t, "9007199254740993", embedded.Get("unknown_counter").Raw)
	require.Equal(t, "keep-trace", embedded.Get("traceparent").String())
}

func TestCodexFingerprintIdentity_ParentWithoutChildIdentity(t *testing.T) {
	account := newTestOAuthAccount(60, nil)
	body := []byte(`{"client_metadata":{"session_id":"root","parent_thread_id":"root"}}`)
	c := newFingerprintStageTestContext(t)
	first := resolveCodexFingerprintIDsForRawBody(c, account, nil, body)
	retry := resolveCodexFingerprintIDsForRawBody(c, account, nil, body)
	sibling := resolveCodexFingerprintIDsForRawBody(newFingerprintStageTestContext(t), account, nil, body)
	require.Equal(t, first.sessionID, first.parentThreadID)
	require.NotEqual(t, first.sessionID, first.threadID)
	require.Equal(t, first.threadID, retry.threadID)
	require.NotEqual(t, first.threadID, sibling.threadID, "a parent alone does not identify a child")
	require.Equal(t, first.parentThreadID, sibling.parentThreadID)
}
