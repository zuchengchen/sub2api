//go:build unit

package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayAudit_NewResponsesFailurePathKeepsOnlyActualUsage(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			input   int
			success bool
		}{
			{"unmetered_read_failure", 0, false},
			{"partially_metered_read_failure", 8, false},
			{"unmetered_success_preserves_estimation", 0, true},
		} {
			t.Run(fmt.Sprintf("passthrough=%t/%s", passthrough, tc.name), func(t *testing.T) {
				wire := ""
				if tc.input > 0 {
					wire = fmt.Sprintf("data: "+`{"type":"response.in_progress","response":{"id":"resp_synthetic_raw_usage","usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n\n", tc.input)
				}
				wire += "data: " + `{"type":"response.output_text.delta","item_id":"msg_synthetic","output_index":0,"content_index":0,"delta":"Synthetic output has enough words to estimate tokens."}` + "\n\n"
				var reader io.Reader
				if tc.success {
					wire += "data: " + `{"type":"response.completed","response":{"id":"resp_synthetic_raw_usage","status":"completed","output":[]}}` + "\n\n"
					reader = strings.NewReader(wire)
				} else {
					reader = io.MultiReader(strings.NewReader(wire), gatewayAuditErrorReader{io.ErrUnexpectedEOF})
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(reader)}}
				svc := newOpenAIImageGenerationControlTestService(upstream)
				c, _ := newOpenAIImageGenerationControlTestContext(true, "synthetic-audit")
				account := newOpenAIImageGenerationControlTestAccount()
				account.Extra = map[string]any{"openai_passthrough": passthrough}
				result, err := svc.Forward(c.Request.Context(), c, account, []byte(`{"model":"gpt-5.5","stream":true,"input":"Synthetic prompt for the existing estimator."}`))
				if tc.success {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				var retry *UpstreamFailoverError
				require.False(t, errors.As(err, &retry), "observed delta forbids replay whether usage was reported or not")
				require.Len(t, upstream.requests, 1)
				if !tc.success && tc.input == 0 {
					require.Nil(t, result, "new failure path cannot settle estimated tokens as actual usage")
					return
				}
				require.NotNil(t, result)
				if tc.success {
					require.Greater(t, result.Usage.InputTokens, 0)
					require.Greater(t, result.Usage.OutputTokens, 0)
				} else {
					require.Equal(t, 8, result.Usage.InputTokens)
					require.Zero(t, result.Usage.OutputTokens, "do not fill unreported fields with estimates")
					require.Zero(t, result.Usage.CacheReadInputTokens)
				}
			})
		}
	}
}
