//go:build unit

package service

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityContractChatCustomExecutionExtensions(t *testing.T) {
	for _, responsesShape := range []bool{false, true} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct{ name, extension, param string }{
				{"async", `"async":true`, "tools[0].async"},
				{"programmatic", `"allowed_callers":["direct","programmatic"]`, "tools[0].allowed_callers"},
				{"synchronous", `"async":false`, ""},
				{"direct", `"allowed_callers":["direct"]`, ""},
			} {
				t.Run(fmt.Sprintf("responses_shape=%t/stream=%t/%s", responsesShape, stream, tc.name), func(t *testing.T) {
					tool := `{"type":"custom","custom":{"name":"run_code","format":{"type":"text"},` + tc.extension + `}}`
					input := `"messages":[{"role":"user","content":"fixture"}]`
					if responsesShape {
						tool = `{"type":"custom","name":"run_code","format":{"type":"text"},` + tc.extension + `}`
						input = `"input":[{"type":"message","role":"user","content":"fixture"}]`
					}
					body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"stream_options":{"include_usage":true},%s,"tools":[%s]}`, stream, input, tool))
					upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityCustomCompletion(`[{"type":"custom_tool_call","call_id":"call_native","name":"run_code","input":"print(1)"}]`))}
					c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
					result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
					if tc.param != "" {
						require.Error(t, err)
						require.Nil(t, result)
						require.Empty(t, upstream.bodies)
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						require.Equal(t, tc.param, gjson.Get(recorder.Body.String(), "error.param").String())
						require.True(t, IsResponseCommitted(c))
						return
					}
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.bodies, 1)
					forwarded := gjson.GetBytes(upstream.lastBody, `tools.#(type=="custom")`)
					if tc.name == "synchronous" {
						require.True(t, forwarded.Get("async").Exists())
						require.Equal(t, gjson.False, forwarded.Get("async").Type)
					} else {
						require.JSONEq(t, `["direct"]`, forwarded.Get("allowed_callers").Raw)
					}
					oauthReliabilityAssertNativeCustomResult(t, recorder.Body.String(), stream, "print(1)")
				})
			}
		}
	}
}
