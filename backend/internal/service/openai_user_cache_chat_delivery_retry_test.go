package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the real Forward seam on one inbound context, as the handler does
// for pre-output SSE account failover. Transport bodies are synthetic and no
// network or upstream credentials are used.
func TestGPT61SolChatCacheDeliveryDiagnosticFourAttempts(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%v", stream), func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			failed := "data: " + `{"type":"response.failed","error":{"type":"server_error","message":"synthetic temporary upstream failure"}}` + "\n\n"
			complete := "data: " + `{"type":"response.created","response":{"id":"retry_response","model":"gpt-6.1-sol","status":"in_progress"}}` + "\n\n" +
				"data: " + `{"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"retry answer"}` + "\n\n" +
				"data: " + `{"type":"response.completed","response":{"id":"retry_response","model":"gpt-6.1-sol","status":"completed","output":[{"type":"message","id":"retry_message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"retry answer","annotations":[]}]}],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":90},"output_tokens":2,"total_tokens":102}}}` + "\n\ndata: [DONE]\n\n"
			upstream := &httpUpstreamSequenceRecorder{}
			for attempt := 1; attempt <= 4; attempt++ {
				wire := failed
				if attempt == 4 {
					wire = complete
				}
				upstream.responses = append(upstream.responses, &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))})
			}
			svc := &OpenAIGatewayService{cfg: codexSynthEnabledConfig(), httpUpstream: upstream, tiboRouteDisabled: true}
			c := userCacheDiagnosticTestContext(428)
			originalWriter := c.Writer
			body := []byte(fmt.Sprintf(`{"model":"gpt-6.1-sol","messages":[{"role":"user","content":"synthetic"}],"stream":%t}`, stream))
			for attempt := 1; attempt <= 4; attempt++ {
				result, err := svc.ForwardAsChatCompletions(context.Background(), c, newCodexSynthTestAccount(int64(23150+attempt), AccountTypeOAuth), body, "", "")
				require.Same(t, originalWriter, c.Writer, "each attempt restores the outer writer")
				if attempt < 4 {
					var failover *UpstreamFailoverError
					require.ErrorAs(t, err, &failover)
					require.Nil(t, result)
					require.False(t, c.Writer.Written(), "failed attempt must leave the handler able to retry")
				} else {
					require.NoError(t, err)
					require.NotNil(t, result)
					require.Equal(t, 100, result.Usage.InputTokens)
				}
			}
			events := userCacheDiagnosticReadEvents(t, path)
			byStage := map[string]map[int]map[string]any{}
			for _, event := range events {
				stage := event["stage"].(string)
				if byStage[stage] == nil {
					byStage[stage] = map[int]map[string]any{}
				}
				byStage[stage][int(event["attempt"].(float64))] = event
			}
			final := byStage["chat_delivery"][4]
			require.NotNil(t, final, "three failed attempts must not exhaust the final delivery record")
			require.Equal(t, true, final["forward_returned_success"])
			require.Equal(t, true, final["downstream_delivered_complete"])
			require.EqualValues(t, 23154, final["account_id"])
			require.Equal(t, byStage["upstream_response"][4]["response_id_sha256"], final["consumer_response_id_sha256"])
			for attempt := 1; attempt <= 3; attempt++ {
				require.Equal(t, false, byStage["chat_delivery"][attempt]["forward_returned_success"])
				require.Equal(t, false, byStage["chat_delivery"][attempt]["downstream_delivered_complete"])
				require.Equal(t, "response.failed", byStage["upstream_response"][attempt]["terminal"])
			}
			require.Len(t, events, 13)
			require.LessOrEqual(t, len(events), userCacheDiagnosticMaxRecords)
		})
	}
}
