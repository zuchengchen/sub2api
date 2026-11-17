//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These contracts exercise the public forwarding entrypoints against synthetic
// upstream Responses streams. No production credentials or network are used.
const oauthReliabilityContractSchemaError = `{"type":"invalid_request_error","code":"invalid_function_parameters","param":"tools[0].parameters","message":"Invalid schema for function 'lookup': additionalProperties must be false."}`

const oauthReliabilityContractCompleted = `{"type":"response.completed","response":{"id":"resp_contract_ok","object":"response","model":"gpt-5.6-sol","status":"completed","output":[{"id":"msg_contract_ok","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"contract answer","annotations":[]}]}],"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`

const oauthReliabilityContractOpaqueMetadata = `{"text":"keep input[1].id as opaque metadata","integer":9007199254740993,"nested":{"id":"item_not_rejected"}}`

func oauthReliabilityContractWithOpaqueMetadata(t *testing.T, body []byte) []byte {
	t.Helper()
	body, err := sjson.SetRawBytes(body, "client_metadata.oauth_contract", []byte(oauthReliabilityContractOpaqueMetadata))
	require.NoError(t, err)
	body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", `{"oauth_contract_extension":`+oauthReliabilityContractOpaqueMetadata+`}`)
	require.NoError(t, err)
	return body
}

func oauthReliabilityContractAssertOpaqueMetadata(t *testing.T, body []byte) {
	t.Helper()
	require.JSONEq(t, oauthReliabilityContractOpaqueMetadata, gjson.GetBytes(body, "client_metadata.oauth_contract").Raw, "user metadata must survive every upstream attempt")
	require.Equal(t, "9007199254740993", gjson.GetBytes(body, "client_metadata.oauth_contract.integer").Raw)
	embedded := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")
	require.Equal(t, gjson.String, embedded.Type, "the wire field must remain a JSON string")
	require.True(t, gjson.Valid(embedded.String()))
	require.JSONEq(t, oauthReliabilityContractOpaqueMetadata, gjson.Get(embedded.String(), "oauth_contract_extension").Raw, "embedded metadata extensions must survive identity projection")
	require.Equal(t, "9007199254740993", gjson.Get(embedded.String(), "oauth_contract_extension.integer").Raw)
}

func oauthReliabilityComparableWireBody(t *testing.T, body []byte) string {
	t.Helper()
	// Existing request construction regenerates these identity values and the
	// turn timestamp. Compare every other field, including all user metadata,
	// request_kind, model, installation identity, and unknown extensions.
	var err error
	for _, key := range []string{"session_id", "thread_id", "turn_id", "root_turn_id", "x-codex-window-id"} {
		body, err = sjson.DeleteBytes(body, "client_metadata."+key)
		require.NoError(t, err)
	}
	embedded := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")
	if embedded.Exists() {
		require.Equal(t, gjson.String, embedded.Type)
		metadata := embedded.String()
		require.True(t, gjson.Valid(metadata))
		for _, key := range []string{"session_id", "thread_id", "turn_id", "root_turn_id", "window_id", "context_window_id", "turn_started_at_unix_ms"} {
			metadata, err = sjson.Delete(metadata, key)
			require.NoError(t, err)
		}
		// Compare the embedded JSON structurally while retaining every key that
		// is not on the explicit identity/timestamp list above.
		body, err = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(metadata))
		require.NoError(t, err)
	}
	return string(body)
}

func oauthReliabilityContractSSE(payloads ...string) *http.Response {
	var wire strings.Builder
	for _, payload := range payloads {
		wire.WriteString("data: ")
		wire.WriteString(payload)
		wire.WriteString("\n\n")
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Header: http.Header{
			"Content-Type": {"text/event-stream"},
			"X-Request-Id": {"synthetic-oauth-reliability-contract"},
		},
		Body: io.NopCloser(strings.NewReader(wire.String())),
	}
}

func oauthReliabilityContractFailed(errorObject, usage string) string {
	if usage != "" {
		usage = `,"usage":` + usage
	}
	return `{"type":"response.failed","response":{"id":"resp_contract_failed","object":"response","model":"gpt-5.6-sol","status":"failed","output":[],"error":` + errorObject + usage + `}}`
}

