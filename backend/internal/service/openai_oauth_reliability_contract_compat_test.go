//go:build unit

package service

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityContractCompatBareParameterWaitsForTerminal(t *testing.T) {
	for _, route := range []string{"chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, terminal := range []string{"metered_failure", "metered_failure_readerr", "completed", "eof"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", route, stream, terminal), func(t *testing.T) {
					frames := []string{`{"type":"error","error":` + oauthReliabilityContractSchemaError + `}`}
					switch terminal {
					case "metered_failure", "metered_failure_readerr":
						frames = append(frames, oauthReliabilityContractFailed(oauthReliabilityContractSchemaError, `{"input_tokens":5,"output_tokens":2}`))
					case "completed":
						frames = append(frames, oauthReliabilityContractCompleted)
					}
					response := oauthReliabilityContractSSE(frames...)
					if terminal == "metered_failure_readerr" {
						wire := "data: " + strings.Join(frames, "\n\ndata: ") + "\n"
						response.Body = &errTailReader{data: []byte(wire), err: io.ErrUnexpectedEOF}
					}
					upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					body := oauthReliabilityContractBody(route, stream)
					c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
					result, err := oauthReliabilityContractForward(svc, c, newOpenAIOAuthNamespaceTestAccount(), route, body)
					require.Len(t, upstream.bodies, 1, "a bare parameter event never authorizes another model execution")
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover))
					if terminal == "completed" {
						require.NoError(t, err)
						require.Contains(t, recorder.Body.String(), "contract answer")
						require.NotContains(t, recorder.Body.String(), "invalid_request_error")
					} else {
						require.Error(t, err)
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						require.Contains(t, recorder.Body.String(), "invalid_request_error")
					}
					if strings.HasPrefix(terminal, "metered_failure") {
						require.NotNil(t, result, "the authoritative terminal's real usage must be retained")
						require.Equal(t, 5, result.Usage.InputTokens)
						require.Equal(t, 2, result.Usage.OutputTokens)
					}
				})
			}
		}
	}
}

// Messages conversion currently strips most unsupported fields before the
// first attempt. Exercise its post-build sender with a future/legacy optional
// ID that genuinely survives to the wire, including an earlier .50 recovery
// that changes the input index. This uses the real HTTP sending/recovery path.
func TestOAuthReliabilityContractMessagesSenderRepairsCurrentWireBody(t *testing.T) {
	for _, encryptedFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("encrypted_first=%t", encryptedFirst), func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","stream":true,"input":[{"type":"message","role":"user","id":"item_BAD","content":"retain exact prompt"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"client_metadata":{"opaque":"keep"}}`)
			upstream := &httpUpstreamRecorder{}
			if encryptedFirst {
				body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","encrypted_content":"CAQSsynthetic-contract"},{"type":"message","role":"user","id":"item_BAD","content":"retain exact prompt"}]`))
				upstream.responses = append(upstream.responses, newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, messagesEncryptedRecoveryError("invalid_encrypted_content", messagesEncryptedRecoveryMessage)))
			}
			upstream.responses = append(upstream.responses,
				newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(rejectedMessageIDError(0, "item_BAD"))),
				oauthReliabilityContractSSE(oauthReliabilityContractCompleted))
			svc := newOpenAIRejectedFieldTestService(upstream)
			c, _ := oauthReliabilityContractContext("/v1/messages", body)
			account := newOpenAIOAuthNamespaceTestAccount()
			request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, "https://synthetic.invalid/responses", bytes.NewReader(body))
			require.NoError(t, err)
			request.Header.Set("X-Synthetic-Identity", "preserve-identity")
			state := openAIResponsesRejectedFieldRetryStateForRequest(c, body)
			var finalWire []byte
			response, err := withOpenAIRejectedFieldRetryAttempt(c, body, state, func() (*http.Response, error) {
				response, sendErr, encrypted := svc.sendOpenAIMessagesResponsesRequest(c.Request.Context(), c, account, request, "", body)
				require.False(t, encrypted)
				value, _ := c.Get(openAIRejectedFieldRetryAttemptKey)
				finalWire = append([]byte(nil), value.(*openAIRejectedFieldRetryAttempt).body...)
				return response, sendErr
			})
			require.NoError(t, err)
			defer response.Body.Close()
			attempts := 2
			if encryptedFirst {
				attempts++
			}
			require.Len(t, upstream.bodies, attempts)
			before := upstream.bodies[attempts-2]
			after := upstream.bodies[attempts-1]
			expected, err := sjson.DeleteBytes(before, "input.0.id")
			require.NoError(t, err)
			require.Equal(t, string(expected), string(after), "repair must modify only the exact field in the current wire body")
			require.Equal(t, string(after), string(finalWire), "SSE reader must see the final post-encrypted-recovery body")
			require.False(t, gjson.GetBytes(after, "input.0.id").Exists())
			for _, sent := range upstream.requests {
				require.Equal(t, "preserve-identity", sent.Header.Get("X-Synthetic-Identity"))
			}
		})
	}
}
