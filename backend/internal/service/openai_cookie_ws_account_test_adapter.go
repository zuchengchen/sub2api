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

// Admin connection/intelligence tests use the same verified Cookie pool. Their
// events stay in the existing TestEvent format and never mutate account health.
func (s *AccountTestService) testOpenAICookieWSAccountConnection(c *gin.Context, account *Account, model, prompt string) (retErr error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.Flush()
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	gateway := s.openaiGatewayService
	if gateway.openAICodexTicketBlocksAccount(account, model) {
		return s.sendErrorAndEnd(c, "No verified Cookie websocket is available for this account")
	}
	reserveCtx, reserveCancel := context.WithTimeout(ctx, gateway.openAIWSAcquireTimeout())
	slot, releaseSlot, err := gateway.reserveOpenAICookieWSSlot(reserveCtx, account, model, "")
	reserveCancel()
	if err != nil {
		return s.sendErrorAndEnd(c, "Cookie websocket capacity is unavailable; retry the test later")
	}
	defer releaseSlot()
	ctx = context.WithValue(ctx, openAICookieWSSlotContextKey{}, slot)
	token, _, err := gateway.GetAccessToken(ctx, account)
	if err != nil || token == "" {
		return s.sendErrorAndEnd(c, "Cookie websocket authentication is unavailable")
	}
	headers, _, err := gateway.buildOpenAIWSHeaders(ctx, c, account, token,
		OpenAIWSProtocolDecision{Transport: OpenAIUpstreamTransportResponsesWebsocketV2, Reason: "cookie_ws"},
		true, "", "", "", model, "")
	if err != nil {
		return s.sendErrorAndEnd(c, "No verified Cookie websocket is available for this account")
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
		return s.sendErrorAndEnd(c, "Account is busy; retry the test later")
	}
	defer func() { finishAccountTrafficTurn(permit, retErr) }()
	lease, err := gateway.getOpenAIWSConnPool().Acquire(trafficCtx, openAIWSAcquireRequest{
		Account: account, WSURL: strings.Replace(chatgptCodexURL, "https://", "wss://", 1), Headers: headers,
	})
	if err != nil {
		return s.sendErrorAndEnd(c, "Cookie websocket connection failed")
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
	if err := lease.WriteJSONContext(trafficCtx, payload); err != nil {
		return s.sendErrorAndEnd(c, "Cookie websocket request failed")
	}
	total, outputBytes := 0, 0
	probeObserver := openAICookieWSProbeObserver{enabled: openAICookieWSIsProbePayload(payload)}
	hasDelta := false
	for {
		message, err := lease.ReadMessageContext(trafficCtx)
		if err != nil {
			return s.sendErrorAndEnd(c, "Cookie websocket stream ended before completion")
		}
		total += len(message)
		if total > intelligentCaptureUpstreamLimit {
			return s.sendErrorAndEnd(c, "Account test response is too large")
		}
		if capture != nil {
			_, _ = capture.Write(append(append([]byte("data: "), message...), '\n', '\n'))
		}
		eventType := gjson.GetBytes(message, "type").String()
		probeObserver.observe(lease, message, eventType)
		switch eventType {
		case "response.output_text.delta":
			text := gjson.GetBytes(message, "delta").String()
			outputBytes += len(text)
			if outputBytes > intelligentCaptureTextLimit {
				return s.sendErrorAndEnd(c, "Account test output is too large")
			}
			if text != "" {
				hasDelta = true
				s.sendEvent(c, TestEvent{Type: "content", Text: text})
			}
		case "response.completed":
			if _, ok := openAICodexSuccessfulCompletionModel(message, "response.completed"); !ok {
				return s.sendErrorAndEnd(c, "Cookie websocket response was not completed successfully")
			}
			if !hasDelta {
				for _, item := range gjson.GetBytes(message, "response.output").Array() {
					for _, part := range item.Get("content").Array() {
						if part.Get("type").String() == "output_text" {
							text := part.Get("text").String()
							outputBytes += len(text)
							if outputBytes > intelligentCaptureTextLimit {
								return s.sendErrorAndEnd(c, "Account test output is too large")
							}
							s.sendEvent(c, TestEvent{Type: "content", Text: text})
						}
					}
				}
			}
			complete = true
			s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
			return nil
		case "error", "response.failed", "response.incomplete", "response.cancelled":
			return s.sendErrorAndEnd(c, "Cookie websocket returned an unsuccessful response")
		}
	}
}
