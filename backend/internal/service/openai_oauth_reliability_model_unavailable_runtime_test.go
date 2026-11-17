//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOAuthReliabilityModelUnavailablePassthroughPoolCannotReplayProgress(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		for _, status := range []int{http.StatusBadRequest, http.StatusBadGateway} {
			for _, progress := range []string{
				`"usage":{"input_tokens":1}`,
				`"usage":{"output_tokens":1}`,
				`"output":[{"type":"message","content":[{"type":"output_text","text":"already generated"}]}]`,
				`"output":[{"type":"function_call","call_id":"call_once","name":"work","arguments":"{}"}]`,
				`"tool_usage":{"web_search":{"count":1}}`,
			} {
				t.Run(fmt.Sprintf("oauth=%t/status=%d/%s", oauth, status, progress), func(t *testing.T) {
					body := strings.TrimSuffix(oauthReliabilityModelUnavailableStructured, "}") + "," + progress + "}"
					svc, account, c, recorder, upstream := oauthReliabilityModelUnavailableForwardFixture(t, oauth, true, newOpenAIRejectedFieldTestResponse(status, body))
					require.True(t, account.IsPoolModeRetryableStatus(status))
					require.False(t, shouldFailoverOpenAIPassthroughResponse(account, status, []byte(body)))
					result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4-mini","instructions":"test","input":"fixture","stream":false}`))
					inputTokens := int(gjson.Get(body, "usage.input_tokens").Int())
					outputTokens := int(gjson.Get(body, "usage.output_tokens").Int())
					if inputTokens > 0 || outputTokens > 0 {
						require.NotNil(t, result, "actual usage must remain available for one settlement")
						require.Equal(t, inputTokens, result.Usage.InputTokens)
						require.Equal(t, outputTokens, result.Usage.OutputTokens)
					} else {
						require.Nil(t, result, "tool/output progress alone does not fabricate token usage")
					}
					require.Error(t, err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(err, &failover), "model rejection cannot authorize a replay after real work")
					require.Len(t, upstream.bodies, 1)
					require.Equal(t, http.StatusBadRequest, recorder.Code)
					require.Equal(t, "model_not_found", gjson.Get(recorder.Body.String(), "error.code").String())
					require.Equal(t, "model", gjson.Get(recorder.Body.String(), "error.param").String())
					require.Equal(t, status, c.GetInt(OpsUpstreamStatusCodeKey))
				})
			}
		}
	}
}

func TestOAuthReliabilityModelUnavailablePassthroughZeroUsageKeepsExistingBudget(t *testing.T) {
	for _, extra := range []string{"", `,"usage":{"input_tokens":0,"output_tokens":0}`, `,"output":[]`, `,"metadata":{"usage_estimate":999}`} {
		for _, status := range []int{400, 502} {
			t.Run(fmt.Sprintf("status=%d/%s", status, extra), func(t *testing.T) {
				body := strings.TrimSuffix(oauthReliabilityModelUnavailableStructured, "}") + extra + "}"
				svc, account, c, _, upstream := oauthReliabilityModelUnavailableForwardFixture(t, true, true, newOpenAIRejectedFieldTestResponse(status, body))
				result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-5.4-mini","instructions":"test","input":"fixture","stream":false}`))
				require.Nil(t, result)
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.True(t, IsOpenAIModelUnavailableFailover(failover))
				require.False(t, failover.RetryableOnSameAccount)
				require.True(t, failover.ShouldRetryNextAccount())
				require.Len(t, upstream.bodies, 1)
				require.False(t, c.Writer.Written())
			})
		}
	}
}

func TestOAuthReliabilityModelUnavailableStreamProgressRemainsTerminal(t *testing.T) {
	for _, route := range []string{"responses", "passthrough", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", route, stream), func(t *testing.T) {
				wire := oauthReliabilityContractSSE(
					`{"type":"response.in_progress","response":{"id":"resp_metered_model","usage":{"input_tokens":11,"output_tokens":3}}}`,
					`{"type":"response.failed","response":{"id":"resp_metered_model","model":"gpt-5.6-sol","status":"failed","output":[],"error":{"type":"invalid_request_error","code":"model_not_found","param":"model","message":"model unavailable"},"usage":{"input_tokens":0,"output_tokens":0}}}`,
				)
				upstream := &httpUpstreamRecorder{resp: wire}
				svc := newOpenAIRejectedFieldTestService(upstream)
				svc.accountRepo = &modelNotFoundManagedAccountRepo{}
				account := newOpenAIOAuthNamespaceTestAccount()
				account.Extra["openai_passthrough"] = route == "passthrough"
				account.Credentials["pool_mode"] = true
				account.Credentials["pool_mode_retry_status_codes"] = []any{float64(400), float64(502)}
				request := oauthReliabilityContractBody(route, stream)
				c, _ := oauthReliabilityContractContext("/v1/"+route, request)
				result, err := oauthReliabilityContractForward(svc, c, account, route, request)
				require.Error(t, err)
				var failover *UpstreamFailoverError
				require.False(t, errors.As(err, &failover))
				require.Len(t, upstream.bodies, 1)
				require.NotNil(t, result)
				require.Equal(t, 11, result.Usage.InputTokens)
				require.Equal(t, 3, result.Usage.OutputTokens)
			})
		}
	}
}
