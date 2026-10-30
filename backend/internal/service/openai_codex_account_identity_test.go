package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexInputTurnReferencesFollowAccountIdentity(t *testing.T) {
	const current = "019abc00-0000-7000-8000-000000000001"
	const historical = "019abc00-0000-7000-8000-000000000002"
	body := []byte(fmt.Sprintf(` {"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":%q,"content_item_kinds":["text"]}},{"type":"function_call","arguments":%q,"internal_chat_message_metadata_passthrough":{"turn_id":%q}},{"type":"function_call_output","output":%q,"internal_chat_message_metadata_passthrough":{"turn_id":%q}}],"client_metadata":{"turn_id":%q},"prompt_cache_key":"session"} `, current, historical, current, current, historical, current, current))
	original := bytes.Clone(body)
	accountA := newTestOAuthAccount(51, map[string]any{codexFingerprintModeExtraKey: "full"})
	accountA.Credentials = map[string]any{"chatgpt_account_id": "input-account-a"}
	accountB := newTestOAuthAccount(52, map[string]any{codexFingerprintModeExtraKey: "full"})
	accountB.Credentials = map[string]any{"chatgpt_account_id": "input-account-b"}
	var previousTurn string
	for _, selection := range []struct {
		account *Account
		key     int64
	}{{accountA, 7}, {accountB, 7}, {accountA, 8}} {
		scoped, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, selection.account, selection.key)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, original, body, "retry/failover must retain the original identity source")
		currentTurn := gjson.GetBytes(scoped, "client_metadata.turn_id").String()
		require.NotEqual(t, current, currentTurn)
		require.NotEqual(t, previousTurn, currentTurn, "different credential or API key must not share a turn")
		previousTurn = currentTurn
		for _, index := range []int{1, 2} {
			require.Equal(t, currentTurn, gjson.GetBytes(scoped, fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough.turn_id", index)).String())
		}
		historyTurn := gjson.GetBytes(scoped, "input.0.internal_chat_message_metadata_passthrough.turn_id").String()
		require.NotEqual(t, currentTurn, historyTurn, "history must not be relabeled as the current turn")
		require.Equal(t, scopeCodexAccountIdentityValue(selection.account, selection.key, "turn", historical), historyTurn)
		require.Equal(t, current, gjson.GetBytes(scoped, "input.0.content.0.text").String())
		require.Equal(t, current, gjson.GetBytes(scoped, "input.1.arguments").String())
		require.Equal(t, historical, gjson.GetBytes(scoped, "input.2.output").String())
		var mapped map[string]any
		require.NoError(t, json.Unmarshal(body, &mapped))
		require.True(t, applyCodexAccountIdentityClientMetadataMap(mapped, selection.account, selection.key))
		encoded, err := json.Marshal(mapped)
		require.NoError(t, err)
		require.JSONEq(t, string(scoped), string(encoded), "raw and decoded entry points must use identical mappings")
	}
	setup := &Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{"access_token": "setup"}}
	scoped, _, err := applyCodexAccountIdentityClientMetadataRaw(body, setup, 7)
	require.NoError(t, err)
	require.Equal(t, current, gjson.GetBytes(scoped, "input.1.internal_chat_message_metadata_passthrough.turn_id").String())
}

