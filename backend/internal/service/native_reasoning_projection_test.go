package service

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func nativeProjectionChat(t *testing.T, req NativeReasoningReplayRequest) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"model": req.Scope.Model, "messages": req.Messages, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "python", "parameters": map[string]any{"type": "object"}}}}, "reasoning_effort": "high", "stream": false})
	require.NoError(t, err)
	return raw
}

// This helper uses the actual compatibility converter and OAuth transform.
// Synthetic prefix injection stands in for the separately tested synthesis
// stage; its position/contents must be included in SourceInput and Parameters.
func nativeProjectionNormalized(t *testing.T, chat []byte, synthesize bool) ([]byte, map[string]string) {
	t.Helper()
	var request apicompat.ChatCompletionsRequest
	require.NoError(t, json.Unmarshal(chat, &request))
	converted, err := apicompat.ChatCompletionsToResponses(&request)
	require.NoError(t, err)
	raw, err := json.Marshal(converted)
	require.NoError(t, err)
	var decoded map[string]any
	require.NoError(t, decodeOpenAIJSONUseNumber(raw, &decoded))
	if synthesize {
		input, _ := decoded["input"].([]any)
		decoded["input"] = append([]any{map[string]any{"type": "message", "role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "Synthetic stable base instructions."}}, "content_item_kinds": []any{"model.base_instructions"}}}, input...)
	}
	result := applyCodexOAuthTransformWithOptions(decoded, codexOAuthTransformOptions{SkipDefaultInstructions: true, OmitPromotedSystemMessagesFromInput: true})
	require.NoError(t, result.Error)
	raw, err = json.Marshal(decoded)
	require.NoError(t, err)
	raw, _, err = removeOpenAIPromptCacheConfiguration(raw)
	require.NoError(t, err)
	return raw, result.ToolNameReverse
}

func nativeProjectionFixture(t *testing.T) (*NativeReasoningReplay, NativeReasoningReplayRequest, NativeReasoningReplayCompletion, []byte, []NativeReasoningReplayReplacement, map[string]string) {
	t.Helper()
	engine, _, req, completion := nativeReplayFixture(t)
	chat := nativeProjectionChat(t, req)
	initial, reverse := nativeProjectionNormalized(t, chat, true)
	var err error
	req.Parameters, err = nativeReasoningProjectionParameters(chat, initial, reverse)
	require.NoError(t, err)
	completion.SourceInput = json.RawMessage(gjson.GetBytes(initial, "input").Raw)
	require.NoError(t, engine.Commit(context.Background(), req, completion))
	next := nativeReplayContinuation(req, completion)
	nextChat := nativeProjectionChat(t, next)
	body, reverse := nativeProjectionNormalized(t, nextChat, true)
	next.Parameters, err = nativeReasoningProjectionParameters(nextChat, body, reverse)
	require.NoError(t, err)
	require.JSONEq(t, string(req.Parameters), string(next.Parameters), "semantic parameters and leading input stay stable as history grows")
	replacements, err := engine.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, replacements, 1)
	return engine, next, completion, body, replacements, reverse
}

func TestNativeReasoningReplayProjectionNormalizedPrefixAndPairing(t *testing.T) {
	_, _, completion, body, replacements, reverse := nativeProjectionFixture(t)
	original := append([]byte(nil), body...)
	require.Contains(t, string(body), "fc_synthetic")
	require.NotContains(t, string(body), "opaque-synthetic-reasoning")
	updated, refs, err := applyNativeReasoningProjection(body, replacements, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Equal(t, []NativeReasoningReplayReference{replacements[0].Reference}, refs)
	require.Equal(t, original, body, "the original normalized request remains unchanged")
	input := gjson.GetBytes(updated, "input").Array()
	require.Equal(t, "developer", input[0].Get("role").String())
	require.Equal(t, "user", input[1].Get("role").String())
	var native struct {
		Output []json.RawMessage `json:"output"`
	}
	require.NoError(t, json.Unmarshal(completion.Response, &native))
	for i, item := range native.Output {
		require.JSONEq(t, string(item), input[i+2].Raw)
	}
	require.Equal(t, "call_synthetic", input[5].Get("call_id").String())
	require.Equal(t, "synthetic clear", input[5].Get("output").String())
	require.Contains(t, string(updated), `"phase":"commentary"`)
	require.Contains(t, string(updated), "9007199254740993")
	require.NotContains(t, string(updated), `\u003cthinking\u003e`)
}

func TestNativeReasoningReplayProjectionConsecutiveCausalPrefixes(t *testing.T) {
	engine, next, _, body, first, reverse := nativeProjectionFixture(t)
	firstUpstream, refs, err := applyNativeReasoningProjection(body, first, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	_, _, _, second := nativeReplayFixture(t)
	second.Response = json.RawMessage(strings.ReplaceAll(string(second.Response), "synthetic", "second"))
	second.DeliveredAssistant = json.RawMessage(strings.ReplaceAll(string(second.DeliveredAssistant), "synthetic", "second"))
	second.SourceInput = json.RawMessage(gjson.GetBytes(firstUpstream, "input").Raw)
	require.NoError(t, engine.Commit(context.Background(), next, second))
	third := nativeReplayContinuation(next, second)
	third.Messages[len(third.Messages)-1] = json.RawMessage(`{"role":"tool","tool_call_id":"call_second","content":"second result"}`)
	chat := nativeProjectionChat(t, third)
	body, reverse = nativeProjectionNormalized(t, chat, true)
	third.Parameters, err = nativeReasoningProjectionParameters(chat, body, reverse)
	require.NoError(t, err)
	both, err := engine.Restore(context.Background(), third)
	require.NoError(t, err)
	require.Len(t, both, 2)
	updated, refs, err := applyNativeReasoningProjection(body, both, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Len(t, refs, 2)
	require.Contains(t, string(updated), "opaque-synthetic-reasoning")
	require.Contains(t, string(updated), "opaque-second-reasoning")
	require.NotContains(t, string(updated), `"call_id":"fc_synthetic"`)
	// B's SourceInput contains A's original result call_id. This assertion fails
	// if call-result repairs are postponed until after B's causal check.
	require.NotContains(t, string(updated), `"call_id":"fc_second"`)
	missing, refs, err := applyNativeReasoningProjection(body, both[1:], reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Empty(t, refs)
	require.Equal(t, body, missing)
	tampered, err := sjson.SetBytes(body, "input.1.content.0.text", "edited ancestor")
	require.NoError(t, err)
	miss, refs, err := applyNativeReasoningProjection(tampered, both, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Empty(t, refs)
	require.Equal(t, tampered, miss)
}

func TestNativeReasoningReplayProjectionParametersBindUnknownSemanticsAndPrefix(t *testing.T) {
	chat := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"fixture"}],"tools":[{"type":"function","function":{"name":"python"}}],"unknown_client_option":{"value":9007199254740993},"stream":false,"stream_options":{"include_usage":false},"client_metadata":{"turn":"old"}}`)
	body := []byte(`{"model":"gpt-5.5","unknown_upstream_option":true,"client_metadata":{"turn":"old"},"input":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"synthesized"}]},{"type":"message","role":"user","content":"fixture"},{"type":"function_call","call_id":"fc_x","name":"python__custom","arguments":"{}"},{"type":"function_call_output","call_id":"fc_x","output":"result"}]}`)
	reverse := map[string]string{"python__custom": "python"}
	base, err := nativeReasoningProjectionParameters(chat, body, reverse)
	require.NoError(t, err)
	transportChat, _ := sjson.SetBytes(chat, "stream", true)
	transportChat, _ = sjson.SetBytes(transportChat, "client_metadata.turn", "new")
	transportBody, _ := sjson.SetBytes(body, "client_metadata.turn", "new")
	stable, err := nativeReasoningProjectionParameters(transportChat, transportBody, reverse)
	require.NoError(t, err)
	require.JSONEq(t, string(base), string(stable))
	for name, change := range map[string]func() ([]byte, []byte, map[string]string){
		"raw_tool_name": func() ([]byte, []byte, map[string]string) {
			x, _ := sjson.SetBytes(chat, "tools.0.function.name", "PYTHON")
			return x, body, reverse
		},
		"unknown_client": func() ([]byte, []byte, map[string]string) {
			x, _ := sjson.SetBytes(chat, "unknown_client_option.value", 2)
			return x, body, reverse
		},
		"unknown_upstream": func() ([]byte, []byte, map[string]string) {
			x, _ := sjson.SetBytes(body, "unknown_upstream_option", false)
			return chat, x, reverse
		},
		"synthesized_prompt": func() ([]byte, []byte, map[string]string) {
			x, _ := sjson.SetBytes(body, "input.0.content.0.text", "changed")
			return chat, x, reverse
		},
		"reverse_map": func() ([]byte, []byte, map[string]string) {
			return chat, body, map[string]string{"python__custom": "PYTHON"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c, b, r := change()
			changed, err := nativeReasoningProjectionParameters(c, b, r)
			require.NoError(t, err)
			require.NotEqual(t, string(base), string(changed))
		})
	}
	changedResult, _ := sjson.SetBytes(body, "input.3.output", "new result")
	stable, err = nativeReasoningProjectionParameters(chat, changedResult, reverse)
	require.NoError(t, err)
	require.JSONEq(t, string(base), string(stable), "history suffix is bound by the history fingerprint instead")
	require.Contains(t, string(base), "9007199254740993")
}

func TestNativeReasoningReplayProjectionAmbiguityAndCapacityFallback(t *testing.T) {
	_, _, _, body, replacements, reverse := nativeProjectionFixture(t)
	for _, scenario := range []string{"duplicate_call", "duplicate_result", "missing_result", "changed_fragment", "changed_anchor", "changed_mapping", "ambiguous_mapping", "output_capacity", "input_capacity", "unknown_source"} {
		t.Run(scenario, func(t *testing.T) {
			candidate := append([]byte(nil), body...)
			groups := append([]NativeReasoningReplayReplacement(nil), replacements...)
			mapping := reverse
			limits := NativeReasoningReplayLimits{}
			var input []json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(candidate, "input").Raw), &input))
			switch scenario {
			case "duplicate_call":
				input = append(input, input[len(input)-2])
				candidate, _ = sjson.SetRawBytes(candidate, "input", nativeReasoningProjectionArray(input))
			case "duplicate_result":
				input = append(input, input[len(input)-1])
				candidate, _ = sjson.SetRawBytes(candidate, "input", nativeReasoningProjectionArray(input))
			case "missing_result":
				candidate, _ = sjson.SetRawBytes(candidate, "input", nativeReasoningProjectionArray(input[:len(input)-1]))
			case "changed_fragment":
				candidate, _ = sjson.SetBytes(candidate, "input.3.arguments", `{"city":"different"}`)
			case "changed_anchor":
				candidate, _ = sjson.SetBytes(candidate, "input.4.output", "different result")
			case "changed_mapping":
				mapping = map[string]string{"python__custom": "PYTHON"}
			case "ambiguous_mapping":
				mapping = map[string]string{"python__custom": "python", "second_alias": "python"}
			case "output_capacity":
				limits.MaxEntryBytes = 128
			case "input_capacity":
				limits.MaxHistoryBytes = len(body) - 1
			case "unknown_source":
				groups[0].ExpectedSourceInputDigest = nativeReplayDigest([]byte("unknown native prefix"))
			}
			untouched := append([]byte(nil), candidate...)
			updated, refs, err := applyNativeReasoningProjection(candidate, groups, mapping, limits)
			if scenario == "ambiguous_mapping" || scenario == "output_capacity" || scenario == "input_capacity" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Empty(t, refs)
			require.Equal(t, untouched, updated)
			require.Equal(t, untouched, candidate)
		})
	}
}

func TestNativeReasoningReplayProjectionPlatformFieldsAndCustomTools(t *testing.T) {
	_, _, _, body, replacements, reverse := nativeProjectionFixture(t)
	// These are precisely the Platform-only fields removed by the production
	// Codex forwarding path, not arbitrary ignored fields in the comparator.
	groups := append([]NativeReasoningReplayReplacement(nil), replacements...)
	groups[0].Compatibility = append([]json.RawMessage(nil), groups[0].Compatibility...)
	groups[0].Compatibility[0], _ = sjson.SetBytes(groups[0].Compatibility[0], "content.0.prompt_cache_breakpoint.mode", "explicit")
	groups[0].Before, _ = sjson.SetBytes(groups[0].Before, "prompt_cache_breakpoint.mode", "explicit")
	updated, refs, err := applyNativeReasoningProjection(body, groups, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	require.NotContains(t, string(updated), "prompt_cache_breakpoint")
	// A native custom call projected to Chat's function call must regain its
	// custom result type as well as the original ID, while preserving input.
	groups[0].Output = append([]json.RawMessage(nil), groups[0].Output...)
	custom := groups[0].Output[2]
	arguments := gjson.GetBytes(custom, "arguments").String()
	custom, _ = sjson.DeleteBytes(custom, "arguments")
	custom, _ = sjson.SetBytes(custom, "type", "custom_tool_call")
	custom, _ = sjson.SetBytes(custom, "input", arguments)
	groups[0].Output[2] = custom
	updated, refs, err = applyNativeReasoningProjection(body, groups, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	input := gjson.GetBytes(updated, "input").Array()
	require.Equal(t, "custom_tool_call", input[4].Get("type").String())
	require.Equal(t, "custom_tool_call_output", input[5].Get("type").String())
	require.Equal(t, "call_synthetic", input[5].Get("call_id").String())
	require.Equal(t, "synthetic clear", input[5].Get("output").String())
}

func TestNativeReasoningReplayProjectionMultipleToolsPreserveUnrelatedBytes(t *testing.T) {
	engine, _, req, c := nativeReplayFixture(t)
	var response map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(c.Response, &response))
	var output []json.RawMessage
	require.NoError(t, json.Unmarshal(response["output"], &output))
	output = append(output, json.RawMessage(`{"type":"function_call","id":"fc_parallel","call_id":"call_parallel","name":"lookup","arguments":"{}","future_call_field":{"value":9007199254740993}}`))
	response["output"], _ = json.Marshal(output)
	c.Response, _ = json.Marshal(response)
	var visible apicompat.ResponsesResponse
	require.NoError(t, json.Unmarshal(c.Response, &visible))
	visible.Output[2].Name = "python"
	c.DeliveredAssistant, _ = json.Marshal(apicompat.ResponsesToChatCompletions(&visible, req.Scope.Model).Choices[0].Message)
	addTool := func(chat []byte) []byte {
		raw, err := sjson.SetRawBytes(chat, "tools.-1", []byte(`{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}`))
		require.NoError(t, err)
		return raw
	}
	chat := addTool(nativeProjectionChat(t, req))
	initial, reverse := nativeProjectionNormalized(t, chat, true)
	var err error
	req.Parameters, err = nativeReasoningProjectionParameters(chat, initial, reverse)
	require.NoError(t, err)
	c.SourceInput = json.RawMessage(gjson.GetBytes(initial, "input").Raw)
	require.NoError(t, engine.Commit(context.Background(), req, c))
	next := nativeReplayContinuation(req, c)
	next.Messages = append(next.Messages, json.RawMessage(`{"role":"tool","tool_call_id":"call_parallel","content":"parallel result"}`))
	next.Messages[3], next.Messages[4] = next.Messages[4], next.Messages[3]
	chat = addTool(nativeProjectionChat(t, next))
	body, reverse := nativeProjectionNormalized(t, chat, true)
	next.Parameters, err = nativeReasoningProjectionParameters(chat, body, reverse)
	require.NoError(t, err)
	groups, err := engine.Restore(context.Background(), next)
	require.NoError(t, err)
	require.Len(t, groups, 1)
	secondResult := []byte(`{"type":"function_call_output","call_id":"fc_synthetic","output":"synthetic clear","future_numeric":9007199254740993,"layout" : [ 1, 2 ]}`)
	body, err = sjson.SetRawBytes(body, "input.6", secondResult)
	require.NoError(t, err)
	var pretty bytes.Buffer
	require.NoError(t, json.Indent(&pretty, []byte(gjson.GetBytes(body, "input.1").Raw), "", " "))
	body, err = sjson.SetRawBytes(body, "input.1", pretty.Bytes())
	require.NoError(t, err)
	originalUser := gjson.GetBytes(body, "input.1").Raw
	updated, refs, err := applyNativeReasoningProjection(body, groups, reverse, NativeReasoningReplayLimits{})
	require.NoError(t, err)
	require.Len(t, refs, 1)
	input := gjson.GetBytes(updated, "input").Array()
	require.Equal(t, originalUser, input[1].Raw)
	require.Equal(t, "call_parallel", input[6].Get("call_id").String())
	require.Equal(t, "parallel result", input[6].Get("output").String())
	expectedResult, _ := sjson.SetBytes(secondResult, "call_id", "call_synthetic")
	require.Equal(t, string(expectedResult), input[7].Raw, "only the paired result ID may change")
	require.Contains(t, string(updated), "future_call_field")
	require.Contains(t, string(updated), "9007199254740993")
}

func TestNativeReasoningReplayProjectionRestoredInputBudget(t *testing.T) {
	_, _, _, body, groups, reverse := nativeProjectionFixture(t)
	groups[0].Output = append([]json.RawMessage(nil), groups[0].Output...)
	groups[0].Output[0], _ = sjson.SetBytes(groups[0].Output[0], "future_large_field", strings.Repeat("x", 2048))
	limits := NativeReasoningReplayLimits{MaxHistoryBytes: len(body) + 256}
	updated, refs, err := applyNativeReasoningProjection(body, groups, reverse, limits)
	require.ErrorIs(t, err, ErrNativeReasoningReplayCapacity)
	require.Empty(t, refs)
	require.Equal(t, body, updated)
}
