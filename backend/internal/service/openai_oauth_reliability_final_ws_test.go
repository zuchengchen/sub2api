//go:build unit

package service

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityFinalWSPreflightRejectsWithoutDial(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough, OpenAIWSIngressModeHTTPBridge} {
		for _, tc := range []struct{ name, fields, param string }{
			{"additional_hosted", `"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"web_search"}]}]`, "input[0].tools[0].type"},
			{"implicit_tool_search", `"tools":[{"type":"tool_search"}]`, "tools[0].execution"},
			{"search_history", `"input":[{"type":"web_search_call","id":"ws_fixture","status":"completed"}]`, "input[0].type"},
			{"astra_none", `"reasoning":{"effort":"none"}`, "reasoning.effort"},
			{"astra_minimal", `"reasoning":{"effort":"minimal"}`, "reasoning.effort"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				cfg := newOpenAIWSExecutionScopeTestConfig()
				cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
				upstreamConn := &openAIWSCaptureConn{}
				dialer := &openAIWSCaptureDialer{conn: upstreamConn}
				pool := newOpenAIWSConnPool(cfg)
				defer pool.Close()
				pool.setClientDialerForTest(dialer)
				httpUpstream := &httpUpstreamRecorder{}
				svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: httpUpstream, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
				account := newOpenAIOAuthNamespaceTestAccount()
				account.Extra["responses_websockets_v2_enabled"] = true
				account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				server, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
				defer server.Close()
				client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				require.NoError(t, err)
				defer client.CloseNow()
				body := []byte(`{"type":"response.create","model":"gpt-6-astra","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},` + tc.fields + `}`)
				require.NoError(t, client.Write(ctx, coderws.MessageText, body))
				_, event, err := client.Read(ctx)
				require.NoError(t, err)
				require.Equal(t, "error", gjson.GetBytes(event, "type").String())
				require.Equal(t, int64(http.StatusBadRequest), gjson.GetBytes(event, "status").Int())
				require.Equal(t, "invalid_request_error", gjson.GetBytes(event, "error.type").String())
				require.Equal(t, tc.param, gjson.GetBytes(event, "error.param").String())
				select {
				case err := <-serverErr:
					var closeErr *OpenAIWSClientCloseError
					require.ErrorAs(t, err, &closeErr)
					require.Equal(t, coderws.StatusPolicyViolation, closeErr.StatusCode())
				case <-ctx.Done():
					t.Fatal("local rejection did not settle the websocket turn")
				}
				require.Zero(t, dialer.DialCount())
				require.Empty(t, httpUpstream.bodies)
			})
		}
	}
}

func TestOAuthReliabilityFinalWSValidThenInvalidTurn(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			upstream := &seededWSObservedConn{newStagedPassthroughConn()}
			dialer := &openAIWSQueueDialer{conns: []openAIWSClientConn{upstream}}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
			account := newOpenAIOAuthNamespaceTestAccount()
			account.Extra["responses_websockets_v2_enabled"] = true
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			turnDone := make(chan *OpenAIForwardResult, 2)
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(*gin.Context) *OpenAIWSIngressHooks {
				return &OpenAIWSIngressHooks{
					ReasoningEffortMappings: []ReasoningEffortMapping{{From: "none", To: "low", MatchType: "exact", Model: "gpt-6-astra"}},
					AfterTurn:               func(_ int, result *OpenAIForwardResult, _ error) { turnDone <- result },
				}
			})
			defer server.Close()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			first := []byte(`{"type":"response.create","model":"gpt-6-astra","reasoning":{"effort":"none"},"input":[{"role":"user","content":"fixture"}]}`)
			require.NoError(t, client.Write(ctx, coderws.MessageText, first))
			select {
			case sent := <-upstream.writes:
				require.Equal(t, "low", gjson.GetBytes(sent, "reasoning.effort").String())
			case <-ctx.Done():
				t.Fatal("valid first turn was not sent")
			}
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_first","model":"gpt-6-astra","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}}`)
			_, event, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
			select {
			case result := <-turnDone:
				require.NotNil(t, result)
				require.NotNil(t, result.ReasoningEffort)
				require.NotNil(t, result.RequestedReasoningEffort)
				require.Equal(t, "low", *result.ReasoningEffort)
				require.Equal(t, "none", *result.RequestedReasoningEffort)
			case <-ctx.Done():
				t.Fatal("valid turn did not settle")
			}
			// Omitted model inherits the negotiated session model; the invalid
			// effort still must be checked on every response.create.
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","reasoning":{"effort":"minimal"},"input":[{"role":"user","content":"second"}]}`)))
			_, event, err = client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "reasoning.effort", gjson.GetBytes(event, "error.param").String())
			// Read and acknowledge the policy close. A peer which stops reading
			// after the JSON error makes websocket.Close wait for its bounded
			// handshake timeout; that is not an upstream relay deadlock.
			if mode == OpenAIWSIngressModePassthrough {
				_, _, closeErr := client.Read(ctx)
				require.Equal(t, coderws.StatusPolicyViolation, coderws.CloseStatus(closeErr))
			}
			select {
			case err := <-serverErr:
				require.Error(t, err)
			case <-ctx.Done():
				t.Fatal("invalid second turn did not stop")
			}
			require.Empty(t, upstream.writes, "invalid turn must not reach the healthy reused socket")
			require.Equal(t, 1, dialer.DialCount())
		})
	}
}

