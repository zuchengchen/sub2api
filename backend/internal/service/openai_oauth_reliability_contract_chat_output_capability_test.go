//go:build unit

package service

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// These are documented Responses declarations, not speculative tool-name
// matching. Their client output or image payload has no native Chat carrier.
func TestOAuthReliabilityContractChatResponsesShapeRejectsUnrepresentableTools(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, tool, param string }{
			{"apply_patch", `{"type":"apply_patch"}`, "tools[0].type"},
			{"computer", `{"type":"computer"}`, "tools[0].type"},
			{"computer_preview", `{"type":"computer_use_preview","display_width":1024,"display_height":768,"environment":"browser"}`, "tools[0].type"},
			{"local_shell", `{"type":"local_shell"}`, "tools[0].type"},
			{"shell_local", `{"type":"shell","environment":{"type":"local"}}`, "tools[0].type"},
			{"image_generation", `{"type":"image_generation"}`, "tools[0].type"},
			{"namespaced_custom", `{"type":"namespace","name":"PRIVATE_NAMESPACE","tools":[{"type":"custom","name":"patch","format":{"type":"text"}}]}`, "tools[0].tools[0].type"},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[{"type":"message","role":"user","content":"fixture"}],"tools":[%s]}`, stream, tc.tool))
				upstream := &httpUpstreamRecorder{}
				c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
				result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
				require.Error(t, err)
				require.Nil(t, result)
				require.Empty(t, upstream.bodies)
				require.Equal(t, http.StatusBadRequest, recorder.Code)
				require.Equal(t, "invalid_request_error", gjson.Get(recorder.Body.String(), "error.type").String())
				require.Equal(t, tc.param, gjson.Get(recorder.Body.String(), "error.param").String())
				require.True(t, IsResponseCommitted(c))
				require.NotContains(t, recorder.Body.String(), "PRIVATE")
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
			})
		}
	}
}

func TestOAuthReliabilityContractChatHostedAndUnknownShellExecutionPreserved(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct{ name, tool string }{
			{"web_search", `{"type":"web_search"}`},
			{"file_search", `{"type":"file_search","vector_store_ids":["vs_synthetic"]}`},
			{"shell_hosted", `{"type":"shell","environment":{"type":"container_auto"}}`},
			{"shell_unspecified", `{"type":"shell"}`},
			{"shell_unknown", `{"type":"shell","environment":{"type":"future_execution"}}`},
		} {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, tc.name), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[{"type":"message","role":"user","content":"fixture"}],"tools":[%s]}`, stream, tc.tool))
				upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
				c, recorder := oauthReliabilityContractContext("/v1/chat/completions", body)
				result, err := newOpenAIRejectedFieldTestService(upstream).ForwardAsChatCompletions(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body, "", "")
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Len(t, upstream.bodies, 1)
				require.JSONEq(t, tc.tool, gjson.GetBytes(upstream.lastBody, "tools.0").Raw)
				require.Contains(t, recorder.Body.String(), "contract answer")
			})
		}
	}
}

func TestOAuthReliabilityContractNativeResponsesKeepsClientTools(t *testing.T) {
	for _, tool := range []string{
		`{"type":"apply_patch"}`,
		`{"type":"computer_use_preview","display_width":1024,"display_height":768,"environment":"browser"}`,
		`{"type":"shell","environment":{"type":"local"}}`,
		`{"type":"namespace","name":"fixture","tools":[{"type":"custom","name":"patch","format":{"type":"text"}}]}`,
	} {
		t.Run(gjson.Get(tool, "type").String(), func(t *testing.T) {
			body := []byte(`{"model":"gpt-6-astra","input":[{"type":"message","role":"user","content":"fixture"}],"tools":[` + tool + `]}`)
			upstream := &httpUpstreamRecorder{resp: oauthReliabilityContractSSE(oauthReliabilityContractCompleted)}
			c, _ := oauthReliabilityContractContext("/v1/responses", body)
			result, err := newOpenAIRejectedFieldTestService(upstream).Forward(c.Request.Context(), c, newOpenAIOAuthNamespaceTestAccount(), body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.Len(t, upstream.bodies, 1)
			require.JSONEq(t, tool, gjson.GetBytes(upstream.lastBody, "tools.0").Raw)
		})
	}
}
