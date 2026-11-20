package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Ordinary Responses clients can replay the provider's output exactly without
// emitting Codex-specific model.base_instructions metadata.
func responsesSynthHistoryBody(t *testing.T, model string, input ...string) []byte {
	t.Helper()
	items := []json.RawMessage{json.RawMessage(autoContinuationUser)}
	for _, item := range input {
		items = append(items, json.RawMessage(item))
	}
	body, err := json.Marshal(map[string]any{
		"model": model, "input": items, "stream": true, "store": false,
		"instructions": "Use the caller's declared tools.",
		"tools": []any{
			map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}}},
			map[string]any{"type": "function", "name": "list_files", "parameters": map[string]any{"type": "object"}},
		},
	})
	require.NoError(t, err)
	return body
}

func responsesHistoryCompleted(model, id, output string) []byte {
	return []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"model":%q,"status":"completed","output":[%s]}}`, id, model, output))
}

func TestResponsesConfiguredContextKeepsTextAndCheckpointPrefix(t *testing.T) {
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig()}
	account := newCodexSynthTestAccount(7812, AccountTypeOAuth)
	account.Extra[codexSandboxModeExtraKey] = "read-only"
	account.Extra[codexWorkspaceCWDExtraKey] = "/fixture"
	headers := map[string]string{"session-id": "responses-prefix", "User-Agent": "claude-cli/2.0.1 (external, cli)"}
	initial := runCodexSynthForward(t, svc, account, responsesSynthHistoryBody(t, "gpt-6.1-sol"), headers).lastBody
	next := runCodexSynthForward(t, svc, account, responsesSynthHistoryBody(t, "gpt-6.1-sol", autoContinuationAnswer,
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"Now list the files."}]}`), headers).lastBody
	firstTurn := gjson.GetBytes(initial, "client_metadata.turn_id").String()
	require.NotEmpty(t, firstTurn)
	require.NotEqual(t, firstTurn, gjson.GetBytes(next, "client_metadata.turn_id").String())
	require.Equal(t, gjson.GetBytes(initial, "tools").Raw, gjson.GetBytes(next, "tools").Raw)
	require.Equal(t, gjson.GetBytes(initial, "prompt_cache_key").String(), gjson.GetBytes(next, "prompt_cache_key").String())
	first, second := gjson.GetBytes(initial, "input").Array(), gjson.GetBytes(next, "input").Array()
	require.Len(t, second, len(first)+2)
	require.Contains(t, string(initial), "/fixture")
	require.Contains(t, string(initial), "read-only")
	for i, item := range first {
		// A new turn has new tracing metadata. Every other wire field in
		// the existing prefix, including all client and account text, is stable.
		a, b := item.Raw, second[i].Raw
		for _, key := range []string{"turn_id", "create_time"} {
			a, _ = sjson.Delete(a, "internal_chat_message_metadata_passthrough."+key)
			b, _ = sjson.Delete(b, "internal_chat_message_metadata_passthrough."+key)
		}
		require.Equal(t, a, b, "already sent item %d", i)
		require.Equal(t, openAIWSDeltaItemHash([]byte(item.Raw)), openAIWSDeltaItemHash([]byte(second[i].Raw)))
	}
	require.Contains(t, second[len(second)-1].Raw, "Now list the files.")
}

