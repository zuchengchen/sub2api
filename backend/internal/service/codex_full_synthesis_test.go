package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func boolPtrForTest(v bool) *bool { return &v }

func codexSynthEnabledConfig() *config.Config {
	cfg := &config.Config{}
	cfg.Gateway.CodexFullSynthesis = boolPtrForTest(true)
	cfg.Gateway.CodexWire.MapOrderJitter = boolPtrForTest(false)
	cfg.Gateway.CodexWire.Profile = "auto"
	return cfg
}

func pinCodexTestVersion(t *testing.T, version string) {
	t.Helper()
	previous := codexCanonicalUAResolver
	SetCodexCanonicalUserAgentResolver(func() string { return buildCodexCLIUserAgent(version) })
	t.Cleanup(func() { SetCodexCanonicalUserAgentResolver(previous) })
}

func newCodexSynthTestAccount(id int64, accountType string) *Account {
	return &Account{
		ID: id, Name: "codex-synth", Platform: PlatformOpenAI, Type: accountType,
		Concurrency: 1, Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
		Credentials: map[string]any{"access_token": "oauth-token", "chatgpt_account_id": "chatgpt-account"},
		Extra: map[string]any{
			codexFingerprintModeExtraKey: "full",
			codexFingerprintSeedExtraKey: "11111111-1111-4111-8111-111111111111",
		},
	}
}

func runCodexSynthForward(t *testing.T, svc *OpenAIGatewayService, account *Account, body []byte, headers map[string]string) *httpUpstreamRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		c.Request.Header.Set(name, value)
	}
	upstream := &httpUpstreamRecorder{resp: &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
				"data: [DONE]\n\n",
		)),
	}}
	svc.httpUpstream = upstream
	svc.tiboRouteDisabled = true
	_, err := svc.Forward(context.Background(), c, account, body)
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	return upstream
}

func TestIsCodexOAuthAccountScope(t *testing.T) {
	require.True(t, isCodexOAuthAccount(&Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}))
	require.True(t, isCodexOAuthAccount(&Account{Platform: "", Type: AccountTypeOAuth}), "legacy empty platform is OpenAI")
	require.False(t, isCodexOAuthAccount(&Account{Platform: PlatformOpenAI, Type: AccountTypeSetupToken}))
	require.False(t, isCodexOAuthAccount(&Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}))
	require.False(t, isCodexOAuthAccount(&Account{Platform: PlatformGrok, Type: AccountTypeOAuth}))
	require.False(t, isCodexOAuthAccount(nil))
}

func TestResolveCodexDeviceProfileFamily(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	account := newCodexSynthTestAccount(7001, AccountTypeOAuth)
	account.Extra[codexClientFamilyExtraKey] = "exec"
	profile, ok := resolveCodexDeviceProfile(account)
	require.True(t, ok)
	require.Equal(t, "exec", profile.Family)
	require.Equal(t, "codex_exec", profile.Originator)
	require.Equal(t, "codex_exec/0.163.0 (Ubuntu 26.4.0; x86_64) xterm-256color (codex_exec; 0.163.0)", profile.UserAgent)
	require.Equal(t, "read-only", profile.SandboxMode)

	account.Extra[codexClientFamilyExtraKey] = "vscode"
	profile, ok = resolveCodexDeviceProfile(account)
	require.True(t, ok)
	require.Equal(t, "codex_vscode", profile.Originator)
	require.True(t, strings.HasPrefix(profile.UserAgent, "codex_vscode/0.163.0 "))

	// Non-OAuth accounts never construct a device profile, even with the keys set.
	apiKey := newCodexSynthTestAccount(7002, AccountTypeAPIKey)
	_, ok = resolveCodexDeviceProfile(apiKey)
	require.False(t, ok)
}

func TestResolveCodexWireProfile(t *testing.T) {
	stable, known := resolveCodexWireProfile("0.163.0", "auto")
	require.True(t, known)
	require.Equal(t, "0.163", stable.Name)
	require.False(t, stable.ClearContentItemMetadata)

	main, known := resolveCodexWireProfile("0.164.0", "auto")
	require.True(t, known)
	require.Equal(t, "0.164", main.Name)
	require.True(t, main.ClearContentItemMetadata)
	require.True(t, main.DedupeConfigurationUpdates)

	pinnedStable, _ := resolveCodexWireProfile("0.164.0", "stable")
	require.Equal(t, "0.163", pinnedStable.Name)

	alpha, known := resolveCodexWireProfile("0.164.0-alpha.9", "auto")
	require.True(t, known)
	require.Equal(t, "0.164", alpha.Name)
}

