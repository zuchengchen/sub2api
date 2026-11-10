package service

import (
	"context"
	"encoding/json"
	"fmt"
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

// Fixed Codex e54a5713 declares only ttl (30m or 12h) in PromptCacheOptions.
// Exercise the two production HTTP forwarding branches, including their final
// request ordering, rather than checking the unsupported-field list alone.
func TestCodexOAuthPromptCacheTTLHTTPForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, passthrough := range []bool{false, true} {
		for _, ttl := range []string{"30m", "12h"} {
			t.Run(fmt.Sprintf("passthrough=%t/ttl=%s", passthrough, ttl), func(t *testing.T) {
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_cache_ttl\",\"status\":\"completed\",\"output\":[{\"id\":\"msg_cache_ttl\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")),
				}}
				svc := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 123, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Concurrency: 1, Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
					Credentials: map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": "synthetic-account"},
					Extra:       astraHealthyTestExtra(map[string]any{"openai_passthrough": passthrough, "codex_fingerprint_mode": "off"}),
				}
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":true,"prompt_cache_key":"cache-session","prompt_cache_options":{"ttl":%q},"text":{"verbosity":"low"},"input":[{"role":"user","content":"keep this request"}]}`, ttl))
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.168.0")
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				result, err := svc.Forward(context.Background(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.NotEmpty(t, upstream.lastBody)
				require.JSONEq(t, fmt.Sprintf(`{"ttl":%q}`, ttl), gjson.GetBytes(upstream.lastBody, "prompt_cache_options").Raw)
				require.Contains(t, string(upstream.lastBody), "keep this request")
				assertCodexPromptCacheWireOrder(t, upstream.lastBody)
			})
		}
	}
}

func assertCodexPromptCacheWireOrder(t *testing.T, body []byte) {
	t.Helper()
	var keys []string
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool { keys = append(keys, key.String()); return true })
	index := func(want string) int {
		for i, key := range keys {
			if key == want {
				return i
			}
		}
		return -1
	}
	require.GreaterOrEqual(t, index("prompt_cache_key"), 0)
	require.Greater(t, index("prompt_cache_options"), index("prompt_cache_key"))
	require.Greater(t, index("text"), index("prompt_cache_options"))
}

func TestCodexOAuthPromptCacheTTLWSFrame(t *testing.T) {
	for _, ttl := range []string{"30m", "12h"} {
		t.Run(ttl, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","prompt_cache_key":"cache-session","prompt_cache_options":{"ttl":%q},"text":{"verbosity":"low"},"input":[{"role":"user","content":"request"}]}`, ttl))
			var payload map[string]any
			require.NoError(t, json.Unmarshal(body, &payload))
			applyCodexOAuthTransform(payload, true, false)
			payload["type"] = "response.create"
			frame, err := marshalCodexWSCreateFrame(body, payload, false)
			require.NoError(t, err)
			require.Equal(t, ttl, gjson.GetBytes(frame, "prompt_cache_options.ttl").String())
			assertCodexPromptCacheWireOrder(t, frame)
		})
	}
}

func TestCodexOAuthPromptCacheTTLFiltersOnlyUnsupportedConfiguration(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, tc := range []struct{ name, options, want string }{
		{"native 30m", `{"ttl":"30m"}`, `{"ttl":"30m"}`},
		{"native 12h", `{"ttl":"12h"}`, `{"ttl":"12h"}`},
		{"platform mode", `{"mode":"implicit","ttl":"30m"}`, ""},
		{"unsupported ttl", `{"ttl":"24h"}`, ""},
		{"numeric ttl", `{"ttl":30}`, ""},
		{"unknown option", `{"ttl":"12h","unknown":true}`, ""},
		{"empty options", `{}`, ""},
		{"null options", `null`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","prompt_cache_key":"keep","prompt_cache_options":` + tc.options + `,"input":[{"role":"developer","content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`)
			filtered, changed, err := filterOpenAIResponsesPromptCacheConfiguration(account, "gpt-6-astra", body)
			require.NoError(t, err)
			require.True(t, changed)
			if tc.want == "" {
				require.False(t, gjson.GetBytes(filtered, "prompt_cache_options").Exists())
			} else {
				require.JSONEq(t, tc.want, gjson.GetBytes(filtered, "prompt_cache_options").Raw)
			}
			require.False(t, gjson.GetBytes(filtered, "input.0.content.0.prompt_cache_breakpoint").Exists())
			require.Equal(t, "stable", gjson.GetBytes(filtered, "input.0.content.0.text").String())
			require.Equal(t, "keep", gjson.GetBytes(filtered, "prompt_cache_key").String())
		})
	}
}

func TestCodexOAuthPromptCacheTTLChatCompletions(t *testing.T) {
	for _, ttl := range []string{"30m", "12h"} {
		t.Run(ttl, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-luna","stream":false,"prompt_cache_options":{"ttl":%q},"messages":[{"role":"system","content":"stable instructions"},{"role":"user","content":"question"}]}`, ttl))
			out := forwardOAuthChatCompletionsForUpstreamBodyWithModel(t, body, "gpt-5.6-luna")
			require.JSONEq(t, fmt.Sprintf(`{"ttl":%q}`, ttl), gjson.GetBytes(out, "prompt_cache_options").Raw)
			require.Contains(t, string(out), "stable instructions")
			require.Contains(t, string(out), "question")
			require.False(t, gjson.GetBytes(out, "input.0.content.0.prompt_cache_breakpoint").Exists())
		})
	}
}