func TestResponsesExactToolHistoryContinuesOnWebSocket(t *testing.T) {
	t.Setenv(responsesCacheAffinityScopeEnv, "key:123")
	const reason = `{"type":"reasoning","id":"rs_fixture","summary":[],"encrypted_content":"opaque_fixture"}`
	frames := make(chan []byte, 2)
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			frames <- frame
			n := count.Add(1)
			output := autoContinuationAnswer
			if n%2 == 1 {
				output = reason + "," + autoContinuationCall
			}
			event := responsesHistoryCompleted(gjson.GetBytes(frame, "model").String(), fmt.Sprintf("resp_synth_%d", n), output)
			if conn.WriteMessage(websocket.TextMessage, event) != nil {
				return
			}
		}
	}))
	defer server.Close()
	svc, account, httpFallback := httpBridgeFixture(t, server.URL)
	defer svc.openaiWSPool.Close()
	svc.cfg.Gateway.CodexFullSynthesis = boolPtrForTest(true)
	account.Extra[codexFingerprintSeedExtraKey] = "11111111-1111-4111-8111-111111111111"
	for _, tc := range []struct {
		name, model            string
		key                    int64
		anonymous, changeInput bool
	}{
		{name: "exact_sol_history", model: "gpt-6.1-sol", key: 123},
		{name: "exact_astra_history", model: "gpt-6-astra", key: 123},
		{name: "different_arguments", model: "gpt-6.1-sol", key: 123, changeInput: true},
		{name: "without_session", model: "gpt-6.1-sol", key: 123, anonymous: true},
		{name: "outside_pilot", model: "gpt-6.1-sol", key: 124},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := autoContinuationCall
			if tc.changeInput {
				call = strings.Replace(call, `\"test\"`, `\"changed\"`, 1)
				require.NotEqual(t, autoContinuationCall, call)
			}
			wantDelta := tc.key == 123 && !tc.anonymous && !tc.changeInput
			for turn, items := range [][]string{nil, {reason, call, autoContinuationTool}} {
				session := tc.name
				if tc.anonymous {
					session = ""
				}
				c, _ := httpBridgeContext(session)
				c.Set("api_key", &APIKey{ID: tc.key, UserID: 444, Group: &Group{Platform: PlatformOpenAI}})
				c.Request.Header.Set("User-Agent", "claude-cli/2.0.1 (external, cli)")
				result, err := svc.Forward(context.Background(), c, account, responsesSynthHistoryBody(t, tc.model, items...))
				require.NoError(t, err)
				require.True(t, result.OpenAIWSMode)
				frame := <-frames
				require.Equal(t, tc.model, gjson.GetBytes(frame, "model").String())
				if turn == 0 {
					require.False(t, gjson.GetBytes(frame, "previous_response_id").Exists())
					require.Contains(t, string(frame), "Use the caller's declared tools.")
					continue
				}
				require.Equal(t, wantDelta, c.GetBool("openai_ws_auto_incremental_succeeded"), c.GetString("openai_ws_auto_incremental_reason"))
				if wantDelta {
					require.Equal(t, fmt.Sprintf("resp_synth_%d", count.Load()-1), gjson.GetBytes(frame, "previous_response_id").String())
					require.Len(t, gjson.GetBytes(frame, "input").Array(), 1)
					require.JSONEq(t, autoContinuationTool, gjson.GetBytes(frame, "input.0").Raw)
					continue
				}
				require.False(t, gjson.GetBytes(frame, "previous_response_id").Exists())
				if !tc.anonymous {
					require.Equal(t, "input_mismatch", c.GetString("openai_ws_auto_incremental_reason"))
				}
				replayed := gjson.GetBytes(frame, `input.#(type=="function_call")`)
				require.Equal(t, "fc_test", replayed.Get("id").String())
				require.Equal(t, gjson.Get(call, "arguments").String(), replayed.Get("arguments").String())
				callID := "call_lookup"
				if tc.key != 123 {
					callID = "fc_lookup"
				}
				require.Equal(t, callID, replayed.Get("call_id").String())
				require.Equal(t, callID, gjson.GetBytes(frame, `input.#(type=="function_call_output").call_id`).String())
			}
		})
	}
	require.Zero(t, httpFallback.callCount)
}

func TestResponsesPilotPreservesPairedFunctionIDsOnHTTP(t *testing.T) {
	t.Setenv(responsesCacheAffinityScopeEnv, "key:123")
	transport := &httpUpstreamRecorder{err: errors.New("captured upstream request")}
	svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: transport, tiboRouteDisabled: true}
	account := newCodexSynthTestAccount(7813, AccountTypeOAuth)
	body := responsesSynthHistoryBody(t, "gpt-6.1-sol", autoContinuationCall, autoContinuationTool)
	for _, tc := range []struct {
		key  int64
		want string
	}{{123, "call_lookup"}, {124, "fc_lookup"}} {
		c, _ := httpBridgeContext("responses-function")
		c.Set("api_key", &APIKey{ID: tc.key, UserID: 444, Group: &Group{Platform: PlatformOpenAI}})
		before := len(transport.requests)
		_, err := svc.Forward(context.Background(), c, account, body)
		require.Error(t, err)
		require.Len(t, transport.requests, before+1)
		require.Equal(t, chatgptCodexURL, transport.lastReq.URL.String())
		call := gjson.GetBytes(transport.lastBody, `input.#(type=="function_call")`)
		require.Equal(t, "fc_test", call.Get("id").String())
		require.Equal(t, tc.want, call.Get("call_id").String())
		require.Equal(t, tc.want, gjson.GetBytes(transport.lastBody, `input.#(type=="function_call_output").call_id`).String())
	}
}

