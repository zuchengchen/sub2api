//go:build unit

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const (
	grokTestPingFrame       = "event: ping\ndata: {\"type\":\"ping\",\"x-opencode-type\":\"inference-cost\",\"cost\":\"0\"}\n\n"
	grokTestCommentFrame    = ": keepalive\n\n"
	grokTestInProgressFrame = "event: response.in_progress\ndata: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n"
	grokTestCreatedFrame    = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\"}}\n\n"
	grokTestTextDeltaFrame  = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"a\"}\n\n"
	grokTestCompletedFrame  = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":3}}}\n\n"
	grokTestChatRequestBody = `{"model":"grok-4.6","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`
)

type grokTestStep struct {
	delay time.Duration
	data  string
}

// grokScriptedBody streams scripted SSE steps. When repeat is set it then
// writes repeat every repeatEvery until closed; otherwise it ends with EOF.
type grokScriptedBody struct {
	reader *io.PipeReader
	closed chan struct{}
	once   sync.Once
}

func newGrokScriptedBody(steps []grokTestStep, repeat string, repeatEvery time.Duration) *grokScriptedBody {
	reader, writer := io.Pipe()
	body := &grokScriptedBody{reader: reader, closed: make(chan struct{})}
	go func() {
		wait := func(d time.Duration) bool {
			if d <= 0 {
				return true
			}
			select {
			case <-time.After(d):
				return true
			case <-body.closed:
				return false
			}
		}
		for _, step := range steps {
			if !wait(step.delay) {
				return
			}
			if _, err := io.WriteString(writer, step.data); err != nil {
				return
			}
		}
		if repeat == "" {
			_ = writer.Close()
			return
		}
		for wait(repeatEvery) {
			if _, err := io.WriteString(writer, repeat); err != nil {
				return
			}
		}
	}()
	return body
}

func (b *grokScriptedBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *grokScriptedBody) Close() error {
	b.once.Do(func() { close(b.closed) })
	return b.reader.Close()
}

func newGrokTestStreamResponse(body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
}

func newGrokTestChatContext(w http.ResponseWriter) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c
}

func requireGrokNoProgressError(t *testing.T, err error) {
	t.Helper()
	require.Error(t, err)
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) {
		require.Contains(t, string(failover.ResponseBody), "empty_upstream")
		require.Contains(t, string(failover.ResponseBody), "no upstream progress")
		return
	}
	require.Contains(t, err.Error(), grokChatReasoningIdleTimeoutCode)
	require.Contains(t, err.Error(), "no upstream progress")
}

// grokBrokenClientWriter simulates a client that went away: every write fails.
type grokBrokenClientWriter struct{ header http.Header }

func (w *grokBrokenClientWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *grokBrokenClientWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
func (w *grokBrokenClientWriter) WriteHeader(int)           {}
func (w *grokBrokenClientWriter) Flush()                    {}

func TestGrokSSEEventIsProgress(t *testing.T) {
	t.Parallel()

	cases := []struct {
		eventType string
		payload   string
		want      bool
	}{
		{"", `{}`, false},
		{"ping", `{"type":"ping"}`, false},
		{"keepalive", `{}`, false},
		{"heartbeat", `{}`, false},
		{"response.queued", `{}`, false},
		{"response.in_progress", `{}`, false},
		{" response.in_progress ", `{}`, false},
		{"response.output_text.delta", `{"delta":""}`, false},
		{"response.reasoning_summary_text.delta", `{"delta":""}`, false},
		{"response.output_text.delta", `{"delta":"a"}`, true},
		{"response.reasoning_text.delta", `{"delta":"x"}`, true},
		{"response.function_call_arguments.delta", `{"delta":"{\"a\""}`, true},
		{"response.custom.delta", `{"delta":{"k":1}}`, true},
		{"response.created", `{}`, true},
		{"response.output_item.added", `{"item":{"type":"reasoning","encrypted_content":"x"}}`, true},
		{"response.output_item.done", `{}`, true},
		{"response.completed", `{}`, true},
		{"response.failed", `{}`, true},
		{"error", `{}`, true},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, grokSSEEventIsProgress(tc.eventType, []byte(tc.payload)), "event=%q payload=%s", tc.eventType, tc.payload)
	}
}

