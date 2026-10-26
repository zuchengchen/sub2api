package service

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexHTTPHeaderParityIncludesOptionalTrace(t *testing.T) {
	account := &Account{ID: 90242, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "header-parity-account"}}
	input := []byte(`{"model":"gpt-5.5","input":[],"stream":true}`)
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(input))
	for name, value := range map[string]string{
		"session-id":              "019df690-cde8-7cd1-8f30-9b79a5cb2011",
		"thread-id":               "019df690-cde8-7cd1-8f30-9b79a5cb2012",
		"x-openai-memgen-request": "true", "x-responsesapi-include-timing-metrics": "true",
		"x-openai-subagent": "explore", "x-codex-parent-thread-id": "019df690-cde8-7cd1-8f30-9b79a5cb2011",
		"x-codex-inference-call-id": "dbbb6e16-e135-472a-b0ad-e6a78271ff48",
		"traceparent":               "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01", "tracestate": "vendor=value",
		"Cookie": "must-not-forward", "X-Api-Key": "must-not-forward",
	} {
		ctx.Request.Header.Set(name, value)
	}
	regular, err := svc.buildUpstreamRequest(context.Background(), ctx, account, input, "test-token", true, "", true)
	require.NoError(t, err)
	defer func() { _ = regular.Body.Close() }()
	passthrough, err := svc.buildUpstreamRequestOpenAIPassthrough(context.Background(), ctx, account, input, "test-token")
	require.NoError(t, err)
	defer func() { _ = passthrough.Body.Close() }()
	for _, name := range []string{"session-id", "thread-id", "x-openai-memgen-request", "x-responsesapi-include-timing-metrics", "x-openai-subagent", "x-codex-parent-thread-id", "x-codex-inference-call-id", "traceparent", "tracestate"} {
		require.NotEmpty(t, regular.Header.Get(name), name)
		require.Equal(t, regular.Header.Get(name), passthrough.Header.Get(name), name)
	}
	for _, req := range []*http.Request{regular, passthrough} {
		require.Empty(t, req.Header.Get("Cookie"))
		require.Empty(t, req.Header.Get("X-Api-Key"))
	}
	invalid := make(http.Header)
	copyOpenAICodexTraceHeaders(invalid, http.Header{"Traceparent": {"invalid"}, "Tracestate": {"vendor=value"}}, account)
	require.Empty(t, invalid)
}

func TestCodexHTTPMetadataPreservesNativeFactsAndLargeIntegers(t *testing.T) {
	metadata, err := json.Marshal(map[string]any{
		"thread_source": "subagent", "sandbox": "seatbelt", "workspaces": map[string]any{"/client/workspace": map[string]any{}},
		"model": "old-model", "reasoning_effort": "low", "root_turn_id": "root-turn", "tool_namespaces_info": []string{"custom"},
	})
	require.NoError(t, err)
	encodedMetadata, err := json.Marshal(string(metadata))
	require.NoError(t, err)
	input := `[{"role":"user","content":[{"type":"input_text","text":"hello"}],"unknown":9007199254740993}]`
	body := []byte(`{"model":"gpt-5.5","reasoning":{"effort":"high"},"input":` + input + `,"client_metadata":{"x-codex-turn-metadata":` + string(encodedMetadata) + `}}`)
	req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
	require.NoError(t, err)
	next, err := applyCodexHTTPWireMetadata(req, body, &Account{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth})
	require.NoError(t, err)
	require.Equal(t, input, gjson.GetBytes(next, "input").Raw)
	md := gjson.Parse(gjson.GetBytes(next, "client_metadata.x-codex-turn-metadata").String())
	require.Equal(t, "seatbelt", md.Get("sandbox").String())
	require.Equal(t, "subagent", md.Get("thread_source").String())
	require.Equal(t, "gpt-5.5", md.Get("model").String())
	require.Equal(t, "high", md.Get("reasoning_effort").String())
	require.Equal(t, "root-turn", md.Get("root_turn_id").String())
	require.True(t, md.Get("tool_namespaces_info").Exists())
	require.False(t, gjson.Get(req.Header.Get(openAIWSTurnMetadataHeader), "tool_namespaces_info").Exists())
	for _, name := range []string{"agent_name", "sandbox_mode", "analytics_enabled", "guardian_credits_requested", "node_repl_disabled"} {
		require.False(t, md.Get(name).Exists(), name)
	}
	require.False(t, gjson.GetBytes(next, "client_metadata.x-codex-ws-stream-request-start-ms").Exists())
	replay, err := req.GetBody()
	require.NoError(t, err)
	raw, err := io.ReadAll(replay)
	require.NoError(t, replay.Close())
	require.NoError(t, err)
	require.Equal(t, next, raw)
}

