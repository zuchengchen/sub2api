//go:build unit

package service

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityContractToolSchemaPatternsPreservedAcrossForwarding(t *testing.T) {
	const schema = `{"type":"object","properties":{"code":{"type":"string","pattern":"^(?=a)[a-z]+$"},"tail":{"type":"string","pattern":"(?<!bad)ok"}},"required":["code"],"additionalProperties":false}`
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, rejected := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/rejected=%t", route, stream, rejected), func(t *testing.T) {
					body := oauthReliabilityContractBody(route, stream)
					tool := `{"type":"function","name":"lookup","parameters":` + schema + `,"strict":false}`
					if route == "chat" {
						tool = `{"type":"function","function":{"name":"lookup","parameters":` + schema + `,"strict":false}}`
					}
					if route == "messages" {
						tool = `{"name":"lookup","input_schema":` + schema + `}`
					}
					body, err := sjson.SetRawBytes(body, "tools", []byte(`[`+tool+`]`))
					require.NoError(t, err)
					original := bytes.Clone(body)
					response := oauthReliabilityContractSSE(oauthReliabilityContractCompleted)
					if rejected {
						response = newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"invalid_function_parameters","param":"tools[0].parameters.properties.code.pattern","message":"Unsupported pattern"}}`)
					}
					upstream := &httpUpstreamRecorder{resp: response}
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_passthrough"] = route == "passthrough"
					c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
					result, err := oauthReliabilityContractForward(newOpenAIRejectedFieldTestService(upstream), c, account, route, body)
					if rejected {
						require.Error(t, err)
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						var failover *UpstreamFailoverError
						require.False(t, errors.As(err, &failover))
					} else {
						require.NoError(t, err)
						require.NotNil(t, result)
					}
					require.Len(t, upstream.bodies, 1, "schema rejection must not authorize relaxing the pattern or another execution")
					forwarded := gjson.GetBytes(upstream.lastBody, `tools.#(name=="lookup").parameters`)
					require.JSONEq(t, schema, forwarded.Raw)
					require.Equal(t, "^(?=a)[a-z]+$", forwarded.Get("properties.code.pattern").String())
					require.Equal(t, "(?<!bad)ok", forwarded.Get("properties.tail.pattern").String())
					require.Equal(t, original, body)
				})
			}
		}
	}
}
