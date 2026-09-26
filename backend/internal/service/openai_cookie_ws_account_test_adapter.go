package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	accountTestCookieWSHTTPFallbackKey     = "account_test_cookie_ws_http_fallback"
	accountTestCookieWSHTTPFallbackMessage = "Cookie websocket is not ready; testing via HTTP /responses"
)

var errAccountTestCookieWSHTTPFallback = errors.New("cookie websocket account test fallback to HTTP")

func (s *AccountTestService) cookieWSHTTPFallbackOrError(c *gin.Context, account *Account, stage string, err error) error {
	classified := cookieWSTestOperationError(stage, err)
	if intelligentContext(c.Request.Context()) == nil &&
		(shouldOpenAICookieWSHTTPFallback(err) || shouldOpenAICookieWSHTTPFallback(classified)) {
		c.Set(accountTestCookieWSHTTPFallbackKey, true)
		s.sendEvent(c, TestEvent{Type: "content", Text: accountTestCookieWSHTTPFallbackMessage})
		return errAccountTestCookieWSHTTPFallback
	}
	return s.sendCookieWSTestError(c, account, stage, err)
}

// Admin connection/intelligence tests use the same verified Cookie pool when a
// process-ready ticket exists. Otherwise they follow live traffic onto HTTP
// /responses. Events stay in TestEvent format and never mutate account health.
func (s *AccountTestService) testOpenAICookieWSAccountConnection(c *gin.Context, account *Account, model, prompt string) (retErr error) {
	ctx := c.Request.Context()
	// Intelligent runs already own their configured deadline (up to 600s).
	// A connectivity test keeps its shorter, independent default deadline.
	if intelligentContext(ctx) == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
	}
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	gateway := s.openaiGatewayService
	if !gateway.openAICookieWSHasReadyTicket(account, model) {
		return s.cookieWSHTTPFallbackOrError(c, account, "acquire", ErrOpenAICodexTicketUnavailable)
	}
	reserveCtx, reserveCancel := context.WithTimeout(ctx, gateway.openAIWSAcquireTimeout())
	slot, releaseSlot, err := gateway.reserveOpenAICookieWSSlot(reserveCtx, account, model, "")
	reserveCancel()
	if err != nil {
		return s.cookieWSHTTPFallbackOrError(c, account, "reserve", err)
	}
	defer releaseSlot()
	ctx = context.WithValue(ctx, openAICookieWSSlotContextKey{}, slot)
	token, _, err := gateway.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return s.cookieWSHTTPFallbackOrError(c, account, "authentication", err)
	}
	headers, _, err := gateway.buildOpenAIWSHeaders(ctx, c, account, token,
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2, Reason: "cookie_ws"},
		true, "", "", "", model, "")
	if err != nil {
		return s.cookieWSHTTPFallbackOrError(c, account, "acquire", err)
	}
	setOpenAICookieWSExecutionScope(headers, c, "admin-account-test")
	payload := openAICookieWSProbePayload(true)
	text := strings.TrimSpace(prompt)
	if text == "" {
		text = "hi"
	}
	payload["input"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}
	applyIntelligentPayloadPrompt(ctx, payload)
	applyOpenAICookieWSMetadataFromHeaders(headers, payload)
	encoded, _ := json.Marshal(payload)
	if err := checkAccountRequestIntegrity(c, account, nil, encoded); err != nil {
		return s.sendErrorAndEnd(c, "Account test request integrity check failed")
	}
	trafficCtx, permit, err := beginAccountTrafficTurn(ctx, s.httpUpstream, account)
	if err != nil {
		if run := intelligentContext(ctx); run != nil {
			var wait *AccountTrafficLimitError
			if errors.As(err, &wait) {
				run.capture.trafficWait = wait
			}
		}
		return s.cookieWSHTTPFallbackOrError(c, account, "reserve", err)
	}
	defer func() { finishAccountTrafficTurn(permit, retErr) }()
	lease, err := gateway.getOpenAIWSConnPool().Acquire(trafficCtx, openAIWSAcquireRequest{
		Account: account, WSURL: strings.Replace(chatgptCodexURL, "https://", "wss://", 1), Headers: headers,
	})
	if err != nil {
		return s.cookieWSHTTPFallbackOrError(c, account, "acquire", err)
	}
	complete := false
	defer func() {
		if !complete {
			lease.MarkBroken()
		}
		lease.Release()
	}()
	var capture *intelligentCapture
	if run := intelligentContext(ctx); run != nil {
		capture = run.capture
		capture.mu.Lock()
		capture.status = http.StatusOK
		capture.secrets = append(capture.secrets, token, headers.Get("Cookie"))
		capture.mu.Unlock()
	}
	collector := capture
	if collector == nil {
		collector = &intelligentCapture{}
	}
	if err := lease.WriteJSONContext(trafficCtx, payload); err != nil {
		return s.sendCookieWSTestError(c, account, "write", err)
	}
	total, outputBytes := 0, 0
	probeObserver := openAICookieWSProbeObserver{enabled: openAICookieWSIsProbePayload(payload)}
	var emitted strings.Builder
	for {
		message, err := lease.ReadMessageContext(trafficCtx)
		if err != nil {
			return s.sendCookieWSTestError(c, account, "read", err)
		}
		total += len(message)
		if total > intelligentCaptureUpstreamLimit {
			return s.sendErrorAndEnd(c, "Account test response is too large")
		}
		_, _ = collector.Write(append(append([]byte("data: "), message...), '\n', '\n'))
		eventType := gjson.GetBytes(message, "type").String()
		probeEventType := eventType
		if probeEventType == "response.done" {
			probeEventType = "response.completed"
		}
		probeObserver.observe(lease, message, probeEventType)
		switch eventType {
		case "response.output_text.delta":
			text := gjson.GetBytes(message, "delta").String()
			outputBytes += len(text)
			if outputBytes > intelligentCaptureTextLimit {
				return s.sendErrorAndEnd(c, "Account test output is too large")
			}
			if text != "" && capture == nil {
				emitted.WriteString(text)
				s.sendEvent(c, TestEvent{Type: "content", Text: text})
			}
		case "response.completed", "response.done":
			if _, ok := openAICodexSuccessfulCompletionModel(message, "response.completed"); !ok || !collector.upstreamComplete() {
				if status := cookieWSTestPayloadStatus(message); status >= 400 {
					return s.sendCookieWSTestError(c, account, "response", cookieWSTestStatusError("response", status))
				}
				return s.sendErrorAndEnd(c, "Cookie websocket response was not completed successfully")
			}
			finalText := collector.outputText()
			if !strings.HasPrefix(finalText, emitted.String()) {
				// TestEvent content is append-only. A normal live connectivity
				// view cannot replace text already sent without client support.
				return s.sendErrorAndEnd(c, "Cookie websocket final output differs from the streamed text")
			}
			if suffix := strings.TrimPrefix(finalText, emitted.String()); suffix != "" {
				s.sendEvent(c, TestEvent{Type: "content", Text: suffix})
			}
			complete = true
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		case "error", "response.failed", "response.incomplete", "response.cancelled", "response.canceled":
			if status := cookieWSTestPayloadStatus(message); status >= 400 {
				return s.sendCookieWSTestError(c, account, "response", cookieWSTestStatusError("response", status))
			}
			return s.sendErrorAndEnd(c, "Cookie websocket returned an unsuccessful response")
		}
	}
}