func TestCodexHTTPMetadataDoesNotInventChildRoot(t *testing.T) {
	for _, source := range []string{"header", "flat-metadata"} {
		t.Run(source, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.5","input":[],"client_metadata":{"turn_id":"child-turn"}}`)
			if source == "flat-metadata" {
				body = []byte(`{"model":"gpt-5.5","input":[],"client_metadata":{"turn_id":"child-turn","parent_thread_id":"parent-thread"}}`)
			}
			req, err := http.NewRequest(http.MethodPost, chatgptCodexURL, bytes.NewReader(body))
			require.NoError(t, err)
			if source == "header" {
				req.Header.Set("x-codex-parent-thread-id", "parent-thread")
			}
			next, err := applyCodexHTTPWireMetadata(req, body, &Account{ID: 4, Platform: PlatformOpenAI, Type: AccountTypeOAuth})
			require.NoError(t, err)
			md := gjson.Parse(gjson.GetBytes(next, "client_metadata.x-codex-turn-metadata").String())
			require.Equal(t, "parent-thread", md.Get("parent_thread_id").String())
			require.False(t, md.Get("root_turn_id").Exists())
		})
	}
}

func TestCodexHTTPAnonymousSessionIsRequestScoped(t *testing.T) {
	account := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	body := []byte(`{"model":"gpt-5.5","prompt_cache_key":"shared-cache","input":"same prompt"}`)
	newContext := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		return c
	}
	first := newContext()
	restore := stageCodexHTTPClientSession(first, account, body)
	firstID := first.GetHeader("session-id")
	require.NotEmpty(t, firstID)
	restore()
	require.Empty(t, first.GetHeader("session-id"))
	restore = stageCodexHTTPClientSession(first, account, body)
	require.Equal(t, firstID, first.GetHeader("session-id"))
	restore()
	second := newContext()
	defer stageCodexHTTPClientSession(second, account, body)()
	require.NotEqual(t, firstID, second.GetHeader("session-id"))
}

func TestCodexHTTPMessagesTurnStateSeparatesChildThread(t *testing.T) {
	account := &Account{ID: 523, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
		Credentials: map[string]any{"chatgpt_account_id": "parent-child-state"}}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	newContext := func(thread string) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		c.Request.Header.Set("session-id", "same-session")
		c.Request.Header.Set("thread-id", thread)
		c.Set("api_key", &APIKey{ID: 22})
		stageCodexHTTPClientSession(c, account, nil)
		return c
	}
	root := newContext("root")
	svc.bindOpenAICompatSessionTurnState(context.Background(), root, account, "shared-cache", "root-state")
	require.Equal(t, "root-state", svc.getOpenAICompatSessionTurnState(context.Background(), newContext("root"), account, "changed-cache"))
	require.Empty(t, svc.getOpenAICompatSessionTurnState(context.Background(), newContext("child"), account, "shared-cache"))
}

func TestCodexHTTPMessagesRetainsAccountClientIdentityAndCachePolicy(t *testing.T) {
	for _, mode := range []string{"full", "device"} {
		for _, policy := range []string{"session-derived", "custom", "omit"} {
			t.Run(mode+"/"+policy, func(t *testing.T) {
				body := []byte(`{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
				c.Request.Header.Set("session-id", "account-family-"+mode+"-"+policy)
				account := &Account{ID: 9235, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
					Credentials: map[string]any{"access_token": "test-token"},
					Extra:       map[string]any{codexClientFamilyExtraKey: "exec", codexFingerprintModeExtraKey: mode, codexFingerprintSeedExtraKey: testCodexFingerprintSeed, codexPromptCacheKeyExtraKey: policy}}
				upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_identity", "gpt-5.5")}
				svc := &OpenAIGatewayService{cfg: &config.Config{}, httpUpstream: upstream}
				_, err := svc.ForwardAsAnthropic(context.Background(), c, account, body, "explicit-cache", "")
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(upstream.lastReq.UserAgent(), "codex_exec/"))
				require.Equal(t, "codex_exec", upstream.lastReq.Header.Get("originator"))
				switch policy {
				case "omit":
					require.False(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").Exists())
				case "custom":
					require.Equal(t, scopeCodexAccountIdentityValue(account, 0, "prompt-cache", "explicit-cache"), gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
				default:
					require.NotEmpty(t, gjson.GetBytes(upstream.lastBody, "prompt_cache_key").String())
				}
			})
		}
	}
}