func BenchmarkCodexAccountIdentityConversation(b *testing.B) {
	account := newTestOAuthAccount(51, map[string]any{codexFingerprintModeExtraKey: "full"})
	account.Credentials = map[string]any{"chatgpt_account_id": "bench-account"}
	for _, count := range []int{1, 64} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			items := make([]string, count)
			for i := range items {
				items[i] = fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":%q}],"internal_chat_message_metadata_passthrough":{"turn_id":"019abc00-0000-7000-8000-000000000001"}}`, strings.Repeat("content ", 512))
			}
			body := []byte(`{"input":[` + strings.Join(items, ",") + `],"client_metadata":{"turn_id":"019abc00-0000-7000-8000-000000000001","session_id":"client-session"},"prompt_cache_key":"client-session"}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				result, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 7)
				if err != nil || !changed || len(result) == 0 {
					b.Fatal("identity rewrite did not execute", err)
				}
			}
		})
	}
}

func TestCodexRequestBodyIdentityNamespaceIsStablePerOAuthAccount(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-codex","prompt_cache_key":"client-session","client_metadata":{"x-codex-installation-id":"client-installation","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window","x-codex-turn-metadata":"{\"installation_id\":\"client-installation\",\"session_id\":\"client-session\",\"thread_id\":\"client-thread\",\"turn_id\":\"client-turn\",\"window_id\":\"client-window\"}"}}`)
	account11 := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11"}}
	account19 := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-19"}}

	first, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account11, 77)
	require.NoError(t, err)
	require.True(t, changed)
	firstAgain, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account11, 77)
	require.NoError(t, err)
	require.True(t, changed)
	second, changed, err := applyCodexAccountIdentityClientMetadataRaw(body, account19, 77)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, string(first), string(firstAgain))

	paths := []string{
		"prompt_cache_key",
		"client_metadata.x-codex-installation-id",
		"client_metadata.session_id",
		"client_metadata.thread_id",
		"client_metadata.x-codex-window-id",
	}
	for _, path := range paths {
		require.NotEqual(t, gjson.GetBytes(body, path).String(), gjson.GetBytes(first, path).String(), path)
		require.NotEqual(t, gjson.GetBytes(first, path).String(), gjson.GetBytes(second, path).String(), path)
	}
	require.Equal(t, gjson.GetBytes(first, "prompt_cache_key").String(), gjson.GetBytes(first, "client_metadata.session_id").String())

	var embeddedFirst map[string]any
	var embeddedSecond map[string]any
	require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(first, "client_metadata.x-codex-turn-metadata").String()), &embeddedFirst))
	require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(second, "client_metadata.x-codex-turn-metadata").String()), &embeddedSecond))
	for _, field := range []string{"installation_id", "session_id", "thread_id", "turn_id", "window_id"} {
		require.NotEqual(t, embeddedFirst[field], embeddedSecond[field], field)
	}
}

func TestCodexAccountIdentityNamespaceUsesStableCredentialSource(t *testing.T) {
	firstRow := &Account{ID: 8, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "shared-upstream-account"}}
	secondRow := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "shared-upstream-account"}}
	require.Equal(t, codexAccountIdentityNamespace(firstRow), codexAccountIdentityNamespace(secondRow))

	firstUser := &Account{ID: 20, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-account", "chatgpt_user_id": "user-1"}}
	sameUser := &Account{ID: 21, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-account", "chatgpt_user_id": "user-1"}}
	secondUser := &Account{ID: 22, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "team-account", "chatgpt_user_id": "user-2"}}
	require.Equal(t, codexAccountIdentityNamespace(firstUser), codexAccountIdentityNamespace(sameUser))
	require.NotEqual(t, codexAccountIdentityNamespace(firstUser), codexAccountIdentityNamespace(secondUser))

	seed := "11111111-1111-4111-8111-111111111111"
	seeded := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{codexFingerprintSeedExtraKey: seed}}
	require.Equal(t, "seed:"+seed, codexAccountIdentityNamespace(seeded))

	// Local row IDs repeat across independent deployments, so they are not a
	// safe fallback for upstream identity.
	require.Empty(t, codexAccountIdentityNamespace(&Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth}))

	setupTokenA := &Account{ID: 30, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{"access_token": "setup-token-a"}}
	setupTokenADuplicate := &Account{ID: 31, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{"access_token": "setup-token-a"}}
	setupTokenB := &Account{ID: 32, Platform: PlatformOpenAI, Type: AccountTypeSetupToken, Credentials: map[string]any{"access_token": "setup-token-b"}}
	setupNamespace := codexAccountIdentityNamespace(setupTokenA)
	require.NotEmpty(t, setupNamespace)
	require.NotContains(t, setupNamespace, "setup-token-a")
	require.Equal(t, setupNamespace, codexAccountIdentityNamespace(setupTokenADuplicate))
	require.NotEqual(t, setupNamespace, codexAccountIdentityNamespace(setupTokenB))
}

