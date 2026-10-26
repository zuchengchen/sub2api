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
	require.Len(t, gjson.GetBytes(sent, "input").Array(), 3)
	require.Contains(t, gjson.GetBytes(sent, "input.1.content.0.text").String(), "Use only the tools declared for this request")
	require.Equal(t, "hello", gjson.GetBytes(sent, "input.2.content").String())
	require.NotContains(t, string(sent), "<permissions instructions>")
	require.NotContains(t, string(sent), "<environment_context>")
	require.NotContains(t, string(sent), "<skills_instructions>")

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

func TestCodexSynthesisCompatibleClientPreservesDeclaredTools(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7102, AccountTypeOAuth)
	account.Extra[codexClientCapabilityExtraKey] = "compatible"
	body := []byte(`{"model":"gpt-5.5","stream":true,"tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}}],"input":[{"type":"message","role":"user","content":"hello"}]}`)

	upstream := runCodexSynthForward(t, svc, account, body, map[string]string{"User-Agent": "third-party/1.0"})
	sent := upstream.lastBody
	require.JSONEq(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(sent, "tools").Raw)
	require.NotContains(t, string(sent), "<skills_instructions>")
	require.NotContains(t, string(sent), "<permissions instructions>")
	require.NotContains(t, string(sent), "<environment_context>")
}

func TestCodexSynthesisToolContractRoundTrip(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	gin.SetMode(gin.TestMode)
	for _, capability := range []string{"generic", "compatible"} {
		for _, tool := range []struct {
			name     string
			field    string
			value    string
			wireName string
		}{
			{"shell", "command", "printf hello", "shell"},
			{"bash", "script", "printf hello", "bash"},
			{"edit", "replacement", "replacement text", "edit"},
			{"exec_command", "command", "printf hello", "exec_command"},
			{"python", "code", "print('hello')", codexPythonToolAlias},
		} {
			t.Run(capability+"/"+tool.name, func(t *testing.T) {
				account := newCodexSynthTestAccount(7105, AccountTypeOAuth)
				account.Extra[codexClientCapabilityExtraKey] = capability
				schema := map[string]any{
					"type": "object", "properties": map[string]any{tool.field: map[string]any{"type": "string"}},
					"required": []any{tool.field}, "additionalProperties": false,
				}
				arguments, err := json.Marshal(map[string]string{tool.field: tool.value})
				require.NoError(t, err)
				upstreamCall := map[string]any{
					"type": "function_call", "id": "fc_item", "call_id": "call_contract",
					"name": tool.wireName, "arguments": string(arguments), "status": "completed",
				}
				completed, err := json.Marshal(map[string]any{
					"type": "response.completed", "response": map[string]any{
						"id": "resp_contract", "model": "gpt-5.5", "status": "completed", "output": []any{upstreamCall},
						"usage": map[string]any{"input_tokens": 1, "output_tokens": 1},
					},
				})
				require.NoError(t, err)
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
					Body: io.NopCloser(strings.NewReader("data: " + string(completed) + "\n\ndata: [DONE]\n\n")),
				}}
				svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
				request := map[string]any{
					"model": "gpt-5.5", "stream": false,
					"tools":       []any{map[string]any{"type": "function", "name": tool.name, "strict": true, "parameters": schema}},
					"tool_choice": map[string]any{"type": "function", "name": tool.name},
					"input":       []any{map[string]any{"type": "message", "role": "user", "content": "use the declared tool"}},
				}
				body, err := json.Marshal(request)
				require.NoError(t, err)
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("User-Agent", "third-party/1.0")
				_, err = svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, rec.Code)
				sent := upstream.lastBody
				require.Equal(t, tool.wireName, gjson.GetBytes(sent, "tools.0.name").String())
				require.JSONEq(t, gjson.GetBytes(body, "tools.0.parameters").Raw, gjson.GetBytes(sent, "tools.0.parameters").Raw)
				require.True(t, gjson.GetBytes(sent, "tools.0.strict").Bool())
				require.Equal(t, tool.wireName, gjson.GetBytes(sent, "tool_choice.name").String())
				for _, declaration := range gjson.GetBytes(sent, "tools").Array() {
					if declaration.Get("type").String() != "web_search" {
						require.Equal(t, tool.wireName, declaration.Get("name").String(), "no undeclared client executor")
					}
				}

				// The downstream dispatches by name and decodes its own argument
				// schema. A name-only alias to exec_command/apply_patch fails here.
				call := gjson.Get(rec.Body.String(), "output.0")
				require.Equal(t, "function_call", call.Get("type").String())
				require.Equal(t, tool.name, call.Get("name").String())
				var received map[string]string
				require.NoError(t, json.Unmarshal([]byte(call.Get("arguments").String()), &received))
				require.Equal(t, map[string]string{tool.field: tool.value}, received)
				var replay map[string]any
				require.NoError(t, json.Unmarshal([]byte(call.Raw), &replay))
				input, ok := request["input"].([]any)
				require.True(t, ok)
				request["input"] = append(input, replay, map[string]any{
					"type": "function_call_output", "call_id": call.Get("call_id").String(), "output": "downstream-result",
				})
				body, err = json.Marshal(request)
				require.NoError(t, err)
				sent = runCodexSynthForward(t, svc, account, body, map[string]string{"User-Agent": "third-party/1.0"}).lastBody
				replayed := gjson.GetBytes(sent, `input.#(type=="function_call")`)
				output := gjson.GetBytes(sent, `input.#(type=="function_call_output")`)
				require.Equal(t, tool.wireName, replayed.Get("name").String())
				require.JSONEq(t, string(arguments), replayed.Get("arguments").String())
				require.NotEmpty(t, output.Get("call_id").String())
				require.Equal(t, replayed.Get("call_id").String(), output.Get("call_id").String())
				require.Equal(t, "downstream-result", output.Get("output").String())
			})
		}
	}
}

