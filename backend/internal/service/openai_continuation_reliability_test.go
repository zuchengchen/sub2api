package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityContinuationForwardOrphansAreNeverDropped(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, input := range []string{
			`{"type":"function_call_output","call_id":"missing","output":"keep synthetic result"}`,
			`{"type":"custom_tool_call_output","call_id":"missing","output":"keep patch result"}`,
			`{"type":"computer_call_output","call_id":"missing","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,eA=="}}`,
			`{"type":"shell_call_output","call_id":"missing","output":[]}`,
			`{"type":"apply_patch_call_output","call_id":"missing","status":"completed"}`,
			`{"type":"tool_search_output","call_id":"missing","execution":"client","status":"completed","tools":[]}`,
			`{"type":"function_call","call_id":"same","name":"lookup","arguments":"{}"},{"type":"custom_tool_call_output","call_id":"same","output":"wrong family"}`,
			`{"type":"function_call","call_id":"paired","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"unpaired","output":"partial context"}`,
			`{"type":"reasoning","encrypted_content":"synthetic"},{"type":"function_call_output","call_id":"missing","output":"cannot hide orphan behind opaque state"}`,
		} {
			body := []byte(`{"model":"gpt-5.5","stream":false,"input":[` + input + `]}`)
			before := bytes.Clone(body)
			upstream := &httpUpstreamRecorder{}
			account := newOpenAIOAuthNamespaceTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			c := newOpenAIRejectedFieldTestContext(body)
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, account, body)
			require.Error(t, err, input)
			var contextErr *OpenAIContinuationContextError
			require.ErrorAs(t, err, &contextErr)
			require.Equal(t, "input", contextErr.Param)
			require.Nil(t, result)
			require.Empty(t, upstream.bodies)
			require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			require.Equal(t, before, body)
		}
	}
}

