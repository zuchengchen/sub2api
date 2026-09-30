package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// excelBPSHeartbeatSignal wraps the response writer and closes seen once the
// BPS forwarder has written its first keepalive comment.
type excelBPSHeartbeatSignal struct {
	gin.ResponseWriter
	once sync.Once
	seen chan struct{}
}

func (w *excelBPSHeartbeatSignal) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	if strings.Contains(s, "keepalive") {
		w.once.Do(func() { close(w.seen) })
	}
	return n, err
}

// forwardExcelBPSAfterHeartbeat streams a BPS request whose upstream stays
// silent until the first heartbeat, then sends tail and ends the stream.
func forwardExcelBPSAfterHeartbeat(t *testing.T, tail string) (*httpUpstreamRecorder, *httptest.ResponseRecorder, *gin.Context, error) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	reader, writer := io.Pipe()
	upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK,
		Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}}
	svc := openAIClientToolsTestService(upstream)
	svc.excelBPSHeartbeatInterval = 20 * time.Millisecond
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	signal := &excelBPSHeartbeatSignal{ResponseWriter: c.Writer, seen: make(chan struct{})}
	c.Writer = signal
	go func() {
		select {
		case <-signal.seen:
		case <-time.After(5 * time.Second):
		}
		if tail != "" {
			_, _ = writer.Write([]byte(tail))
		}
		_ = writer.Close()
	}()
	_, err := svc.Forward(context.Background(), c, excelAccount(), []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`))
	return upstream, rec, c, err
}

// Keepalive comments are not model output: an uncertain failure after only a
// heartbeat still switches accounts inside the same SSE response.
func TestExcelBPSHeartbeatOnlyFailureStillSwitchesAccount(t *testing.T) {
	upstream, rec, c, err := forwardExcelBPSAfterHeartbeat(t, "")
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, err, &failoverErr)
	require.False(t, failoverErr.RetryableOnSameAccount)
	require.Len(t, upstream.requests, 1, "never replayed on this account")
	body := rec.Body.String()
	require.Contains(t, body, ": keepalive")
	require.NotContains(t, body, "data:", "no model output reached the client")
	require.False(t, IsResponseCommitted(c))
	require.Equal(t, -1, OpenAICompactKeepaliveAdjustedWrittenSize(c), "the handler sees an unwritten response and may fail over")
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"), "committed SSE headers stay")
}

func TestExcelBPSOutputAfterHeartbeatStaysCommitted(t *testing.T) {
	created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_cut\",\"status\":\"in_progress\"}}\n\n"
	upstream, rec, c, err := forwardExcelBPSAfterHeartbeat(t, created)
	require.Error(t, err)
	var failoverErr *UpstreamFailoverError
	require.NotErrorAs(t, err, &failoverErr, "output reached the client")
	require.Len(t, upstream.requests, 1)
	require.True(t, IsResponseCommitted(c))
	require.Contains(t, rec.Body.String(), "response.created")
	require.Positive(t, OpenAICompactKeepaliveAdjustedWrittenSize(c))
}

// An uncertain failure before anything was written drops the SSE headers so
// the handler's final JSON error is not labeled as an event stream.
func TestExcelBPSUncertainFailoverDropsUnsentStreamHeaders(t *testing.T) {
	upstream := &excelBPSTraceUpstream{steps: []excelBPSWireStep{{headersWritten: true, wrote: true, resp: excelBPSSSE()}}}
	got := excelBPSForwardTrace(t, context.Background(), upstream, true)
	var failoverErr *UpstreamFailoverError
	require.ErrorAs(t, got.err, &failoverErr)
	require.False(t, got.c.Writer.Written())
	require.Empty(t, got.c.Writer.Header().Get("Content-Type"))
	require.Empty(t, got.c.Writer.Header().Get("Cache-Control"))
	require.Empty(t, got.c.Writer.Header().Get("X-Accel-Buffering"))
}

// After an earlier attempt wrote only keepalive comments, a Cookie WS turn
// that was never sent may still move to this account's HTTP route.
func TestCookieWSFallsBackToHTTPAfterEarlierHeartbeat(t *testing.T) {
	gin.SetMode(gin.TestMode)
	svc, account, _, dialer := newCookieForwardFixture(t, &openAIWSCaptureConn{})
	svc.cfg.Gateway.OpenAIWS.CookieWSHTTPFallbackThresholdBytes = 1 // refuse before sending
	upstream := mustTestValue[*httpUpstreamRecorder](t, svc.httpUpstream)
	upstream.resp = cookieWSHTTPResponse("http after heartbeat")
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	SetOpenAIClientTransport(c, OpenAIClientTransportHTTP)
	c.Header("Content-Type", "text/event-stream")
	n, err := c.Writer.WriteString(": keepalive\n\n")
	require.NoError(t, err)
	recordOpenAIStreamKeepaliveBytes(c, n)

	result, err := svc.Forward(context.Background(), c, account, []byte(`{"model":"gpt-6-astra","stream":true,"input":"hello"}`))
	require.NoError(t, err)
	require.False(t, result.OpenAIWSMode)
	require.Zero(t, dialer.DialCount())
	require.Contains(t, rec.Body.String(), "http after heartbeat")
}
