package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func prismBrowserPrepareChatCompletionsAdapterBody(body []byte) ([]byte, error) {
	out := body
	var err error
	for _, field := range []string{"max_output_tokens", "temperature", "top_p", "service_tier", "background"} {
		if !gjson.GetBytes(out, field).Exists() {
			continue
		}
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, err
		}
	}
	out, err = sjson.SetBytes(out, "stream", false)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func writePrismBrowserChatCompletionsError(c *gin.Context, clientErr *prismBrowserClientError) {
	if clientErr == nil {
		return
	}
	status := clientErr.Status
	if status == 0 {
		status = http.StatusBadGateway
	}
	code := strings.TrimSpace(clientErr.Code)
	if code == "" {
		code = gjson.GetBytes(clientErr.Raw, "error.type").String()
	}
	if code == "" {
		code = "api_error"
	}
	message := strings.TrimSpace(clientErr.Message)
	if message == "" {
		message = gjson.GetBytes(clientErr.Raw, "error.message").String()
	}
	if message == "" {
		message = "Prism adapter request failed"
	}
	writeChatCompletionsError(c, status, code, message)
}

func chatUsageFromOpenAIUsage(usage OpenAIUsage) *apicompat.ChatUsage {
	if usage.InputTokens == 0 && usage.OutputTokens == 0 {
		return nil
	}
	out := &apicompat.ChatUsage{
		PromptTokens:     usage.InputTokens,
		CompletionTokens: usage.OutputTokens,
		TotalTokens:      usage.InputTokens + usage.OutputTokens,
	}
	if usage.CacheReadInputTokens > 0 || usage.CacheCreationInputTokens > 0 {
		out.PromptTokensDetails = &apicompat.ChatTokenDetails{
			CachedTokens:        usage.CacheReadInputTokens,
			CacheCreationTokens: usage.CacheCreationInputTokens,
		}
	}
	return out
}

func chatMessageContentString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return ""
}

