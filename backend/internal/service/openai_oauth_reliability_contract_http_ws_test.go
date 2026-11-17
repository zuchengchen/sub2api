//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// The real resolver, OAuth frame builder, pool, socket reader, and HTTP output
// conversion run here. Only the upstream network destination is local.
func oauthReliabilityContractWSServer(t *testing.T, firstEvents []string) (*httptest.Server, *atomic.Int32, <-chan []byte) {
	t.Helper()
	var calls atomic.Int32
	requests := make(chan []byte, 16)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			requests <- raw
			events := []string{oauthReliabilityContractCompleted}
			if calls.Add(1) == 1 {
				events = firstEvents
			}
			for _, event := range events {
				if conn.WriteMessage(websocket.TextMessage, []byte(event)) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, &calls, requests
}

func oauthReliabilityContractWSRequest(t *testing.T, requests <-chan []byte) []byte {
	t.Helper()
	select {
	case body := <-requests:
		return body
	default:
		t.Fatal("the forwarding result returned without a captured upstream request")
		return nil
	}
}

func TestOAuthReliabilityContractHTTPToWSTerminalIDRepairUsesWireIndex(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, exactFrame := range []bool{false, true} {
			for _, prependInstructions := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%t/exact_frame=%t/prepend_instructions=%t", stream, exactFrame, prependInstructions), func(t *testing.T) {
					body := rejectedMessageIDHistory(t, 1, 0)
					var err error
					body, err = sjson.SetBytes(body, "stream", stream)
					require.NoError(t, err)
					wireIndex := 1
					if prependInstructions {
						wireIndex = 2
						body, err = sjson.SetBytes(body, "instructions", "Preserve these explicit contract instructions exactly once.")
					} else {
						body, err = sjson.DeleteBytes(body, "instructions")
					}
					require.NoError(t, err)
					body, err = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"function","name":"contract_lookup","parameters":{"type":"object","properties":{"value":{"type":"string"}},"additionalProperties":false}}]`))
					require.NoError(t, err)
					errorObject := gjson.GetBytes(rejectedMessageIDError(wireIndex, "item_BAD"), "error").Raw
					server, calls, requests := oauthReliabilityContractWSServer(t, []string{oauthReliabilityContractFailed(errorObject, "")})
					svc, account, httpUpstream := httpBridgeFixture(t, server.URL)
					// Keep the real local websocket dialer while exercising both
					// outgoing serializers in the production forwarder.
					svc.cfg.Gateway.OpenAIWS.ExactFidelityEnabled = exactFrame
					poolConfig := *svc.cfg
					poolConfig.Gateway.OpenAIWS.ExactFidelityEnabled = false
					svc.openaiWSPool.cfg = &poolConfig
					c, recorder := httpBridgeContext("contract-terminal-id-repair")
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					c.Request = c.Request.WithContext(ctx)
					result, err := svc.Forward(ctx, c, account, body)
					require.NoError(t, err)
					require.Equal(t, int32(2), calls.Load(), "the terminal rejection must be repaired on the same logical turn")
					require.Zero(t, httpUpstream.callCount, "field repair must not silently fall back to HTTP")
					first := oauthReliabilityContractWSRequest(t, requests)
					second := oauthReliabilityContractWSRequest(t, requests)
					idPath := fmt.Sprintf("input.%d.id", wireIndex)
					require.Equal(t, "item_BAD", gjson.GetBytes(first, idPath).String())
					require.False(t, gjson.GetBytes(second, idPath).Exists())
					expected, err := sjson.DeleteBytes(first, idPath)
					require.NoError(t, err)
					require.JSONEq(t, gjson.GetBytes(expected, "input").Raw, gjson.GetBytes(second, "input").Raw, "repair must preserve every message and the exact instructions position")
					require.JSONEq(t, gjson.GetBytes(first, "tools").Raw, gjson.GetBytes(second, "tools").Raw)
					require.False(t, gjson.GetBytes(second, "instructions").Exists())
					if prependInstructions {
						require.Equal(t, "Preserve these explicit contract instructions exactly once.", gjson.GetBytes(second, "input.0.content.0.text").String())
						require.Equal(t, "msg_base", gjson.GetBytes(second, "input.1.id").String(), "the index shift must not target the original developer message")
					}
					require.NotNil(t, result)
					require.True(t, result.OpenAIWSMode)
					require.Equal(t, stream, result.Stream, "the original HTTP client stream preference must survive the WS frame repair")
					require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
					require.Equal(t, 7, result.Usage.InputTokens)
					require.Contains(t, recorder.Body.String(), "contract answer")
					require.NotContains(t, recorder.Body.String(), "resp_contract_failed")
					require.NotContains(t, recorder.Body.String(), "invalid_value")
				})
			}
		}
	}
}

func TestOAuthReliabilityContractHTTPToWSIDRepairStopsAfterWork(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, boundary := range []struct {
			name, prior, usage string
		}{
			{"reasoning_progress", `{"type":"response.reasoning_text.delta","item_id":"rs_contract","output_index":0,"content_index":0,"delta":"working"}`, ""},
			{"hosted_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_contract","type":"web_search_call","status":"in_progress"}}`, ""},
			{"prior_usage", `{"type":"response.in_progress","response":{"id":"resp_contract_failed","usage":{"input_tokens":5,"output_tokens":0}}}`, ""},
			{"terminal_usage", "", `{"input_tokens":5,"output_tokens":0}`},
			{"bare_then_metered_terminal", "", `{"input_tokens":5,"output_tokens":0}`},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, boundary.name), func(t *testing.T) {
				body, err := sjson.SetBytes(rejectedMessageIDHistory(t, 1, 0), "stream", stream)
				require.NoError(t, err)
				body, err = sjson.DeleteBytes(body, "instructions")
				require.NoError(t, err)
				errorObject := gjson.GetBytes(rejectedMessageIDError(1, "item_BAD"), "error").Raw
				var frames []string
				if boundary.prior != "" {
					frames = append(frames, boundary.prior)
				}
				if boundary.name == "bare_then_metered_terminal" {
					frames = append(frames, `{"type":"error","error":`+errorObject+`}`)
				}
				frames = append(frames, oauthReliabilityContractFailed(errorObject, boundary.usage))
				server, calls, requests := oauthReliabilityContractWSServer(t, frames)
				svc, account, httpUpstream := httpBridgeFixture(t, server.URL)
				c, recorder := httpBridgeContext("contract-id-repair-boundary")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				result, err := svc.Forward(ctx, c, account, body)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "already-started or metered work must not invite an account replay")
				require.Equal(t, int32(1), calls.Load(), "the current WS turn must not be submitted twice")
				require.Zero(t, httpUpstream.callCount, "HTTP fallback would also replay this turn")
				first := oauthReliabilityContractWSRequest(t, requests)
				require.Equal(t, "item_BAD", gjson.GetBytes(first, "input.1.id").String())
				require.NotContains(t, recorder.Body.String(), "contract answer")
				if boundary.name == "prior_usage" || boundary.usage != "" {
					require.NotNil(t, result, "authoritative real usage must survive a preceding bare error")
					require.Equal(t, 5, result.Usage.InputTokens)
					require.Equal(t, "response.failed", result.UpstreamTerminalEvent)
				}
			})
		}
	}
}
