//go:build unit

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOAuthReliabilityContractNativeWSBareErrorDoesNotReplayMeteredTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body, err := sjson.SetBytes(rejectedMessageIDHistory(t, 1, 0), "type", "response.create")
	require.NoError(t, err)
	errorObject := gjson.GetBytes(rejectedMessageIDError(1, "item_BAD"), "error").Raw
	upstreamConn := &openAIWSCaptureConn{events: [][]byte{
		[]byte(`{"type":"error","error":` + errorObject + `}`),
		[]byte(oauthReliabilityContractFailed(errorObject, `{"input_tokens":5,"output_tokens":0}`)),
	}}
	cfg := &config.Config{}
	cfg.Gateway.OpenAIWS.Enabled = true
	cfg.Gateway.OpenAIWS.OAuthEnabled = true
	cfg.Gateway.OpenAIWS.ResponsesWebsocketsV2 = true
	cfg.Gateway.OpenAIWS.MaxConnsPerAccount = 1
	cfg.Gateway.OpenAIWS.MaxIdlePerAccount = 1
	cfg.Gateway.OpenAIWS.ReadTimeoutSeconds = 3
	cfg.Gateway.OpenAIWS.WriteTimeoutSeconds = 3
	pool := newOpenAIWSConnPool(cfg)
	defer pool.Close()
	dialer := &openAIWSCaptureDialer{conn: upstreamConn}
	pool.setClientDialerForTest(dialer)
	svc := &OpenAIGatewayService{
		cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool,
		tiboRouteDisabled: true,
	}
	account := newOpenAIOAuthNamespaceTestAccount()
	account.Extra["responses_websockets_v2_enabled"] = true
	account.Extra["openai_oauth_responses_websockets_v2_mode"] = OpenAIWSIngressModeCtxPool
	type turnOutcome struct {
		result *OpenAIForwardResult
		err    error
	}
	outcomes := make(chan turnOutcome, 2)
	serverDone := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, acceptErr := coderws.Accept(w, r, nil)
		if acceptErr != nil {
			serverDone <- acceptErr
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		_, first, readErr := conn.Read(ctx)
		if readErr != nil {
			serverDone <- readErr
			return
		}
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r.WithContext(ctx)
		serverDone <- svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "synthetic-token", first, &OpenAIWSIngressHooks{
			AfterTurn: func(_ int, result *OpenAIForwardResult, turnErr error) {
				outcomes <- turnOutcome{result, turnErr}
			},
		})
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer client.CloseNow()
	require.NoError(t, client.Write(ctx, coderws.MessageText, body))
	var events []string
	for i := 0; i < 4; i++ {
		_, event, readErr := client.Read(ctx)
		require.NoError(t, readErr)
		events = append(events, string(event))
		if gjson.GetBytes(event, "type").String() == "response.failed" {
			break
		}
	}
	select {
	case outcome := <-outcomes:
		require.NoError(t, outcome.err)
		require.NotNil(t, outcome.result)
		require.Equal(t, "response.failed", outcome.result.UpstreamTerminalEvent)
		require.Equal(t, 5, outcome.result.Usage.InputTokens)
	case <-ctx.Done():
		t.Fatal("native WS did not settle the failed response")
	}
	require.Contains(t, strings.Join(events, "\n"), "response.failed")
	upstreamConn.mu.Lock()
	writes := len(upstreamConn.writes)
	upstreamConn.mu.Unlock()
	require.Equal(t, 1, writes, "the initial bare error must not replay before the authoritative metered failure is read")
	require.Equal(t, 1, dialer.DialCount())
	_ = client.Close(coderws.StatusNormalClosure, "done")
	select {
	case <-serverDone:
	case <-ctx.Done():
		t.Fatal("native WS handler did not terminate after client close")
	}
}
