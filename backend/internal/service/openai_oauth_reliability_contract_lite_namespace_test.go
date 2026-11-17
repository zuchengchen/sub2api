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

func TestOAuthReliabilityContractFinalLiteNamespaceChildren(t *testing.T) {
	for _, carrier := range []string{"top_level", "additional_tools", "tool_search_output"} {
		for _, both := range []bool{false, true} {
			for _, tc := range []struct {
				name, child string
				rejected    bool
			}{
				{"function", `{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"tool":{"const":"web_search"}}}}`, false},
				{"custom", `{"type":"custom","name":"patch","format":{"type":"text"}}`, false},
				{"hosted", `{"type":"web_search"}`, true},
				{"server_search", `{"type":"tool_search","execution":"server"}`, true},
			} {
				t.Run(fmt.Sprintf("%s/both=%t/%s", carrier, both, tc.name), func(t *testing.T) {
					toolsField := ""
					if both {
						toolsField = `,"tools":[{"type":"function","name":"ordinary","parameters":{"type":"object"}}]`
					}
					namespace := `{"type":"namespace","name":"fixture_namespace","children":[` + tc.child + `]` + toolsField + `}`
					body := `{"model":"gpt-6-astra","stream":false,"input":[{"type":"message","role":"user","content":"fixture"}`
					switch carrier {
					case "additional_tools":
						body += `,{"type":"additional_tools","role":"developer","tools":[` + namespace + `]}]}`
					case "tool_search_output":
						body += `,{"type":"tool_search_call","id":"tsc_fixture","call_id":"call_search","execution":"client","arguments":{}},{"type":"tool_search_output","call_id":"call_search","execution":"client","tools":[` + namespace + `]}]}`
					default:
						body += `],"tools":[` + namespace + `]}`
					}
					original := []byte(body)
					upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
					c, recorder := oauthReliabilityContractContext("/v1/responses", original)
					c.Request.Header.Set(responsesLiteHeader, "true")
					result, err := newOpenAIRejectedFieldTestService(upstream).Forward(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), original)
					if tc.rejected || both {
						require.Error(t, err)
						require.Nil(t, result)
						require.Empty(t, upstream.bodies)
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						param := gjson.Get(recorder.Body.String(), "error.param").String()
						if both {
							require.Contains(t, param, "tools[0]")
						} else {
							require.Contains(t, param, ".children[0].")
						}
						require.NotContains(t, recorder.Body.String(), "fixture_namespace")
						return
					}
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Len(t, upstream.bodies, 1)
					inputType := "additional_tools"
					if carrier == "tool_search_output" {
						inputType = "tool_search_output"
					}
					forwarded := gjson.GetBytes(upstream.lastBody, `input.#(type=="`+inputType+`").tools.#(name=="fixture_namespace")`)
					require.JSONEq(t, namespace, forwarded.Raw, "validation must not remove the alternate declaration list")
					require.Equal(t, []byte(body), original)
				})
			}
		}
	}
}

func TestOAuthReliabilityContractNamespaceFlattenPreservesRawSchemaNumbers(t *testing.T) {
	const schema = `{"type":"object","properties":{"value":{"minimum":9007199254740993},"list":{"uniqueItems":true}},"minProperties":1,"x_future":{"integer":9007199254740993}}`
	for _, flatten := range []bool{false, true} {
		for _, passthrough := range []bool{false, true} {
			t.Run(fmt.Sprintf("flatten=%t/passthrough=%t", flatten, passthrough), func(t *testing.T) {
				body := []byte(`{"model":"gpt-6-astra","stream":false,"input":[{"type":"message","role":"user","content":"fixture"}],"tools":[{"type":"namespace","name":"fixture","tools":[{"type":"function","name":"lookup","parameters":` + schema + `}]}],"text":{"format":{"type":"json_schema","name":"fixture","strict":false,"schema":` + schema + `}}}`)
				original := bytes.Clone(body)
				upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
				account := newOpenAIOAuthNamespaceTestAccount()
				account.Extra["openai_responses_flatten_namespaces"] = flatten
				account.Extra["openai_passthrough"] = passthrough
				c, recorder := oauthReliabilityContractContext("/v1/responses", body)
				result, err := newOpenAIRejectedFieldTestService(upstream).Forward(c.Request.Context(), c, account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, http.StatusOK, recorder.Code)
				require.Len(t, upstream.bodies, 1)
				toolPath := `tools.#(type=="namespace").tools.0.parameters`
				if flatten {
					toolPath = `tools.#(name=="fixture__lookup").parameters`
				}
				for _, field := range []string{toolPath, "text.format.schema"} {
					got := gjson.GetBytes(upstream.lastBody, field)
					require.JSONEq(t, schema, got.Raw)
					require.Equal(t, "9007199254740993", got.Get("properties.value.minimum").Raw)
					require.Equal(t, "9007199254740993", got.Get("x_future.integer").Raw)
					require.True(t, got.Get("properties.list.uniqueItems").Bool())
				}
				require.Equal(t, original, body)
			})
		}
	}
}

func TestOAuthReliabilityContractNamespaceFlattenRejectsLossyDeclarations(t *testing.T) {
	for _, flatten := range []bool{false, true} {
		for _, tc := range []struct {
			name, namespace, param string
			allowed                bool
		}{
			{"ambiguous", `{"type":"namespace","name":"fixture","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"children":[{"type":"custom","name":"patch"}]}`, "tools[0]", false},
			{"mixed_custom", `{"type":"namespace","name":"fixture","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"custom","name":"patch","format":{"type":"text"}}]}`, "tools[0].tools[1].type", false},
			{"excluded_namespace", `{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"lookup"}],"children":[{"type":"custom","name":"patch"}]}`, "", true},
			{"custom_only_no_flatten", `{"type":"namespace","name":"fixture","children":[{"type":"custom","name":"patch","format":{"type":"text"}}]}`, "", true},
			{"both_native_no_flatten", `{"type":"namespace","name":"fixture","tools":[{"type":"custom","name":"first"}],"children":[{"type":"custom","name":"second"}]}`, "", true},
		} {
			t.Run(fmt.Sprintf("flatten=%t/%s", flatten, tc.name), func(t *testing.T) {
				body := []byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":"fixture"}],"tools":[` + tc.namespace + `]}`)
				upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
				account := newOpenAIOAuthNamespaceTestAccount()
				account.Extra["openai_responses_flatten_namespaces"] = flatten
				c, recorder := oauthReliabilityContractContext("/v1/responses", body)
				result, err := newOpenAIRejectedFieldTestService(upstream).Forward(c.Request.Context(), c, account, body)
				if flatten && !tc.allowed {
					require.Error(t, err)
					require.Nil(t, result)
					require.Empty(t, upstream.bodies)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, tc.param, gjson.Get(recorder.Body.String(), "error.param").String())
					return
				}
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.JSONEq(t, tc.namespace, gjson.GetBytes(upstream.lastBody, `tools.#(type=="namespace")`).Raw)
			})
		}
	}
}
