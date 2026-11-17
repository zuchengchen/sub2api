package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityNamespaceMappedOutputIdentity(t *testing.T) {
	call := map[string]any{"type": "function_call", "call_id": "call_inline", "namespace": "files", "name": "read", "arguments": "{}"}
	output := map[string]any{"type": "function_call_output", "call_id": "call_inline", "namespace": "files", "name": "read", "output": "opaque result"}
	clone := func(value map[string]any, key string, replacement any) map[string]any {
		copy := make(map[string]any, len(value))
		for k, v := range value {
			copy[k] = v
		}
		if replacement == nil {
			delete(copy, key)
		} else {
			copy[key] = replacement
		}
		return copy
	}
	for _, tc := range []struct {
		name         string
		input        []any
		outputIndex  int
		wantMapped   bool
		nameOptional bool
	}{
		{"matched", []any{call, output}, 1, true, false},
		{"optional_name", []any{call, clone(output, "name", nil)}, 1, true, true},
		{"optional_namespace", []any{call, clone(output, "namespace", nil)}, 1, true, false},
		{"no_inline_call", []any{output}, 0, false, false},
		{"duplicate_call", []any{call, clone(call, "name", "write"), output}, 2, false, false},
		{"unknown_identity_collision", []any{call, map[string]any{"type": "future_call", "call_id": "call_inline"}, output}, 2, false, false},
		{"wrong_namespace", []any{call, clone(output, "namespace", "other")}, 1, false, false},
		{"wrong_name", []any{call, clone(output, "name", "write")}, 1, false, false},
		{"wrong_call_id", []any{call, clone(output, "call_id", "call_other")}, 1, false, false},
		{"result_precedes_call", []any{output, call}, 0, false, false},
		{"unmapped_call", []any{clone(call, "name", "unmapped"), output}, 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := json.Marshal(map[string]any{
				"tools": []any{map[string]any{"type": "namespace", "name": "files", "tools": []any{
					map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}},
					map[string]any{"type": "function", "name": "write", "parameters": map[string]any{"type": "object"}},
				}}},
				"input": tc.input,
			})
			require.NoError(t, err)
			result, err := flattenOpenAIResponsesNamespaces(nil, body)
			require.NoError(t, err)
			got := gjson.GetBytes(result, "input").Array()[tc.outputIndex]
			if !tc.wantMapped {
				require.JSONEq(t, gjson.GetBytes(body, "input").Array()[tc.outputIndex].Raw, got.Raw)
				return
			}
			require.False(t, got.Get("namespace").Exists())
			if tc.nameOptional {
				require.False(t, got.Get("name").Exists())
			} else {
				require.Equal(t, "files__read", got.Get("name").String())
			}
			require.Equal(t, "call_inline", got.Get("call_id").String())
			require.Equal(t, "opaque result", got.Get("output").String())
		})
	}
}
