package service

import (
	"context"
	"encoding/json"
	coderws "github.com/coder/websocket"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"
)

func TestOAuthExplicitWSPreviousResponsePreservesSameOwnerRefresh(t *testing.T) {
	for _, mode := range []string{"http_to_ws", OpenAIWSIngressModePassthrough} {
		t.Run(mode, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			capture := &openAIWSCaptureConn{events: [][]byte{[]byte(`{"type":"response.completed","response":{"id":"resp_unexpected","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)}}
			dialer := &openAIWSCaptureDialer{conn: capture}
			pool := newOpenAIWSConnPool(cfg)
			pool.setClientDialerForTest(dialer)
			defer pool.Close()
			svc := &OpenAIGatewayService{cfg: cfg, openaiWSPool: pool, openaiWSPassthroughDialer: dialer, toolCorrector: NewCodexToolCorrector(), tiboRouteDisabled: true}
			extra := astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": true, "openai_oauth_responses_websockets_v2_mode": mode, "codex_fingerprint_mode": "off"})
			account := &Account{ID: 989, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Concurrency: 1, Status: StatusActive, Schedulable: true, Extra: extra, Credentials: map[string]any{"access_token": "owner-a-access", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner-a"}}
			origin := newOpenAIWSExecutionScopeTestContext(nil)
			svc.bindHTTPResponseAccount(context.Background(), origin, account, "resp_known_owner_a")
			account.Credentials = map[string]any{"access_token": "owner-b-access", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner-a"}
			raw := []byte(`{"type":"response.create","model":"gpt-5.2","previous_response_id":"resp_known_owner_a","input":[{"role":"user","content":"next"}]}`)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if mode == "http_to_ws" {
				payload := map[string]any{}
				require.NoError(t, json.Unmarshal(raw, &payload))
				_, _ = svc.forwardOpenAIWSV2(ctx, newOpenAIWSExecutionScopeTestContext(nil), account, payload, raw, "", "synthetic-thread", "owner-b-access", OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2}, false, false, "gpt-5.2", "gpt-5.2", time.Now(), 1, "", nil)
			} else {
				server, serverErr := startPassthroughLifecycleServer(t, ctx, svc, account)
				defer server.Close()
				client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
				require.NoError(t, err)
				require.NoError(t, client.Write(ctx, coderws.MessageText, raw))
				_, _, _ = client.Read(ctx)
				_ = client.CloseNow()
				select {
				case <-serverErr:
				case <-ctx.Done():
					t.Fatal("websocket relay did not stop")
				}
			}
			capture.mu.Lock()
			defer capture.mu.Unlock()
			require.NotEmpty(t, capture.writes)
			for _, frame := range capture.writes {
				require.Equal(t, "resp_known_owner_a", openAIWSPayloadString(frame, "previous_response_id"), "same owner refresh retains native explicit continuation")
			}
		})
	}
}