func TestGrokStreamProgressTracksOnlyModelProgress(t *testing.T) {
	t.Parallel()

	start := time.Now().Add(-time.Minute)
	p := newGrokStreamProgress(start)
	require.InDelta(t, time.Minute.Seconds(), p.SinceProgress(time.Now()).Seconds(), 1)

	p.ObserveLine(": ping")
	p.ObserveLine("")
	p.ObserveLine("event: ping")
	p.ObserveEvent("ping", []byte(`{"type":"ping"}`))
	p.ObserveEvent("response.in_progress", []byte(`{}`))
	p.ObserveEvent("response.output_text.delta", []byte(`{"delta":""}`))
	require.InDelta(t, time.Minute.Seconds(), p.SinceProgress(time.Now()).Seconds(), 1, "keepalive must not refresh progress")

	p.ObserveEvent("response.created", []byte(`{}`))
	require.Less(t, p.SinceProgress(time.Now()), time.Second)

	p.lastProgressAt = start
	p.ObserveEvent("response.created", []byte(`{}`))
	require.InDelta(t, time.Minute.Seconds(), p.SinceProgress(time.Now()).Seconds(), 1, "a repeated response.created is not progress")

	p.ObserveEvent("response.output_text.delta", []byte(`{"delta":"hi"}`))
	require.Less(t, p.SinceProgress(time.Now()), time.Second)

	summary := p.Summary(time.Now(), time.Now().Add(-3*time.Second))
	require.Contains(t, summary, "last_event=response.output_text.delta")
	require.Contains(t, summary, "lines=3")
	require.Contains(t, summary, "comments=1")
	require.Contains(t, summary, "pings=1")
	require.Contains(t, summary, "idle_events=3")
	require.Contains(t, summary, "progress_events=2")
	require.Contains(t, summary, "since_read=3s")
}

func TestGrokStreamProgressNilReceiverIsNoop(t *testing.T) {
	t.Parallel()

	var p *grokStreamProgress
	p.ObserveLine(": ping")
	p.ObserveEvent("response.completed", []byte(`{}`))
	require.Zero(t, p.SinceProgress(time.Now()))
	require.Empty(t, p.Summary(time.Now(), time.Time{}))
}

func TestGrokStreamIdleCheckInterval(t *testing.T) {
	t.Parallel()

	require.Zero(t, grokStreamIdleCheckInterval(0))
	require.Equal(t, 15*time.Second, grokStreamIdleCheckInterval(600*time.Second))
	require.Equal(t, 15*time.Second, grokStreamIdleCheckInterval(180*time.Second))
	require.Equal(t, 10*time.Second, grokStreamIdleCheckInterval(40*time.Second))
	require.Equal(t, 500*time.Millisecond, grokStreamIdleCheckInterval(2*time.Second))
	require.Equal(t, 100*time.Millisecond, grokStreamIdleCheckInterval(time.Millisecond))
}

func TestGrokStreamEffectiveIdle(t *testing.T) {
	t.Parallel()

	require.Equal(t, 600*time.Second, grokStreamEffectiveIdle(600*time.Second, false))
	require.Equal(t, 120*time.Second, grokStreamEffectiveIdle(600*time.Second, true))
	require.Equal(t, 60*time.Second, grokStreamEffectiveIdle(60*time.Second, true))
}

