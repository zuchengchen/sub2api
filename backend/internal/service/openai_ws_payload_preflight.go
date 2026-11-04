package service

import (
	"encoding/json"
	"errors"

	"github.com/gin-gonic/gin"
)

// Measure the same serialized, uncompressed frame that will be sent. Metadata,
// Codex wire defaults and non-ASCII escaping count towards the WS frame limit.
func (s *OpenAIGatewayService) marshalOpenAIWSOutgoingFrame(body []byte, payload map[string]any, account *Account) ([]byte, error) {
	if account != nil && account.UsesOpenAICodexProtocol() && s.isOpenAIWSExactFidelityEnabled() {
		return marshalCodexWSCreateFrame(body, payload, s.codexMapOrderJitterEnabled())
	}
	return json.Marshal(payload)
}

func (s *OpenAIGatewayService) shouldPreflightOpenAIHTTPToWS(c *gin.Context, account *Account, request map[string]any) bool {
	return s.openAIWSHTTPBridgeEnabled() && GetOpenAIClientTransport(c) == OpenAIClientTransportHTTP &&
		account != nil && account.IsOpenAI() && account.IsOpenAIOAuthLike() && !account.IsOpenAIPassthroughEnabled() &&
		openAIWSPayloadString(request, "previous_response_id") == ""
}

func (s *OpenAIGatewayService) checkOpenAIWSOutgoingFrameSize(frame []byte) error {
	if int64(len(frame)) >= s.openAIWSHTTPBridgeThresholdBytes() {
		return wrapOpenAIWSFallback("payload_too_large", errors.New("outgoing websocket frame exceeds HTTP bridge threshold"))
	}
	return nil
}