func oauthReliabilityContractContext(path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Request.Header.Set("User-Agent", "synthetic-contract-client/1.0")
	return c, recorder
}

func oauthReliabilityContractBody(route string, stream bool) []byte {
	switch route {
	case "chat":
		return []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"messages":[{"role":"user","content":"synthetic contract prompt"}]}`, stream))
	case "messages":
		return []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"max_tokens":32,"messages":[{"role":"user","content":"synthetic contract prompt"}]}`, stream))
	default:
		return []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":%t,"store":false,"input":[{"role":"user","content":"synthetic contract prompt"}]}`, stream))
	}
}

func oauthReliabilityContractForward(svc *OpenAIGatewayService, c *gin.Context, account *Account, route string, body []byte) (*OpenAIForwardResult, error) {
	switch route {
	case "chat":
		return svc.ForwardAsChatCompletions(c.Request.Context(), c, account, body, "", "")
	case "messages":
		return svc.ForwardAsAnthropic(c.Request.Context(), c, account, body, "", "")
	default:
		return svc.Forward(c.Request.Context(), c, account, body)
	}
}

func TestOAuthReliabilityContractSemantic400AcrossHTTPEntrypoints(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, envelope := range []string{"http400", "failed", "bare_then_failed", "unknown_bare400"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", route, stream, envelope), func(t *testing.T) {
					failed := oauthReliabilityContractFailed(oauthReliabilityContractSchemaError, "")
					response := oauthReliabilityContractSSE(failed)
					switch envelope {
					case "http400":
						response = newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, `{"error":`+oauthReliabilityContractSchemaError+`}`)
					case "bare_then_failed":
						response = oauthReliabilityContractSSE(`{"type":"error","error":`+oauthReliabilityContractSchemaError+`}`, failed)
					case "unknown_bare400":
						response = oauthReliabilityContractSSE(`{"type":"error","status":400,"error":{"message":"Bad Request"}}`)
					}
					upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					svc.toolCorrector = NewCodexToolCorrector()
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = route == "passthrough"
					body := oauthReliabilityContractBody(route, stream)
					c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
					_, err := oauthReliabilityContractForward(svc, c, account, route, body)
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "a deterministic request error must not invite another account attempt")
					require.Len(t, upstream.requests, 1, "unknown or non-repairable 400 must not be replayed")
					if envelope == "unknown_bare400" {
						// A bare transport400 without parameter semantics is not
						// permission to guess a repair or add an account retry.
						// Existing sanitized wire-status policy remains independent.
						require.Contains(t, recorder.Body.String(), "error")
						return
					}
					if stream && (route == "responses" || route == "passthrough") && recorder.Code == http.StatusOK {
						require.Contains(t, recorder.Body.String(), "response.failed", "an HTTP200 Responses stream needs a failure terminal")
						require.Equal(t, http.StatusBadRequest, c.GetInt(OpsUpstreamStatusCodeKey), "the logical failure status must preserve parameter400")
					} else {
						require.Equal(t, http.StatusBadRequest, recorder.Code, "an uncommitted parameter failure must not become502")
					}
					// Direct passthrough HTTP errors deliberately use a sanitized
					// envelope; preserve that privacy policy while locking400.
					if !(route == "passthrough" && envelope == "http400") {
						require.Contains(t, recorder.Body.String(), "invalid_request_error")
						require.Contains(t, recorder.Body.String(), "additionalProperties must be false")
						if route == "responses" || route == "passthrough" {
							require.Contains(t, recorder.Body.String(), "invalid_function_parameters")
							require.Contains(t, recorder.Body.String(), "tools[0].parameters")
						}
					}
				})
			}
		}
	}
}