func TestCodexSynthesisGenericClientGetsFullCodexShape(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7101, AccountTypeOAuth)
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":"hello"}]}`)

	upstream := runCodexSynthForward(t, svc, account, body, map[string]string{"User-Agent": "curl/8.0"})
	sent := upstream.lastBody

	require.False(t, gjson.GetBytes(sent, "instructions").Exists(), "OAuth HTTP must not send top-level instructions")
	require.Equal(t, []string{
		"model", "stream", "input", "tools", "tool_choice", "parallel_tool_calls",
		"reasoning", "store", "include", "prompt_cache_key", "text", "client_metadata",
	}, openAIUpstreamJSONTopLevelKeys(sent))
	require.Equal(t, "developer", gjson.GetBytes(sent, "input.0.role").String())
	require.True(t, isCodexBaseInstructionsItemMustParse(t, sent, 0))
	require.Contains(t, gjson.GetBytes(sent, "input.1.content.0.text").String(), "<permissions instructions>")
	require.Equal(t, "user", gjson.GetBytes(sent, "input.2.role").String())
	require.Contains(t, gjson.GetBytes(sent, "input.2.content.0.text").String(), "<environment_context>")
	require.Equal(t, "hello", gjson.GetBytes(sent, "input.3.content").String())

	// Generic clients get hosted tools only, never client-executed shell tools.
	require.False(t, gjson.GetBytes(sent, `tools.#(name=="exec_command")`).Exists())
	require.False(t, gjson.GetBytes(sent, `tools.#(name=="apply_patch")`).Exists())
	require.True(t, gjson.GetBytes(sent, `tools.#(type=="web_search")`).Exists())

	// Multi-user identity invariants: one thread per downstream session.
	require.NotEmpty(t, gjson.GetBytes(sent, "prompt_cache_key").String())
	require.Equal(t, gjson.GetBytes(sent, "prompt_cache_key").String(), gjson.GetBytes(sent, "client_metadata.session_id").String())
	require.Equal(t, gjson.GetBytes(sent, "client_metadata.session_id").String(), gjson.GetBytes(sent, "client_metadata.thread_id").String())
	require.NotEmpty(t, gjson.GetBytes(sent, "client_metadata.turn_id").String())
	require.NotEmpty(t, upstream.lastReq.Header.Get("session-id"))
	require.NotEmpty(t, upstream.lastReq.Header.Get("thread-id"))
}

func isCodexBaseInstructionsItemMustParse(t *testing.T, body []byte, index int) bool {
	t.Helper()
	raw := gjson.GetBytes(body, "input."+itoaForTest(index)).Raw
	var item map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &item))
	return isCodexBaseInstructionsItem(item)
}

func itoaForTest(v int) string { return strconv.Itoa(v) }

func TestCodexSynthesisCompatibleClientMapsOfficialToolNames(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7102, AccountTypeOAuth)
	body := []byte(`{"model":"gpt-5.5","stream":true,"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"input":[{"type":"message","role":"user","content":"hello"}]}`)

	upstream := runCodexSynthForward(t, svc, account, body, map[string]string{"User-Agent": "third-party/1.0"})
	sent := upstream.lastBody
	require.True(t, gjson.GetBytes(sent, `tools.#(name=="exec_command")`).Exists(), "client shell tool is aliased to exec_command")
	require.False(t, gjson.GetBytes(sent, `tools.#(name=="shell")`).Exists())
	require.True(t, gjson.GetBytes(sent, `tools.#(name=="apply_patch")`).Exists())
	require.True(t, gjson.GetBytes(sent, `tools.#(name=="update_plan")`).Exists())
	require.Contains(t, string(sent), "<skills_instructions>")
}

func TestCodexSynthesisNativeRequestIsNotRebuilt(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7103, AccountTypeOAuth)
	base := "native base prompt"
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","id":"msg_x","role":"developer","content":[{"type":"input_text","text":"native base prompt"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}},{"type":"message","role":"user","content":"hello"}]}`)

	upstream := runCodexSynthForward(t, svc, account, body, map[string]string{
		"User-Agent": "codex_exec/0.163.0 (Ubuntu 26.4.0; x86_64) xterm-256color (codex_exec; 0.163.0)",
		"originator": "codex_exec",
	})
	sent := upstream.lastBody
	require.Equal(t, base, gjson.GetBytes(sent, "input.0.content.0.text").String())
	require.Equal(t, "hello", gjson.GetBytes(sent, "input.1.content").String())
	require.Equal(t, 2, len(gjson.GetBytes(sent, "input").Array()), "native requests keep their original input")
}

