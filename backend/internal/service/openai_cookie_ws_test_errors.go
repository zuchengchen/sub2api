package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Codes and messages are local allowlisted descriptions. Never retain the raw
// dial/probe error, response body, headers or URLs in this public test error.
type openAICookieWSTestError struct {
	Code       string
	Category   string
	Message    string
	HTTPStatus int
	cause      error // only safe sentinels used by errors.Is
}

func (e *openAICookieWSTestError) Error() string { return e.Message }
func (e *openAICookieWSTestError) Unwrap() error { return e.cause }

func newCookieWSTestError(code, category, message string, status int, cause error) *openAICookieWSTestError {
	return &openAICookieWSTestError{Code: code, Category: category, Message: message, HTTPStatus: status, cause: cause}
}

func cookieWSTestStatusError(stage string, status int) *openAICookieWSTestError {
	category := "request_error"
	switch {
	case status == http.StatusTooManyRequests:
		category = "rate_limited"
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		category = "account_error"
	case status == http.StatusNotFound:
		category = "model_error"
	case status >= 500:
		category = "network_error"
	}
	label := "upstream request"
	if stage == "handshake" {
		label = "handshake"
	} else if stage == "validation" {
		label = "validation probe"
	}
	return newCookieWSTestError("cookie_ws_"+stage+"_http_"+fmt.Sprint(status), category,
		fmt.Sprintf("Cookie websocket %s was rejected (HTTP %d)", label, status), status, nil)
}

func cookieWSTestPayloadStatus(payload []byte) int {
	if !gjson.ValidBytes(payload) {
		return 0
	}
	for _, path := range []string{"response.error.status_code", "response.error.status", "error.status_code", "error.status", "status_code", "status"} {
		status := int(gjson.GetBytes(payload, path).Int())
		if status >= 400 && status <= 599 {
			return status
		}
	}
	status := openAIStreamFailureStatus(payload, extractOpenAISSEErrorMessage(payload))
	if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusTooManyRequests || status == 529 {
		return status
	}
	return openAIWSPayloadTransientStatus(payload)
}

func cookieWSTestOperationError(stage string, err error) *openAICookieWSTestError {
	var classified *openAICookieWSTestError
	if errors.As(err, &classified) {
		return classified
	}
	var dialErr *openAIWSDialError
	if errors.As(err, &dialErr) && dialErr.StatusCode >= 100 && dialErr.StatusCode <= 599 {
		return cookieWSTestStatusError("handshake", dialErr.StatusCode)
	}
	if errors.Is(err, context.Canceled) {
		return newCookieWSTestError("cookie_ws_cancelled", "failed", "Cookie websocket test was cancelled", 0, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if stage == "reserve" {
			return newCookieWSTestError("cookie_ws_capacity_timeout", "rate_limited", "Cookie websocket capacity remained busy until the wait deadline", 0, context.DeadlineExceeded)
		}
		return newCookieWSTestError("cookie_ws_"+stage+"_timeout", "network_error", "Cookie websocket "+cookieWSTestStageLabel(stage)+" timed out", 0, context.DeadlineExceeded)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return newCookieWSTestError("cookie_ws_"+stage+"_timeout", "network_error", "Cookie websocket "+cookieWSTestStageLabel(stage)+" timed out", 0, nil)
	}
	var trafficErr *AccountTrafficLimitError
	if errors.Is(err, errOpenAIWSConnQueueFull) || errors.As(err, &trafficErr) {
		return newCookieWSTestError("cookie_ws_capacity_busy", "rate_limited", "Cookie websocket account capacity is busy", 0, errOpenAIWSConnQueueFull)
	}
	if errors.Is(err, errOpenAICookieWSAccountUnavailable) {
		return newCookieWSTestError("cookie_ws_account_unavailable", "account_error", "Cookie websocket account is unavailable or temporarily unschedulable", 0, errOpenAICookieWSAccountUnavailable)
	}
	if errors.Is(err, errOpenAIWSCookieValidatorMissing) {
		return newCookieWSTestError("cookie_ws_validator_unavailable", "failed", "Cookie websocket validator is unavailable", 0, errOpenAIWSCookieValidatorMissing)
	}
	if errors.Is(err, errOpenAIWSCookieExpired) {
		return newCookieWSTestError("cookie_ws_cookie_expired", "account_error", "Cookie websocket Cookie has expired", 0, errOpenAIWSCookieExpired)
	}
	if errors.Is(err, errOpenAIWSCookieRetired) {
		return newCookieWSTestError("cookie_ws_cookie_retired", "account_error", "Cookie websocket Cookie was replaced and is no longer assignable", 0, errOpenAIWSCookieRetired)
	}
	if errors.Is(err, ErrOpenAICodexTicketUnavailable) {
		return newCookieWSTestError("cookie_ws_cookie_unavailable", "account_error", "No verified Cookie websocket is available for this account", 0, ErrOpenAICodexTicketUnavailable)
	}
	var failover *UpstreamFailoverError
	if errors.As(err, &failover) && failover.StatusCode == http.StatusServiceUnavailable &&
		(stage == "reserve" || failover.ClientMessage == "No verified Cookie websocket is available for this account") {
		return cookieWSTestOperationError(stage, ErrOpenAICodexTicketUnavailable)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errOpenAIWSConnClosed) {
		return newCookieWSTestError("cookie_ws_"+stage+"_closed", "network_error", "Cookie websocket closed during "+cookieWSTestStageLabel(stage)+" before completion", 0, nil)
	}
	if stage == "authentication" {
		return newCookieWSTestError("cookie_ws_authentication_unavailable", "account_error", "Cookie websocket account authentication is unavailable", 0, nil)
	}
	return newCookieWSTestError("cookie_ws_"+stage+"_failed", "network_error", "Cookie websocket "+cookieWSTestStageLabel(stage)+" failed", 0, nil)
}

func cookieWSTestStageLabel(stage string) string {
	switch stage {
	case "reserve":
		return "capacity wait"
	case "acquire":
		return "connection acquisition"
	case "validation":
		return "validation probe"
	case "write":
		return "request write"
	case "read":
		return "response stream"
	case "authentication":
		return "authentication"
	default:
		return "request"
	}
}

func (s *AccountTestService) sendCookieWSTestError(c *gin.Context, account *Account, stage string, err error) error {
	detail := cookieWSTestOperationError(stage, err)
	if detail.Code == "cookie_ws_account_unavailable" && s.openaiGatewayService != nil && account != nil {
		current := account
		if s.openaiGatewayService.accountRepo != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
			latest, readErr := s.openaiGatewayService.accountRepo.GetByID(ctx, account.ID)
			cancel()
			if readErr == nil && latest != nil {
				current = latest
			}
		}
		reason := openAICookieWSAccountSkipReason(current, time.Now())
		if reason == "rate_limited" || reason == "model_rate_limited" || strings.HasPrefix(reason, "quota_") {
			detail = newCookieWSTestError("cookie_ws_account_rate_limited", "rate_limited", "Cookie websocket account is rate limited or its quota is exhausted", 0, errOpenAICookieWSAccountUnavailable)
		}
	}
	s.sendEvent(c, TestEvent{Type: "error", Code: detail.Code, Error: detail.Message})
	return detail
}
