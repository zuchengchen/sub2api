package apicompat

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityAnthropicOutputSchemaSurvivesConversions(t *testing.T) {
	const schema = `{"properties":{"values":{"items":{"minimum":9007199254740993},"uniqueItems":true}},"minProperties":1,"required":["values"],"additionalProperties":false,"$defs":{"optional":{"type":["string","null"]}},"x_future_keyword":{"value":7}}`
	for _, target := range []string{"responses", "chat", "responses_then_chat"} {
		t.Run(target, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","max_tokens":32,"messages":[{"role":"user","content":"fixture"}],"output_config":{"effort":"low","format":{"type":"json_schema","schema":` + schema + `}}}`)
			var req AnthropicRequest
			require.NoError(t, json.Unmarshal(body, &req))
			original := append([]byte(nil), req.OutputConfig.Format...)
			var out any
			var err error
			field := "text.format"
			switch target {
			case "chat":
				out, err = AnthropicToChatCompletionsRequest(&req)
				field = "response_format.json_schema"
			case "responses_then_chat":
				responses, convertErr := AnthropicToResponses(&req)
				require.NoError(t, convertErr)
				out, err = ResponsesToChatCompletionsRequest(responses)
				field = "response_format.json_schema"
			default:
				out, err = AnthropicToResponses(&req)
			}
			require.NoError(t, err)
			wire, err := json.Marshal(out)
			require.NoError(t, err)
			require.JSONEq(t, schema, gjson.GetBytes(wire, field+".schema").Raw)
			require.Equal(t, "9007199254740993", gjson.GetBytes(wire, field+".schema.properties.values.items.minimum").Raw)
			require.False(t, gjson.GetBytes(wire, field+".schema.type").Exists())
			require.False(t, gjson.GetBytes(wire, field+".schema.properties.values.type").Exists())
			require.True(t, gjson.GetBytes(wire, field+".strict").Bool())
			require.Equal(t, "response", gjson.GetBytes(wire, field+".name").String())
			require.Equal(t, original, []byte(req.OutputConfig.Format))
		})
	}
}

func TestOAuthReliabilityAnthropicOutputFormatRejectsAmbiguousContract(t *testing.T) {
	cases := []struct{ name, format, param string }{
		{"non_object", `"PRIVATE_VALUE"`, "output_config.format"},
		{"array", `[]`, "output_config.format"},
		{"unknown_type", `{"type":"PRIVATE_UNSUPPORTED_FORMAT","schema":{}}`, "output_config.format.type"},
		{"missing_type", `{"schema":{}}`, "output_config.format.type"},
		{"missing_schema", `{"type":"json_schema"}`, "output_config.format.schema"},
		{"null_schema", `{"type":"json_schema","schema":null}`, "output_config.format.schema"},
		{"boolean_schema", `{"type":"json_schema","schema":false}`, "output_config.format.schema"},
		{"array_schema", `{"type":"json_schema","schema":[]}`, "output_config.format.schema"},
		{"strict_false", `{"type":"json_schema","schema":{},"strict":false}`, "output_config.format.strict"},
		{"strict_null", `{"type":"json_schema","schema":{},"strict":null}`, "output_config.format.strict"},
		{"duplicate_schema", `{"type":"json_schema","schema":{"const":"PRIVATE_FIRST"},"schema":{}}`, "output_config.format"},
		{"escaped_duplicate_schema", `{"type":"json_schema","schema":{},"sch\u0065ma":{"const":"PRIVATE_LAST"}}`, "output_config.format"},
	}
	for _, tc := range cases {
		for _, target := range []string{"responses", "chat"} {
			t.Run(tc.name+"/"+target, func(t *testing.T) {
				var req AnthropicRequest
				require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-5.6-sol","messages":[{"role":"user","content":"fixture"}],"output_config":{"format":`+tc.format+`}}`), &req))
				var err error
				if target == "responses" {
					_, err = AnthropicToResponses(&req)
				} else {
					_, err = AnthropicToChatCompletionsRequest(&req)
				}
				var formatErr *AnthropicOutputFormatError
				require.ErrorAs(t, err, &formatErr)
				require.Equal(t, tc.param, formatErr.Param)
				require.NotContains(t, err.Error(), "PRIVATE")
				require.Less(t, len(err.Error()), 256)
			})
		}
	}
}

func TestOAuthReliabilityAnthropicOutputFormatNullAndEffortRemainIndependent(t *testing.T) {
	for _, field := range []string{"", `,"format":null`} {
		for _, target := range []string{"responses", "chat"} {
			t.Run(fmt.Sprintf("%s/%s", target, field), func(t *testing.T) {
				var req AnthropicRequest
				require.NoError(t, json.Unmarshal([]byte(`{"model":"gpt-5.6-sol","max_tokens":32,"messages":[{"role":"user","content":"fixture"}],"output_config":{"effort":"low"`+field+`}}`), &req))
				if target == "responses" {
					out, err := AnthropicToResponses(&req)
					require.NoError(t, err)
					require.Empty(t, out.Text.Format)
					require.Equal(t, "low", out.Reasoning.Effort)
				} else {
					out, err := AnthropicToChatCompletionsRequest(&req)
					require.NoError(t, err)
					require.Empty(t, out.ResponseFormat)
					require.Equal(t, "low", out.ReasoningEffort)
				}
			})
		}
	}
}

func TestOAuthReliabilityAnthropicOutputFormatPreservesEnvelopeExtensions(t *testing.T) {
	format, err := anthropicOutputFormatToResponsesTextFormat(json.RawMessage(`{"type":"json_schema","name":"custom_name","description":"retain description","strict":true,"schema":{"const":9007199254740993},"x_future_extension":{"enabled":true}}`))
	require.NoError(t, err)
	require.Equal(t, "custom_name", gjson.GetBytes(format, "name").String())
	require.Equal(t, "retain description", gjson.GetBytes(format, "description").String())
	require.True(t, gjson.GetBytes(format, "x_future_extension.enabled").Bool())
	require.Equal(t, "9007199254740993", gjson.GetBytes(format, "schema.const").Raw)
}
