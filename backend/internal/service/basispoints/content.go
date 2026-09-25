package basispoints

import "fmt"

func validateHistoryContent(value any, inputIndex int, field string) error {
	content, _ := value.([]any)
	for index, rawPart := range content {
		part, _ := rawPart.(object)
		switch text(part["type"]) {
		case "input_text", "output_text", "text", "refusal":
		case "input_image":
			if err := validateImage(part); err != nil {
				return fmt.Errorf("%w (path=input[%d].%s[%d])", err, inputIndex, field, index)
			}
		default:
			return fmt.Errorf("basispoints supports text and HTTPS input_image content only (path=input[%d].%s[%d]; type=%s)", inputIndex, field, index, contentTypeDiagnostic(part))
		}
	}
	return nil
}

// Report only fixed protocol labels. A caller-controlled type can itself contain
// private data or log injection, so unrecognized values are never echoed.
func contentTypeDiagnostic(part object) string {
	if part == nil {
		return "non_object"
	}
	value, present := part["type"]
	if !present {
		return "missing"
	}
	kind, ok := value.(string)
	if !ok {
		return "non_string"
	}
	switch kind {
	case "image", "image_url", "input_file", "file", "document",
		"input_audio", "output_audio", "audio", "reasoning_text", "summary_text",
		"tool_use", "tool_result", "thinking", "redacted_thinking":
		return kind
	default:
		return "unknown"
	}
}