// The 2026-10-27 stall: a cli-chat-proxy upstream kept streaming vendor pings
// after the turn stopped producing output, so the byte-level idle never fired.
func TestHandleChatStreamingResponse_GrokPingOnlyUpstreamFailoversBeforeOutput(t *testing.T) {
	body := newGrokScriptedBody(nil, grokTestPingFrame, 100*time.Millisecond)
	defer body.Close()
	rec := httptest.NewRecorder()
	c := newGrokTestChatContext(rec)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{GrokStreamDataIntervalTimeout: 1}}}
	account := &Account{ID: 31, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	started := time.Now()
	result, err := svc.handleChatStreamingResponse(newGrokTestStreamResponse(body), c, account,
		"grok-4.6", "grok-4.6", "grok-4.6", time.Now(), []byte(grokTestChatRequestBody))

	require.Less(t, time.Since(started), 3*time.Second, "pings must not keep a stalled turn alive")
	require.GreaterOrEqual(t, time.Since(started), time.Second)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	requireGrokNoProgressError(t, err)
	require.NotNil(t, result)
	require.False(t, c.Writer.Written())
}

func TestHandleChatStreamingResponse_GrokCommentKeepaliveAfterHeartbeatWritesTerminal(t *testing.T) {
	body := newGrokScriptedBody(nil, grokTestCommentFrame, 100*time.Millisecond)
	defer body.Close()
	rec := httptest.NewRecorder()
	c := newGrokTestChatContext(rec)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamKeepaliveInterval:       1,
		GrokStreamDataIntervalTimeout: 2,
	}}}
	account := &Account{ID: 32, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	started := time.Now()
	result, err := svc.handleChatStreamingResponse(newGrokTestStreamResponse(body), c, account,
		"grok-4.6", "grok-4.6", "grok-4.6", time.Now(), []byte(grokTestChatRequestBody))

	require.Less(t, time.Since(started), 4*time.Second)
	requireGrokNoProgressError(t, err)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), `"reasoning_content":"…"`)
	requireGrokChatAbortTerminal(t, rec.Body.String(), "Grok stream idle timeout after 2s with no upstream progress", "stop")
}

func TestHandleChatStreamingResponse_GrokRepeatedInProgressIsNotProgress(t *testing.T) {
	body := newGrokScriptedBody([]grokTestStep{{data: grokTestCreatedFrame}}, grokTestInProgressFrame, 100*time.Millisecond)
	defer body.Close()
	rec := httptest.NewRecorder()
	c := newGrokTestChatContext(rec)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{GrokStreamDataIntervalTimeout: 1}}}
	account := &Account{ID: 33, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	started := time.Now()
	_, err := svc.handleChatStreamingResponse(newGrokTestStreamResponse(body), c, account,
		"grok-4.6", "grok-4.6", "grok-4.6", time.Now(), []byte(grokTestChatRequestBody))

	require.Less(t, time.Since(started), 3*time.Second)
	requireGrokNoProgressError(t, err)
}

func TestHandleChatStreamingResponse_GrokSteadyProgressOutlivesIdleTimeout(t *testing.T) {
	steps := []grokTestStep{{data: grokTestCreatedFrame}}
	// 3s of output in 300ms steps, interleaved with pings, against a 1s idle.
	for i := 0; i < 10; i++ {
		steps = append(steps,
			grokTestStep{delay: 150 * time.Millisecond, data: grokTestPingFrame},
			grokTestStep{delay: 150 * time.Millisecond, data: grokTestTextDeltaFrame},
		)
	}
	steps = append(steps, grokTestStep{data: grokTestCompletedFrame})
	body := newGrokScriptedBody(steps, "", 0)
	defer body.Close()
	rec := httptest.NewRecorder()
	c := newGrokTestChatContext(rec)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{GrokStreamDataIntervalTimeout: 1}}}
	account := &Account{ID: 34, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	started := time.Now()
	result, err := svc.handleChatStreamingResponse(newGrokTestStreamResponse(body), c, account,
		"grok-4.6", "grok-4.6", "grok-4.6", time.Now(), []byte(grokTestChatRequestBody))

	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), 2500*time.Millisecond)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.NotContains(t, rec.Body.String(), "no upstream progress")
}

