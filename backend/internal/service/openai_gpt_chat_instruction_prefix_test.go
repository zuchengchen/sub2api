package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPTChatLateInstructionsKeepCodexWirePrefix(t *testing.T) {
	for _, tc := range []struct {
		name, model           string
		chat, synthesis, json bool
	}{
		{"chat_promoted", "gpt-5.4", true, false, false},
		{"chat_json", "gpt-5.4", true, false, true},
		{"chat_cache_preparation", "gpt-6.2-sol", true, false, false},
		{"chat_synthesis", "gpt-6.2-astra", true, true, false},
		{"responses_promoted", "gpt-5.6-sol", false, false, false},
		{"responses_json", "gpt-5.6-sol", false, false, true},
		{"responses_synthesis", "gpt-5.6-sol", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const lateSystem, lateDeveloper = "Apply this JSON rule from here.", "Keep this later developer directive."
			message := func(role, content string) map[string]any { return map[string]any{"role": role, "content": content} }
			history := []any{message("system", "Use JSON from the start."), message("developer", "Existing developer rule."), message("user", "Look up the value.")}
			payload := map[string]any{"model": tc.model, "instructions": "Caller instructions."}
			field, path := "input", "/v1/responses"
			function := map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}
			if tc.chat {
				field, path = "messages", "/v1/chat/completions"
				payload["tools"] = []any{map[string]any{"type": "function", "function": function}}
				history = append(history,
					map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"type": "function", "id": "call_lookup", "function": map[string]any{"name": "lookup", "arguments": "{}"}}}},
					map[string]any{"role": "tool", "tool_call_id": "call_lookup", "content": "tool result"})
				if tc.json {
					payload["response_format"] = map[string]any{"type": "json_object"}
				}
			} else {
				function["type"] = "function"
				payload["tools"] = []any{function}
				history = append(history,
					map[string]any{"type": "function_call", "call_id": "call_lookup", "name": "lookup", "arguments": "{}"},
					map[string]any{"type": "function_call_output", "call_id": "call_lookup", "output": "tool result"})
				if tc.json {
					payload["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
				}
			}
			cfg := &config.Config{}
			cfg.Gateway.CodexFullSynthesis = &tc.synthesis
			cfg.Gateway.DisableOpenAIClientLocalTimeRewrite = true
			account := gpt61ChatCacheAccount("fixture-owner")
			transport := &httpUpstreamRecorder{err: errors.New("stop after capturing the Codex request")}
			gateway := &OpenAIGatewayService{cfg: cfg, httpUpstream: transport, tiboRouteDisabled: true}
			forward := func(items []any) []byte {
				t.Helper()
				payload[field] = items
				raw, err := json.Marshal(payload)
				require.NoError(t, err)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Set("api_key", &APIKey{ID: 707, UserID: 7007})
				c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
				c.Request.Header.Set("Content-Type", "application/json")
				c.Request.Header.Set("session-id", "fixture-session")
				before := len(transport.requests)
				if tc.chat {
					_, err = gateway.ForwardAsChatCompletions(context.Background(), c, account, raw, "", "")
				} else {
					_, err = gateway.Forward(context.Background(), c, account, raw)
				}
				require.Error(t, err)
				require.Len(t, transport.requests, before+1)
				require.Equal(t, chatgptCodexURL, transport.lastReq.URL.String())
				return bytes.Clone(transport.lastBody)
			}
			first := forward(history)
			next := forward(append(history, message("system", lateSystem), message("developer", lateDeveloper), message("user", "Continue.")))
			initial := gjson.GetBytes(first, "input").Array()
			continued := gjson.GetBytes(next, "input").Array()
			require.Len(t, continued, len(initial)+3)
			require.Equal(t, gjson.GetBytes(first, "instructions").Raw, gjson.GetBytes(next, "instructions").Raw)
			require.Equal(t, gjson.GetBytes(first, "tools").Raw, gjson.GetBytes(next, "tools").Raw)
			for i := range initial {
				require.Equal(t, initial[i].Raw, continued[i].Raw, "previously sent input item %d", i)
			}
			for i, text := range []string{lateSystem, lateDeveloper} {
				item := continued[len(initial)+i]
				require.Equal(t, "developer", item.Get("role").String())
				require.Contains(t, item.Get("content").Raw, text)
				require.Equal(t, 1, strings.Count(string(next), text))
			}
			call := gjson.GetBytes(next, `input.#(type=="function_call").call_id`).String()
			require.NotEmpty(t, call)
			require.Equal(t, call, gjson.GetBytes(next, `input.#(type=="function_call_output").call_id`).String())
		})
	}
}

func TestGPTChatLateSystemInContinuationInput(t *testing.T) {
	for _, tc := range []struct {
		field string
		value any
	}{
		{"previous_response_id", "resp_fixture"},
		{"conversation", map[string]any{"id": "conv_fixture"}},
	} {
		t.Run(tc.field, func(t *testing.T) {
			content := []any{map[string]any{"type": "input_text", "text": "Later."}, map[string]any{"type": "input_image", "image_url": "fixture://image"}}
			system := map[string]any{"type": "message", "role": "system", "content": content, "id": "msg_fixture"}
			body := map[string]any{"instructions": "Existing.", "model": "gpt-5.6-sol", tc.field: tc.value, "input": []any{system}}
			result := applyCodexOAuthTransformWithOptions(body, codexOAuthTransformOptions{PreserveNativeInput: true, OmitPromotedSystemMessagesFromInput: true})
			require.NoError(t, result.Error)
			require.Equal(t, "Existing.", body["instructions"])
			require.Equal(t, "developer", system["role"])
			require.Equal(t, content, system["content"])
			require.Equal(t, []any{system}, body["input"])
		})
	}
}

func TestCodexLateSystemKeepsOtherModelsCompatible(t *testing.T) {
	const input = `[{"role":"system","content":"First."},{"role":"user","content":"Hello."},{"role":"system","content":"Later."}]`
	body := map[string]any{"model": "o3"}
	require.NoError(t, json.Unmarshal([]byte(`{"input":`+input+`}`), &body))
	result := applyCodexOAuthTransformWithOptions(body, codexOAuthTransformOptions{OmitPromotedSystemMessagesFromInput: true})
	require.NoError(t, result.Error)
	require.Equal(t, "First.\n\nLater.", body["instructions"])
	require.Len(t, body["input"], 1)

	// The Platform preparation path keeps its original role representation.
	original := []byte(`{"model":"gpt-5.6-sol","input":` + input + `}`)
	updated, _, err := prepareOpenAIGPT56PromptCaching(original, "gpt-5.6-sol", true)
	require.NoError(t, err)
	require.Equal(t, original, updated)
}