func TestOAuthReliabilityFinalWSMappedGPT55PreservesHostedTools(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			upstream := &seededWSObservedConn{newStagedPassthroughConn()}
			dialer := &openAIWSQueueDialer{conns: []openAIWSClientConn{upstream}}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
			account := newOpenAIOAuthNamespaceTestAccount()
			account.Extra["responses_websockets_v2_enabled"] = true
			account.Extra["openai_oauth_responses_websockets_v2_mode"] = mode
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(*gin.Context) *OpenAIWSIngressHooks {
				return &OpenAIWSIngressHooks{MapRequestModel: func(int, string) (string, error) { return "gpt-5.5", nil }}
			})
			defer server.Close()
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			require.NoError(t, err)
			defer client.CloseNow()
			body := []byte(`{"type":"response.create","model":"gpt-6-astra","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},"tools":[{"type":"web_search","external_web_access":false}],"tool_choice":"none","input":[{"role":"user","content":"fixture"}]}`)
			require.NoError(t, client.Write(ctx, coderws.MessageText, body))
			select {
			case sent := <-upstream.writes:
				require.Equal(t, "gpt-5.5", gjson.GetBytes(sent, "model").String())
				require.False(t, isOpenAIResponsesLiteWebSocketPayload(sent))
				require.JSONEq(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(sent, "tools").Raw)
			case <-ctx.Done():
				t.Fatal("mapped non-Lite request was not sent")
			}
			upstream.Send(`{"type":"response.completed","response":{"id":"resp_mapped","model":"gpt-5.5","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)
			_, event, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
			_ = client.Close(coderws.StatusNormalClosure, "done")
			select {
			case <-serverErr:
			case <-ctx.Done():
				t.Fatal("mapped turn did not stop after client close")
			}
			require.Equal(t, 1, dialer.DialCount())
		})
	}
}

func TestOAuthReliabilityFinalWSHTTPBridgePreservesSchemaAndRejectsLite(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprintf("invalid=%t", invalid), func(t *testing.T) {
			const schema = `{"properties":{"v":{"items":{"type":"string"},"uniqueItems":true}},"minProperties":1,"required":["v"],"additionalProperties":false}`
			tools := `[{"type":"tool_search","execution":"client"}]`
			if invalid {
				tools = `[{"type":"web_search"}]`
			}
			body := []byte(`{"type":"response.create","model":"gpt-6-astra","client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"},"text":{"format":{"type":"json_schema","name":"fixture","strict":false,"schema":` + schema + `}},"input":[{"type":"additional_tools","role":"developer","tools":` + tools + `},{"role":"user","content":"fixture"}]}`)
			upstream := &httpUpstreamRecorder{resp: openAICompatSSECompletedResponse("resp_bridge_schema", "gpt-6-astra")}
			svc := newOpenAIRejectedFieldTestService(upstream)
			c, _ := oauthReliabilityContractContext("/v1/responses", body)
			var events [][]byte
			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), "synthetic-token", body, len(body), "gpt-6-astra", "", "", "", "", 1, func(event []byte) error {
				events = append(events, append([]byte(nil), event...))
				return nil
			})
			if invalid {
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, upstream.bodies)
				require.Len(t, events, 1)
				require.Equal(t, "input[0].tools[0].type", gjson.GetBytes(events[0], "error.param").String())
				return
			}
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			require.Equal(t, "true", upstream.lastReq.Header.Get(responsesLiteHeader))
			require.JSONEq(t, schema, gjson.GetBytes(upstream.lastBody, "text.format.schema").Raw)
			require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(upstream.lastBody, "input").Raw)
		})
	}
}

func TestOAuthReliabilityFinalHTTPToWSProjectsLiteMode(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-5.5"} {
		for _, exact := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/exact=%t", model, exact), func(t *testing.T) {
				server, calls, requests := oauthReliabilityContractWSServer(t, []string{oauthReliabilityContractCompleted})
				svc, account, httpUpstream := httpBridgeFixture(t, server.URL)
				svc.cfg.Gateway.OpenAIWS.ExactFidelityEnabled = exact
				poolConfig := *svc.cfg
				poolConfig.Gateway.OpenAIWS.ExactFidelityEnabled = false
				svc.openaiWSPool.cfg = &poolConfig
				c, _ := httpBridgeContext("final-lite-mode")
				c.Request.Header.Set(responsesLiteHeader, "true")
				body := []byte(fmt.Sprintf(`{"model":%q,"stream":false,"reasoning":{"effort":"low"},"input":[{"role":"user","content":"fixture"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`, model))
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				result, err := svc.Forward(ctx, c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, int32(1), calls.Load())
				require.Zero(t, httpUpstream.callCount)
				sent := oauthReliabilityContractWSRequest(t, requests)
				require.Equal(t, model != "gpt-5.5", isOpenAIResponsesLiteWebSocketPayload(sent))
				require.Equal(t, "true", c.GetHeader(responsesLiteHeader))
			})
		}
	}
}
