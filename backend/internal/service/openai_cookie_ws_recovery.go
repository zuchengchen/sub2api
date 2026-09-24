package service

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Recovery diagnostics describe this process only. Cookies, credentials, raw
// upstream errors, response bodies and headers must never enter this DTO.
type OpenAICookieWSRecoveryDiagnostic struct {
	Phase         string                       `json:"phase"`
	Attempts      int                          `json:"attempts"`
	LastAttemptAt *time.Time                   `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time                   `json:"last_success_at,omitempty"`
	LastFailureAt *time.Time                   `json:"last_failure_at,omitempty"`
	NextAttemptAt *time.Time                   `json:"next_attempt_at,omitempty"`
	LastError     *OpenAICookieWSRecoveryError `json:"last_error,omitempty"`
}

type OpenAICookieWSRecoveryError struct {
	Stage      string `json:"stage"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

type openAICookieWSRecoveryKey struct {
	accountID int64
	slot      int
	operation string
}

type openAICookieWSRecoveryState struct {
	mu         sync.Mutex
	diagnostic OpenAICookieWSRecoveryDiagnostic
}

func (s *OpenAIGatewayService) openAICookieWSRecoveryState(accountID int64, slot int, operation string) *openAICookieWSRecoveryState {
	if s == nil || accountID <= 0 || slot < 0 || slot >= openAICookieWSSlotCount || (operation != "refresh" && operation != "warmup") {
		return nil
	}
	key := openAICookieWSRecoveryKey{accountID, slot, operation}
	state, _ := s.openaiCookieWSRecovery.LoadOrStore(key, &openAICookieWSRecoveryState{diagnostic: OpenAICookieWSRecoveryDiagnostic{Phase: "waiting"}})
	return state.(*openAICookieWSRecoveryState)
}

func (s *OpenAIGatewayService) openAICookieWSRecoverySnapshot(accountID int64, slot int, operation string) *OpenAICookieWSRecoveryDiagnostic {
	if s == nil {
		return nil
	}
	raw, ok := s.openaiCookieWSRecovery.Load(openAICookieWSRecoveryKey{accountID, slot, operation})
	if !ok {
		return nil
	}
	state := raw.(*openAICookieWSRecoveryState)
	state.mu.Lock()
	defer state.mu.Unlock()
	copy := state.diagnostic
	if copy.Phase == "backoff" && copy.NextAttemptAt != nil && !time.Now().Before(*copy.NextAttemptAt) {
		copy.Phase = "waiting"
	}
	cloneTime := func(value *time.Time) *time.Time {
		if value == nil {
			return nil
		}
		copy := *value
		return &copy
	}
	copy.LastAttemptAt = cloneTime(copy.LastAttemptAt)
	copy.LastSuccessAt = cloneTime(copy.LastSuccessAt)
	copy.LastFailureAt = cloneTime(copy.LastFailureAt)
	copy.NextAttemptAt = cloneTime(copy.NextAttemptAt)
	if copy.LastError != nil {
		errCopy := *copy.LastError
		copy.LastError = &errCopy
	}
	return &copy
}

func (s *OpenAIGatewayService) beginOpenAICookieWSRecovery(accountID int64, slot int, operation, phase string) {
	state := s.openAICookieWSRecoveryState(accountID, slot, operation)
	if state == nil {
		return
	}
	now := time.Now()
	state.mu.Lock()
	state.diagnostic.Phase = phase
	state.diagnostic.Attempts++
	state.diagnostic.LastAttemptAt = &now
	state.diagnostic.NextAttemptAt = nil
	state.mu.Unlock()
}

func (s *OpenAIGatewayService) phaseOpenAICookieWSRecovery(accountID int64, slot int, operation, phase string, next *time.Time) {
	state := s.openAICookieWSRecoveryState(accountID, slot, operation)
	if state == nil {
		return
	}
	state.mu.Lock()
	state.diagnostic.Phase = phase
	state.diagnostic.NextAttemptAt = nil
	if next != nil {
		value := *next
		state.diagnostic.NextAttemptAt = &value
	}
	state.mu.Unlock()
}

func (s *OpenAIGatewayService) failOpenAICookieWSRecovery(accountID int64, slot int, operation, phase string, failure *openAICookieWSRecoveryFailure, next *time.Time) {
	state := s.openAICookieWSRecoveryState(accountID, slot, operation)
	if state == nil || failure == nil {
		return
	}
	now := time.Now()
	state.mu.Lock()
	state.diagnostic.Phase = phase
	state.diagnostic.LastFailureAt = &now
	detail := failure.detail
	state.diagnostic.LastError = &detail
	state.diagnostic.NextAttemptAt = nil
	if next != nil {
		value := *next
		state.diagnostic.NextAttemptAt = &value
	}
	attempts := state.diagnostic.Attempts
	state.mu.Unlock()
	fields := []zap.Field{zap.Int64("account_id", accountID), zap.Int("slot", slot), zap.String("operation", operation),
		zap.String("phase", phase), zap.Int("attempts", attempts), zap.String("stage", detail.Stage),
		zap.String("code", detail.Code), zap.String("message", detail.Message), zap.Int("http_status", detail.HTTPStatus)}
	if next != nil {
		fields = append(fields, zap.Time("next_attempt_at", *next))
	}
	logger.L().Info("openai_cookie_ws recovery failed", fields...)
}

func (s *OpenAIGatewayService) succeedOpenAICookieWSRecovery(accountID int64, slot int, operation string) {
	state := s.openAICookieWSRecoveryState(accountID, slot, operation)
	if state == nil {
		return
	}
	now := time.Now()
	state.mu.Lock()
	state.diagnostic.Phase = "idle"
	state.diagnostic.LastSuccessAt = &now
	state.diagnostic.LastError = nil
	state.diagnostic.NextAttemptAt = nil
	state.mu.Unlock()
}

// Only a safe sentinel and a parsed retry time survive error classification.
// In particular, Unwrap cannot reveal a URL, proxy password or response body.
type openAICookieWSRecoveryFailure struct {
	detail      OpenAICookieWSRecoveryError
	cause       error
	retryAt     *time.Time
	retryStatus int
}

func (e *openAICookieWSRecoveryFailure) Error() string { return e.detail.Message }
func (e *openAICookieWSRecoveryFailure) Unwrap() error { return e.cause }
func (e *openAICookieWSRecoveryFailure) retryHeaders() http.Header {
	if e.retryAt == nil {
		return nil
	}
	return http.Header{"Retry-After": []string{e.retryAt.UTC().Format(http.TimeFormat)}}
}

func cookieWSRecoveryFailure(stage, code, message string, status int, cause error) *openAICookieWSRecoveryFailure {
	return &openAICookieWSRecoveryFailure{detail: OpenAICookieWSRecoveryError{Stage: stage, Code: code, Message: message, HTTPStatus: status}, cause: cause, retryStatus: status}
}

func cookieWSRecoveryStatusFailure(stage string, status int) *openAICookieWSRecoveryFailure {
	if status == http.StatusProxyAuthRequired {
		return cookieWSRecoveryFailure(stage, "cookie_ws_proxy_authentication", "Cookie recovery proxy authentication was rejected (HTTP 407)", status, nil)
	}
	return cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_http_"+fmt.Sprint(status),
		fmt.Sprintf("Cookie recovery %s was rejected (HTTP %d)", cookieWSRecoveryStageLabel(stage), status), status, nil)
}