func TestCodexOAuthPromptCacheTTLKeepsRawValuesAndAbsence(t *testing.T) {
	account := &Account{Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	for _, breakpoint := range []string{"", `,"prompt_cache_breakpoint":{"mode":"explicit"}`} {
		body := []byte(`{"model":"gpt-6-astra","prompt_cache_options":{"ttl":"12h"},"input":[{"role":"developer","content":[{"type":"input_text","text":"literal prompt_cache_breakpoint 9007199254740993","large_id":9007199254740993` + breakpoint + `}]}]}`)
		out, changed, err := filterOpenAIResponsesPromptCacheConfiguration(account, "gpt-6-astra", body)
		require.NoError(t, err)
		require.Equal(t, breakpoint != "", changed)
		require.Equal(t, "9007199254740993", gjson.GetBytes(out, "input.0.content.0.large_id").Raw)
		require.Equal(t, "literal prompt_cache_breakpoint 9007199254740993", gjson.GetBytes(out, "input.0.content.0.text").String())
		require.Equal(t, "12h", gjson.GetBytes(out, "prompt_cache_options.ttl").String())
		if breakpoint == "" {
			require.Equal(t, body, out)
		}
	}
	// A later turn (and the CLI's prewarm) must not inherit the prior TTL.
	body := []byte(`{"model":"gpt-6-astra","prompt_cache_key":"same-session","input":[]}`)
	out, changed, err := filterOpenAIResponsesPromptCacheConfiguration(account, "gpt-6-astra", body)
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, body, out)
	invalid := []byte(`{"model":"gpt-6-astra","prompt_cache_options":{"mode":"explicit"},"input":[{"role":"user","content":[{"type":"input_text","text":"keep","large_id":9007199254740993,"prompt_cache_breakpoint":{}}]}]}`)
	out, changed, err = filterOpenAIResponsesPromptCacheConfiguration(account, "gpt-6-astra", invalid)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(out, "prompt_cache_options").Exists())
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "input.0.content.0.large_id").Raw)
}

func TestCodexOAuthPromptCacheTTLControlsDeltaContinuation(t *testing.T) {
	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":"first"}`)}
	current := append(append([]json.RawMessage{}, input...), json.RawMessage(`{"type":"message","role":"user","content":"next"}`))
	for _, before := range []string{"", "30m", "12h"} {
		for _, after := range []string{"", "30m", "12h"} {
			t.Run(before+"_to_"+after, func(t *testing.T) {
				fingerprint := func(ttl string) string {
					payload := map[string]any{"model": "gpt-6-astra", "input": []any{map[string]any{"role": "user", "content": "request"}}}
					if ttl != "" {
						payload["prompt_cache_options"] = map[string]any{"ttl": ttl}
					}
					applyCodexOAuthTransform(payload, true, false)
					return openAIWSDeltaFieldsFingerprint(payload)
				}
				chain := &openAIWSDeltaChainState{ResponseID: "resp_prior", AccountID: 1, FieldsFP: fingerprint(before), BaselineHashes: openAIWSDeltaItemsHash(input)}
				decision := resolveOpenAIWSDelta(chain, 1, fingerprint(after), current)
				require.Equal(t, before == after, decision.Delta, decision.Reason)
				if before == after {
					require.Equal(t, "resp_prior", decision.PreviousResponse)
					require.Equal(t, current[1:], decision.TailItems)
				} else {
					require.Equal(t, "request_property_changed", decision.Reason)
					require.Empty(t, decision.PreviousResponse)
				}
			})
		}
	}
}

func TestCodexOAuthPromptCacheEscapedKeysAndInputShapes(t *testing.T) {
	for _, escaped := range []string{`\u0070rompt_cache_options`, `prompt_cache_\u006fptions`} {
		body := []byte(`{"` + escaped + `":{"mode":"explicit"},"input":[{"content":[{"\u0070rompt_cache_breakpoint":{},"large_id":9007199254740993}]}]}`)
		out, changed, err := filterCodexPromptCacheConfiguration(body)
		require.NoError(t, err)
		require.True(t, changed)
		require.False(t, gjson.GetBytes(out, "prompt_cache_options").Exists())
		require.False(t, gjson.GetBytes(out, "input.0.content.0.prompt_cache_breakpoint").Exists())
		require.Equal(t, "9007199254740993", gjson.GetBytes(out, "input.0.content.0.large_id").Raw)
	}
	for _, body := range []string{
		`{"prompt_cache_key":"session","input":"literal prompt_cache_breakpoint"}`,
		`{"input":{"0":{"prompt_cache_breakpoint":"literal non-array input"}}}`,
		`{"input":[{"content":{"0":{"prompt_cache_breakpoint":"literal non-array content"}}}]}`,
	} {
		out, changed, err := filterCodexPromptCacheConfiguration([]byte(body))
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, body, string(out))
	}
}
