//go:build unit

package service

import (
	"bytes"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityContractNamespaceFlattenKeepsUnmappedHistory(t *testing.T) {
	for _, flatten := range []bool{false, true} {
		for _, compact := range []bool{false, true} {
			for _, passthrough := range []bool{false, true} {
				for _, family := range []string{"excluded_function", "native_custom", "ordinary_function"} {
					t.Run(fmt.Sprintf("flatten=%t/compact=%t/passthrough=%t/%s", flatten, compact, passthrough, family), func(t *testing.T) {
						namespace, callType, childType, argument := "fixture", "function_call", "function", `"arguments":"{}"`
						if family == "excluded_function" {
							namespace = "image_gen"
						}
						if family == "native_custom" {
							callType, childType, argument = "custom_tool_call", "custom", `"input":"raw source"`
						}
						outputType := callType + "_output"
						tool := fmt.Sprintf(`{"type":"namespace","name":%q,"tools":[{"type":%q,"name":"lookup"}]}`, namespace, childType)
						outputIdentity := fmt.Sprintf(`,"name":"lookup","namespace":%q`, namespace)
						if family == "native_custom" {
							// Native custom outputs pair by call_id; they do not add
							// the function-output name/namespace extension.
							outputIdentity = ""
						}
						body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":"fixture"},{"type":%q,"call_id":"call_history","name":"lookup","namespace":%q,%s},{"type":%q,"call_id":"call_history"%s,"output":"opaque tool output"}],"tools":[%s]}`, callType, namespace, argument, outputType, outputIdentity, tool))
						original := bytes.Clone(body)
						upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse)}
						account := newOpenAIOAuthNamespaceTestAccount()
						account.Extra["openai_responses_flatten_namespaces"] = flatten
						account.Extra["openai_passthrough"] = passthrough
						path := "/v1/responses"
						if compact {
							path += "/compact"
						}
						c, recorder := oauthReliabilityContractContext(path, body)
						result, err := newOpenAIRejectedFieldTestService(upstream).Forward(c.Request.Context(), c, account, body)
						require.NoError(t, err)
						require.NotNil(t, result)
						require.Equal(t, http.StatusOK, recorder.Code)
						require.Len(t, upstream.bodies, 1)
						call := gjson.GetBytes(upstream.lastBody, `input.#(type=="`+callType+`")`)
						output := gjson.GetBytes(upstream.lastBody, `input.#(type=="`+outputType+`")`)
						require.NotEmpty(t, call.Get("call_id").String())
						require.Equal(t, call.Get("call_id").String(), output.Get("call_id").String())
						require.Equal(t, "opaque tool output", output.Get("output").String())
						if family == "native_custom" {
							require.Equal(t, "raw source", call.Get("input").String())
						}
						mapped := family == "ordinary_function" && (flatten || compact)
						if compact || mapped {
							require.False(t, call.Get("namespace").Exists())
							require.False(t, output.Get("namespace").Exists())
						} else {
							require.Equal(t, namespace, call.Get("namespace").String())
							if family != "native_custom" {
								require.Equal(t, namespace, output.Get("namespace").String())
							}
						}
						if mapped {
							require.Equal(t, "fixture__lookup", call.Get("name").String())
							if output.Get("name").Exists() {
								require.Equal(t, "fixture__lookup", output.Get("name").String(), "a named result must preserve its mapped call identity")
							}
							require.True(t, gjson.GetBytes(upstream.lastBody, `tools.#(name=="fixture__lookup")`).Exists())
						} else {
							require.Equal(t, "lookup", call.Get("name").String())
							if family != "native_custom" {
								require.Equal(t, "lookup", output.Get("name").String())
							}
							require.JSONEq(t, tool, gjson.GetBytes(upstream.lastBody, `tools.#(type=="namespace")`).Raw)
						}
						require.Equal(t, original, body)
					})
				}
			}
		}
	}
}