func TestCodexSynthesisLitePreservesToolContracts(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	for _, capability := range []string{"generic", "compatible"} {
		t.Run(capability, func(t *testing.T) {
			account := newCodexSynthTestAccount(7106, AccountTypeOAuth)
			account.Extra[codexClientCapabilityExtraKey] = capability
			body := []byte(`{"model":"gpt-5.5","input":[
				{"type":"additional_tools","role":"developer","tools":[
					{"type":"namespace","name":"local","tools":[{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"array","items":{"type":"string"}}},"required":["command"]}}]},
					{"type":"custom","name":"edit","format":{"type":"text"}}
				]},
				{"type":"function_call","name":"shell","namespace":"local","call_id":"fc_shell","arguments":"{\"command\":[\"pwd\"]}"},
				{"type":"function_call_output","call_id":"fc_shell","output":"/client"},
				{"type":"custom_tool_call","name":"edit","call_id":"ctc_edit","input":"client-specific edit"},
				{"type":"custom_tool_call_output","call_id":"ctc_edit","output":"edited"}
			],"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"shell","namespace":"local"},{"type":"custom","name":"edit"}]}}`)
			var request map[string]any
			require.NoError(t, json.Unmarshal(body, &request))
			result, err := synthesizeCodexOAuthRequest(request, account, "gpt-5.5", codexSynthOptions{ResponsesLite: true, WireProfile: codexWireProfileStable})
			require.NoError(t, err)
			require.Empty(t, result.ToolNameReverse)
			sent, err := json.Marshal(request)
			require.NoError(t, err)
			require.False(t, gjson.GetBytes(sent, "tools").Exists())
			additional := gjson.GetBytes(sent, `input.#(type=="additional_tools").tools`)
			for i, original := range gjson.GetBytes(body, "input.0.tools").Array() {
				require.JSONEq(t, original.Raw, additional.Array()[i].Raw)
			}
			wantTools := 2
			if capability == "generic" {
				wantTools++
				require.Equal(t, "web_search", additional.Array()[2].Get("type").String())
			}
			require.Len(t, additional.Array(), wantTools)
			require.JSONEq(t, gjson.GetBytes(body, "tool_choice").Raw, gjson.GetBytes(sent, "tool_choice").Raw)
			for i, original := range gjson.GetBytes(body, "input").Array()[1:] {
				require.JSONEq(t, original.Raw, gjson.GetBytes(sent, "input").Array()[i+3].Raw)
			}
			svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
			sent = runCodexSynthForward(t, svc, account, body, map[string]string{
				"User-Agent": "third-party/1.0", responsesLiteHeader: "true",
			}).lastBody
			require.False(t, gjson.GetBytes(sent, "tools").Exists())
			additional = gjson.GetBytes(sent, `input.#(type=="additional_tools").tools`)
			for i, original := range gjson.GetBytes(body, "input.0.tools").Array() {
				require.JSONEq(t, original.Raw, additional.Array()[i].Raw)
			}
			require.Equal(t, "shell", gjson.GetBytes(sent, `input.#(type=="function_call").name`).String())
			require.Equal(t, "edit", gjson.GetBytes(sent, `input.#(type=="custom_tool_call").name`).String())
			require.Equal(t, "client-specific edit", gjson.GetBytes(sent, `input.#(type=="custom_tool_call").input`).String())
			for _, kind := range []string{"function", "custom_tool"} {
				callID := gjson.GetBytes(sent, `input.#(type=="`+kind+`_call").call_id`).String()
				require.NotEmpty(t, callID)
				require.Equal(t, callID, gjson.GetBytes(sent, `input.#(type=="`+kind+`_call_output").call_id`).String())
			}
		})
	}
}