func TestCodexSynthesisSkippedForNonOAuthScope(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	for _, accountType := range []string{AccountTypeAPIKey, AccountTypeSetupToken} {
		t.Run(accountType, func(t *testing.T) {
			svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
			account := newCodexSynthTestAccount(7104, accountType)
			if accountType == AccountTypeAPIKey {
				account.Credentials = map[string]any{"api_key": "sk-test"}
			}
			body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":"hello"}]}`)
			upstream := runCodexSynthForward(t, svc, account, body, map[string]string{"User-Agent": "curl/8.0"})
			sent := upstream.lastBody
			require.Equal(t, "hello", gjson.GetBytes(sent, "input.0.content").String())
			require.False(t, gjson.GetBytes(sent, `input.#(internal_chat_message_metadata_passthrough.content_item_kinds.0=="model.base_instructions")`).Exists())
			require.False(t, gjson.GetBytes(sent, `tools.#(type=="web_search")`).Exists())
			if accountType == AccountTypeAPIKey {
				require.Empty(t, upstream.lastReq.Header.Get("session-id"))
				require.Empty(t, upstream.lastReq.Header.Get("thread-id"))
				require.False(t, strings.HasPrefix(upstream.lastReq.Header.Get("User-Agent"), "codex-tui/"))
			}
		})
	}
}

func TestCodexWireProfileMainClearsContentItemMetadataAndDedupesConfigurationUpdate(t *testing.T) {
	profile := codexWireProfileMain
	body := map[string]any{
		"model": "gpt-5.5",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": "hi", "internal_chat_message_metadata_passthrough": map[string]any{"content_item_metadata": map[string]any{"x": "y"}}},
			map[string]any{"type": "configuration_update", "value": 1},
			map[string]any{"type": "configuration_update", "value": 2},
			map[string]any{"type": "message", "role": "assistant", "content": "ok"},
		},
	}
	require.True(t, applyCodexWireProfileInputRules(body, profile, false))
	input, ok := body["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 3, "adjacent configuration_update keeps the latest only")
	first, ok := input[0].(map[string]any)
	require.True(t, ok)
	meta, ok := first["internal_chat_message_metadata_passthrough"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, meta, "content_item_metadata")
	second, ok := input[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 2, second["value"])
}

func TestCodexCatalogModelPrompts(t *testing.T) {
	fiveSix, ok := openai.CodexCatalogModelForSlug("gpt-5.6-sol-high")
	require.True(t, ok)
	require.NotEmpty(t, fiveSix.ModelMessages.InstructionsTemplate)
	require.NotEqual(t, openai.CodexBaseInstructionsForModel("gpt-5.5"), openai.CodexBaseInstructionsForModel("gpt-5.6-sol"))
	require.NotEmpty(t, openai.CodexBaseInstructionsForModel("gpt-6-sol"))
	require.NotEmpty(t, openai.CodexBaseInstructionsForModel("gpt-daybreak-blue-latest"))
}

func TestCodexClientMetadataOrderJitterIsPerTurn(t *testing.T) {
	metadataA := map[string]any{"turn_id": "turn-a", "session_id": "s", "x-codex-turn-state": "abc"}
	metadataB := map[string]any{"turn_id": "turn-b", "session_id": "s", "x-codex-turn-state": "abc"}
	orderA1 := codexShuffledClientMetadataKeys(metadataA)
	orderA2 := codexShuffledClientMetadataKeys(metadataA)
	require.Equal(t, orderA1, orderA2, "same turn keeps a stable order for delta chains")
	orderB := codexShuffledClientMetadataKeys(metadataB)
	require.NotEqual(t, orderA1, orderB)
}

