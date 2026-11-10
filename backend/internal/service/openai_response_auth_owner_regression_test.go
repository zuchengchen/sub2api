package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	coderws "github.com/coder/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Regression expectation: a response issued for a prior OAuth owner must not be
// forwarded with the new owner's bearer merely because the local row and site
// user are unchanged. All upstream responses are synthetic in-process fixtures.
func TestOAuthPassthroughPreviousResponseDoesNotCrossReauthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const responseID = "resp_before_reauthorization"
	upstream := &httpUpstreamRecorder{responses: []*http.Response{}}
	for _, id := range []string{responseID, "resp_after_reauthorization"} {
		upstream.responses = append(upstream.responses, &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\ndata: [DONE]\n\n", id)))})
	}
	svc := &OpenAIGatewayService{tiboRouteDisabled: true, cfg: &config.Config{}, httpUpstream: upstream, openaiWSStateStore: NewOpenAIWSStateStore(nil)}
	account := &Account{ID: 988, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1, Credentials: map[string]any{"access_token": "owner-a-access", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner-a"}, Extra: astraHealthyTestExtra(map[string]any{"openai_passthrough": true, "openai_oauth_responses_websockets_v2_mode": OpenAIWSIngressModeOff})}
	request := func(body []byte) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		SetOpenAIHTTPResponseOwner(c, 101, 201)
		return c
	}
	first := []byte(`{"model":"gpt-5.2","stream":false,"instructions":"synthetic regression","input":[{"role":"user","content":"first"}]}`)
	result, err := svc.Forward(context.Background(), request(first), account, first)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, responseID, result.ResponseID)
	owned, err := svc.ValidateOpenAIHTTPResponseOwner(context.Background(), 0, responseID, 101, 201)
	require.NoError(t, err)
	require.True(t, owned, "the authenticated downstream owner is unchanged")
	account.Credentials = map[string]any{"access_token": "owner-b-access", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner-b"}
	second := []byte(`{"model":"gpt-5.2","stream":false,"instructions":"synthetic regression","previous_response_id":"resp_before_reauthorization","input":[{"role":"user","content":"next"}]}`)
	_, _ = svc.Forward(context.Background(), request(second), account, second)
	if upstream.lastReq != nil && upstream.lastReq.Header.Get("Authorization") == "Bearer owner-b-access" {
		require.NotEqual(t, responseID, gjson.GetBytes(upstream.lastBody, "previous_response_id").String(), "old response reference reached the new OAuth owner's HTTP request")
	}
}

func TestOAuthExplicitWSPreviousResponseDoesNotCrossReauthorization(t *testing.T) {
	for _, mode := range []string{"http_to_ws", OpenAIWSIngressModeCtxPool, OpenAIWSIngressModePassthrough} {
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
			account.Credentials = map[string]any{"access_token": "owner-b-access", "chatgpt_account_id": "workspace", "chatgpt_user_id": "owner-b"}
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
			for _, frame := range capture.writes {
				require.NotEqual(t, "resp_known_owner_a", openAIWSPayloadString(frame, "previous_response_id"), "old response origin must be checked even when a new pool connection is selected")
			}
		})
	}
}