func TestCodexSynthesisNativeClassificationPreservesPromptAndTools(t *testing.T) {
	for _, tc := range []struct {
		name       string
		official   bool
		capability string
		input      string
	}{
		{"marked_with_compatible_setting", false, "compatible", `[{"type":"message","role":"developer","content":"native base","internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}}]`},
		{"official_unmarked", true, "compatible", `[{"type":"message","role":"developer","content":"native base"}]`},
		{"explicit_native", false, "native", `[{"type":"message","role":"user","content":"hi"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := newCodexSynthTestAccount(7107, AccountTypeOAuth)
			account.Extra[codexClientCapabilityExtraKey] = tc.capability
			body := `{"input":` + tc.input + `,"tools":[{"type":"function","name":"bash","parameters":{"type":"object","properties":{"script":{"type":"string"}},"required":["script"]}}]}`
			var request map[string]any
			require.NoError(t, json.Unmarshal([]byte(body), &request))
			result, err := synthesizeCodexOAuthRequest(request, account, "gpt-5.5", codexSynthOptions{OfficialClient: tc.official})
			require.NoError(t, err)
			require.Equal(t, codexCapabilityNative, result.Capability)
			require.False(t, result.Modified)
			sent, err := json.Marshal(request)
			require.NoError(t, err)
			require.JSONEq(t, body, string(sent))
		})
	}
}

func TestCodexSynthesisUserAgentAndToolNamesDoNotGrantCapabilities(t *testing.T) {
	account := newCodexSynthTestAccount(7108, AccountTypeOAuth)
	request := map[string]any{
		"input": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}}},
	}
	result, err := synthesizeCodexOAuthRequest(request, account, "gpt-5.5", codexSynthOptions{OfficialClient: true})
	require.NoError(t, err)
	require.Equal(t, codexCapabilityGeneric, result.Capability)
	require.Empty(t, result.ToolNameReverse)
	sent, err := json.Marshal(request)
	require.NoError(t, err)
	require.Equal(t, "shell", gjson.GetBytes(sent, "tools.0.name").String())
	require.Len(t, gjson.GetBytes(sent, "tools").Array(), 2, "only declared shell and hosted search")
	require.Len(t, gjson.GetBytes(sent, "input").Array(), 3, "base prompt, actual capability guidance and client message")
}

func TestCodexSynthesisEnvironmentRequiresExplicitFacts(t *testing.T) {
	account := newCodexSynthTestAccount(7109, AccountTypeOAuth)
	account.Extra[codexClientCapabilityExtraKey] = "compatible"
	device, ok := codexDeviceProfileForRequest(account)
	require.True(t, ok)
	prefix, skills := codexSynthesisPrefixItems(openai.CodexCatalogModel{}, false, codexCapabilityCompatible, "base", codexSynthOptions{Device: device})
	require.Len(t, prefix, 2, "device defaults do not describe downstream permissions")
	require.Nil(t, skills)
	account.Extra[codexWorkspaceCWDExtraKey] = "/client/<workspace>"
	env := codexSynthesisConfiguredEnvironment(account, nil)
	require.Equal(t, codexDeviceProfile{WorkspaceCWD: "/client/<workspace>"}, env)
	require.Empty(t, codexSynthesisPermissionsText(env), "a cwd does not imply write permission")
	account.Extra[codexSandboxModeExtraKey] = "read-only"
	account.Extra[codexTimezoneExtraKey] = "America/Los_Angeles"
	request := map[string]any{"input": "hi"}
	_, err := synthesizeCodexOAuthRequest(request, account, "gpt-5.5", codexSynthOptions{
		Device: device, Now: time.Date(2026, 2, 3, 2, 0, 0, 0, time.UTC),
	})
	require.NoError(t, err)
	sent, err := json.Marshal(request)
	require.NoError(t, err)
	permissions := gjson.GetBytes(sent, "input.2.content.0.text").String()
	environment := gjson.GetBytes(sent, "input.3.content.0.text").String()
	require.Contains(t, permissions, "`read-only`")
	require.Contains(t, environment, "<cwd>/client/&lt;workspace&gt;</cwd>")
	require.Contains(t, environment, "2026-02-02")
	require.Contains(t, environment, "America/Los_Angeles")
	for _, invented := range []string{"<shell>", "<filesystem>", "Approval policy", "Network access", "<skills_instructions>", "writable_roots"} {
		require.NotContains(t, permissions+environment, invented)
	}

	account.Extra[codexWorkspaceCWDExtraKey] = "../etc"
	account.Extra[codexSandboxModeExtraKey] = "unsafe"
	account.Extra[codexTimezoneExtraKey] = "Not/AZone"
	require.Equal(t, codexDeviceProfile{}, codexSynthesisConfiguredEnvironment(account, nil))
}

func TestCodexSynthesisClientContextRemainsAuthoritative(t *testing.T) {
	account := newCodexSynthTestAccount(7110, AccountTypeOAuth)
	account.Extra[codexWorkspaceCWDExtraKey] = "/account-default"
	account.Extra[codexSandboxModeExtraKey] = "workspace-write"
	body := `{"input":[{"type":"message","role":"developer","content":"<permissions instructions>read-only; ask for approval</permissions instructions>"},{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>/client</cwd><shell>fish</shell></environment_context>"}]},{"type":"message","role":"developer","content":"<skills_instructions>Only project-review at /client/review/SKILL.md</skills_instructions>"}]}`
	var request map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &request))
	_, err := synthesizeCodexOAuthRequest(request, account, "gpt-5.5", codexSynthOptions{})
	require.NoError(t, err)
	sent, err := json.Marshal(request)
	require.NoError(t, err)
	input := gjson.GetBytes(sent, "input").Array()
	require.Len(t, input, 5)
	for i, original := range gjson.Get(body, "input").Array() {
		require.JSONEq(t, original.Raw, input[i+2].Raw)
	}
	require.NotContains(t, string(sent), "/account-default")

	request = map[string]any{"input": "<environment_context><cwd>/client</cwd></environment_context>"}
	require.Equal(t, codexDeviceProfile{}, codexSynthesisConfiguredEnvironment(account, request), "string input also carries client context")
}

func TestCodexSynthesisNativeRequestIsNotRebuilt(t *testing.T) {
	pinCodexTestVersion(t, "0.163.0")
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7103, AccountTypeOAuth)
	base := "native base prompt"
	body := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"message","id":"msg_x","role":"developer","content":[{"type":"input_text","text":"native base prompt"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}},{"type":"message","role":"user","content":"hello"}],"tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}},{"type":"function","name":"shell","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}]}`)

	upstream := runCodexSynthForward(t, svc, account, body, map[string]string{
		"User-Agent": "codex_exec/0.163.0 (Ubuntu 26.4.0; x86_64) xterm-256color (codex_exec; 0.163.0)",
		"originator": "codex_exec",
	})
	sent := upstream.lastBody
	require.Equal(t, base, gjson.GetBytes(sent, "input.0.content.0.text").String())
	require.Equal(t, "hello", gjson.GetBytes(sent, "input.1.content").String())
	require.Equal(t, 2, len(gjson.GetBytes(sent, "input").Array()), "native requests keep their original input")
	require.JSONEq(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(sent, "tools").Raw)
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
	require.Equal(t, "object", gjson.GetBytes(sent, `tools.#(name=="shell").parameters.type`).String())
	require.False(t, gjson.GetBytes(sent, `tools.#(name=="exec_command")`).Exists())
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
	require.Equal(t, "object", gjson.GetBytes(sent, `tools.#(name=="shell").parameters.type`).String())
	require.False(t, gjson.GetBytes(sent, `tools.#(name=="exec_command")`).Exists())
	require.Contains(t, string(sent), "be terse")
}

func TestCodexFingerprintMultiUserIsolation(t *testing.T) {
	accountA := newCodexSynthTestAccount(7301, AccountTypeOAuth)
	accountB := newCodexSynthTestAccount(7302, AccountTypeOAuth)
	accountB.Credentials["chatgpt_account_id"] = "other-chatgpt-account"
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