func TestOAuthReliabilityContractResponsesSSERejectedIDRepair(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, envelope := range []string{"failed", "bare_then_failed", "bare_eof", "zero_usage"} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/%s", passthrough, stream, envelope), func(t *testing.T) {
					body := oauthReliabilityContractWithOpaqueMetadata(t, rejectedMessageIDHistory(t, 1, 0))
					body, err := sjson.SetBytes(body, "stream", stream)
					require.NoError(t, err)
					errorObject := gjson.GetBytes(rejectedMessageIDError(1, "item_BAD"), "error").Raw
					failed := oauthReliabilityContractFailed(errorObject, "")
					frames := []string{failed}
					switch envelope {
					case "bare_then_failed":
						frames = []string{`{"type":"error","error":` + errorObject + `}`, failed}
					case "bare_eof":
						frames = []string{`{"type":"error","error":` + errorObject + `}`}
					case "zero_usage":
						frames = []string{`{"type":"response.in_progress","response":{"id":"resp_contract_failed","usage":{"input_tokens":0,"output_tokens":0}}}`, oauthReliabilityContractFailed(errorObject, `{"input_tokens":0,"output_tokens":0}`)}
					}
					upstream := &httpUpstreamRecorder{responses: []*http.Response{oauthReliabilityContractSSE(frames...), oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					svc.toolCorrector = NewCodexToolCorrector()
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = passthrough
					c, recorder := oauthReliabilityContractContext("/v1/responses", body)
					result, err := svc.Forward(c.Request.Context(), c, account, body)
					if envelope == "bare_eof" {
						// A bare error is not an authoritative failed response. Its
						// missing terminal must not authorize a new model execution.
						require.Error(t, err)
						var failover *UpstreamFailoverError
						require.False(t, errors.As(err, &failover))
						require.Len(t, upstream.bodies, 1)
						require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.1.id").String())
						require.Contains(t, recorder.Body.String(), "error")
						return
					}
					require.NoError(t, err, "a specifically rejected optional inline ID is repairable before work begins")
					require.Len(t, upstream.bodies, 2)
					for _, sentBody := range upstream.bodies {
						oauthReliabilityContractAssertOpaqueMetadata(t, sentBody)
					}
					expected, err := sjson.DeleteBytes(upstream.bodies[0], "input.1.id")
					require.NoError(t, err)
					require.JSONEq(t, oauthReliabilityComparableWireBody(t, expected), oauthReliabilityComparableWireBody(t, upstream.bodies[1]), "only the rejected optional ID and generated turn identity may change between attempts")
					require.NotNil(t, result)
					require.Equal(t, 7, result.Usage.InputTokens)
					require.Equal(t, 2, result.Usage.OutputTokens)
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Contains(t, recorder.Body.String(), "contract answer")
					require.NotContains(t, recorder.Body.String(), "resp_contract_failed", "the failed attempt must remain hidden")
					require.NotContains(t, recorder.Body.String(), "invalid_value")
				})
			}
		}
	}
}

func TestOAuthReliabilityContractResponsesSSERepairStopsAtReplayBoundary(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, boundary := range []struct {
				name, prior, usage string
			}{
				{"reasoning_progress", `{"type":"response.reasoning_text.delta","item_id":"rs_contract","output_index":0,"content_index":0,"delta":"working"}`, ""},
				{"hosted_tool", `{"type":"response.output_item.added","output_index":0,"item":{"id":"ws_contract","type":"web_search_call","status":"in_progress"}}`, ""},
				{"function_call", `{"type":"response.function_call_arguments.delta","item_id":"fc_contract","output_index":0,"delta":"{\"value\":"}`, ""},
				{"prior_usage", `{"type":"response.in_progress","response":{"id":"resp_contract_failed","usage":{"input_tokens":5,"output_tokens":0}}}`, ""},
				{"terminal_usage", "", `{"input_tokens":5,"output_tokens":0}`},
				{"bare_then_metered_terminal", "", `{"input_tokens":5,"output_tokens":0}`},
				{"canceled", "", ""},
				{"client_output", "", ""},
			} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/%s", passthrough, stream, boundary.name), func(t *testing.T) {
					body, err := sjson.SetBytes(rejectedMessageIDHistory(t, 1, 0), "stream", stream)
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
					upstream := &httpUpstreamRecorder{responses: []*http.Response{oauthReliabilityContractSSE(frames...), oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					svc.toolCorrector = NewCodexToolCorrector()
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = passthrough
					c, recorder := oauthReliabilityContractContext("/v1/responses", body)
					if boundary.name == "canceled" {
						ctx, cancel := context.WithCancel(c.Request.Context())
						cancel()
						c.Request = c.Request.WithContext(ctx)
					}
					if boundary.name == "client_output" {
						_, err := c.Writer.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"already sent\"}\n\n"))
						require.NoError(t, err)
					}
					result, err := svc.Forward(c.Request.Context(), c, account, body)
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "observable progress/cancellation forbids outer account replay too")
					require.LessOrEqual(t, len(upstream.bodies), 1, "unsafe repair must not send a second attempt")
					if boundary.name != "canceled" {
						require.Len(t, upstream.bodies, 1)
						require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.1.id").String())
					}
					if boundary.name == "prior_usage" || boundary.usage != "" {
						require.NotNil(t, result, "actual upstream usage must survive an unrepaired terminal")
						require.Equal(t, 5, result.Usage.InputTokens)
					}
					require.NotContains(t, recorder.Body.String(), "contract answer")
				})
			}
		}
	}
}

