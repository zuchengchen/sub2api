package service

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Upstream work and downstream delivery are independent replay boundaries.
// Keep only a monotonic bit: no payload is retained, and a later sparse/zero
// terminal cannot erase already observed work or metered usage.
type openAIResponseReplayGuard struct{ progressed bool }

func (g *openAIResponseReplayGuard) Observe(payload []byte, eventType string) {
	if g == nil || g.progressed {
		return
	}
	if trimmed := bytes.TrimSpace(payload); len(trimmed) == 0 || bytes.Equal(trimmed, []byte("[DONE]")) {
		return
	}
	// Malformed fragments and transport heartbeats are not evidence of model
	// work. Downstream commitment is still checked separately by each caller.
	if !gjson.ValidBytes(payload) {
		return
	}
	eventType = strings.TrimSpace(eventType)
	if eventType != "ping" && eventType != "error" && !openAIStreamEventTypeIsTerminal(eventType) &&
		(openAIStreamDataStartsClientOutput(string(payload), eventType) || openAIStreamDataStartsVisibleOutput(string(payload), eventType)) {
		g.progressed = true
		return
	}
	if bytes.Contains(payload, []byte(`"usage"`)) {
		for _, path := range []string{"usage", "response.usage", "data.usage", "data.response.usage"} {
			if usage, ok := openAIUsageFromGJSON(gjson.GetBytes(payload, path)); ok && openAIUsageHasTokens(&usage) {
				g.progressed = true
				return
			}
		}
	}
	// Buffered terminals can contain output that was never sent as deltas.
	if bytes.Contains(payload, []byte(`"output"`)) {
		for _, path := range []string{"response.output", "output"} {
			if output := gjson.GetBytes(payload, path); output.IsArray() && len(output.Array()) > 0 {
				g.progressed = true
				return
			}
		}
	}
	if bytes.Contains(payload, []byte(`"tool_usage"`)) {
		for _, path := range []string{"response.tool_usage", "tool_usage"} {
			if usage := gjson.GetBytes(payload, path); usage.IsObject() && len(usage.Map()) > 0 {
				g.progressed = true
				return
			}
		}
	}
}

func (g *openAIResponseReplayGuard) CanReplay(c *gin.Context) bool {
	if (g != nil && g.progressed) || GetOpsCyberPolicy(c) != nil {
		return false
	}
	return c == nil || c.Request == nil || c.Request.Context().Err() == nil
}

func openAIResponseReplayGuardForSSE(body string) openAIResponseReplayGuard {
	var guard openAIResponseReplayGuard
	forEachOpenAISSEFrame(body, func(eventType string, payload []byte) {
		guard.Observe(payload, eventType)
	})
	return guard
}

// Preserve only already known usage. This does not introduce estimates or
// change pricing, and cyber-policy usage keeps its existing settlement path.
func openAICompatPartialForwardResult(c *gin.Context, resp *http.Response, usage OpenAIUsage, responseID, originalModel, billingModel, upstreamModel string, startTime time.Time) *OpenAIForwardResult {
	if !openAIUsageHasTokens(&usage) || GetOpsCyberPolicy(c) != nil {
		return nil
	}
	result := &OpenAIForwardResult{
		ResponseID: responseID, Usage: usage, Model: originalModel,
		BillingModel: billingModel, UpstreamModel: upstreamModel,
		UpstreamResponseModel:         observedUpstreamResponseModel(c),
		UpstreamResponseModelConflict: observedUpstreamResponseModelConflict(c),
		UpstreamResponseServiceTier:   observedUpstreamResponseServiceTier(c),
		Duration:                      time.Since(startTime),
	}
	if resp != nil {
		result.RequestID, result.UpstreamHeaders = resp.Header.Get("x-request-id"), resp.Header
	}
	return result
}

// Record a fixed vocabulary only. Raw transport errors can contain destination
// URLs/addresses; neither they nor response/request payloads belong here.
func recordOpenAIStreamReadFailure(c *gin.Context, account *Account, upstreamRequestID string, cause error) {
	if c == nil || c.Request == nil || !shouldClassifyOpenAIUpstreamStreamReadError(cause, c.Request.Context()) {
		return
	}
	code, message := classifyOpenAIUpstreamStreamReadError(cause)
	reason := code
	var netErr net.Error
	switch {
	case errors.As(cause, &netErr) && netErr.Timeout():
		reason = "upstream_stream_read_timeout"
	case errors.Is(cause, io.ErrUnexpectedEOF):
		reason = "upstream_stream_unexpected_eof"
	case errors.Is(cause, net.ErrClosed):
		reason = "upstream_stream_connection_closed"
	}
	event := OpsUpstreamErrorEvent{
		ProxyID: opsUpstreamProxyID(account), ProxyName: opsUpstreamProxyName(account),
		Platform: PlatformOpenAI, UpstreamStatusCode: http.StatusBadGateway,
		UpstreamRequestID: strings.TrimSpace(upstreamRequestID),
		Kind:              "stream_read_error", Stage: "response_body", Reason: reason, Message: message,
	}
	if account != nil {
		event.Platform, event.AccountID, event.AccountName = account.Platform, account.ID, account.Name
	}
	setOpsUpstreamError(c, http.StatusBadGateway, message, "")
	appendOpsUpstreamError(c, event)
}

// Match parseSSEUsageBytesWithType: a zero terminal must not erase metered
// progress from an earlier frame. A nonzero terminal remains authoritative.
func retainOpenAITerminalUsage(dst *OpenAIUsage, terminal OpenAIUsage) {
	if dst != nil && (openAIUsageHasTokens(&terminal) || !openAIUsageHasTokens(dst)) {
		*dst = terminal
	}
}