func cookieWSRecoveryOperationError(stage string, err error, status int, headers http.Header) *openAICookieWSRecoveryFailure {
	var existing *openAICookieWSRecoveryFailure
	if errors.As(err, &existing) {
		copy := *existing
		if copy.detail.HTTPStatus == 0 {
			copy.detail.HTTPStatus = status
		}
		if copy.retryStatus == 0 {
			copy.retryStatus = status
		}
		if copy.retryAt == nil {
			copy.retryAt = parseRetryAfterResetTime(headers, time.Now())
		}
		return &copy
	}
	var dialErr *openAIWSDialError
	if errors.As(err, &dialErr) {
		stage = "ws_handshake"
		if dialErr.StatusCode >= 100 && dialErr.StatusCode <= 599 {
			status = dialErr.StatusCode
		}
		headers = dialErr.ResponseHeaders
	}
	var failure *openAICookieWSRecoveryFailure
	var tested *openAICookieWSTestError
	if errors.As(err, &tested) {
		failure = cookieWSRecoveryFailure(stage, tested.Code, tested.Message, tested.HTTPStatus, tested.cause)
		if strings.HasPrefix(tested.Code, "cookie_ws_validation_") {
			failure.detail.Stage = "ws_validation"
		}
	} else if status >= 400 && status <= 599 && stage != "http_stream" {
		failure = cookieWSRecoveryStatusFailure(stage, status)
	} else {
		label := cookieWSRecoveryStageLabel(stage)
		var dnsErr *net.DNSError
		var tlsErr *tls.CertificateVerificationError
		var certErr x509.UnknownAuthorityError
		var netErr net.Error
		// Some SOCKS/CONNECT errors lack a typed cause. Use their known markers
		// only for classification; never retain or expose any part of the text.
		message := ""
		if err != nil {
			message = strings.ToLower(err.Error())
		}
		switch {
		case errors.Is(err, context.Canceled):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_cancelled", "Cookie recovery "+label+" was cancelled", status, context.Canceled)
		case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_timeout", "Cookie recovery "+label+" timed out", status, context.DeadlineExceeded)
		case errors.Is(err, errOpenAICookieWSAccountUnavailable):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_account_unavailable", "Cookie recovery account is unavailable or temporarily unschedulable", status, errOpenAICookieWSAccountUnavailable)
		case stage == "http_stream":
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_http_stream_failed", "Cookie recovery HTTP response stream could not be read completely", status, nil)
		case errors.As(err, &dnsErr):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_dns", "Cookie recovery "+label+" could not resolve the server name", status, nil)
		case strings.Contains(message, "proxy authentication required") || strings.Contains(message, "username/password authentication failed") || strings.Contains(message, "socks authentication failed"):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_proxy_authentication", "Cookie recovery proxy authentication failed", status, nil)
		case strings.Contains(message, "proxyconnect") || strings.Contains(message, "socks connect"):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_proxy_connection", "Cookie recovery could not connect through the configured proxy", status, nil)
		case errors.As(err, &tlsErr) || errors.As(err, &certErr) || strings.Contains(message, "tls:") || strings.Contains(message, "tls handshake") || strings.Contains(message, "x509:"):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_tls", "Cookie recovery "+label+" failed during TLS negotiation", status, nil)
		case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, errOpenAIWSConnClosed):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_closed", "Cookie recovery "+label+" closed before completion", status, nil)
		case errors.Is(err, syscall.ECONNRESET):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_connection_reset", "Cookie recovery "+label+" connection was reset", status, nil)
		case errors.Is(err, syscall.ECONNREFUSED):
			failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_connection_refused", "Cookie recovery "+label+" connection was refused", status, nil)
		default:
			tested = cookieWSTestOperationError("acquire", err)
			if tested.Code != "cookie_ws_acquire_failed" && tested.Code != "cookie_ws_acquire_closed" {
				failure = cookieWSRecoveryFailure(stage, tested.Code, tested.Message, status, tested.cause)
			} else {
				failure = cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_failed", "Cookie recovery "+label+" failed", status, nil)
			}
		}
	}
	failure.retryAt = parseRetryAfterResetTime(headers, time.Now())
	return failure
}

