package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	wsHTTPContinuationGroupID = 37
	wsHTTPContinuationUserID  = 101
	wsHTTPContinuationKeyID   = 201
	wsHTTPContinuationID      = "resp_ws_http_continuation"
)

func setWSHTTPContinuationTestAPIKey(c *gin.Context, userID, keyID int64) {
	groupID := int64(wsHTTPContinuationGroupID)
	c.Set("api_key", &APIKey{ID: keyID, UserID: userID, GroupID: &groupID})
}

func requireWSHTTPContinuationOwner(t *testing.T, svc *OpenAIGatewayService, want bool) {
	t.Helper()
	ctx := context.Background()
	owned, err := svc.ValidateOpenAIHTTPResponseOwner(ctx, wsHTTPContinuationGroupID, wsHTTPContinuationID, wsHTTPContinuationUserID, wsHTTPContinuationKeyID)
	require.NoError(t, err)
	require.Equal(t, want, owned)
	if want {
		owned, err = svc.ValidateOpenAIHTTPResponseOwner(ctx, wsHTTPContinuationGroupID, wsHTTPContinuationID, wsHTTPContinuationUserID, wsHTTPContinuationKeyID+1)
		require.NoError(t, err)
		require.True(t, owned, "the same user can continue with another key in the group")

		owned, err = svc.ValidateOpenAIHTTPResponseOwner(ctx, wsHTTPContinuationGroupID, wsHTTPContinuationID, wsHTTPContinuationUserID+1, wsHTTPContinuationKeyID+1)
		require.NoError(t, err)
		require.False(t, owned, "another user in the same group cannot continue")

		owned, err = svc.ValidateOpenAIHTTPResponseOwner(ctx, wsHTTPContinuationGroupID+1, wsHTTPContinuationID, wsHTTPContinuationUserID, wsHTTPContinuationKeyID)
		require.NoError(t, err)
		require.False(t, owned, "the same user cannot continue in another group")
	}
}

type wsHTTPContinuationProbeWriter struct {
	gin.ResponseWriter
	beforeWrite func([]byte)
}

func (w *wsHTTPContinuationProbeWriter) Write(p []byte) (int, error) {
	w.beforeWrite(p)
	return w.ResponseWriter.Write(p)
}

func TestOpenAIHTTPToWSV2ContinuationOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name          string
		terminal      string
		status        string
		stream        bool
		omitID        bool
		topLevelError bool
		existingOwner bool
		want          bool
	}{
		{name: "sse_completed", terminal: "response.completed", status: `,"status":"completed"`, stream: true, want: true},
		{name: "json_done_without_status", terminal: "response.done", want: true},
		{name: "sse_failed", terminal: "response.failed", status: `,"status":"failed"`, stream: true},
		{name: "sse_incomplete", terminal: "response.incomplete", status: `,"status":"incomplete"`, stream: true},
		{name: "sse_done_incomplete", terminal: "response.done", status: `,"status":"incomplete"`, stream: true},
		{name: "sse_completed_without_terminal_id", terminal: "response.completed", stream: true, omitID: true},
		{name: "sse_completed_with_error", terminal: "response.completed", status: `,"status":"completed"`, stream: true, topLevelError: true},
		{name: "sse_duplicate_id_keeps_existing_owner", terminal: "response.completed", status: `,"status":"completed"`, stream: true, existingOwner: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			account := responseAuthOwnerAccount()
			account.Extra["openai_passthrough"] = false
			account.Extra["openai_oauth_responses_websockets_v2_enabled"] = true
			idField := fmt.Sprintf(`,"id":%q`, wsHTTPContinuationID)
			if test.omitID {
				idField = ""
			}
			errorField := ""
			if test.topLevelError {
				errorField = `,"error":{"message":"terminal error"}`
			}
			created := fmt.Sprintf(`{"type":"response.created","response":{"id":%q,"status":"in_progress"}}`, wsHTTPContinuationID)
			terminal := fmt.Sprintf(`{"type":%q,"response":{"model":"gpt-5.2","output":[],"usage":{"input_tokens":1,"output_tokens":1}%s%s}%s}`, test.terminal, idField, test.status, errorField)
			pool := newOpenAIWSConnPool(cfg)
			t.Cleanup(pool.Close)
			pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: &openAIWSCaptureConn{events: [][]byte{[]byte(created), []byte(terminal)}}})
			svc := &OpenAIGatewayService{
				cfg:                cfg,
				httpUpstream:       &httpUpstreamRecorder{err: errors.New("unexpected HTTP upstream fallback")},
				openaiWSPool:       pool,
				openaiWSStateStore: NewOpenAIWSStateStore(nil),
				toolCorrector:      NewCodexToolCorrector(),
				tiboRouteDisabled:  true,
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
			setWSHTTPContinuationTestAPIKey(c, wsHTTPContinuationUserID, wsHTTPContinuationKeyID)
			SetOpenAIHTTPResponseOwner(c, wsHTTPContinuationUserID, wsHTTPContinuationKeyID)
			ctx := c.Request.Context()
			if test.existingOwner {
				origin, _ := gin.CreateTestContext(httptest.NewRecorder())
				setWSHTTPContinuationTestAPIKey(origin, wsHTTPContinuationUserID+1, wsHTTPContinuationKeyID+1)
				SetOpenAIHTTPResponseOwner(origin, wsHTTPContinuationUserID+1, wsHTTPContinuationKeyID+1)
				svc.bindHTTPResponseAccount(ctx, origin, account, wsHTTPContinuationID)
			}
			terminalWritten := false
			// A client can open its HTTP continuation as soon as the terminal is visible.
			c.Writer = &wsHTTPContinuationProbeWriter{
				ResponseWriter: c.Writer,
				beforeWrite: func(p []byte) {
					if test.stream {
						if gjson.Get(strings.TrimPrefix(string(p), "data: "), "type").String() != test.terminal {
							return
						}
					} else if gjson.GetBytes(p, "id").String() != wsHTTPContinuationID {
						return
					}
					terminalWritten = true
					requireWSHTTPContinuationOwner(t, svc, test.want)
					if test.want {
						store := svc.getOpenAIWSStateStore()
						accountID, err := store.GetResponseAccount(ctx, wsHTTPContinuationGroupID, wsHTTPContinuationID)
						require.NoError(t, err)
						require.Equal(t, account.ID, accountID)
						connID, found := store.GetResponseConn(wsHTTPContinuationID)
						require.True(t, found)
						require.NotEmpty(t, connID)
					}
				},
			}

			body := []byte(fmt.Sprintf(`{"model":"gpt-5.2","instructions":"synthetic","stream":%t,"input":[{"role":"user","content":"hello"}]}`, test.stream))
			result, err := svc.Forward(ctx, c, account, body)
			require.NoError(t, err)
			require.NotNil(t, result)
			require.True(t, result.OpenAIWSMode)
			require.True(t, terminalWritten, "the upstream terminal response reached the HTTP writer")
			requireWSHTTPContinuationOwner(t, svc, test.want)
			if test.existingOwner {
				owned, err := svc.ValidateOpenAIHTTPResponseOwner(ctx, wsHTTPContinuationGroupID, wsHTTPContinuationID, wsHTTPContinuationUserID+1, wsHTTPContinuationKeyID+1)
				require.NoError(t, err)
				require.True(t, owned, "a repeated terminal ID preserves the original user’s ownership")
			}
		})
	}
}