func TestCodexAccountIdentitySourceOverwritesFailoverContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	account := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"chatgpt_account_id": "team-account",
		"chatgpt_user_id":    "user-1",
	}}
	service := &OpenAIGatewayService{}

	resolved, err := service.prepareCodexAccountIdentitySource(context.Background(), c, account)
	require.NoError(t, err)
	require.Same(t, account, resolved)
	require.Same(t, account, codexAccountIdentitySource(c, nil))

	req, err := service.buildUpstreamRequest(
		context.Background(), c, account,
		[]byte(`{"model":"gpt-5.6-codex","stream":true,"prompt_cache_key":"client-session"}`),
		"token", true, "client-session", true,
	)
	require.NoError(t, err)
	require.Equal(t, canonicalCodexSessionHeaderValue(isolateOpenAIUpstreamSessionID(0, account, "client-session")), req.Header.Get("session-id"))

	next := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"chatgpt_account_id": "other-account",
		"chatgpt_user_id":    "user-2",
	}}
	resolved, err = service.prepareCodexAccountIdentitySource(context.Background(), c, next)
	require.NoError(t, err)
	require.Same(t, next, resolved)
	require.Same(t, next, codexAccountIdentitySource(c, account))
}

func TestBuildOpenAIWSHeadersNamespacesCodexIdentityByOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	c.Set("api_key_id", int64(77))
	c.Request.Header.Set("x-codex-installation-id", "client-installation")
	c.Request.Header.Set("thread-id", "client-thread")
	c.Request.Header.Set("x-codex-window-id", "client-window")
	c.Request.Header.Set("x-client-request-id", "client-request")

	account11 := &Account{ID: 11, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-11"}}
	account19 := &Account{ID: 19, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "chatgpt-account-19"}}
	service := &OpenAIGatewayService{}
	build := func(account *Account) http.Header {
		headers, _, err := service.buildOpenAIWSHeaders(
			context.Background(), c, account, "token",
			OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2},
			true, "", "", "client-session", "", "",
		)
		require.NoError(t, err)
		return headers
	}

	first := build(account11)
	firstAgain := build(account11)
	second := build(account19)
	// 官方线序：session_id/x-codex-installation-id 是网关旧习惯头，projectCodexOutboundHeaders
	// 会在出站收口删掉；身份隔离落在官方字段 session-id/thread-id 上。
	for _, header := range []string{"session-id", "thread-id", "x-codex-window-id", "x-client-request-id"} {
		require.NotEmpty(t, first.Get(header), header)
		require.Equal(t, first.Get(header), firstAgain.Get(header), header)
		require.NotEqual(t, first.Get(header), second.Get(header), header)
	}
	require.Empty(t, first.Get("session_id"), "underscored session_id is stripped from the official wire")
	require.Empty(t, first.Get("x-codex-installation-id"), "x-codex-installation-id is not a WS handshake header upstream")

	httpRequest, err := service.buildUpstreamRequest(
		context.Background(), c, account11,
		[]byte(`{"model":"gpt-5.6-codex","stream":true,"prompt_cache_key":"client-session"}`),
		"token", true, "client-session", true,
	)
	require.NoError(t, err)
	require.Equal(t, httpRequest.Header.Get("session-id"), first.Get("session-id"), "HTTP and WS must derive the same identity from the raw client key")
}

