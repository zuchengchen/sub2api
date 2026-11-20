//go:build unit

package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/testutil"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNativeResponsesHTTPClaudeSessionSurvivesCompaction(t *testing.T) {
	t.Setenv("SUB2_RESPONSES_CACHE_AFFINITY_SCOPE", "key:8101")
	bindings := testutil.NewRedisGatewayCache(t)
	scheduler := &openAIQueueSchedulerCache{available: func(int64) bool { return true }}
	h, upstream, _ := newOpenAIQueueHandlerWithScheduler(t, 2, &openAIQueueAdmissionCache{}, scheduler, time.Second, bindings)
	const id = "9ed40161-35bb-44b6-919b-f242a4213723"
	const model = "gpt-5.1"
	var threadHash string
	for i, keyID := range []int64{8101, 8101, 8102} {
		body := `{"model":"gpt-5.1","input":"original"}`
		if i > 0 {
			body = fmt.Sprintf(`{"model":"gpt-5.1","input":"summary","metadata":{"user_id":%q}}`, `{"session_id":"`+id+`"}`)
		}
		c, rec := openAIQueueRequestContext(t, "/v1/responses", body)
		key, ok := middleware.GetAPIKeyFromContext(c)
		require.True(t, ok)
		key.ID, key.UserID, key.User.ID = keyID, 701, 701
		c.Set(string(middleware.ContextKeyUser), middleware.AuthSubject{UserID: 701})
		if i == 0 {
			// Seed the HTTP Codex thread's established binding to the lower-priority
			// account. The new Claude carrier must resolve to this same scoped key.
			c.Request.Header.Set("thread-id", id)
			threadHash = h.gatewayService.GenerateResponsesAccountSessionHash(c, []byte(body), model)
			require.NoError(t, h.gatewayService.BindStickySession(c.Request.Context(), key.GroupID, threadHash, 2))
			c.Request.Header.Del("thread-id")
			c.Request.Header.Set("X-Claude-Code-Session-Id", id)
		}
		actualHash := h.gatewayService.GenerateResponsesAccountSessionHash(c, []byte(body), model)
		if keyID == 8101 {
			require.Equal(t, threadHash, actualHash)
		} else {
			require.NotEqual(t, threadHash, actualHash)
		}
		h.Responses(c)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	}
	require.Equal(t, []int64{2, 2, 1}, upstream.accountIDs)
}

func TestNativeResponsesWSFirstFrameUsesHTTPThreadBinding(t *testing.T) {
	t.Setenv("SUB2_RESPONSES_CACHE_AFFINITY_SCOPE", "key:1851")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{CompressionMode: coderws.CompressionContextTakeover})
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, _, err := conn.Read(ctx); err != nil {
			t.Error(err)
			return
		}
		response := []byte(`{"type":"response.completed","response":{"id":"resp_native_affinity","model":"gpt-5.1","usage":{"input_tokens":2,"output_tokens":1}}}`)
		if err := conn.Write(ctx, coderws.MessageText, response); err != nil {
			t.Error(err)
		}
	}))
	defer upstream.Close()
	h := newOpenAIWSPassthroughHandlerHarness(t, upstream.URL)
	const id = "9ed40161-35bb-44b6-919b-f242a4213723"
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("thread-id", id)
	c.Set("api_key", h.apiKey)
	expected := h.handler.gatewayService.GenerateResponsesAccountSessionHash(c, []byte(`{"model":"gpt-5.1","input":"original history"}`), "gpt-5.1")
	frame := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.1","input":"summary after reconnect","metadata":{"user_id":%q}}`, `{"session_id":"`+id+`"}`)
	c.Request.Header.Del("thread-id")
	old := h.handler.gatewayService.GenerateSessionHash(c, []byte(frame))
	require.NotEqual(t, expected, old)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, h.clientConn.Write(ctx, coderws.MessageText, []byte(frame)))
	_, result, err := h.clientConn.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(result, "type").String(), string(result))
	accountID, err := h.gatewayCache.GetSessionAccountID(ctx, *h.apiKey.GroupID, "openai:"+expected)
	require.NoError(t, err, "WS first frame must bind the existing HTTP conversation hash")
	require.EqualValues(t, 9951, accountID)
	_, err = h.gatewayCache.GetSessionAccountID(ctx, *h.apiKey.GroupID, "openai:"+old)
	require.ErrorIs(t, err, service.ErrStickySessionNotFound)
}
