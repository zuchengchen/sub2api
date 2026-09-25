package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

// Only the exact standalone Tibo probe can retire a connection for a non-True
// answer. Ordinary business answers (including a literal False) are untouched.
func openAICookieWSIsProbePayloadRaw(payload []byte) bool {
	if !gjson.ValidBytes(payload) || gjson.GetBytes(payload, "model").String() != openAICodexTicketDefaultModel ||
		strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String()) != "" ||
		strings.TrimSpace(gjson.GetBytes(payload, "instructions").String()) != "" ||
		len(gjson.GetBytes(payload, "tools").Array()) != 0 {
		return false
	}
	input := gjson.GetBytes(payload, "input")
	if input.Type == gjson.String {
		return input.String() == openAICookieWSProbePrompt
	}
	items := input.Array()
	if len(items) != 1 || items[0].Get("role").String() != "user" {
		return false
	}
	content := items[0].Get("content")
	if content.Type == gjson.String {
		return content.String() == openAICookieWSProbePrompt
	}
	parts := content.Array()
	return len(parts) == 1 && parts[0].Get("type").String() == "input_text" && parts[0].Get("text").String() == openAICookieWSProbePrompt
}

func openAICookieWSIsProbePayload(payload map[string]any) bool {
	raw, err := json.Marshal(payload)
	return err == nil && openAICookieWSIsProbePayloadRaw(raw)
}

type openAICookieWSProbeObserver struct {
	enabled     bool
	observation openAICookieWSObservation
}

func (o *openAICookieWSProbeObserver) observe(lease *openAIWSConnLease, payload []byte, eventType string) {
	if o == nil || !o.enabled {
		return
	}
	o.observation.event(payload, eventType)
	// Retire only after the terminal event has been read. Its bytes remain
	// available to the normal output and usage collectors; never replay it.
	if isOpenAIWSTerminalEvent(eventType) && (!o.observation.completed || o.observation.failed || !o.observation.trueAnswer || !o.observation.modelMatch) {
		lease.MarkBroken()
	}
}

// validateOpenAICookieWSBusinessConn verifies every newly opened business
// socket before the pool publishes it. These internal probe events never enter
// the client's response stream or business usage accounting.
func (s *OpenAIGatewayService) validateOpenAICookieWSBusinessConn(ctx context.Context, account *Account, lease *openAIWSConnLease) (err error) {
	if s == nil || account == nil || lease == nil {
		return errors.New("Cookie websocket validation is unavailable")
	}
	if s.cookieWSSkipBusinessProbe {
		return nil
	}
	verified := false
	defer func() {
		if !verified {
			lease.MarkBroken()
			if err != nil {
				err = errors.Join(ErrOpenAICodexTicketUnavailable, err)
			}
		}
	}()
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	current, eligibilityErr := s.latestOpenAICookieWSAccount(probeCtx, account.ID)
	if eligibilityErr != nil {
		return errOpenAICookieWSAccountUnavailable
	}
	account = current
	if writeErr := lease.WriteJSONContext(probeCtx, openAICookieWSProbePayload(true)); writeErr != nil {
		return cookieWSTestOperationError("validation", writeErr)
	}
	var observation openAICookieWSObservation
	total := 0
	for {
		message, readErr := lease.ReadMessageContext(probeCtx)
		if readErr != nil {
			return cookieWSTestOperationError("validation", readErr)
		}
		total += len(message)
		if total > 4<<20 {
			return newCookieWSTestError("cookie_ws_validation_too_large", "request_error", "Cookie websocket validation response exceeded its size limit", 0, nil)
		}
		if !gjson.ValidBytes(message) {
			return newCookieWSTestError("cookie_ws_validation_malformed", "failed", "Cookie websocket validation response was malformed", 0, nil)
		}
		eventType := gjson.GetBytes(message, "type").String()
		observation.event(message, eventType)
		if observation.failed {
			s.applyOpenAICookieWSValidationFailure(probeCtx, account, lease.HandshakeHeaders(), message)
			status := cookieWSTestPayloadStatus(message)
			if status >= 400 && status <= 599 {
				return cookieWSTestStatusError("validation", status)
			}
			if observation.completed && !observation.modelMatch {
				if _, success := openAICodexSuccessfulCompletionModel(message, eventType); !success {
					return newCookieWSTestError("cookie_ws_validation_incomplete", "failed", "Cookie websocket validation probe did not complete successfully", 0, nil)
				}
				return newCookieWSTestError("cookie_ws_validation_model_mismatch", "model_error", "Cookie websocket validation response did not match the required model", 0, nil)
			}
			return newCookieWSTestError("cookie_ws_validation_incomplete", "failed", "Cookie websocket validation probe did not complete successfully", 0, nil)
		}
		if observation.completed {
			if !observation.modelMatch || !observation.trueAnswer {
				return newCookieWSTestError("cookie_ws_validation_not_true", "failed", "Cookie websocket validation probe did not return True; this socket was closed", 0, nil)
			}
			verified = true
			return nil
		}
	}
}

func (s *OpenAIGatewayService) applyOpenAICookieWSValidationFailure(ctx context.Context, account *Account, _ http.Header, payload []byte) {
	message := extractOpenAISSEErrorMessage(payload)
	status := openAIStreamFailureStatus(payload, message)
	if status == http.StatusTooManyRequests {
		// Cookie validation is an auxiliary health probe. Preserve quota telemetry
		// when the probe exposes it, but never feed the probe failure into the
		// normal request error path: doing so can pause or disable a healthy
		// account before normal traffic has had a chance to authenticate it.
		body := openAIStreamFailedEventPassthroughBody(payload, message)
		s.applyOpenAICodexTicketHarvestProbeOutcome(ctx, account, openAICodexTicketDefaultModel, &openAICodexTicketProbeResult{
			status: status, body: body,
		})
	}
}