func TestBuildUpstreamRequestNamespacesCodexIdentityByOAuthAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	body := []byte(`{"model":"gpt-5.6-codex","stream":true,"prompt_cache_key":"client-session"}`)

	build := func(accountID int64, chatgptAccountID string) http.Header {
		t.Helper()
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Set("api_key", &APIKey{ID: 77})
		c.Request.Header.Set("User-Agent", "codex_cli_rs/0.144.0")
		c.Request.Header.Set("x-codex-installation-id", "client-installation")
		c.Request.Header.Set("x-codex-window-id", "client-window")
		c.Request.Header.Set("session-id", "client-session")
		c.Request.Header.Set("thread-id", "client-thread")
		c.Request.Header.Set("x-client-request-id", "client-request")
		c.Request.Header.Set("x-codex-turn-metadata", `{"installation_id":"client-installation","session_id":"client-session","thread_id":"client-thread","turn_id":"client-turn","window_id":"client-window"}`)

		account := &Account{
			ID:       accountID,
			Platform: PlatformOpenAI,
			Type:     AccountTypeOAuth,
			Credentials: map[string]any{
				"chatgpt_account_id": chatgptAccountID,
			},
		}
		req, err := svc.buildUpstreamRequest(
			context.Background(), c, account, body, "oauth-token", true, "client-session", true,
		)
		require.NoError(t, err)
		return req.Header
	}

	first := build(11, "chatgpt-account-11")
	firstAgain := build(11, "chatgpt-account-11")
	second := build(19, "chatgpt-account-19")

	// 官方 /responses 线序只携带连字符会话头 + window/request/turn-metadata；
	// session_id、conversation_id、x-codex-installation-id 官方从不发，必须缺席
	// （隔离与安装 ID 仍通过官方头与 body client_metadata 传递）。
	identityHeaders := []string{
		"x-codex-window-id",
		"session-id",
		"thread-id",
		"x-client-request-id",
	}
	checked := 0
	for _, header := range identityHeaders {
		require.NotEmpty(t, first.Get(header), header)
		require.Equal(t, first.Get(header), firstAgain.Get(header), "same account must retain stable identity: %s", header)
		require.NotEqual(t, first.Get(header), second.Get(header), "account failover must rotate upstream identity: %s", header)
		checked++
	}
	for _, header := range []string{"session_id", "conversation_id", "x-codex-installation-id"} {
		require.Empty(t, first.Get(header), "official /responses never sends %s", header)
	}

	// turn-metadata 含逐请求生成的 turn_started_at_unix_ms，只能比较身份字段，
	// 否则时间戳跨毫秒抖动会让断言随机失败。
	metadataIdentity := func(h http.Header) map[string]any {
		t.Helper()
		var decoded map[string]any
		require.NoError(t, json.Unmarshal([]byte(h.Get(openAIWSTurnMetadataHeader)), &decoded))
		delete(decoded, "turn_started_at_unix_ms")
		return decoded
	}
	firstMeta, repeatMeta, secondMeta := metadataIdentity(first), metadataIdentity(firstAgain), metadataIdentity(second)
	require.Equal(t, firstMeta, repeatMeta, "same account must retain stable identity: x-codex-turn-metadata")
	require.NotEqual(t, firstMeta, secondMeta, "account failover must rotate upstream identity: x-codex-turn-metadata")
	require.Equal(t, first.Get("session-id"), firstMeta["session_id"], "session identity must agree between header and body metadata")
	require.GreaterOrEqual(t, checked, 4, "test must exercise the real outbound identity surface")
}

func TestCodexAccountIdentityParentHeaderMatchesBody(t *testing.T) {
	for _, mode := range []string{"device", "off", "full", "unseeded"} {
		t.Run(mode, func(t *testing.T) {
			account := newTestOAuthAccount(61, map[string]any{codexFingerprintModeExtraKey: mode})
			account.Credentials = map[string]any{"chatgpt_account_id": "parent-header-credential"}
			if mode == "unseeded" {
				account.Extra = nil
			}
			c := newFingerprintStageTestContext(t)
			c.Set("api_key", &APIKey{ID: 13})
			c.Request.Header.Set("session-id", convTestSession)
			c.Request.Header.Set("thread-id", convTestThread)
			c.Request.Header.Set("x-codex-parent-thread-id", convTestParentThread)
			body := []byte(`{"model":"gpt-5.5","input":[],"client_metadata":{"session_id":"` + convTestSession + `","thread_id":"` + convTestThread + `","parent_thread_id":"` + convTestParentThread + `"}}`)
			ids := resolveCodexFingerprintIDsForRawBody(c, account, nil, body)
			scoped, _, err := applyCodexAccountIdentityClientMetadataRaw(body, account, 13)
			require.NoError(t, err)
			projected, _, err := applyCodexFingerprintClientMetadataRaw(scoped, ids)
			require.NoError(t, err)
			stageCodexFingerprintIDs(c, ids)
			stageCodexConvergenceBodyIdentityRaw(c, account, projected)

			svc := &OpenAIGatewayService{}
			req, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), c, account, projected, "token")
			require.NoError(t, err)
			parent := scopeCodexAccountIdentityValue(account, 13, "thread", convTestParentThread)
			require.Equal(t, parent, req.Header.Get("x-codex-parent-thread-id"))
			require.Equal(t, parent, gjson.GetBytes(projected, "client_metadata.parent_thread_id").String())
			require.NotEqual(t, convTestParentThread, parent)
		})
	}

	for _, account := range []*Account{
		{Platform: PlatformOpenAI, Type: AccountTypeAPIKey},
		{Platform: PlatformAnthropic, Type: AccountTypeOAuth},
	} {
		headers := http.Header{}
		headers.Set("x-codex-parent-thread-id", convTestParentThread)
		applyCodexAccountIdentityHeaders(headers, account, 13)
		require.Equal(t, convTestParentThread, headers.Get("x-codex-parent-thread-id"))
	}
}