func cookieWSRecoveryStageLabel(stage string) string {
	switch stage {
	case "account":
		return "account availability check"
	case "authentication":
		return "account authentication"
	case "account_headers":
		return "account identity preparation"
	case "http_request":
		return "HTTP connection"
	case "http_stream":
		return "HTTP response stream"
	case "http_validation":
		return "HTTP validation probe"
	case "ws_handshake":
		return "websocket handshake"
	case "ws_acquire":
		return "websocket acquisition"
	case "ws_write":
		return "websocket probe write"
	case "ws_read":
		return "websocket probe read"
	case "ws_validation":
		return "websocket validation probe"
	case "persistence":
		return "Cookie persistence"
	default:
		return "operation"
	}
}

func cookieWSRecoveryObservationError(stage string, observation *openAICookieWSObservation, status int) *openAICookieWSRecoveryFailure {
	if observation.failureStatus >= 400 && observation.failureStatus <= 599 {
		failure := cookieWSRecoveryStatusFailure(stage, observation.failureStatus)
		// A status carried inside a successful HTTP/WS stream is useful for
		// diagnosis, but must not change the existing transport retry policy.
		failure.retryStatus = status
		return failure
	}
	if observation.completed && observation.successfulCompletion && !observation.modelMatch {
		return cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_model_mismatch", "Cookie recovery probe response did not match the required model", status, nil)
	}
	if !observation.completed || observation.failed {
		return cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_incomplete", "Cookie recovery probe did not complete successfully", status, nil)
	}
	return cookieWSRecoveryFailure(stage, "cookie_ws_"+stage+"_not_true", "Cookie recovery probe did not return True", status, nil)
}

func (s *OpenAIGatewayService) deferOpenAICookieWSRecovery(accountID int64, slot int, reason string, failure *openAICookieWSRecoveryFailure) {
	s.noteOpenAICookieWSSlotMiss(accountID, slot, time.Now(), reason, failure.retryStatus, failure.retryHeaders())
	var next *time.Time
	if raw, ok := s.openaiCookieWSRetry.Load(openAICookieWSKeySlot(accountID, openAICodexTicketDefaultModel, slot)); ok {
		if retry, ok := raw.(*openAICookieWSRetryState); ok {
			next = &retry.nextAttemptAt
		}
	}
	s.failOpenAICookieWSRecovery(accountID, slot, "refresh", "backoff", failure, next)
}