func TestOAuthReliabilityContinuationForwardCompleteFamiliesAndNativeReferences(t *testing.T) {
	cases := []string{
		`{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}`,
		`{"type":"function_call","call_id":"c","name":"lookup","namespace":"mcp__server","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"}`,
		`{"type":"custom_tool_call","call_id":"c","name":"patch","input":"x"},{"type":"custom_tool_call_output","call_id":"c","output":"ok"}`,
		`{"type":"local_shell_call","call_id":"c","action":{"type":"exec","command":["true"]}},{"type":"function_call_output","call_id":"c","output":"ok"}`,
		`{"type":"computer_call","call_id":"c","action":{"type":"screenshot"}},{"type":"computer_call_output","call_id":"c","output":{"type":"computer_screenshot","image_url":"data:image/png;base64,eA=="}}`,
		`{"type":"shell_call","call_id":"c","environment":{"type":"local"},"action":{"commands":["true"]}},{"type":"shell_call_output","call_id":"c","output":[]}`,
		`{"type":"apply_patch_call","call_id":"c","operation":{"type":"delete_file","path":"synthetic"}},{"type":"apply_patch_call_output","call_id":"c","status":"completed"}`,
		`{"type":"tool_search_call","call_id":"c","execution":"client","arguments":{}},{"type":"tool_search_output","call_id":"c","execution":"client","status":"completed","tools":[]}`,
		`{"type":"function_call_output","name":"send_message_to_thread","namespace":"codex_app","output":"native delegation"}`,
		`{"type":"item_reference","id":"call_native"},{"type":"function_call_output","call_id":"call_native","output":"native stored call"}`,
	}
	for _, passthrough := range []bool{false, true} {
		for index, input := range cases {
			t.Run(fmt.Sprintf("pass_%t/family_%d", passthrough, index), func(t *testing.T) {
				body := []byte(`{"model":"gpt-5.5","stream":false,"input":[` + input + `]}`)
				upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_ok","object":"response","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`)}}
				account := newOpenAIRejectedFieldTestAccount()
				account.Extra["openai_passthrough"] = passthrough
				result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.JSONEq(t, "["+input+"]", gjson.GetBytes(upstream.lastBody, "input").Raw)
			})
		}
	}
}

func TestOAuthReliabilityContinuationHTTPAnchorPreservation(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			body := []byte(`{"model":"gpt-5.5","stream":false,"previous_response_id":"resp_native","input":[{"type":"function_call_output","call_id":"call_native","output":"keep result"}]}`)
			upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_ok","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)}}
			account := newOpenAIRejectedFieldTestAccount()
			if oauth {
				account = newOpenAIOAuthNamespaceTestAccount()
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
			}
			account.Extra["openai_passthrough"] = passthrough
			c := newOpenAIRejectedFieldTestContext(body)
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, account, body)
			if oauth && !passthrough {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, upstream.bodies)
				require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, "resp_native", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
				require.Equal(t, "keep result", gjson.GetBytes(upstream.lastBody, "input.0.output").String())
			}
		}
	}
}

func TestOAuthReliabilityContinuationReplayRequiresIndependentHistory(t *testing.T) {
	full := []json.RawMessage{json.RawMessage(`{"role":"user","content":"full root"}`), json.RawMessage(`{"type":"message","role":"assistant","content":"earlier decision"}`)}
	payload := []byte(`{"previous_response_id":"resp_external","input":[{"role":"user","content":"delta"}]}`)
	require.False(t, openAIContinuationReplayInputProof(payload, full, false, true), "inline tool coverage cannot establish a missing root")
	require.False(t, openAIContinuationStartsCompleteHistory(payload))
	require.False(t, OpenAIContinuationMaySwitchAccount(payload))
	require.False(t, openAIContinuationMatchesObservedResponse(payload, "resp_other"))
	require.True(t, openAIContinuationMatchesObservedResponse(payload, "resp_external"))
	require.True(t, openAIContinuationReplayInputProof(payload, full, true, true))
	withOpaque := append(bytesToRawMessages(full), json.RawMessage(`{"type":"reasoning","summary":[],"encrypted_content":"synthetic-ciphertext"}`))
	require.True(t, openAIContinuationReplayInputProof(payload, withOpaque, true, true), "same-owner complete encrypted reasoning can remain inline")
	require.False(t, openAIContinuationReplayInputProof(payload, withOpaque, true, false), "ciphertext cannot migrate to another credential")
	withReference := append(bytesToRawMessages(full), json.RawMessage(`{"type":"item_reference","id":"msg_remote"}`))
	require.False(t, openAIContinuationReplayInputProof(payload, withReference, true, true))
	require.False(t, OpenAIContinuationMaySwitchAccount([]byte(`{"input":[{"type":"item_reference","id":"msg_remote"}]}`)))
}

func bytesToRawMessages(items []json.RawMessage) []json.RawMessage {
	return append([]json.RawMessage(nil), items...)
}

func TestOAuthReliabilityContinuationSparseOutputCannotProveHistory(t *testing.T) {
	for _, tt := range []struct {
		name   string
		events []string
		want   bool
	}{
		{"empty_authoritative", []string{`{"type":"response.completed","response":{"output":[]}}`}, true},
		{"sparse_terminal", []string{`{"type":"response.completed","response":{"id":"resp_only"}}`}, false},
		{"lost_delta", []string{`{"type":"response.output_text.delta","item_id":"msg_lost","delta":"semantic output"}`, `{"type":"response.completed","response":{"output":[]}}`}, false},
		{"unidentified_delta", []string{`{"type":"response.output_text.delta","delta":"unknown item"}`, `{"type":"response.completed","response":{"output":[{"type":"message","id":"msg_other","role":"assistant","content":"other output"}]}}`}, false},
		{"terminal_omits_done", []string{`{"type":"response.output_item.done","item":{"type":"message","id":"msg_done","role":"assistant","content":"keep output"}}`, `{"type":"response.completed","response":{"output":[]}}`}, false},
		{"complete_delta", []string{`{"type":"response.output_text.delta","item_id":"msg_ok","delta":"semantic output"}`, `{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_ok","role":"assistant","content":"semantic output"}}`, `{"type":"response.completed","response":{"id":"resp_only"}}`}, true},
		{"duplicate_terminal", []string{`{"type":"response.completed","response":{"output":[],"output":[{"type":"message"}]}}`}, false},
		{"conflicting_item", []string{`{"type":"response.output_item.done","item":{"type":"message","id":"msg_same","role":"assistant","content":"first"}}`, `{"type":"response.completed","response":{"output":[{"type":"message","id":"msg_same","role":"assistant","content":"different"}]}}`}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			collector := &openAIWSToolCallReplayCollector{}
			for _, event := range tt.events {
				collector.AddEvent(gjson.Get(event, "type").String(), []byte(event))
			}
			require.Equal(t, tt.want, collector.Complete())
		})
	}
}

func TestOAuthReliabilityContinuationNativeWSPreservesUnknownAndSameOwner(t *testing.T) {
	for _, owner := range []string{"unknown", "same_owner_refresh", "different_owner"} {
		t.Run(owner, func(t *testing.T) {
			cfg := passthroughLifecycleConfig()
			cfg.Gateway.OpenAIWS.OAuthEnabled = true
			upstream := newStagedPassthroughConn()
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_ok","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
			svc := newPassthroughLifecycleService(cfg, upstream)
			account := responseAuthOwnerAccount()
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModePassthrough
			if owner != "unknown" {
				svc.bindHTTPResponseAccount(context.Background(), responseAuthOwnerContext(101, 201), account, "resp_owner")
				account = snapshotOAuthRefreshAccount(account)
				account.Credentials["access_token"] = "refreshed-token"
				if owner == "different_owner" {
					account.Credentials["chatgpt_user_id"] = "different-owner"
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			server, done := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
				c.Set("api_key", &APIKey{ID: 201, UserID: 101})
				return nil
			})
			defer server.Close()
			payload := `{"type":"response.create","model":"gpt-5.2","previous_response_id":"resp_owner","input":[{"type":"shell_call_output","call_id":"call_native","output":[]}]}`
			client := dialPassthroughLifecycleClientWithPayload(t, server, payload)
			defer client.CloseNow()
			if owner == "different_owner" {
				_, _, _ = client.Read(ctx)
				select {
				case err := <-done:
					require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
				case <-ctx.Done():
					t.Fatal("owner rejection did not finish")
				}
				select {
				case <-upstream.writes:
					t.Fatal("foreign origin reached upstream")
				default:
				}
				return
			}
			wire := requirePassthroughUpstreamWrite(t, upstream, time.Second)
			require.Equal(t, "resp_owner", gjson.GetBytes(wire, "previous_response_id").String())
			require.Equal(t, "shell_call_output", gjson.GetBytes(wire, "input.0.type").String())
			_, _, err := client.Read(ctx)
			require.NoError(t, err)
			_ = client.CloseNow()
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("relay did not stop")
			}
		})
	}
}

func TestOAuthReliabilityContinuationWSHTTPBridgeHistoryAndAnchorMismatch(t *testing.T) {
	for _, mode := range []string{"complete", "mismatched_anchor", "filtered_reasoning", "filtered_annotation"} {
		t.Run(mode, func(t *testing.T) {
			cfg := diagnosticContinuationConfig()
			output := diagnosticContinuationOutput
			if mode == "filtered_reasoning" {
				output = strings.TrimSuffix(output, "]") + `,{"type":"reasoning","id":"rs_remote","summary":[{"type":"summary_text","text":"must not silently lose summary"}]}]`
			}
			if mode == "filtered_annotation" {
				output = strings.Replace(output, `"summary":[]`, `"summary":[],"content":[{"type":"text","text":"must preserve reasoning text"}]`, 1)
			}
			upstream := &httpUpstreamRecorder{responses: []*http.Response{diagnosticContinuationSSE("resp_first", output), diagnosticContinuationSSE("resp_second", `[]`)}}
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: upstream, cache: &stubGatewayCache{}, openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, done := startPassthroughLifecycleServer(t, ctx, svc, diagnosticContinuationAccount())
			defer server.Close()
			client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-6-astra","input":[{"role":"user","content":"synthetic root"}]}`)
			defer client.CloseNow()
			_, _, err := client.Read(ctx)
			require.NoError(t, err)
			previous := "resp_first"
			if mode == "mismatched_anchor" {
				previous = "resp_elsewhere"
			}
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-6-astra","previous_response_id":"`+previous+`","input":[{"type":"function_call_output","call_id":"call_diagnostic","output":"synthetic result"}]}`)))
			_, _, err = client.Read(ctx)
			if mode != "complete" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			_ = client.CloseNow()
			select {
			case err := <-done:
				if mode != "complete" {
					var contextErr *OpenAIContinuationContextError
					require.True(t, errors.As(err, &contextErr))
				}
			case <-ctx.Done():
				t.Fatal("bridge did not stop")
			}
			if mode != "complete" {
				require.Len(t, upstream.bodies, 1, "unrelated anchor must not be replaced with the prior turn")
			} else {
				require.Len(t, upstream.bodies, 2)
				require.False(t, gjson.GetBytes(upstream.bodies[1], "previous_response_id").Exists())
				require.Contains(t, string(upstream.bodies[1]), "Chosen invariant: preserve every prior decision.")
				require.Contains(t, string(upstream.bodies[1]), "fixture-encrypted-content")
				require.Contains(t, string(upstream.bodies[1]), "synthetic result")
			}
		})
	}
}

func TestOAuthReliabilityContinuationAPIKeyOriginPreservesNativeAndRejectsChangedOwner(t *testing.T) {
	for _, mode := range []string{"same_credential", "same_site_user_other_key", "other_account", "rotated_upstream_key", "other_endpoint", "other_site_user", "unknown_native"} {
		t.Run(mode, func(t *testing.T) {
			account := newOpenAIRejectedFieldTestAccount()
			upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(200, `{"id":"resp_next","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)}}
			svc := newOpenAIRejectedFieldTestService(upstream)
			origin := responseAuthOwnerContext(101, 201)
			if mode != "unknown_native" {
				svc.bindHTTPResponseAccount(context.Background(), origin, account, "resp_key_owned")
			}
			current := snapshotOAuthRefreshAccount(account)
			c := responseAuthOwnerContext(101, 201)
			allowed := mode == "same_credential" || mode == "same_site_user_other_key" || mode == "unknown_native"
			switch mode {
			case "same_site_user_other_key":
				c = responseAuthOwnerContext(101, 202)
			case "other_account":
				current.ID++
			case "rotated_upstream_key":
				current.Credentials["api_key"] = "different-synthetic-key"
			case "other_endpoint":
				current.Credentials["base_url"] = "https://other.example"
			case "other_site_user":
				c = responseAuthOwnerContext(102, 201)
			}
			body := []byte(`{"model":"gpt-5.5","stream":false,"previous_response_id":"resp_key_owned","input":[{"role":"user","content":"native delta"}]}`)
			result, err := svc.Forward(context.Background(), c, current, body)
			if allowed {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.Equal(t, "resp_key_owned", gjson.GetBytes(upstream.lastBody, "previous_response_id").String())
			} else {
				require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
				require.Nil(t, result)
				require.Empty(t, upstream.bodies)
				require.Equal(t, http.StatusBadRequest, c.Writer.Status())
			}
		})
	}
}

func TestOAuthReliabilityConcurrentContinuationCredentialBindings(t *testing.T) {
	svc := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(nil)}
	const attempts = 16
	var workers sync.WaitGroup
	for index := 0; index < attempts; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			account := newOpenAIRejectedFieldTestAccount()
			account.ID += int64(index)
			account.Credentials["api_key"] = fmt.Sprintf("synthetic-key-%d", index)
			svc.bindOpenAIResponseAuthOwner(context.Background(), nil, account, "resp_same")
		}(index)
	}
	workers.Wait()
	winners := 0
	for index := 0; index < attempts; index++ {
		account := newOpenAIRejectedFieldTestAccount()
		account.ID += int64(index)
		account.Credentials["api_key"] = fmt.Sprintf("synthetic-key-%d", index)
		err := svc.validateOpenAIResponseAuthOwner(context.Background(), nil, account, "resp_same")
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
		}
	}
	require.Equal(t, 1, winners, "first confirmed credential origin must stay immutable")
}

func TestOAuthReliabilityContinuationLegacyMessagesKeepExplicitAnchor(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","previous_response_id":"resp_keep","messages":[{"role":"user","content":"delta"}]}`)
	normalized, changed, err := normalizeOpenAIResponsesLegacyIngress(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "resp_keep", gjson.GetBytes(normalized, "previous_response_id").String())
	require.NotEmpty(t, gjson.GetBytes(normalized, "input").Array())
}

func TestOAuthReliabilityContinuationHTTPToWSMissingExternalAnchorDoesNotReplay(t *testing.T) {
	cfg := newOpenAIWSExecutionScopeTestConfig()
	cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
	capture := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response not found"}}`)}}
	dialer := &openAIWSCaptureDialer{conn: capture}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(dialer)
	defer pool.Close()
	upstream := &httpUpstreamRecorder{}
	svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool, httpUpstream: upstream, toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
	account := newOpenAIOAuthNamespaceTestAccount()
	account.Extra["responses_websockets_v2_enabled"] = true
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	body := []byte(`{"model":"gpt-5.2","stream":false,"previous_response_id":"resp_external","input":[{"role":"user","content":"only a delta"}]}`)
	c := newOpenAIWSExecutionScopeTestContext(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := svc.Forward(ctx, c, account, body)
	require.Error(t, err)
	require.Nil(t, result)
	require.Equal(t, http.StatusBadRequest, c.Writer.Status())
	require.Empty(t, upstream.bodies)
	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.writes, 1, "unknown external history must not be replayed as a new root")
	require.Equal(t, "resp_external", capture.writes[0]["previous_response_id"])
}
