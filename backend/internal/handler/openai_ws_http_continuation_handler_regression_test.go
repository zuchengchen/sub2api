//go:build unit

package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestOpenAIResponsesHTTPContinuesCompletedWebSocketTurn(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const originID = "resp_ws_handler_origin"
	const continuationID = "resp_ws_handler_continuation"
	const origin = `{"id":"` + originID + `","object":"response","model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`
	const continuation = `{"id":"` + continuationID + `","object":"response","model":"gpt-5.6-sol","status":"completed","output":[],"usage":{"input_tokens":2,"output_tokens":1}}`
	upstream := newAstraProCapturedUpstream(
		&http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(`data: {"type":"response.completed","response":` + origin + "}\n\n")),
		},
		&http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(continuation)),
		},
	)
	h := oauthContinuationClassificationHandler(t, upstream, false, true)
	seed, _, _ := oauthContinuationClassificationRequest(t, "responses", `"input":"seed"`, true)
	key, ok := middleware.GetAPIKeyFromContext(seed)
	require.True(t, ok)
	subject, ok := middleware.GetAuthSubjectFromContext(seed)
	require.True(t, ok)

	router := gin.New()
	router.Use(func(c *gin.Context) {
		requestKey := *key
		requestSubject := subject
		if c.GetHeader("X-Test-User") == "other" {
			requestKey.ID++
			requestKey.UserID++
			requestKey.User = &service.User{ID: requestKey.UserID}
			requestSubject.UserID = requestKey.UserID
		}
		c.Set(string(middleware.ContextKeyAPIKey), &requestKey)
		c.Set(string(middleware.ContextKeyUser), requestSubject)
		c.Next()
	})
	handlerDone := make(chan struct{})
	router.GET("/v1/responses", func(c *gin.Context) {
		defer close(handlerDone)
		h.ResponsesWebSocket(c)
	})
	router.POST("/v1/responses", h.Responses)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client, _, err := coderws.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer client.CloseNow()
	require.NoError(t, client.Write(ctx, coderws.MessageText, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"first turn"}`)))
	_, terminal, err := client.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "response.completed", gjson.GetBytes(terminal, "type").String())
	require.Equal(t, originID, gjson.GetBytes(terminal, "response.id").String())

	const followup = `{"model":"gpt-5.6-sol","stream":false,"previous_response_id":"` + originID + `","input":"continue"}`
	request := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", strings.NewReader(followup))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	require.Equal(t, continuationID, gjson.GetBytes(recorder.Body.Bytes(), "id").String())

	foreignRequest := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", strings.NewReader(followup))
	foreignRequest.Header.Set("Content-Type", "application/json")
	foreignRequest.Header.Set("X-Test-User", "other")
	foreignRecorder := httptest.NewRecorder()
	router.ServeHTTP(foreignRecorder, foreignRequest)
	require.Equal(t, http.StatusBadRequest, foreignRecorder.Code, foreignRecorder.Body.String())
	require.Equal(t, "previous_response_id is not available for this user", gjson.GetBytes(foreignRecorder.Body.Bytes(), "error.message").String())

	_, accountIDs, bodies := upstream.snapshot()
	require.Equal(t, []int64{9910, 9910}, accountIDs)
	require.Len(t, bodies, 2)
	require.Equal(t, originID, gjson.GetBytes(bodies[1], "previous_response_id").String())

	_ = client.CloseNow()
	select {
	case <-handlerDone:
	case <-ctx.Done():
		t.Fatal("WebSocket handler did not stop")
	}
}
