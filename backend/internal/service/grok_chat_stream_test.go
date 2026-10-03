//go:build unit

package service

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGrokChatDeltaIsVisibleIgnoresHeartbeat(t *testing.T) {
	t.Parallel()

	mark := grokChatReasoningHeartbeatMark
	require.False(t, grokChatDeltaIsVisible(apicompat.ChatDelta{ReasoningContent: &mark}))
	text := "real plan"
	require.True(t, grokChatDeltaIsVisible(apicompat.ChatDelta{ReasoningContent: &text}))
	content := "hello"
	require.True(t, grokChatDeltaIsVisible(apicompat.ChatDelta{Content: &content}))
}

func TestStripGrokSyntheticReasoningTextKeepsRealSpaces(t *testing.T) {
	t.Parallel()

	got := stripGrokSyntheticReasoningText(grokChatReasoningHeartbeatMark + grokChatReasoningHeartbeatMark)
	require.Empty(t, got)
	got = stripGrokSyntheticReasoningText("hello world" + grokChatReasoningHeartbeatMark)
	require.Equal(t, "hello world", got)
	progress := grokChatReasoningProgressText(2 * time.Minute)
	require.Equal(t, "Grok encrypted reasoning still running (2m elapsed)", progress)
	got = stripGrokSyntheticReasoningText("hello world" + progress + grokChatReasoningHeartbeatMark)
	require.Equal(t, "hello world", got)
	progressDelta := grokChatReasoningProgressText(time.Minute)
	require.False(t, grokChatDeltaIsVisible(apicompat.ChatDelta{ReasoningContent: &progressDelta}))
}

func TestGrokChatWindowFuseExceeded(t *testing.T) {
	t.Parallel()

	require.False(t, grokChatWindowFuseExceeded(OpenAIUsage{InputTokens: 100, OutputTokens: 50}, nil))
	require.True(t, grokChatWindowFuseExceeded(OpenAIUsage{InputTokens: 300000, OutputTokens: 180000}, nil))
	payload := []byte(`{"usage":{"output_tokens_details":{"reasoning_tokens":200000}}}`)
	require.True(t, grokChatWindowFuseExceeded(OpenAIUsage{InputTokens: 10, OutputTokens: 10}, payload))
}

func TestSanitizeGrokChatSyntheticReasoningHistory(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"grok-4.6","messages":[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"……"},{"role":"assistant","content":"keep","reasoning_content":"real plan……Grok encrypted reasoning still running (12m elapsed)"},{"role":"assistant","reasoning_content":"keep this plan"}]}`)
	cleaned, err := sanitizeGrokChatSyntheticReasoningHistory(body)
	require.NoError(t, err)
	messages := gjson.GetBytes(cleaned, "messages").Array()
	require.Len(t, messages, 3)
	require.Equal(t, "user", messages[0].Get("role").String())
	require.Equal(t, "keep", messages[1].Get("content").String())
	require.Equal(t, "real plan", messages[1].Get("reasoning_content").String())
	require.Equal(t, "keep this plan", messages[2].Get("reasoning_content").String())
}

func TestEnsureGrokResponsesReasoningSummary(t *testing.T) {
	t.Parallel()

	out, err := ensureGrokResponsesReasoningSummary([]byte(`{"reasoning":{"effort":"xhigh"}}`))
	require.NoError(t, err)
	require.Equal(t, "auto", gjson.GetBytes(out, "reasoning.summary").String())

	out, err = ensureGrokResponsesReasoningSummary([]byte(`{"reasoning":{"effort":"xhigh","summary":"detailed"}}`))
	require.NoError(t, err)
	require.Equal(t, "detailed", gjson.GetBytes(out, "reasoning.summary").String())
}

type grokChatHangBody struct {
	mu     sync.Mutex
	closed chan struct{}
}

func newGrokChatHangBody() *grokChatHangBody {
	return &grokChatHangBody{closed: make(chan struct{})}
}

func (b *grokChatHangBody) Read(p []byte) (int, error) {
	<-b.closed
	return 0, io.EOF
}

func (b *grokChatHangBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return nil
}

type grokChatHangErrBody struct {
	*grokChatHangBody
	err error
}

func newGrokChatHangErrBody(err error) *grokChatHangErrBody {
	return &grokChatHangErrBody{grokChatHangBody: newGrokChatHangBody(), err: err}
}

func (b *grokChatHangErrBody) Read(p []byte) (int, error) {
	<-b.closed
	if b.err != nil {
		return 0, b.err
	}
	return 0, io.EOF
}

func TestHandleChatStreamingResponse_GrokHeartbeatWithoutVisibleDelta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := newGrokChatHangBody()
	defer body.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			StreamKeepaliveInterval:       1,
			GrokStreamDataIntervalTimeout: 30,
		},
	}}
	account := &Account{ID: 9, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.handleChatStreamingResponse(
			resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now(),
			[]byte(`{"model":"grok-4.6","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`),
		)
	}()

	select {
	case <-time.After(1500 * time.Millisecond):
	case <-done:
		t.Fatal("stream returned before heartbeat window")
	}
	_ = body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream did not return after closing hung body")
	}
	require.Contains(t, rec.Body.String(), `"reasoning_content":"…"`)
}

func grokChatLargeRequestBody() []byte {
	return []byte(`{"model":"grok-4.6","reasoning_effort":"xhigh","messages":[{"role":"user","content":"` + strings.Repeat("x", openAISilentRefusalMinRequestBodyBytes) + `"}]}`)
}

