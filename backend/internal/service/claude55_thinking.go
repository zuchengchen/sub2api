package service

import (
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// adaptThinkingToClaude55 translates disabled thinking for a Sonnet 5.5
// composite target. The native forwarding path handles legacy enabled
// thinking once the final account model is known.
func adaptThinkingToClaude55(body []byte, model string) []byte {
	if claude.IsSonnet55(model) && gjson.GetBytes(body, "thinking.type").String() == "disabled" {
		body, _ = sjson.SetBytes(body, "thinking.type", "between_tools")
	}
	return body
}
