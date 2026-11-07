package service

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeReasoningReplayIdentityCompleteOpaqueAndLegacyBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		id, encrypted, status any
		keep                  bool
	}{
		{"complete", "rs_native", "opaque", "completed", true},
		{"optional_status", "rs_native", "opaque", nil, true},
		{"future_prefix", "future_native", "opaque", nil, true},
		{"long_server_id", "rs_" + strings.Repeat("a", 80), "opaque", nil, true},
		{"legacy", "legacy", "opaque", nil, false},
		{"empty_prefix", "_suffix", "opaque", nil, false},
		{"empty_suffix", "rs_", "opaque", nil, false},
		{"bare", "rs_native", nil, nil, false},
		{"empty_cipher", "rs_native", "   ", nil, false},
		{"invalid_cipher_type", "rs_native", 42, nil, false},
		{"incomplete", "rs_native", "opaque", "incomplete", false},
		{"failed", "rs_native", "opaque", "failed", false},
		{"in_progress", "rs_native", "opaque", "in_progress", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.keep, keepOpenAIReasoningReplayID(tc.id, tc.encrypted, tc.status))
			item := map[string]any{"type": "reasoning", "id": tc.id, "encrypted_content": tc.encrypted, "status": tc.status, "summary": []any{}}
			for _, preserve := range []bool{false, true} {
				got := filterCodexInput([]any{item}, preserve)[0].(map[string]any)
				_, hasID := got["id"]
				require.Equal(t, tc.keep, hasID)
				require.Equal(t, tc.id, item["id"], "caller input must not be mutated")
			}
			body, _ := json.Marshal(map[string]any{"store": false, "input": []any{item}})
			for _, normalize := range []func([]byte, bool) ([]byte, bool, error){normalizeOpenAIAPIKeyStoreFalseReasoningReplay, normalizeOpenAIAPIKeyStoreFalseReasoningReplayDecoded} {
				out, _, err := normalize(body, false)
				require.NoError(t, err)
				require.Equal(t, tc.keep, gjson.GetBytes(out, "input.0.id").Exists())
			}
		})
	}
}

func TestNativeReasoningReplayIdentityNativeForwardPreservesPrefixedOpaque(t *testing.T) {
	for _, kind := range []string{"oauth", "api_key"} {
		for _, id := range []string{"rs_native", "future_native", "rs_" + strings.Repeat("n", 80)} {
			t.Run(kind+"/"+id[:min(12, len(id))], func(t *testing.T) {
				body, _ := json.Marshal(map[string]any{"model": "gpt-5.5", "stream": false, "store": false, "instructions": "synthetic identity regression", "input": []any{
					map[string]any{"type": "reasoning", "id": id, "status": "completed", "summary": []any{}, "encrypted_content": "opaque-native-synthetic", "future": json.Number("9007199254740993")},
					map[string]any{"type": "message", "role": "user", "content": "continue synthetic fixture"},
				}})
				upstream := &httpUpstreamRecorder{responses: []*http.Response{newOpenAIRejectedFieldTestResponse(http.StatusOK, namespaceForwardOKResponse)}}
				account := newOpenAIRejectedFieldTestAccount()
				if kind == "oauth" {
					account = newOpenAIOAuthNamespaceTestAccount()
				}
				result, err := newOpenAIRejectedFieldTestService(upstream).Forward(context.Background(), newOpenAIRejectedFieldTestContext(body), account, body)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				var reasoning gjson.Result
				for _, item := range gjson.GetBytes(upstream.bodies[0], "input").Array() {
					if item.Get("type").String() == "reasoning" {
						reasoning = item
						break
					}
				}
				require.True(t, reasoning.Exists())
				require.Equal(t, id, reasoning.Get("id").String())
				require.Equal(t, "opaque-native-synthetic", reasoning.Get("encrypted_content").String())
				require.Equal(t, "9007199254740993", reasoning.Get("future").Raw)
			})
		}
	}
}
