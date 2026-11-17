//go:build unit

package service

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityContractHTTP400RepairRejectsExecutionEvidence(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, evidence := range []string{"none", "zero_usage", "real_usage", "tool_output"} {
				t.Run(fmt.Sprintf("passthrough=%t/stream=%t/%s", passthrough, stream, evidence), func(t *testing.T) {
					body, err := sjson.SetBytes(rejectedMessageIDHistory(t, 1, 0), "stream", stream)
					require.NoError(t, err)
					failure, err := sjson.SetBytes(rejectedMessageIDError(1, "item_BAD"), "error.code", "invalid_id_prefix")
					require.NoError(t, err)
					switch evidence {
					case "zero_usage":
						failure, err = sjson.SetRawBytes(failure, "usage", []byte(`{"input_tokens":0,"output_tokens":0}`))
					case "real_usage":
						failure, err = sjson.SetRawBytes(failure, "usage", []byte(`{"input_tokens":11,"output_tokens":3}`))
					case "tool_output":
						failure, err = sjson.SetRawBytes(failure, "output", []byte(`[{"type":"web_search_call","id":"ws_contract_executed","status":"completed"}]`))
					}
					require.NoError(t, err)
					upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, string(failure)), oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					svc.toolCorrector = NewCodexToolCorrector()
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = passthrough
					c, recorder := oauthReliabilityContractContext("/v1/responses", body)
					_, err = svc.Forward(c.Request.Context(), c, account, body)
					if evidence == "none" || evidence == "zero_usage" {
						require.NoError(t, err)
						require.Len(t, upstream.bodies, 2, "an exact optional-ID rejection before any work remains repairable")
						require.False(t, gjson.GetBytes(upstream.bodies[1], "input.1.id").Exists())
						require.Contains(t, recorder.Body.String(), "contract answer")
					} else {
						require.Error(t, err)
						var failover *UpstreamFailoverError
						require.False(t, errors.As(err, &failover))
						require.Len(t, upstream.bodies, 1, "HTTP400 transport status must not erase actual execution evidence")
						require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.1.id").String())
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						require.NotContains(t, recorder.Body.String(), "contract answer")
					}
				})
			}
		}
	}
}