func TestOpenAINativeWSIngressHTTPContinuationOwnership(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tests := []struct {
		name        string
		mode        string
		messageType coderws.MessageType
	}{
		{name: "ctx_pool", mode: OpenAIWSIngressModeCtxPool, messageType: coderws.MessageText},
		{name: "http_bridge", mode: OpenAIWSIngressModeHTTPBridge, messageType: coderws.MessageText},
		{name: "passthrough_text", mode: OpenAIWSIngressModePassthrough, messageType: coderws.MessageText},
		{name: "passthrough_binary", mode: OpenAIWSIngressModePassthrough, messageType: coderws.MessageBinary},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := newOpenAIWSExecutionScopeTestConfig()
			cfg.Gateway.OpenAIWS.ModeRouterV2Enabled = true
			account := passthroughLifecycleAccount()
			account.Extra["openai_apikey_responses_websockets_v2_mode"] = test.mode
			terminal := fmt.Sprintf(`{"type":"response.completed","response":{"id":%q,"status":"completed","model":"gpt-5.2","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`, wsHTTPContinuationID)
			svc := &OpenAIGatewayService{
				cfg:                cfg,
				openaiWSStateStore: NewOpenAIWSStateStore(nil),
				toolCorrector:      NewCodexToolCorrector(),
			}
			switch test.mode {
			case OpenAIWSIngressModeCtxPool:
				pool := newOpenAIWSConnPool(cfg)
				t.Cleanup(pool.Close)
				pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: &openAIWSCaptureConn{events: [][]byte{[]byte(terminal)}}})
				svc.openaiWSPool = pool
			case OpenAIWSIngressModeHTTPBridge:
				svc.httpUpstream = &httpUpstreamRecorder{resp: &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(strings.NewReader("data: " + terminal + "\n\ndata: [DONE]\n\n")),
				}}
			case OpenAIWSIngressModePassthrough:
				upstream := newStagedPassthroughConn()
				upstream.frames <- stagedPassthroughFrame{messageType: test.messageType, payload: []byte(terminal)}
				svc.openaiWSPassthroughDialer = &stagedPassthroughDialer{conn: upstream}
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			server, serverErr := startPassthroughLifecycleServerWithHooks(t, ctx, svc, account, func(c *gin.Context) *OpenAIWSIngressHooks {
				SetOpenAIClientTransport(c, OpenAIClientTransportWS)
				setWSHTTPContinuationTestAPIKey(c, wsHTTPContinuationUserID, wsHTTPContinuationKeyID)
				return nil
			})
			defer server.Close()
			client := dialPassthroughLifecycleClientWithPayload(t, server, `{"type":"response.create","model":"gpt-5.2","input":[{"role":"user","content":"hello"}]}`)
			defer client.CloseNow()
			messageType, completed, err := client.Read(ctx)
			require.NoError(t, err)
			require.Equal(t, test.messageType, messageType)
			require.Equal(t, "response.completed", gjson.GetBytes(completed, "type").String())
			require.Equal(t, wsHTTPContinuationID, gjson.GetBytes(completed, "response.id").String())
			requireWSHTTPContinuationOwner(t, svc, true)

			_ = client.Close(coderws.StatusNormalClosure, "done")
			select {
			case err := <-serverErr:
				require.True(t, err == nil || isOpenAIWSClientDisconnectError(err), "unexpected ingress error: %v", err)
			case <-ctx.Done():
				t.Fatal("WebSocket ingress did not stop")
			}
		})
	}
}