func TestHandleChatStreamingResponse_GrokHeartbeatWithLargeRequestBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := newGrokChatHangBody()
	defer body.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			StreamKeepaliveInterval:       1,
			GrokStreamDataIntervalTimeout: 30,
		},
	}}
	account := &Account{ID: 19, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}
	requestBody := grokChatLargeRequestBody()
	require.GreaterOrEqual(t, len(requestBody), openAISilentRefusalMinRequestBodyBytes)
	require.True(t, newOpenAIChatSilentRefusalDetector(len(requestBody)).Enabled())
	require.False(t, newOpenAIChatSilentRefusalDetector(0).Enabled())

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = svc.handleChatStreamingResponse(
			resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now(),
			requestBody,
		)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.Body.String(), `"reasoning_content":"…"`) {
			break
		}
		if time.Now().After(deadline) {
			_ = body.Close()
			t.Fatalf("expected Grok reasoning heartbeat on >=64KiB request, got %q", rec.Body.String())
		}
		select {
		case <-done:
			t.Fatalf("stream returned before heartbeat, body=%q", rec.Body.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = body.Close()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("stream did not return after closing hung body")
	}
	require.Contains(t, rec.Body.String(), `"reasoning_content":"…"`)
	require.NotContains(t, rec.Body.String(), ":\n\n")
}

func TestHandleChatStreamingResponse_GrokIdleTimeoutFailoversBeforeOutput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := newGrokChatHangBody()
	defer body.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			GrokStreamDataIntervalTimeout: 1,
		},
	}}
	account := &Account{ID: 11, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	result, err := svc.handleChatStreamingResponse(
		resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now(),
		[]byte(`{"model":"grok-4.6","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`),
	)
	require.Error(t, err)
	var failover *UpstreamFailoverError
	require.ErrorAs(t, err, &failover)
	require.Contains(t, string(failover.ResponseBody), "empty_upstream")
	require.NotNil(t, result)
	require.False(t, c.Writer.Written())
}

func TestHandleChatStreamingResponse_GrokIdleTimeoutAfterHeartbeatWritesSSEError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := newGrokChatHangBody()
	defer body.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			StreamKeepaliveInterval:       1,
			GrokStreamDataIntervalTimeout: 2,
		},
	}}
	account := &Account{ID: 13, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	result, err := svc.handleChatStreamingResponse(
		resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now(),
		[]byte(`{"model":"grok-4.6","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), grokChatReasoningIdleTimeoutCode)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), `"reasoning_content":"…"`)
	require.Contains(t, rec.Body.String(), grokChatReasoningIdleTimeoutCode)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}

func TestHandleChatStreamingResponse_GrokUpstreamCutAfterHeartbeatWritesSSEError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := newGrokChatHangErrBody(io.ErrUnexpectedEOF)
	defer body.Close()

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{
		Gateway: config.GatewayConfig{
			StreamKeepaliveInterval:       1,
			GrokStreamDataIntervalTimeout: 30,
		},
	}}
	account := &Account{ID: 21, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	done := make(chan struct{})
	var result *OpenAIForwardResult
	var err error
	go func() {
		defer close(done)
		result, err = svc.handleChatStreamingResponse(
			resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now(),
			[]byte(`{"model":"grok-4.6","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`),
		)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if strings.Contains(rec.Body.String(), `"reasoning_content":"…"`) {
			break
		}
		if time.Now().After(deadline) {
			_ = body.Close()
			t.Fatalf("expected Grok reasoning heartbeat before upstream cut, got %q", rec.Body.String())
		}
		select {
		case <-done:
			t.Fatalf("stream returned before heartbeat, body=%q", rec.Body.String())
		case <-time.After(50 * time.Millisecond):
		}
	}
	_ = body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not return after upstream cut")
	}
	require.Error(t, err)
	require.Contains(t, err.Error(), grokChatReasoningUpstreamCutCode)
	var failover *UpstreamFailoverError
	require.False(t, errors.As(err, &failover))
	require.NotNil(t, result)
	require.Contains(t, rec.Body.String(), `"reasoning_content":"…"`)
	require.Contains(t, rec.Body.String(), grokChatReasoningUpstreamCutCode)
	require.Contains(t, rec.Body.String(), grokChatReasoningUpstreamCutMsg)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
	require.NotContains(t, rec.Body.String(), OpenAIUpstreamStreamReadErrorCode)
}

func TestHandleChatStreamingResponse_GrokWindowFuse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	payload := "data: {\"type\":\"response.in_progress\",\"usage\":{\"input_tokens\":10,\"output_tokens\":10,\"output_tokens_details\":{\"reasoning_tokens\":200000}}}\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(payload)),
	}
	svc := &OpenAIGatewayService{cfg: &config.Config{}}
	account := &Account{ID: 12, Name: "grok-oauth", Platform: PlatformGrok, Type: AccountTypeOAuth}

	_, err := svc.handleChatStreamingResponse(
		resp, c, account, "grok-4.6", "grok-4.6", "grok-4.6", time.Now(),
		[]byte(`{"model":"grok-4.6","reasoning_effort":"xhigh","messages":[{"role":"user","content":"hi"}]}`),
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), grokChatReasoningWindowCode)
	require.Contains(t, rec.Body.String(), grokChatReasoningWindowCode)
	require.Contains(t, rec.Body.String(), "data: [DONE]")
}