func TestRestoreCodexToolNameFieldsCustomToolCall(t *testing.T) {
	event := map[string]any{
		"type": "response.output_item.done",
		"item": map[string]any{"type": "custom_tool_call", "name": "apply_patch", "input": "x"},
	}
	changed := restoreCodexToolNameFields(event, map[string]string{"apply_patch": "edit"})
	require.True(t, changed)
	item, ok := event["item"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "edit", item["name"])

	tools := map[string]any{"type": "session.updated", "session": map[string]any{"tools": []any{map[string]any{"type": "custom", "name": "apply_patch"}}}}
	changed = restoreCodexToolNameFields(tools, map[string]string{"apply_patch": "edit"})
	require.True(t, changed)
	session, ok := tools["session"].(map[string]any)
	require.True(t, ok)
	sessionTools, ok := session["tools"].([]any)
	require.True(t, ok)
	require.NotEmpty(t, sessionTools)
	tool, ok := sessionTools[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "edit", tool["name"])
}

func TestCodexSynthesisChatCompletionsBridge(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	gin.SetMode(gin.TestMode)
	body := []byte(`{
		"model":"gpt-5.5","stream":false,
		"messages":[{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"shell","description":"run","parameters":{"type":"object"}}}]
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "third-party/1.0")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_chat_synth", "gpt-5.5")}
	cfg := codexSynthEnabledConfig()
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := newCodexSynthTestAccount(7201, AccountTypeOAuth)

	_, err := svc.ForwardAsChatCompletions(context.Background(), c, account, body, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	sent := upstream.lastBody
	require.False(t, gjson.GetBytes(sent, "instructions").Exists())
	require.True(t, isCodexBaseInstructionsItemMustParse(t, sent, 0), "base prompt must be input[0]")
	require.True(t, gjson.GetBytes(sent, `tools.#(name=="exec_command")`).Exists(), string(sent))
	require.Contains(t, string(sent), "be terse", "client system message is preserved as a developer item")
}

func TestCodexSynthesisAnthropicBridge(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	gin.SetMode(gin.TestMode)
	body := []byte(`{
		"model":"claude-sonnet-4-5","max_tokens":16,"stream":false,
		"system":"be terse",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"shell","description":"run","input_schema":{"type":"object"}}]
	}`)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "third-party/1.0")

	upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_msg_synth", "gpt-5.5")}
	cfg := codexSynthEnabledConfig()
	cfg.Security.URLAllowlist.Enabled = false
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream}
	account := newCodexSynthTestAccount(7202, AccountTypeOAuth)

	_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "", "gpt-5.5")
	require.NoError(t, err)
	require.NotNil(t, upstream.lastReq)
	sent := upstream.lastBody
	require.False(t, gjson.GetBytes(sent, "instructions").Exists())
	require.True(t, isCodexBaseInstructionsItemMustParse(t, sent, 0), "base prompt must be input[0]")
	require.True(t, gjson.GetBytes(sent, `tools.#(name=="exec_command")`).Exists(), string(sent))
	require.Contains(t, string(sent), "be terse")
}

func TestCodexFingerprintMultiUserIsolation(t *testing.T) {
	accountA := newCodexSynthTestAccount(7301, AccountTypeOAuth)
	accountB := newCodexSynthTestAccount(7302, AccountTypeOAuth)
	accountB.Extra[codexFingerprintSeedExtraKey] = "22222222-2222-4222-8222-222222222222"

	idsA1 := resolveCodexFingerprintIDs(accountA, "shared-client-session", codexFingerprintFull)
	idsB1 := resolveCodexFingerprintIDs(accountB, "shared-client-session", codexFingerprintFull)
	require.NotNil(t, idsA1)
	require.NotNil(t, idsB1)
	require.NotEqual(t, idsA1.sessionID, idsB1.sessionID)
	require.NotEqual(t, idsA1.threadID, idsB1.threadID)
	require.NotEqual(t, idsA1.turnID, idsB1.turnID)
	require.NotEqual(t, idsA1.installationID, idsB1.installationID)

	// Same user/session, new prompt: same thread, new turn.
	idsA2 := resolveCodexFingerprintIDs(accountA, "shared-client-session", codexFingerprintFull)
	require.Equal(t, idsA1.sessionID, idsA2.sessionID)
	require.Equal(t, idsA1.threadID, idsA2.threadID)
	require.NotEqual(t, idsA1.turnID, idsA2.turnID)

	// Different client session: an independent session/thread under the same user.
	idsA3 := resolveCodexFingerprintIDs(accountA, "another-window", codexFingerprintFull)
	require.NotEqual(t, idsA1.sessionID, idsA3.sessionID)
	require.NotEqual(t, idsA1.threadID, idsA3.threadID)
}

