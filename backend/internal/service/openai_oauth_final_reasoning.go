package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
)

type openAIReasoningEffortValidationError struct {
	param   string
	message string
}

func (e *openAIReasoningEffortValidationError) Error() string { return e.message }

// validateOpenAIOAuthFinalReasoning validates the effective request after model
// mapping, conversion and configured effort policy. It deliberately does not
// consult the original requested effort: a configured none -> low mapping must
// remain valid while reporting none as the client's original intent. Callers
// render this as a parameter 400 before sending; this is never a retry signal.
func validateOpenAIOAuthFinalReasoning(account *Account, finalModel string, body []byte) error {
	if account == nil || !account.IsOpenAIOAuthLike() {
		return nil
	}
	for _, param := range []string{"reasoning.effort", "reasoning_effort", "output_config.effort"} {
		value := gjson.GetBytes(body, param)
		if value.Type != gjson.String {
			continue
		}
		if err := openai.ValidateGPT6AstraReasoningEffort(finalModel, value.String()); err != nil {
			return &openAIReasoningEffortValidationError{param: param, message: err.Error()}
		}
	}
	return nil
}
