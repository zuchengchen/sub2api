package handler

import "github.com/Wei-Shaw/sub2api/internal/service"

// An acknowledged client stop is a neutral scheduling sample. The caller must
// still record its observed usage and preserve the actual upstream terminal.
// Ordinary incomplete/error responses and unconfirmed interruption remain
// subject to the existing success/failure classification.
func openAIForwardHasSchedulingSample(result *service.OpenAIForwardResult) bool {
	return result == nil || !result.OpenAIWSMode || !result.ClientInterrupted
}
