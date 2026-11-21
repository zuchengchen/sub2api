//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	wsExecNotificationCall   = `{"type":"custom_tool_call","id":"ctc_exec","call_id":"call_exec","name":"exec","namespace":"functions","input":"fixture","status":"completed"}`
	wsExecFirstNotification  = `{"type":"custom_tool_call_output","call_id":"call_exec","name":"exec","output":"progress one"}`
	wsExecSecondNotification = `{"type":"custom_tool_call_output","call_id":"call_exec","name":"exec","output":"progress two"}`
	wsExecActualOutput       = `{"type":"custom_tool_call_output","call_id":"call_exec","output":[{"type":"input_text","text":"completed fixture"}]}`
	wsPrivateExecCall        = `{"type":"custom_tool_call","call_id":"call_exec","name":"exec","namespace":"vendor","input":"fixture"}`
	wsOtherCustomToolCall    = `{"type":"custom_tool_call","call_id":"call_exec","name":"reader","input":"fixture"}`
	wsPendingLookupCall      = `{"type":"function_call","call_id":"call_pending","name":"lookup","arguments":"{}"}`
	wsPendingLookupOutput    = `{"type":"function_call_output","call_id":"call_pending","output":"found"}`
	wsExecMalformedNotice    = `{"type":"custom_tool_call_output","call_id":7,"name":"exec","output":"progress"}`
)

func wsExecNotificationFrame(t *testing.T, previous string, items ...string) []byte {
	t.Helper()
	request := autoContinuationPayload(t, items...)
	request["tools"] = []any{map[string]any{"type": "custom", "name": "exec", "format": map[string]string{"type": "text"}}}
	if previous != "" {
		request["previous_response_id"] = previous
	}
	frame, err := json.Marshal(request)
	require.NoError(t, err)
	return frame
}

func TestWSExecNotificationIngressPreservesHistory(t *testing.T) {
	notifications := []string{wsExecNotificationCall, wsExecFirstNotification, wsExecActualOutput, wsExecSecondNotification}
	for _, test := range []struct {
		name       string
		mode       string
		items      []string
		anchor     string
		wantAnchor string
		rejected   bool
	}{
		{"native_full", OpenAIWSIngressModeCtxPool, notifications, "", "", false},
		{"bridge_full", OpenAIWSIngressModeHTTPBridge, notifications, "", "", false},
		{"native_single_notification", OpenAIWSIngressModeCtxPool, []string{wsExecNotificationCall, wsExecFirstNotification}, "", "", false},
		{"bridge_single_notification", OpenAIWSIngressModeHTTPBridge, []string{wsExecNotificationCall, wsExecFirstNotification}, "", "", false},
		{"native_explicit_anchor", OpenAIWSIngressModeCtxPool, []string{wsExecFirstNotification}, "resp_notice_root", "resp_notice_root", false},
		{"bridge_explicit_anchor", OpenAIWSIngressModeHTTPBridge, []string{wsExecFirstNotification}, "resp_notice_root", "", true},
		{"native_missing_call", OpenAIWSIngressModeCtxPool, []string{wsExecFirstNotification}, "", "", true},
		{"bridge_missing_call", OpenAIWSIngressModeHTTPBridge, []string{wsExecFirstNotification}, "", "", true},
		{"native_malformed_call_id", OpenAIWSIngressModeCtxPool, []string{wsExecMalformedNotice}, "", "", true},
		{"bridge_malformed_call_id", OpenAIWSIngressModeHTTPBridge, []string{wsExecMalformedNotice}, "", "", true},
		{"native_private_exec_and_pending_result", OpenAIWSIngressModeCtxPool, []string{wsPrivateExecCall, wsExecFirstNotification, wsPendingLookupOutput}, "", "resp_notice_root", false},
		{"native_other_tool_and_pending_result", OpenAIWSIngressModeCtxPool, []string{wsOtherCustomToolCall, wsExecFirstNotification, wsPendingLookupOutput}, "", "resp_notice_root", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootOutput := wsExecNotificationCall
			if test.wantAnchor != test.anchor && !test.rejected {
				rootOutput = wsPendingLookupCall
			}
			root := string(autoContinuationEvent("resp_notice_root", rootOutput))
			h := newOpenAIWSHTTPUpstreamHarness(newOpenAIWSHTTPUpstreamTestConfig(true), root, openAIWSHTTPUpstreamCompleted)
			defer h.svc.openaiWSPool.Close()
			h.upstream.responses = []*http.Response{
				diagnosticContinuationSSE("resp_notice_root", "["+rootOutput+"]"),
				diagnosticContinuationSSE("resp_notice_next", "[]"),
			}
			account := newOpenAIWSHTTPUpstreamOAuthAccount(map[string]any{"openai_oauth_responses_websockets_v2_mode": test.mode})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, done := startPassthroughLifecycleServer(t, ctx, h.svc, account)
			defer server.Close()
			client := dialPassthroughLifecycleClientWithPayload(t, server, string(wsExecNotificationFrame(t, "")))
			defer client.CloseNow()
			_, event, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "resp_notice_root", gjson.GetBytes(event, "response.id").String())
			followup := wsExecNotificationFrame(t, test.anchor, test.items...)
			require.NoError(t, client.Write(ctx, coderws.MessageText, followup))
			_, event, err = client.Read(ctx)
			if test.rejected {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
			}
			_ = client.CloseNow()
			select {
			case err := <-done:
				if test.rejected {
					var contextErr *OpenAIContinuationContextError
					require.ErrorAs(t, err, &contextErr)
				}
			case <-ctx.Done():
				t.Fatal("WebSocket relay did not finish")
			}
			var requests [][]byte
			if test.mode == OpenAIWSIngressModeHTTPBridge {
				requests = h.upstream.bodies
			} else {
				for _, write := range h.conn.writes {
					requests = append(requests, []byte(requestToJSONString(write)))
				}
			}
			if test.rejected {
				require.Len(t, requests, 1, "must not invent an anchor or send a partial replay")
				return
			}
			require.Len(t, requests, 2)
			require.Equal(t, test.wantAnchor, gjson.GetBytes(requests[1], "previous_response_id").String())
			require.JSONEq(t, gjson.GetBytes(followup, "input").Raw, gjson.GetBytes(requests[1], "input").Raw)
		})
	}
}