func TestOAuthReliabilityContractRepairBudgetSurvivesAccountAndBridgeSwitch(t *testing.T) {
	for _, nextPath := range []string{"next_account_http", "ws_http_bridge"} {
		t.Run(nextPath, func(t *testing.T) {
			items := make([]json.RawMessage, 9)
			items[0] = json.RawMessage(`{"type":"message","id":"msg_contract_base","role":"developer","content":"Keep the conversation.","internal_chat_message_metadata_passthrough":{"content_item_kinds":["model.base_instructions"]}}`)
			for i := 1; i < len(items); i++ {
				items[i] = json.RawMessage(fmt.Sprintf(`{"type":"message","id":"item_BAD_%d","role":"user","content":"retain this message"}`, i))
			}
			body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "stream": false, "store": false, "input": items})
			require.NoError(t, err)
			upstream := &httpUpstreamRecorder{}
			for i := 1; i <= 6; i++ {
				upstream.responses = append(upstream.responses, newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(i, fmt.Sprintf("item_BAD_%d", i)))))
			}
			upstream.responses = append(upstream.responses, newOpenAIRejectedFieldTestResponse(http.StatusServiceUnavailable, `{"error":{"type":"server_error","code":"server_is_overloaded","message":"synthetic temporary capacity failure"}}`))
			svc := newOpenAIRejectedFieldTestService(upstream)
			account := newOpenAIOAuthNamespaceTestAccount()
			c, _ := oauthReliabilityContractContext("/v1/responses", body)
			_, err = svc.Forward(c.Request.Context(), c, account, body)
			var failover *UpstreamFailoverError
			require.ErrorAs(t, err, &failover, "the unrelated capacity failure remains eligible for the handler")
			require.Len(t, upstream.bodies, 7, "six authorized repairs must have actually consumed the shared budget")
			require.False(t, c.Writer.Written(), "the first account failed before output")
			for i := 1; i <= 6; i++ {
				require.False(t, gjson.GetBytes(upstream.bodies[6], fmt.Sprintf("input.%d.id", i)).Exists())
			}
			account.ID++
			if nextPath == "next_account_http" {
				upstream.responses = []*http.Response{
					newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(1, "item_BAD_1"))),
					oauthReliabilityContractSSE(oauthReliabilityContractCompleted),
				}
				_, err = svc.Forward(c.Request.Context(), c, account, body)
				require.Error(t, err)
				require.Len(t, upstream.bodies, 8, "an account switch must not reset the six-repair request budget")
			} else {
				bridgeUpstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{
					newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(1, "item_BAD_1"))),
					oauthReliabilityContractSSE(oauthReliabilityContractCompleted),
				}}
				svc.httpUpstream = bridgeUpstream
				_, err = svc.proxyOpenAIWSHTTPBridgeTurn(c.Request.Context(), c, account, "synthetic-token", body, len(body), "gpt-5.6-sol", "", "", "", "", 1, func([]byte) error { return nil })
				require.Error(t, err)
				require.Equal(t, 1, bridgeUpstream.callCount, "a transport switch must share the already spent request budget")
			}
		})
	}
}