func TestResponsesFunctionPairEligibility(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []string
		field string
		value string
		allow bool
	}{
		{name: "complete", items: []string{autoContinuationCall, autoContinuationTool}, allow: true},
		{name: "missing_result", items: []string{autoContinuationCall}},
		{name: "missing_call", items: []string{autoContinuationTool}},
		{name: "result_first", items: []string{autoContinuationTool, autoContinuationCall}},
		{name: "duplicate_output", items: []string{autoContinuationCall, autoContinuationTool, autoContinuationTool}},
		{name: "mismatched_id", items: []string{autoContinuationCall, strings.Replace(autoContinuationTool, "call_lookup", "call_other", 1)}},
		{name: "legacy_call_item_id", items: []string{strings.Replace(autoContinuationCall, "fc_test", "legacy", 1), autoContinuationTool}},
		{name: "overlong_correlation_id", items: []string{strings.ReplaceAll(autoContinuationCall, "call_lookup", "call_"+strings.Repeat("a", 60)), strings.ReplaceAll(autoContinuationTool, "call_lookup", "call_"+strings.Repeat("a", 60))}},
		{name: "incomplete_call", items: []string{strings.Replace(autoContinuationCall, "completed", "in_progress", 1), autoContinuationTool}},
		{name: "referenced_input", items: []string{autoContinuationCall, autoContinuationTool, `{"type":"item_reference","id":"msg_unseen"}`}},
		{name: "existing_anchor", items: []string{autoContinuationCall, autoContinuationTool}, field: "previous_response_id", value: "resp_existing"},
		{name: "stored_conversation", items: []string{autoContinuationCall, autoContinuationTool}, field: "conversation", value: "conv_other"},
		{name: "other_tool_family", items: []string{`{"type":"custom_tool_call","id":"ctc_test","call_id":"call_lookup","name":"lookup","input":"query","status":"completed"}`, `{"type":"custom_tool_call_output","call_id":"call_lookup","output":"found"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := responsesSynthHistoryBody(t, "gpt-6.1-sol", tc.items...)
			if tc.field != "" {
				var err error
				body, err = sjson.SetBytes(body, tc.field, tc.value)
				require.NoError(t, err)
			}
			var decoded map[string]any
			require.NoError(t, json.Unmarshal(body, &decoded))
			require.Equal(t, tc.allow, codexResponsesHaveNativeFunctionPairs(AnalyzeOpenAIContinuationReplayProof(body), body, decoded))
		})
	}
}

func TestResponsesClientWebSocketPreservesFunctionIDsAcrossTransports(t *testing.T) {
	const model = "gpt-6.1-sol"
	frame, err := sjson.SetBytes(responsesSynthHistoryBody(t, model, autoContinuationCall, autoContinuationTool), "type", "response.create")
	require.NoError(t, err)
	event := responsesHistoryCompleted(model, "resp_native", autoContinuationAnswer)
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge} {
		t.Run(mode, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			conn := &seededWSObservedConn{newStagedPassthroughConn()}
			dialer := &seededWSObservedDialer{conn: conn, headers: make(chan http.Header, 2)}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			httpUpstream := &httpUpstreamRecorder{resp: &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader("data: " + string(event) + "\n\ndata: [DONE]\n\n")),
			}}
			svc := &OpenAIGatewayService{cfg: cfg, tiboRouteDisabled: true, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, httpUpstream: httpUpstream, toolCorrector: NewCodexToolCorrector()}
			account := newCodexSynthTestAccount(7814, AccountTypeOAuth)
			account.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, ended := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
				c.Set("api_key", &APIKey{ID: 123, UserID: 444, Group: &Group{Platform: PlatformOpenAI}})
				return nil
			})
			defer server.Close()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", &coderws.DialOptions{
				HTTPHeader: http.Header{"User-Agent": {"claude-cli/2.0.1 (external, cli)"}, "Session-Id": {"native-wire"}},
			})
			require.NoError(t, err)
			defer client.CloseNow()
			require.NoError(t, client.Write(ctx, coderws.MessageText, frame))
			var sent []byte
			if mode != OpenAIWSIngressModeHTTPBridge {
				sent = requirePassthroughUpstreamWrite(t, conn.stagedPassthroughConn, 3*time.Second)
				conn.Send(string(event))
			}
			_, reply, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(reply, "type").String())
			require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
			select {
			case err := <-ended:
				require.NoError(t, err)
			case <-ctx.Done():
				t.Fatal("websocket proxy did not close")
			}
			if mode == OpenAIWSIngressModeHTTPBridge {
				require.Len(t, httpUpstream.bodies, 1)
				sent = httpUpstream.bodies[0]
			}
			call := gjson.GetBytes(sent, `input.#(type=="function_call")`)
			require.Equal(t, "fc_test", call.Get("id").String())
			require.Equal(t, "call_lookup", call.Get("call_id").String())
			require.Equal(t, "call_lookup", gjson.GetBytes(sent, `input.#(type=="function_call_output").call_id`).String())
		})
	}
}