func TestHTTPToWSExecNotificationUsesFullInput(t *testing.T) {
	cfg := newOpenAIWSHTTPUpstreamTestConfig(true)
	cfg.Gateway.OpenAIWS.DeltaContextEnabled = true
	h := newOpenAIWSHTTPUpstreamHarness(cfg, string(autoContinuationEvent("resp_notice_root", wsExecNotificationCall)), openAIWSHTTPUpstreamCompleted)
	defer h.svc.openaiWSPool.Close()
	account := newOpenAIWSHTTPUpstreamOAuthAccount(nil)
	first, _ := httpBridgeContext("exec_notification_delta")
	_, err := h.svc.Forward(context.Background(), first, account, wsExecNotificationFrame(t, ""))
	require.NoError(t, err)
	body := wsExecNotificationFrame(t, "", wsExecNotificationCall, wsExecFirstNotification)
	second, recorder := httpBridgeContext("exec_notification_delta")
	result, err := h.svc.Forward(context.Background(), second, account, body)
	require.NoError(t, err)
	require.True(t, result.OpenAIWSMode)
	require.Contains(t, recorder.Body.String(), "response.completed")
	require.Equal(t, "incomplete_tool_pairs", second.GetString("openai_ws_auto_incremental_reason"))
	require.Len(t, h.conn.writes, 2)
	wire := requestToJSONString(h.conn.writes[1])
	require.False(t, gjson.Get(wire, "previous_response_id").Exists())
	require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.Get(wire, "input").Raw)
	require.Equal(t, 1, h.dialer.DialCount())
	require.Nil(t, h.upstream.lastReq)

	errorBody := gjson.Parse(`{"type":"invalid_request_error","param":"input","message":"No tool output found for custom tool call call_exec."}`)
	var req map[string]any
	require.NoError(t, json.Unmarshal(body, &req))
	require.False(t, openAIWSCanReplayMissingToolOutput(errorBody, req))
	retry, safe, err := buildOpenAIWSCurrentTurnRetryPayload(body, openAIWSExtractRawInputItems(body), true, "")
	require.NoError(t, err)
	require.False(t, safe)
	require.Nil(t, retry)
}

func TestWSExecNotificationCannotConsumePendingOutput(t *testing.T) {
	id := sha256.Sum256([]byte("call_exec"))
	pending := map[[sha256.Size]byte]string{id: "custom_tool_call_output"}
	require.False(t, openAIWSContinuationToolsComplete([]gjson.Result{gjson.Parse(wsExecFirstNotification)}, pending))
	require.True(t, openAIWSContinuationToolsComplete([]gjson.Result{gjson.Parse(wsExecActualOutput)}, pending))

	require.True(t, openAIWSContinuationToolsComplete([]gjson.Result{gjson.Parse(wsPrivateExecCall), gjson.Parse(wsExecFirstNotification)}, nil))
}