// Before the fix a disconnected client plus a pinging upstream drained until
// the 58m wall clock. The drain now stops once progress stalls.
func TestHandleChatStreamingResponse_GrokDisconnectedDrainStopsWithoutProgress(t *testing.T) {
	body := newGrokScriptedBody(nil, grokTestPingFrame, 100*time.Millisecond)
	defer body.Close()
	c := newGrokTestChatContext(&grokBrokenClientWriter{})
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{
		StreamKeepaliveInterval:       1,
		GrokStreamDataIntervalTimeout: 2,
	}}}
	account := &Account{ID: 35, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	started := time.Now()
	result, err := svc.handleChatStreamingResponse(newGrokTestStreamResponse(body), c, account,
		"grok-4.6", "grok-4.6", "grok-4.6", time.Now(), []byte(grokTestChatRequestBody))

	require.Less(t, time.Since(started), 4*time.Second)
	require.Error(t, err)
	require.Contains(t, err.Error(), "stream usage incomplete after timeout")
	require.NotNil(t, result)
}

// Non-Grok accounts keep the byte-level idle: keepalive bytes still count.
func TestHandleChatStreamingResponse_NonGrokKeepaliveStillResetsIdle(t *testing.T) {
	steps := make([]grokTestStep, 0, 12)
	for i := 0; i < 10; i++ {
		steps = append(steps, grokTestStep{delay: 200 * time.Millisecond, data: grokTestCommentFrame})
	}
	steps = append(steps, grokTestStep{data: grokTestCompletedFrame})
	body := newGrokScriptedBody(steps, "", 0)
	defer body.Close()
	rec := httptest.NewRecorder()
	c := newGrokTestChatContext(rec)
	svc := &OpenAIGatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamDataIntervalTimeout: 1}}}
	account := &Account{ID: 36, Name: "openai-apikey", Platform: PlatformOpenAI, Type: AccountTypeAPIKey}

	result, err := svc.handleChatStreamingResponse(newGrokTestStreamResponse(body), c, account,
		"gpt-5", "gpt-5", "gpt-5", time.Now(), []byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestHandleStreamingResponse_GrokPingOnlyUpstreamTimesOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	source := newGrokScriptedBody(nil, grokTestPingFrame, 100*time.Millisecond)
	defer source.Close()
	account := &Account{ID: 37, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}
	resp := newGrokTestStreamResponse(newGrokResponsesBillingPingFilterBody(source, account, defaultMaxLineSize))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			MaxLineSize:               defaultMaxLineSize,
			StreamDataIntervalTimeout: 1,
		}},
		toolCorrector: NewCodexToolCorrector(),
	}

	started := time.Now()
	_, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "grok-4.6", "grok-4.6")

	require.Less(t, time.Since(started), 3*time.Second, "pings must not keep a stalled Responses turn alive")
	require.GreaterOrEqual(t, time.Since(started), time.Second)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Contains(t, string(failover.ResponseBody), "no upstream progress")
}

func TestHandleStreamingResponse_GrokSteadyProgressOutlivesIdleTimeout(t *testing.T) {
	gin.SetMode(gin.TestMode)
	steps := []grokTestStep{{data: grokTestCreatedFrame}}
	for i := 0; i < 10; i++ {
		steps = append(steps,
			grokTestStep{delay: 150 * time.Millisecond, data: grokTestPingFrame},
			grokTestStep{delay: 150 * time.Millisecond, data: grokTestTextDeltaFrame},
		)
	}
	steps = append(steps, grokTestStep{data: grokTestCompletedFrame})
	source := newGrokScriptedBody(steps, "", 0)
	defer source.Close()
	account := &Account{ID: 38, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}
	resp := newGrokTestStreamResponse(newGrokResponsesBillingPingFilterBody(source, account, defaultMaxLineSize))
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	svc := &OpenAIGatewayService{
		cfg: &config.Config{Gateway: config.GatewayConfig{
			MaxLineSize:               defaultMaxLineSize,
			StreamDataIntervalTimeout: 1,
		}},
		toolCorrector: NewCodexToolCorrector(),
	}

	started := time.Now()
	result, err := svc.handleStreamingResponse(context.Background(), resp, c, account, time.Now(), "grok-4.6", "grok-4.6")

	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), 2500*time.Millisecond)
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), "response.completed")
	require.False(t, strings.Contains(rec.Body.String(), "stream_timeout"))
}
