package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func oauthReliabilityIDObject(t *testing.T, index int, id, code, param string, prefix bool) string {
	t.Helper()
	message := fmt.Sprintf("Invalid 'input[%d].id': '%s'. Expected an ID that begins with 'msg'.", index, id)
	if prefix {
		message = fmt.Sprintf("[ApiIdParam] [input[%d].id] [invalid_id_prefix] ", index) + message
	}
	object := map[string]any{"type": "invalid_request_error", "message": message}
	if code != "" {
		object["code"] = code
	}
	if param != "" {
		object["param"] = param
	}
	body, err := json.Marshal(object)
	require.NoError(t, err)
	return string(body)
}

func TestOAuthReliabilityRejectedMessageIDObservedPrefixAcrossEnvelopes(t *testing.T) {
	body := rejectedMessageIDHistory(t, 15, 0)
	for _, code := range []string{"", "invalid_value", "invalid_id_prefix"} {
		for _, param := range []string{"", "input[15].id"} {
			object := oauthReliabilityIDObject(t, 15, "item_BAD", code, param, true)
			envelopes := oauthReliabilityErrorEnvelopes(object)
			envelopes["ws_bare"] = []byte(strings.Replace(object, `"type":"invalid_request_error"`, `"type":"error"`, 1))
			for name, payload := range envelopes {
				t.Run(code+"/"+param+"/"+name, func(t *testing.T) {
					index, id, ok := openAIResponsesRejectedMessageID(payload)
					require.True(t, ok)
					require.Equal(t, 15, index)
					require.Equal(t, "item_BAD", id)
					require.Equal(t, http.StatusBadRequest, openAIStreamFailureStatus(payload, ""))
					require.False(t, openAIStreamFailedEventShouldFailover(payload, ""), "ID repair must not become account failover")
					next, reason, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, payload)
					require.NoError(t, err)
					require.True(t, changed)
					require.Contains(t, reason, "input[15].id")
					require.Equal(t, strings.Replace(string(body), `"id":"item_BAD",`, "", 1), string(next))
					_, _, changed, err = normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, next, payload)
					require.NoError(t, err)
					require.False(t, changed)
				})
			}
		}
	}
}

func TestOAuthReliabilityRejectedMessageIDStrictEvidence(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","id":"item_BAD","role":"assistant","content":"keep"}]}`)
	object := oauthReliabilityIDObject(t, 0, "item_BAD", "invalid_id_prefix", "input[0].id", true)
	tests := map[string]string{
		"wrong validator":          strings.Replace(object, "ApiIdParam", "OtherValidator", 1),
		"wrong prefix code":        strings.Replace(object, "[invalid_id_prefix]", "[invalid_value]", 1),
		"prefix path mismatch":     strings.Replace(object, "[input[0].id]", "[input[1].id]", 1),
		"structured path mismatch": strings.Replace(object, `"param":"input[0].id"`, `"param":"input[1].id"`, 1),
		"different value":          strings.Replace(object, "'item_BAD'", "'item_other'", 1),
		"case sensitive value":     strings.Replace(object, "'item_BAD'", "'item_bad'", 1),
		"wrong structured code":    strings.Replace(object, `"code":"invalid_id_prefix"`, `"code":"server_error"`, 1),
		"wrong structured type":    strings.Replace(object, "invalid_request_error", "server_error", 1),
		"wrong expected prefix":    strings.Replace(object, "'msg'", "'call'", 1),
		"message mention only":     strings.Replace(object, "[ApiIdParam]", "Suggestion: [ApiIdParam]", 1),
		"trailing advice":          strings.Replace(object, "begins with 'msg'.", "begins with 'msg'. Please delete all IDs.", 1),
		"embedded JSON prose":      `{"type":"invalid_request_error","message":` + fmt.Sprintf("%q", `{"error":`+object+`}`) + `}`,
		"noncanonical index":       strings.ReplaceAll(object, "input[0]", "input[00]"),
		"overflow index":           strings.ReplaceAll(object, "input[0]", "input[999999999999999999999999999999]"),
		"wrong path case":          strings.ReplaceAll(object, "input[0]", "Input[0]"),
		"broken path quote":        strings.Replace(object, "Invalid 'input[0].id'", `Invalid 'input[0].id\"`, 1),
		"control character":        strings.Replace(object, "item_BAD", `item_\u0001BAD`, 1),
		"too long value":           oauthReliabilityIDObject(t, 0, strings.Repeat("x", maxOpenAIRejectedMessageIDLength+1), "invalid_id_prefix", "input[0].id", true),
		"plain lacks param":        oauthReliabilityIDObject(t, 0, "item_BAD", "invalid_id_prefix", "", false),
		"plain lacks code":         oauthReliabilityIDObject(t, 0, "item_BAD", "", "input[0].id", false),
		"duplicate error param":    strings.Replace(object, `"param":"input[0].id"`, `"param":"input[0].id","param":"input[1].id"`, 1),
		"escaped duplicate param":  strings.Replace(object, `"param":"input[0].id"`, `"param":"input[0].id","\u0070aram":"input[1].id"`, 1),
		"contradictory status":     strings.TrimSuffix(object, "}") + `,"status_code":429}`,
	}
	for name, errorObject := range tests {
		t.Run(name, func(t *testing.T) {
			payload := []byte(`{"error":` + errorObject + `}`)
			next, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, payload)
			require.NoError(t, err)
			require.False(t, changed)
			require.Nil(t, next)
		})
	}
	for _, code := range []string{"invalid_value", "invalid_id_prefix"} {
		payload := []byte(`{"error":` + oauthReliabilityIDObject(t, 0, "item_BAD", code, "input[0].id", false) + `}`)
		_, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, payload)
		require.NoError(t, err)
		require.True(t, changed, "structured code and exact param support the legacy unprefixed form")
	}
}