func TestCodexDeviceProfileDrivesOutboundHeaders(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7401, AccountTypeOAuth)
	account.Extra[codexClientFamilyExtraKey] = "exec"
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","role":"user","content":"hello"}]}`)
	upstream := runCodexSynthForward(t, svc, account, body, map[string]string{"User-Agent": "curl/8.0"})
	require.True(t, strings.HasPrefix(upstream.lastReq.Header.Get("User-Agent"), "codex_exec/0.163.0 "), upstream.lastReq.Header.Get("User-Agent"))
	require.Equal(t, "codex_exec", upstream.lastReq.Header.Get("originator"))
	require.Equal(t, "0.163.0", upstream.lastReq.Header.Get("version"))
}

func TestCodexOAuthExtraClientHeadersAreScoped(t *testing.T) {
	oauth := newCodexSynthTestAccount(7501, AccountTypeOAuth)
	apiKey := newCodexSynthTestAccount(7502, AccountTypeAPIKey)
	require.True(t, allowOpenAIClientHeader("x-openai-subagent", openaiAllowedHeaders, oauth))
	require.True(t, allowOpenAIClientHeader("x-codex-parent-thread-id", openaiAllowedHeaders, oauth))
	require.False(t, allowOpenAIClientHeader("x-openai-subagent", openaiAllowedHeaders, apiKey))
	require.False(t, allowOpenAIClientHeader("x-codex-parent-thread-id", openaiAllowedHeaders, apiKey))
}

func TestCodexSynthesisPromptFollowsWireProfile(t *testing.T) {
	descriptor, ok := openai.CodexCatalogModelForSlug("gpt-5.5")
	require.True(t, ok)
	stable := codexSynthesisBaseInstructions("gpt-5.5", descriptor, true, codexWireProfileStable)
	main := codexSynthesisBaseInstructions("gpt-5.5", descriptor, true, codexWireProfileMain)
	require.Equal(t, openai.CodexStableGPT55Instructions(), stable)
	require.NotEqual(t, stable, main)
	require.Equal(t, openai.CodexCatalogInstructionsForSlug("gpt-5.5"), main)
}

func TestCodexClientMetadataOrderSharedAcrossLanes(t *testing.T) {
	httpMeta := map[string]any{"turn_id": "turn-a", "session_id": "s", "thread_id": "t"}
	wsMeta := map[string]any{"turn_id": "turn-a", "session_id": "s", "thread_id": "t", "x-codex-turn-state": "state"}
	httpOrder := codexShuffledClientMetadataKeys(httpMeta)
	wsOrder := codexShuffledClientMetadataKeys(wsMeta)
	filtered := make([]string, 0, len(wsOrder))
	for _, key := range wsOrder {
		if _, ok := httpMeta[key]; ok {
			filtered = append(filtered, key)
		}
	}
	require.Equal(t, httpOrder, filtered)
}

func TestCodexSynthesisTrimsOptionalContextBeforeFailing(t *testing.T) {
	base := codexSynthMessageRaw("msg_base", "developer", []string{"base"}, []string{"model.base_instructions"}, "", 0)
	permissions := codexSynthMessageRaw("msg_perm", "developer", []string{strings.Repeat("p", 4000)}, []string{"permissions.instructions"}, "", 0)
	skills := codexSynthMessageRaw("msg_skills", "developer", []string{strings.Repeat("s", 4000)}, []string{"host_skills.instructions", "permissions.instructions"}, "", 0)
	env := codexSynthMessageRaw("msg_env", "user", []string{"env"}, []string{"environments.environment_context"}, "", 0)
	user := map[string]any{"type": "message", "role": "user", "content": "hello"}

	input := []any{base, permissions, skills, env, user}
	// Budget fits base + env + user but not the two optional blocks.
	budget := codexSynthesisEstimatedTokens([]any{base, permissions, env, user}) + 8
	input, truncated, err := codexSynthesisTrimToBudget(input, codexSynthContextLimits{Advertised: budget, Hard: budget}, skills)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, input, 4, "skills first, then permissions")
	require.Equal(t, json.RawMessage(permissions), input[1], "permissions survive when the budget allows")

	// Past the advertised window but under the hard maximum: trim optional
	// context and still forward.
	trimmed, truncated, err := codexSynthesisTrimToBudget(append([]any(nil), input...), codexSynthContextLimits{Advertised: 1, Hard: 1_000_000}, nil)
	require.NoError(t, err)
	require.True(t, truncated)
	require.Len(t, trimmed, 3, "permissions dropped, base/env/user kept")

	_, _, err = codexSynthesisTrimToBudget(input, codexSynthContextLimits{Advertised: 1, Hard: 1}, nil)
	require.ErrorIs(t, err, ErrCodexSynthContextWindowExceeded, "untrimmed base/client input is never silently truncated")
}