func writePrismChatCompletionsStream(c *gin.Context, chatResp *apicompat.ChatCompletionsResponse) error {
	c.Writer.Header().Set("Content-Type", "text/event-stream")
	c.Writer.Header().Set("Cache-Control", "no-cache")
	c.Writer.Header().Set("Connection", "keep-alive")
	c.Writer.Header().Set("X-Accel-Buffering", "no")
	c.Writer.WriteHeader(http.StatusOK)

	write := func(chunk apicompat.ChatCompletionsChunk) error {
		sse, err := apicompat.ChatChunkToSSE(chunk)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprint(c.Writer, sse); err != nil {
			return err
		}
		c.Writer.Flush()
		return nil
	}
	chunk := func(delta apicompat.ChatDelta, finish *string) apicompat.ChatCompletionsChunk {
		return apicompat.ChatCompletionsChunk{
			ID:          chatResp.ID,
			Object:      "chat.completion.chunk",
			Created:     chatResp.Created,
			Model:       chatResp.Model,
			ServiceTier: chatResp.ServiceTier,
			Choices: []apicompat.ChatChunkChoice{{
				Index:        0,
				Delta:        delta,
				FinishReason: finish,
			}},
		}
	}

	if err := write(chunk(apicompat.ChatDelta{Role: "assistant"}, nil)); err != nil {
		return err
	}
	if len(chatResp.Choices) > 0 {
		msg := chatResp.Choices[0].Message
		if reasoning := strings.TrimSpace(msg.ReasoningContent); reasoning != "" {
			if err := write(chunk(apicompat.ChatDelta{ReasoningContent: &reasoning}, nil)); err != nil {
				return err
			}
		}
		if content := chatMessageContentString(msg.Content); content != "" {
			text := content
			if err := write(chunk(apicompat.ChatDelta{Content: &text}, nil)); err != nil {
				return err
			}
		}
		if len(msg.ToolCalls) > 0 {
			calls := make([]apicompat.ChatToolCall, len(msg.ToolCalls))
			for i, call := range msg.ToolCalls {
				idx := i
				calls[i] = call
				calls[i].Index = &idx
			}
			if err := write(chunk(apicompat.ChatDelta{ToolCalls: calls}, nil)); err != nil {
				return err
			}
		}
		finish := chatResp.Choices[0].FinishReason
		if finish == "" {
			finish = "stop"
		}
		empty := ""
		if err := write(chunk(apicompat.ChatDelta{Content: &empty}, &finish)); err != nil {
			return err
		}
	}
	if chatResp.Usage != nil {
		if err := write(apicompat.ChatCompletionsChunk{
			ID:          chatResp.ID,
			Object:      "chat.completion.chunk",
			Created:     chatResp.Created,
			Model:       chatResp.Model,
			ServiceTier: chatResp.ServiceTier,
			Choices:     []apicompat.ChatChunkChoice{},
			Usage:       chatResp.Usage,
		}); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(c.Writer, "data: [DONE]\n\n"); err != nil {
		return err
	}
	c.Writer.Flush()
	return nil
}

func (s *OpenAIGatewayService) forwardPrismBrowserAsChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	billingModel string,
	upstreamModel string,
	clientStream bool,
	started time.Time,
	clientBody []byte,
) (*OpenAIForwardResult, error) {
	defer func() {
		if c.Writer.Written() {
			MarkResponseCommitted(c)
		}
	}()
	executed, err := s.executePrismBrowser(ctx, c, account, body)
	if err != nil {
		if errors.Is(err, errPrismBrowserHTTPFallback) {
			return nil, err
		}
		var clientErr *prismBrowserClientError
		if errors.As(err, &clientErr) {
			writePrismBrowserChatCompletionsError(c, clientErr)
			if clientErr.AdapterHTTP {
				return nil, prismBrowserForwardError(clientErr.Status, clientErr.Raw)
			}
			return nil, err
		}
		writeChatCompletionsError(c, http.StatusBadGateway, "prism_unavailable", "Prism adapter unavailable")
		return nil, err
	}
	terminal := prismBrowserCompletedResponse(executed.Body, executed.Stream)
	if len(terminal) == 0 {
		writeChatCompletionsError(c, http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned no valid terminal response")
		return nil, errors.New("prism adapter returned no valid terminal response")
	}
	var responsesResp apicompat.ResponsesResponse
	if err := json.Unmarshal(terminal, &responsesResp); err != nil {
		writeChatCompletionsError(c, http.StatusBadGateway, "invalid_prism_response", "Prism adapter returned no valid terminal response")
		return nil, err
	}
	chatResp := apicompat.ResponsesToChatCompletions(&responsesResp, originalModel)
	usage := OpenAIUsage{}
	applyEstimatedOpenAIUsageIfMissing(&usage, executed.UpstreamModel, executed.Prepared, prismBrowserTerminalOutputText(executed.Body, executed.Stream))
	if chatUsage := chatUsageFromOpenAIUsage(usage); chatUsage != nil {
		chatResp.Usage = chatUsage
	}
	SetActualOpenAIUpstreamEndpoint(c, prismBrowserUpstreamEndpoint)
	c.Header("X-Prism-Usage", "estimated")
	if clientStream {
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), executed.Headers, s.responseHeaderFilter)
		}
		if err := writePrismChatCompletionsStream(c, chatResp); err != nil {
			return nil, err
		}
	} else {
		if s.responseHeaderFilter != nil {
			responseheaders.WriteFilteredHeaders(c.Writer.Header(), executed.Headers, s.responseHeaderFilter)
		}
		c.Writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		c.JSON(http.StatusOK, chatResp)
	}
	result := &OpenAIForwardResult{
		RequestID:        executed.ResponseID,
		ResponseID:       executed.ResponseID,
		UpstreamHeaders:  executed.Headers,
		Usage:            usage,
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    firstNonEmpty(upstreamModel, executed.UpstreamModel),
		Stream:           clientStream,
		Duration:         time.Since(started),
		UpstreamEndpoint: prismBrowserUpstreamEndpoint,
		ReasoningEffort:  extractOpenAIReasoningEffortFromBody(executed.Prepared, executed.UpstreamModel, originalModel),
	}
	if requested := CanonicalRequestedReasoningEffort(clientBody, originalModel); requested != nil {
		result.RequestedReasoningEffort = requested
	}
	return result, nil
}