func TestWSExecNotificationPassthroughAnchorAndOrphan(t *testing.T) {
	cfg := passthroughLifecycleConfig()
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	upstream := newStagedPassthroughConn()
	svc := newPassthroughLifecycleService(cfg, upstream)
	account := newOpenAIWSHTTPUpstreamOAuthAccount(map[string]any{"openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModePassthrough})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	server, done := startPassthroughLifecycleServer(t, ctx, svc, account)
	defer server.Close()
	client := dialPassthroughLifecycleClientWithPayload(t, server, string(wsExecNotificationFrame(t, "", wsExecNotificationCall, wsExecFirstNotification, wsExecActualOutput)))
	defer client.CloseNow()
	wire := requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.False(t, gjson.GetBytes(wire, "previous_response_id").Exists())
	upstream.Send(string(autoContinuationEvent("resp_notice_root", wsExecNotificationCall)))
	_, _, err := client.Read(ctx)
	require.NoError(t, err)

	followup := wsExecNotificationFrame(t, "resp_notice_root", wsExecSecondNotification)
	require.NoError(t, client.Write(ctx, coderws.MessageText, followup))
	wire = requirePassthroughUpstreamWrite(t, upstream, time.Second)
	require.Equal(t, "resp_notice_root", gjson.GetBytes(wire, "previous_response_id").String())
	require.JSONEq(t, gjson.GetBytes(followup, "input").Raw, gjson.GetBytes(wire, "input").Raw)
	upstream.Send(string(autoContinuationEvent("resp_notice_done", "")))
	_, _, err = client.Read(ctx)
	require.NoError(t, err)

	require.NoError(t, client.Write(ctx, coderws.MessageText, wsExecNotificationFrame(t, "", wsExecSecondNotification)))
	_, _, err = client.Read(ctx)
	require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(err))
	select {
	case <-upstream.writes:
		t.Fatal("orphan notification reached the upstream")
	case err := <-done:
		var contextErr *OpenAIContinuationContextError
		require.ErrorAs(t, err, &contextErr)
	case <-ctx.Done():
		t.Fatal("WebSocket relay did not reject the orphan notification")
	}
}

func TestWSHTTPBridgeExecNotificationToolConversion(t *testing.T) {
	for _, test := range []struct {
		name, platform string
		tools          string
		inherited      bool
		rejected       bool
	}{
		{"api_key_declared", PlatformOpenAI, `[{"type":"custom","name":"exec"}]`, false, true},
		{"api_key_inherited", PlatformOpenAI, "", true, true},
		{"api_key_native", PlatformOpenAI, "", false, false},
		{"api_key_explicit_reset", PlatformOpenAI, "[]", true, false},
		{"grok", PlatformGrok, "", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			account.Platform = test.platform
			upstream := &httpUpstreamRecorder{resp: diagnosticContinuationSSE("resp_bridge_notice", "[]")}
			svc := newOpenAIRejectedFieldTestService(upstream)
			body := wsExecNotificationFrame(t, "resp_external", wsExecNotificationCall, wsExecActualOutput, wsExecFirstNotification)
			var err error
			if test.tools == "" {
				body, err = sjson.DeleteBytes(body, "tools")
			} else {
				body, err = sjson.SetRawBytes(body, "tools", []byte(test.tools))
			}
			require.NoError(t, err)
			c := newOpenAIRejectedFieldTestContext(body)
			if test.inherited {
				setOpenAIWSHTTPBridgeToolState(c, openAIWSHTTPBridgeToolState{
					ClientMapping: apicompat.ResponsesClientToolMapping{CustomTools: map[string]bool{"exec": true}},
				})
			}
			var events [][]byte
			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(context.Background(), c, account, "fixture", body, len(body), "gpt-6-astra", "", "", "", "", 1, func(event []byte) error {
				events = append(events, event)
				return nil
			})
			if !test.rejected {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, "resp_external", gjson.GetBytes(upstream.lastBody, "previous_response_id").Str)
				require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(upstream.lastBody, "input").Raw)
				require.NotEmpty(t, events)
				require.Equal(t, "response.completed", gjson.GetBytes(events[len(events)-1], "type").Str)
				return
			}
			require.Error(t, err)
			require.Nil(t, result)
			require.Len(t, events, 1)
			require.Equal(t, "error", gjson.GetBytes(events[0], "type").String())
			require.Equal(t, "unsupported_parameter", gjson.GetBytes(events[0], "error.code").String())
			require.Equal(t, "input", gjson.GetBytes(events[0], "error.param").String())
			require.NotContains(t, string(events[0]), "progress one")
			require.Empty(t, upstream.bodies)
		})
	}
}
