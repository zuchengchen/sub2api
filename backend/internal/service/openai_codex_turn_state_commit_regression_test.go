package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCodexOAuthTurnStatePassthroughCommit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, abandoned := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/abandoned=%t", stream, abandoned), func(t *testing.T) {
				response := `{"type":"response.completed","response":{"id":"resp_state","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}}`
				if abandoned {
					response = `{"type":"response.failed","response":{"id":"resp_abandoned","status":"failed","error":{"code":"server_error","message":"upstream unavailable"}}}`
				}
				upstream := &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {"issued-state"}},
					Body:       io.NopCloser(strings.NewReader("data: " + response + "\n\n")),
				}}
				svc := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: &config.Config{}, httpUpstream: upstream}
				account := &Account{ID: 710, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					Concurrency: 1, Status: StatusActive, Schedulable: true, RateMultiplier: f64p(1),
					Credentials: map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": "synthetic-account"},
					Extra:       astraHealthyTestExtra(map[string]any{"openai_passthrough": true, "codex_fingerprint_mode": "off"}),
				}
				body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","stream":%t,"input":[{"role":"user","content":"hello"}]}`, stream))
				c, rec := newTurnStateTestContext(t, 71, "state-commit-session")
				c.Request.Header.Set("User-Agent", "codex_cli_rs/0.168.0")
				SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
				_, err := svc.Forward(context.Background(), c, account, body)
				if abandoned {
					require.Error(t, err)
					require.False(t, c.Writer.Written(), "failure must remain eligible for failover")
				} else {
					require.NoError(t, err)
					require.Equal(t, "issued-state", rec.Result().Header.Get(openAICodexTurnStateHeader))
				}
				echo := http.Header{}
				echo.Set(openAICodexTurnStateHeader, "issued-state")
				svc.guardOpenAICodexTurnStateEcho(c, &Account{ID: 711}, echo)
				if abandoned {
					require.Equal(t, "issued-state", echo.Get(openAICodexTurnStateHeader), "an uncommitted upstream response must not create provenance")
				} else {
					require.Empty(t, echo.Get(openAICodexTurnStateHeader), "committed state must not move to another account")
				}
			})
		}
	}
}