func TestCodexStreamIdleTimeoutScope(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	svc.cfg.Gateway.StreamDataIntervalTimeout = 180
	oauth := newCodexSynthTestAccount(7601, AccountTypeOAuth)
	require.Equal(t, 180*time.Second, svc.codexStreamIdleTimeout(oauth), "hand-built Config falls back to stream_data_interval_timeout")
	svc.cfg.Gateway.CodexWire.StreamIdleTimeoutSeconds = 300
	require.Equal(t, 300*time.Second, svc.codexStreamIdleTimeout(oauth), "production viper default")
	svc.cfg.Gateway.CodexWire.StreamIdleTimeoutSeconds = 420
	require.Equal(t, 420*time.Second, svc.codexStreamIdleTimeout(oauth))
	setup := newCodexSynthTestAccount(7602, AccountTypeSetupToken)
	require.Equal(t, 180*time.Second, svc.codexStreamIdleTimeout(setup))
	apiKey := newCodexSynthTestAccount(7603, AccountTypeAPIKey)
	require.Equal(t, 180*time.Second, svc.codexStreamIdleTimeout(apiKey))
}

func TestSanitizeCodexDeviceProfileExtra(t *testing.T) {
	prepared := prepareCodexFingerprintExtraForCreate(PlatformOpenAI, AccountTypeOAuth, map[string]any{
		codexClientFamilyExtraKey:    "EXEC",
		codexPlatformProfileExtraKey: "windows-11-x64",
		codexSandboxModeExtraKey:     "unsafe",
		codexWorkspaceCWDExtraKey:    "../etc",
		codexTimezoneExtraKey:        "Not/AZone",
	})
	require.Equal(t, "exec", prepared[codexClientFamilyExtraKey])
	require.NotContains(t, prepared, codexPlatformProfileExtraKey)
	require.NotContains(t, prepared, codexSandboxModeExtraKey)
	require.NotContains(t, prepared, codexWorkspaceCWDExtraKey)
	require.NotContains(t, prepared, codexTimezoneExtraKey)

	valid := prepareCodexFingerprintExtraForCreate(PlatformOpenAI, AccountTypeOAuth, map[string]any{
		codexPlatformProfileExtraKey: "linux-ubuntu-24.4-x86_64-xterm",
		codexSandboxModeExtraKey:     "read-only",
		codexWorkspaceCWDExtraKey:    "/workspace",
		codexTimezoneExtraKey:        "America/Los_Angeles",
	})
	require.Equal(t, "linux-ubuntu-24.4-x86_64-xterm", valid[codexPlatformProfileExtraKey])
	require.Equal(t, "read-only", valid[codexSandboxModeExtraKey])
	require.Equal(t, "/workspace", valid[codexWorkspaceCWDExtraKey])
	require.Equal(t, "America/Los_Angeles", valid[codexTimezoneExtraKey])
}

func TestCodexSynthesisStrictRejectsUnknownVersion(t *testing.T) {
	account := newCodexSynthTestAccount(7701, AccountTypeOAuth)
	body := map[string]any{"model": "gpt-5.5", "input": []any{map[string]any{"type": "message", "role": "user", "content": "hi"}}}
	_, err := synthesizeCodexOAuthRequest(body, account, "gpt-5.5", codexSynthOptions{
		WireProfile: codexWireProfileMain,
		WireVersion: "0.165.0",
		Strict:      true,
	})
	require.Error(t, err)

	_, err = synthesizeCodexOAuthRequest(body, account, "gpt-5.5", codexSynthOptions{
		WireProfile: codexWireProfileStable,
		WireVersion: "0.165.0",
		Strict:      true,
	})
	require.NoError(t, err)
}

func TestCodexAuthIdentityForContextUsesDeviceProfile(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	account := newCodexSynthTestAccount(7801, AccountTypeOAuth)
	account.Extra[codexClientFamilyExtraKey] = "exec"
	profile, ok := resolveCodexDeviceProfile(account)
	require.True(t, ok)
	userAgent, originator := CodexAuthIdentityForContext(openai.WithCodexClientUserAgent(context.Background(), profile.UserAgent))
	require.Equal(t, profile.UserAgent, userAgent)
	require.Equal(t, "codex_exec", originator)
}
