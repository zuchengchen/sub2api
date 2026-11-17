//go:build unit

package service

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A complete data line followed by silence is a real network shape. Close
// unblocks the reader; no sleep manufactures the timeout or a later terminal.
type oauthReliabilityIdleBody struct {
	reader *strings.Reader
	closed chan struct{}
	once   sync.Once
}

func (r *oauthReliabilityIdleBody) Read(p []byte) (int, error) {
	if n, err := r.reader.Read(p); n > 0 || err != io.EOF {
		return n, err
	}
	<-r.closed
	return 0, io.EOF
}
func (r *oauthReliabilityIdleBody) Close() error { r.once.Do(func() { close(r.closed) }); return nil }

func TestOAuthReliabilityContractCompatIdleTailRetainsActualUsage(t *testing.T) {
	for _, route := range []string{"chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, pending := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/pending_error=%t", route, stream, pending), func(t *testing.T) {
					wire := ""
					if pending {
						wire = "data: {\"type\":\"error\",\"error\":" + oauthReliabilityContractSchemaError + "}\n\n"
					}
					wire += `data: {"type":"response.in_progress","response":{"id":"resp_idle_contract","usage":{"input_tokens":11,"output_tokens":3,"input_tokens_details":{"cached_tokens":2}}}}` + "\n"
					reader := &oauthReliabilityIdleBody{reader: strings.NewReader(wire), closed: make(chan struct{})}
					defer reader.Close()
					response := oauthReliabilityContractSSE()
					response.Body = reader
					upstream := &httpUpstreamRecorder{responses: []*http.Response{response}}
					svc := newOpenAIRejectedFieldTestService(upstream)
					svc.cfg.Gateway.StreamDataIntervalTimeout = 1
					svc.cfg.Gateway.CodexWire.StreamIdleTimeoutSeconds = 0
					svc.cfg.Gateway.StreamKeepaliveInterval = 0
					body := oauthReliabilityContractBody(route, stream)
					c, recorder := oauthReliabilityContractContext("/v1/"+route, body)
					type outcome struct {
						result *OpenAIForwardResult
						err    error
					}
					done := make(chan outcome, 1)
					go func() {
						result, err := oauthReliabilityContractForward(svc, c, newOpenAIOAuthNamespaceTestAccount(), route, body)
						done <- outcome{result, err}
					}()
					var out outcome
					select {
					case out = <-done:
					case <-time.After(5 * time.Second):
						_ = reader.Close()
						<-done
						t.Fatal("configured idle boundary did not finish the actual forward")
					}
					require.Error(t, out.err)
					var failover *UpstreamFailoverError
					require.False(t, errors.As(out.err, &failover), "consumed metered work cannot become a new attempt")
					require.Len(t, upstream.bodies, 1)
					require.NotNil(t, out.result, "idle timeout must retain complete consumed usage before settling the bare rejection")
					require.Equal(t, 11, out.result.Usage.InputTokens)
					require.Equal(t, 3, out.result.Usage.OutputTokens)
					require.Equal(t, 2, out.result.Usage.CacheReadInputTokens)
					if pending {
						require.Equal(t, http.StatusBadRequest, recorder.Code)
						require.Contains(t, recorder.Body.String(), "invalid_request_error")
					}
					require.NotContains(t, recorder.Body.String(), "contract answer")
				})
			}
		}
	}
}
