//go:build unit

package service

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityContractWSHTTPBridgeSSEIDRepair(t *testing.T) {
	for _, envelope := range []string{"failed", "bare_then_failed", "bare_eof", "heartbeat_before_failed"} {
		t.Run(envelope, func(t *testing.T) {
			body := oauthReliabilityContractWithOpaqueMetadata(t, rejectedMessageIDHistory(t, 1, 0))
			errorObject := gjson.GetBytes(rejectedMessageIDError(1, "item_BAD"), "error").Raw
			failed := oauthReliabilityContractFailed(errorObject, "")
			frames := []string{failed}
			switch envelope {
			case "bare_then_failed":
				frames = []string{`{"type":"error","error":` + errorObject + `}`, failed}
			case "bare_eof":
				frames = []string{`{"type":"error","error":` + errorObject + `}`}
			case "heartbeat_before_failed":
				frames = []string{`{"type":"keepalive"}`, `{"type":"response.in_progress","response":{"id":"resp_contract_failed"}}`, failed}
			}
			upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{oauthReliabilityContractSSE(frames...), oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}}
			svc := newOpenAIRejectedFieldTestService(nil)
			svc.httpUpstream = upstream
			c, _ := oauthReliabilityContractContext("/v1/responses", body)
			var writes []string
			result, err := svc.proxyOpenAIWSHTTPBridgeTurn(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), "synthetic-token", body, len(body), "gpt-5.6-sol", "", "", "", "", 1, func(message []byte) error {
				writes = append(writes, string(message))
				return nil
			})
			if envelope == "bare_eof" {
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "a bare error must not be turned into a model replay")
				require.Equal(t, 1, upstream.callCount)
				require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.1.id").String())
				require.Contains(t, strings.Join(writes, "\n"), "response.failed")
				return
			}
			require.NoError(t, err)
			require.Equal(t, 2, upstream.callCount, "the bridge must repair a pre-output ID failure carried inside HTTP200 SSE")
			for _, sentBody := range upstream.bodies {
				oauthReliabilityContractAssertOpaqueMetadata(t, sentBody)
			}
			expected, err := sjson.DeleteBytes(upstream.bodies[0], "input.1.id")
			require.NoError(t, err)
			require.JSONEq(t, oauthReliabilityComparableWireBody(t, expected), oauthReliabilityComparableWireBody(t, upstream.bodies[1]))
			require.NotNil(t, result)
			require.Equal(t, "response.completed", result.UpstreamTerminalEvent)
			require.Equal(t, 7, result.Usage.InputTokens)
			require.Equal(t, 2, result.Usage.OutputTokens)
			require.Contains(t, strings.Join(writes, "\n"), "contract answer")
			require.NotContains(t, strings.Join(writes, "\n"), "invalid_value")
			require.NotContains(t, strings.Join(writes, "\n"), "resp_contract_failed", "only heartbeats from the rejected attempt may be visible")
		})
	}
}

func TestOAuthReliabilityContractWSHTTPBridgeForbiddenReplay(t *testing.T) {
	for _, rejection := range []string{"rejected_id", "capacity"} {
		for _, boundary := range []struct {
			name, prior, usage string
		}{
			{"reasoning_progress", `{"type":"response.reasoning_text.delta","item_id":"rs_contract","output_index":0,"content_index":0,"delta":"working"}`, ""},
			{"hosted_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_contract","type":"web_search_call","status":"in_progress"}}`, ""},
			{"prior_usage", `{"type":"response.in_progress","response":{"id":"resp_contract_failed","usage":{"input_tokens":5,"output_tokens":0}}}`, ""},
			{"terminal_usage", "", `{"input_tokens":5,"output_tokens":0}`},
			{"bare_then_metered_terminal", "", `{"input_tokens":5,"output_tokens":0}`},
		} {
			t.Run(rejection+"/"+boundary.name, func(t *testing.T) {
				body := rejectedMessageIDHistory(t, 1, 0)
				errorObject := gjson.GetBytes(rejectedMessageIDError(1, "item_BAD"), "error").Raw
				if rejection == "capacity" {
					errorObject = `{"code":"server_is_overloaded","type":"server_error","message":"Synthetic provider capacity failure"}`
				}
				var frames []string
				if boundary.prior != "" {
					frames = append(frames, boundary.prior)
				}
				if boundary.name == "bare_then_metered_terminal" {
					frames = append(frames, `{"type":"error","error":`+errorObject+`}`)
				}
				frames = append(frames, oauthReliabilityContractFailed(errorObject, boundary.usage))
				upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{oauthReliabilityContractSSE(frames...), oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}}
				svc := newOpenAIRejectedFieldTestService(nil)
				svc.httpUpstream = upstream
				c, _ := oauthReliabilityContractContext("/v1/responses", body)
				var writes []string
				result, err := svc.proxyOpenAIWSHTTPBridgeTurn(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), "synthetic-token", body, len(body), "gpt-5.6-sol", "", "", "", "", 1, func(message []byte) error {
					writes = append(writes, string(message))
					return nil
				})
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover), "the bridge must not expose a retry to the outer native WS handler after upstream work")
				require.Equal(t, 1, upstream.callCount)
				require.NotNil(t, result)
				require.Equal(t, "response.failed", result.UpstreamTerminalEvent, "native WS records failures as terminal events even when relay returned nil")
				require.Contains(t, strings.Join(writes, "\n"), "response.failed")
				require.NotContains(t, strings.Join(writes, "\n"), "contract answer")
				if boundary.name == "prior_usage" || boundary.usage != "" {
					require.Equal(t, 5, result.Usage.InputTokens, "real upstream tokens survive rejection")
				}
			})
		}
	}
}
