package service

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// OpenAIContextLengthExceededCode is the error code Codex maps to
// ContextWindowExceeded (codex-api/src/sse/responses_error.rs). Codex only
// recognises it inside a streamed response.failed event; an HTTP 400 JSON body
// with the same code is surfaced as a generic error and never triggers
// auto-compaction.
const OpenAIContextLengthExceededCode = "context_length_exceeded"

// OpenAIContextLengthExceededMessage is the official upstream wording.
const OpenAIContextLengthExceededMessage = "Your input exceeds the context window of this model. Please adjust your input and try again."

// anthropicPromptTooLongPrefix is the wording Claude Code keys compaction on.
const anthropicPromptTooLongPrefix = "prompt is too long"

const openAIResponsesClientStreamKey = "openai_responses_client_stream"

// MarkOpenAIResponsesClientStream records the inbound /responses stream flag so
// late error writers (service and handler) can pick the protocol shape the
// client can act on.
func MarkOpenAIResponsesClientStream(c *gin.Context, stream bool) {
	if c != nil {
		c.Set(openAIResponsesClientStreamKey, stream)
	}
}

// OpenAIResponsesClientStream reports the flag recorded by
// MarkOpenAIResponsesClientStream.
func OpenAIResponsesClientStream(c *gin.Context) bool {
	if c == nil {
		return false
	}
	return c.GetBool(openAIResponsesClientStreamKey)
}

// WriteOpenAIResponsesContextLengthExceeded writes a context overflow so Codex
// compacts: a streamed request receives a 200 SSE response.failed with code
// context_length_exceeded; a non-streamed request receives HTTP 400 JSON with
// the same code. The response is marked committed so no fallback error is
// appended afterwards.
func WriteOpenAIResponsesContextLengthExceeded(c *gin.Context, message string) {
	if c == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = OpenAIContextLengthExceededMessage
	}
	errorBody := gin.H{
		"type":    "invalid_request_error",
		"code":    OpenAIContextLengthExceededCode,
		"message": message,
		"param":   "input",
	}
	MarkResponseCommitted(c)
	if StopOpenAICompactSSEKeepaliveCommitted(c) || OpenAIResponsesClientStream(c) || c.Writer.Written() {
		if !c.Writer.Written() {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.Header().Set("Cache-Control", "no-cache")
			c.Writer.WriteHeader(http.StatusOK)
		}
		writeOpenAIResponsesSSEFailure(c, http.StatusBadRequest, errorBody)
		return
	}
	c.JSON(http.StatusBadRequest, gin.H{"error": errorBody})
}

// WriteChatCompletionsContextLengthExceeded writes the OpenAI chat-completions
// overflow error (HTTP 400, code context_length_exceeded).
func WriteChatCompletionsContextLengthExceeded(c *gin.Context, message string) {
	if c == nil {
		return
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = OpenAIContextLengthExceededMessage
	}
	MarkResponseCommitted(c)
	c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{
		"type":    "invalid_request_error",
		"code":    OpenAIContextLengthExceededCode,
		"param":   "messages",
		"message": message,
	}})
}

// AnthropicPromptTooLongMessage prefixes message with Anthropic's overflow
// wording unless it already carries it.
func AnthropicPromptTooLongMessage(message string) string {
	message = strings.TrimSpace(message)
	if message == "" {
		message = OpenAIContextLengthExceededMessage
	}
	if strings.Contains(strings.ToLower(message), anthropicPromptTooLongPrefix) {
		return message
	}
	return anthropicPromptTooLongPrefix + ": " + message
}

// writeChatCompletionsCompatError and writeAnthropicCompatError are the compat
// error writers; they turn upstream context overflow into the protocol's
// overflow error so the client compacts instead of retrying.
func writeChatCompletionsCompatError(c *gin.Context, statusCode int, errType, message string) {
	if isOpenAIContextWindowError(message, nil) {
		WriteChatCompletionsContextLengthExceeded(c, message)
		return
	}
	writeChatCompletionsError(c, statusCode, errType, message)
}

func writeAnthropicCompatError(c *gin.Context, statusCode int, errType, message string) {
	if isOpenAIContextWindowError(message, nil) {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", AnthropicPromptTooLongMessage(message))
		return
	}
	writeAnthropicError(c, statusCode, errType, message)
}