func TestOAuthReliabilityRejectedMessageIDPreservesReferencesAndOpaqueFields(t *testing.T) {
	valid := `{"input":[{"type":"message","id":"item_BAD","role":"assistant","content":"keep"}]}`
	payload := []byte(`{"error":` + oauthReliabilityIDObject(t, 0, "item_BAD", "", "", true) + `}`)
	tests := map[string]string{
		"reference":            strings.TrimSuffix(valid, "]}") + `,{"type":"item_reference","id":"item_BAD"}]}`,
		"same ID unknown item": strings.TrimSuffix(valid, "]}") + `,{"type":"future_item","id":"item_BAD"}]}`,
		"same ID message":      strings.TrimSuffix(valid, "]}") + `,{"type":"message","id":"item_BAD","role":"user","content":"different"}]}`,
		"reasoning":            strings.Replace(valid, `"type":"message"`, `"type":"reasoning"`, 1),
		"function call":        strings.Replace(valid, `"type":"message"`, `"type":"function_call"`, 1),
		"call identity":        strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","call_id":"call_keep"`, 1),
		"reasoning content":    strings.Replace(valid, `"role":"assistant"`, `"role":"assistant","encrypted_content":"opaque"`, 1),
		"duplicate content":    strings.Replace(valid, `"content":"keep"`, `"content":"keep","content":null`, 1),
		"nested duplicate":     strings.Replace(valid, `"content":"keep"`, `"content":[{"type":"output_text","text":"keep","text":"other"}]`, 1),
		"schema duplicate":     strings.TrimSuffix(valid, "}") + `,"tools":[{"parameters":{"type":"object","type":"string"}}]}`,
		"content reference":    strings.Replace(valid, `"content":"keep"`, `"content":[{"type":"item_reference","id":"item_BAD"}]`, 1),
		"content not inline":   strings.Replace(valid, `"content":"keep"`, `"content":[null]`, 1),
		"content incomplete":   strings.Replace(valid, `"content":"keep"`, `"content":[{"type":"output_text"}]`, 1),
		"unknown content":      strings.Replace(valid, `"content":"keep"`, `"content":[{"type":"future_content","value":"keep"}]`, 1),
		"deep request":         strings.TrimSuffix(valid, "}") + `,"metadata":` + strings.Repeat("[", maxOpenAIErrorJSONDepth) + `0` + strings.Repeat("]", maxOpenAIErrorJSONDepth) + `}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			next, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, []byte(body), payload)
			require.NoError(t, err)
			require.False(t, changed)
			require.Nil(t, next)
		})
	}
	for _, id := range []string{"msg_valid", "rs_reasoning", "fc_function", "call_tool"} {
		t.Run(id, func(t *testing.T) {
			errorBody := []byte(`{"error":` + oauthReliabilityIDObject(t, 0, id, "", "", true) + `}`)
			_, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest,
				[]byte(strings.Replace(valid, "item_BAD", id, 1)), errorBody)
			require.NoError(t, err)
			require.False(t, changed)
		})
	}
	// Numbers, duplicate-looking text inside tool JSON strings, schema and a
	// different reference are untouched by the one-field raw JSON patch.
	body := strings.TrimSuffix(valid, "]}") + `,{"type":"function_call","id":"fc_keep","call_id":"call_keep","arguments":"{\"x\":1,\"x\":2}"},{"type":"item_reference","id":"msg_keep"},{"type":"reasoning","id":"rs_keep","encrypted_content":"opaque","future":9007199254740993}],"previous_response_id":"resp_keep","tools":[{"type":"function","parameters":{"type":"object","required":["keep"],"additionalProperties":false}}]}`
	next, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, []byte(body), payload)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, strings.Replace(body, `"id":"item_BAD",`, "", 1), string(next))
}

func TestOAuthReliabilityRejectedMessageIDLargeHistoryAndBoundedValidation(t *testing.T) {
	body := rejectedMessageIDHistory(t, 268, 16<<20)
	payload := []byte(`{"error":` + oauthReliabilityIDObject(t, 268, "item_BAD", "", "", true) + `}`)
	next, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, payload)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, strings.Replace(string(body), `"id":"item_BAD",`, "", 1), string(next))
	oversized := []byte(`{"error":{"type":"invalid_request_error","message":"` + strings.Repeat("x", maxOpenAIErrorNormalizationBytes) + `"}}`)
	_, _, changed, err = normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, oversized)
	require.NoError(t, err)
	require.False(t, changed)
	manyFields := []byte(`{"input":[{"id":"item_BAD","role":"user","content":"keep"}],"metadata":[` + strings.Repeat("0,", maxOpenAIErrorJSONFields) + `0]}`)
	_, _, changed, err = normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, manyFields,
		[]byte(`{"error":`+oauthReliabilityIDObject(t, 0, "item_BAD", "", "", true)+`}`))
	require.NoError(t, err)
	require.False(t, changed, "parsing-work cap declines repair without rejecting or truncating the request")
}

func TestOAuthReliabilityRejectedMessageIDOAuthHTTPPrefixRecovery(t *testing.T) {
	body := rejectedMessageIDHistory(t, 15, 0)
	payload := `{"error":` + oauthReliabilityIDObject(t, 15, "item_BAD", "", "", true) + `}`
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, payload),
		newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse),
	}}
	c := newOpenAIRejectedFieldTestContext(body)
	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), c, newOpenAIOAuthNamespaceTestAccount(), body)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	require.Equal(t, "item_BAD", gjson.GetBytes(upstream.bodies[0], "input.15.id").String())
	require.Equal(t, strings.Replace(string(upstream.bodies[0]), `"id":"item_BAD",`, "", 1), string(upstream.bodies[1]))
}