func TestOAuthReliabilityContractOAuthUnknownContinuationNodesStayNative(t *testing.T) {
	const input = `[{"type":"future_tool_call","id":"opaque_call_id","call_id":"future_pair","namespace":"future_namespace","arguments":{"opaque_integer":9007199254740993}},{"type":"future_tool_output","id":"opaque_result_id","call_id":"future_pair","namespace":"future_namespace","output":{"value":"retain exact native result","integer":9007199254740993}}]`
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprintf("passthrough=%t", passthrough), func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":` + input + `}`)
			original := bytes.Clone(body)
			proof := AnalyzeOpenAIContinuationReplayProof(body)
			require.Equal(t, OpenAIContinuationProofUnproven, proof.Verdict)
			require.NoError(t, ValidateOpenAIContinuationInput(body))
			require.False(t, OpenAIContinuationMaySwitchAccount(body))
			upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse)}
			account := newOpenAIOAuthNamespaceTestAccount()
			account.Extra["openai_passthrough"] = passthrough
			c, _ := oauthReliabilityContractContext("/v1/responses", body)
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(c.Request.Context(), c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			forwarded := gjson.GetBytes(upstream.lastBody, "input")
			require.JSONEq(t, input, forwarded.Raw, "unfamiliar protocol nodes must reach native validation intact, not lose identity fields in later OAuth transforms")
			require.Equal(t, "9007199254740993", forwarded.Get("0.arguments.opaque_integer").Raw)
			require.Equal(t, "9007199254740993", forwarded.Get("1.output.integer").Raw)
			require.Equal(t, original, body)
		})
	}
}

func TestOAuthReliabilityContractNamespaceFlattenOptionalOutputNamespace(t *testing.T) {
	for _, flatten := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			for _, named := range []bool{false, true} {
				t.Run(fmt.Sprintf("flatten=%t/passthrough=%t/named=%t", flatten, passthrough, named), func(t *testing.T) {
					nameField := ""
					if named {
						nameField = `,"name":"lookup"`
					}
					body := []byte(`{"model":"gpt-5.6-sol","stream":false,"input":[{"type":"message","role":"user","content":"fixture"},{"type":"function_call","call_id":"call_optional","name":"lookup","namespace":"fixture","arguments":"{}"},{"type":"function_call_output","call_id":"call_optional"` + nameField + `,"output":"opaque result"}],"tools":[{"type":"namespace","name":"fixture","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`)
					original := bytes.Clone(body)
					require.Equal(t, OpenAIContinuationProofInline, AnalyzeOpenAIContinuationReplayProof(body).Verdict, "omitting an optional result namespace does not break the original call/result association")
					upstream := &httpUpstreamRecorder{resp: newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse)}
					account := newOpenAIOAuthNamespaceTestAccount()
					account.Extra["openai_responses_flatten_namespaces"] = flatten
					account.Extra["openai_passthrough"] = passthrough
					c, recorder := oauthReliabilityContractContext("/v1/responses", body)
					result, err := newOpenAIRejectedFieldTestService(upstream).Forward(c.Request.Context(), c, account, body)
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, http.StatusOK, recorder.Code)
					require.Len(t, upstream.bodies, 1)
					call := gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call")`)
					output := gjson.GetBytes(upstream.lastBody, `input.#(type=="function_call_output")`)
					require.NotEmpty(t, call.Get("call_id").String())
					require.Equal(t, call.Get("call_id").String(), output.Get("call_id").String())
					require.Equal(t, "opaque result", output.Get("output").String())
					require.False(t, output.Get("namespace").Exists(), "do not invent an omitted result namespace")
					expectedName := "lookup"
					if flatten {
						expectedName = "fixture__lookup"
						require.False(t, call.Get("namespace").Exists())
					} else {
						require.Equal(t, "fixture", call.Get("namespace").String())
					}
					require.Equal(t, expectedName, call.Get("name").String())
					if named {
						require.Equal(t, expectedName, output.Get("name").String(), "a present result name must still identify the actual mapped call")
					} else {
						require.False(t, output.Get("name").Exists(), "do not invent an omitted result name")
					}
					require.Equal(t, OpenAIContinuationProofInline, AnalyzeOpenAIContinuationReplayProof(upstream.lastBody).Verdict, "forwarding must retain the original complete inline association")
					require.Equal(t, original, body)
				})
			}
		}
	}
}
