package service

import (
	"context"
	"encoding/json"
	"fmt"
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

// Reproduced by the live CLI 0.166.0 ctx_pool path: the Upgrade describes
// prewarm, but every subsequent frame carries its own turn metadata.
func TestOpenAIWSIngress_FrameMetadataSurvivesPrewarmHandshake(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := newOpenAIWSExecutionScopeTestConfig()
	capture := &openAIWSCaptureConn{}
	metadata := []string{
		`{"request_kind":"prewarm","turn_id":"","thread_id":"acceptance-thread"}`,
		`{"request_kind":"turn","turn_id":"019aa001-1000-7000-8000-000000000001","thread_id":"acceptance-thread","turn_trigger":"exec","turn_started_at_unix_ms":1793551528752}`,
		`{"request_kind":"turn","turn_id":"019aa001-1000-7000-8000-000000000001","thread_id":"acceptance-thread","turn_trigger":"exec","turn_started_at_unix_ms":1793551528752}`,
		`{"request_kind":"compaction","turn_id":"019aa001-1000-7000-8000-000000000001","thread_id":"acceptance-thread","turn_trigger":"exec","turn_started_at_unix_ms":1793551528752}`,
	}
	for i := range metadata {
		capture.events = append(capture.events, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"resp_metadata_%d","model":"gpt-6-astra","usage":{"input_tokens":1,"output_tokens":1}}}`, i)))
	}
	pool := newOpenAIWSConnPool(cfg)
	pool.setClientDialerForTest(&openAIWSCaptureDialer{conn: capture})
	defer pool.Close()
	svc := &OpenAIGatewayService{cfg: cfg, httpUpstream: &httpUpstreamRecorder{}, cache: &stubGatewayCache{},
		openaiWSResolver: NewOpenAIWSProtocolResolver(cfg), toolCorrector: NewCodexToolCorrector(), openaiWSPool: pool}
	account := &Account{ID: 7001, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusActive, Schedulable: true, Concurrency: 1,
		Credentials: map[string]any{"access_token": "synthetic"}, Extra: astraHealthyTestExtra(map[string]any{"responses_websockets_v2_enabled": true})}
	done := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, nil)
		if err != nil {
			done <- err
			return
		}
		defer conn.CloseNow()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = r
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		_, first, err := conn.Read(ctx)
		if err == nil {
			err = svc.ProxyResponsesWebSocketFromClient(ctx, c, conn, account, "synthetic", first, nil)
		}
		done <- err
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	headers := http.Header{}
	headers.Set("User-Agent", "codex_cli_rs/0.166.0")
	headers.Set(openAIWSTurnMetadataHeader, metadata[0])
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), &coderws.DialOptions{HTTPHeader: headers})
	require.NoError(t, err)
	defer client.CloseNow()
	for i, meta := range metadata {
		payload := map[string]any{"type": "response.create", "model": "gpt-6-astra", "store": false,
			"reasoning": map[string]any{"effort": "xhigh"}, "input": []any{}}
		if i == 0 {
			payload["generate"] = false // Metadata omitted: legacy header fallback remains available.
		} else {
			payload["previous_response_id"] = fmt.Sprintf("resp_metadata_%d", i-1)
			payload["client_metadata"] = map[string]any{openAIWSTurnMetadataHeader: meta}
		}
		raw, err := json.Marshal(payload)
		require.NoError(t, err)
		require.NoError(t, client.Write(ctx, coderws.MessageText, raw))
		_, event, err := client.Read(ctx)
		if err != nil {
			select {
			case proxyErr := <-done:
				t.Fatalf("client read: %v; proxy: %v", err, proxyErr)
			default:
			}
		}
		require.NoError(t, err)
		require.Equal(t, "response.completed", gjson.GetBytes(event, "type").String())
	}
	require.NoError(t, client.Close(coderws.StatusNormalClosure, "done"))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("gateway did not finish")
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.writes, len(metadata))
	for i, request := range capture.writes {
		raw, err := json.Marshal(request)
		require.NoError(t, err)
		actual := gjson.Parse(gjson.GetBytes(raw, "client_metadata."+gjsonEscapeKey(openAIWSTurnMetadataHeader)).String())
		want := gjson.Parse(metadata[i])
		for _, key := range []string{"request_kind", "turn_trigger", "turn_started_at_unix_ms"} {
			require.Equal(t, want.Get(key).Raw, actual.Get(key).Raw, "frame %d field %s", i, key)
		}
		if i > 0 {
			require.NotEmpty(t, actual.Get("turn_id").String())
		}
		require.Equal(t, "gpt-6-astra", gjson.GetBytes(raw, "model").String())
		require.Equal(t, "xhigh", gjson.GetBytes(raw, "reasoning.effort").String())
	}
}
