package service

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCodexOAuthTurnStateAcrossHTTPAndWebSocket(t *testing.T) {
	for _, mode := range []string{OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			conn := &openAIWSCaptureConn{events: [][]byte{
				[]byte(`{"type":"response.metadata","headers":{"X-Codex-Turn-State":["ws-issued-state"]}}`),
				[]byte(`{"type":"response.completed","response":{"id":"resp_state","output":[]}}`),
			}}
			dialer := &openAIWSCaptureDialer{conn: conn}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector()}
			account := &Account{ID: 745, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1,
				Credentials: map[string]any{"access_token": "synthetic-token", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner"},
				Extra:       astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": mode, "codex_fingerprint_mode": "off"})}
			origin, _ := newTurnStateTestContext(t, 0, "cross-transport-session")
			svc.relayOpenAICodexTurnState(origin, &Account{ID: 746}, http.Header{"X-Codex-Turn-State": {"http-issued-state"}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
			defer server.Close()
			headers := http.Header{"User-Agent": {"codex_cli_rs/0.168.0"}, "Session-Id": {"cross-transport-session"}}
			client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: headers})
			require.NoError(t, err)
			defer func() { _ = client.CloseNow() }()
			require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.5","input":[{"role":"user","content":"preserve me"}],"client_metadata":{"x-codex-turn-state":"http-issued-state","other":"unchanged"}}`)))
			sawState := false
			for {
				_, event, readErr := client.Read(ctx)
				require.NoError(t, readErr)
				kind := gjson.GetBytes(event, "type").String()
				sawState = sawState || kind == "response.metadata"
				if kind == "response.completed" {
					break
				}
			}
			require.True(t, sawState, "state must actually reach the client before provenance is checked")
			_ = client.Close(coderws.StatusNormalClosure, "done")
			select {
			case <-serverErr:
			case <-ctx.Done():
				t.Fatal("native relay did not stop")
			}
			require.Len(t, conn.writes, 1)
			frame := requestToJSONString(conn.writes[0])
			require.False(t, gjson.Get(frame, "client_metadata."+openAICodexTurnStateHeader).Exists(), "HTTP state cannot be replayed in another account's native WS frame")
			require.Equal(t, "unchanged", gjson.Get(frame, "client_metadata.other").String())
			require.Contains(t, frame, "preserve me")
			echo := http.Header{"X-Codex-Turn-State": {"ws-issued-state"}}
			svc.guardOpenAICodexTurnStateEcho(origin, &Account{ID: 746}, echo)
			require.Empty(t, echo.Get(openAICodexTurnStateHeader), "delivered WS state cannot return over another account's HTTP request")
		})
	}
}
