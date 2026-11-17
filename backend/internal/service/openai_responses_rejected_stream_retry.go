package service

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const openAIRejectedFieldRetryAttemptKey = "openai_rejected_field_retry_attempt"

// An attempt is scoped to its response reader. It uses the transformed wire
// body, never the original client body, and shares the inbound retry budget.
type openAIRejectedFieldRetryAttempt struct {
	body  []byte
	state *openAIResponsesRejectedFieldRetryState
}

type openAIRejectedFieldStreamRetrySignal struct {
	body   []byte
	reason string
}

func (s *openAIRejectedFieldStreamRetrySignal) Error() string {
	return "upstream rejected a repairable Responses field"
}

func asOpenAIRejectedFieldStreamRetrySignal(err error) (*openAIRejectedFieldStreamRetrySignal, bool) {
	var signal *openAIRejectedFieldStreamRetrySignal
	ok := errors.As(err, &signal) && signal != nil
	return signal, ok
}

func withOpenAIRejectedFieldRetryAttempt[T any](c *gin.Context, body []byte, state *openAIResponsesRejectedFieldRetryState, read func() (T, error)) (T, error) {
	if c == nil {
		return read()
	}
	previous, existed := c.Get(openAIRejectedFieldRetryAttemptKey)
	c.Set(openAIRejectedFieldRetryAttemptKey, &openAIRejectedFieldRetryAttempt{body: body, state: state})
	defer func() {
		if existed {
			c.Set(openAIRejectedFieldRetryAttemptKey, previous)
		} else {
			// Set a nil value through Gin's synchronized API. Do not retain the
			// potentially large request after the reader has completed.
			c.Set(openAIRejectedFieldRetryAttemptKey, nil)
		}
	}()
	return read()
}

// Callers must first check their monotonic upstream replay guard. This helper
// additionally enforces cancellation and downstream commitment, and only
// signals after an exact repair has consumed the shared bounded retry budget.
// Unknown errors remain on the reader's original error path.
func newOpenAIRejectedFieldStreamRetrySignal(c *gin.Context, payload []byte) error {
	if gjson.GetBytes(payload, "type").String() != "response.failed" {
		return nil
	}
	if c == nil || !(&openAIResponseReplayGuard{}).CanReplay(c) || openAIStreamClientOutputStarted(c, false) {
		return nil
	}
	value, _ := c.Get(openAIRejectedFieldRetryAttemptKey)
	attempt, _ := value.(*openAIRejectedFieldRetryAttempt)
	if attempt == nil || attempt.state == nil || openAIStreamFailureStatus(payload, extractOpenAISSEErrorMessage(payload)) != http.StatusBadRequest {
		return nil
	}
	body, reason, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, attempt.body, payload)
	if err != nil {
		return err
	}
	if !changed || !(&openAIResponseReplayGuard{}).CanReplay(c) || openAIStreamClientOutputStarted(c, false) || !attempt.state.Allow(body) {
		return nil
	}
	return &openAIRejectedFieldStreamRetrySignal{body: body, reason: reason}
}

// Preserve structured client-actionable fields without copying an upstream
// response object or arbitrary extensions into the public error envelope.
func openAIStreamClientErrorFields(payload []byte, errType, message string) gin.H {
	fields := gin.H{"type": errType, "message": message}
	if normalized, ok := normalizeOpenAIUpstreamError(payload); ok {
		if normalized.Code != "" {
			fields["code"] = normalized.Code
		}
		if normalized.Param != "" {
			fields["param"] = normalized.Param
		}
	}
	return fields
}
