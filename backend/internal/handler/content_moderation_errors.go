package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/googleapi"
	"github.com/Wei-Shaw/sub2api/internal/service"
	coderws "github.com/coder/websocket"
	"github.com/gin-gonic/gin"
)

const riskControlCapacityErrorCode = "risk_control_capacity_exhausted"

func contentModerationDecisionErrorCode(decision *service.ContentModerationDecision) string {
	if decision != nil && decision.Action == service.ContentModerationActionBudgetRejected {
		return riskControlCapacityErrorCode
	}
	return "content_policy_violation"
}

func contentModerationDecisionMessage(decision *service.ContentModerationDecision) string {
	message := "Request blocked by content policy"
	if decision != nil && strings.TrimSpace(decision.Message) != "" {
		message = strings.TrimSpace(decision.Message)
	}
	if decision == nil || !decision.Blocked {
		return message
	}
	if keyword := strings.TrimSpace(decision.MatchedKeyword); keyword != "" {
		return message + "（命中敏感词：" + keyword + "）"
	}
	switch decision.Action {
	case service.ContentModerationActionHashBlock, service.ContentModerationActionCacheBlock:
		return message + "（命中历史风险内容）"
	case service.ContentModerationActionBlock, service.ContentModerationActionSecondLayerBlock:
		if category := strings.TrimSpace(decision.HighestCategory); category != "" {
			return message + "（违规类型：" + category + "）"
		}
	}
	return message
}

func (h *OpenAIGatewayHandler) openAIContentModerationError(c *gin.Context, decision *service.ContentModerationDecision) {
	if decision == nil {
		return
	}
	if decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationDecisionErrorCode(decision), contentModerationDecisionMessage(decision))
		return
	}
	c.JSON(contentModerationStatus(decision), gin.H{"error": gin.H{
		"type": "api_error", "code": contentModerationDecisionErrorCode(decision), "message": contentModerationDecisionMessage(decision),
	}})
}

func (h *GatewayHandler) openAIContentModerationError(c *gin.Context, decision *service.ContentModerationDecision) {
	if decision == nil {
		return
	}
	if decision.Blocked {
		h.chatCompletionsErrorResponse(c, contentModerationStatus(decision), contentModerationDecisionErrorCode(decision), contentModerationDecisionMessage(decision))
		return
	}
	c.JSON(contentModerationStatus(decision), gin.H{"error": gin.H{
		"type": "api_error", "code": contentModerationDecisionErrorCode(decision), "message": contentModerationDecisionMessage(decision),
	}})
}

func (h *GatewayHandler) responsesContentModerationError(c *gin.Context, decision *service.ContentModerationDecision) {
	if decision == nil {
		return
	}
	if decision.Blocked {
		h.responsesErrorResponse(c, contentModerationStatus(decision), contentModerationDecisionErrorCode(decision), contentModerationDecisionMessage(decision))
		return
	}
	c.JSON(contentModerationStatus(decision), gin.H{"error": gin.H{
		"type": "api_error", "code": contentModerationDecisionErrorCode(decision), "message": contentModerationDecisionMessage(decision),
	}})
}

func (h *GatewayHandler) anthropicContentModerationError(c *gin.Context, decision *service.ContentModerationDecision) {
	if decision == nil {
		return
	}
	if decision.Blocked {
		h.errorResponse(c, contentModerationStatus(decision), contentModerationDecisionErrorCode(decision), contentModerationDecisionMessage(decision))
		return
	}
	c.JSON(contentModerationStatus(decision), gin.H{"type": "error", "error": gin.H{
		"type": "api_error", "code": contentModerationDecisionErrorCode(decision), "message": contentModerationDecisionMessage(decision),
	}})
}

func (h *OpenAIGatewayHandler) anthropicContentModerationError(c *gin.Context, decision *service.ContentModerationDecision) {
	if decision == nil {
		return
	}
	if decision.Blocked {
		h.anthropicErrorResponse(c, contentModerationStatus(decision), contentModerationDecisionErrorCode(decision), contentModerationDecisionMessage(decision))
		return
	}
	c.JSON(contentModerationStatus(decision), gin.H{"type": "error", "error": gin.H{
		"type": "api_error", "code": contentModerationDecisionErrorCode(decision), "message": contentModerationDecisionMessage(decision),
	}})
}

func googleContentModerationError(c *gin.Context, decision *service.ContentModerationDecision) {
	if decision == nil {
		return
	}
	if decision.Blocked {
		googleError(c, contentModerationStatus(decision), contentModerationDecisionMessage(decision))
		return
	}
	status := contentModerationStatus(decision)
	googleStatus := googleapi.HTTPStatusToGoogleStatus(status)
	if status == http.StatusServiceUnavailable {
		googleStatus = "UNAVAILABLE"
	}
	requestID := ""
	if c != nil && c.Request != nil {
		requestID = contentModerationRequestID(c.Request.Context())
	}
	c.JSON(status, gin.H{"error": gin.H{
		"code": status, "message": contentModerationDecisionMessage(decision), "status": googleStatus,
		"details": []gin.H{{
			"@type":  "type.googleapis.com/google.rpc.ErrorInfo",
			"reason": contentModerationDecisionErrorCode(decision), "domain": "sub2api.risk_control",
			"metadata": gin.H{"request_id": requestID},
		}},
	}})
}

func writeContentModerationGateWSError(ctx context.Context, conn *coderws.Conn, decision *service.ContentModerationDecision) {
	if conn == nil || decision == nil {
		return
	}
	if decision.Blocked {
		writeContentModerationWSError(ctx, conn, decision)
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, err := json.Marshal(gin.H{
		"event_id": "evt_risk_control_rejected", "type": "error",
		"error": gin.H{"type": "invalid_request_error", "code": contentModerationDecisionErrorCode(decision), "message": contentModerationDecisionMessage(decision)},
	})
	if err != nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = conn.Write(writeCtx, coderws.MessageText, payload)
}

func contentModerationWSCloseStatus(decision *service.ContentModerationDecision) coderws.StatusCode {
	if decision == nil {
		return coderws.StatusInternalError
	}
	if decision.Blocked {
		return coderws.StatusPolicyViolation
	}
	return coderws.StatusTryAgainLater
}

func contentModerationWSCloseReason(decision *service.ContentModerationDecision) string {
	if decision == nil {
		return riskControlCapacityErrorCode
	}
	if decision.Blocked {
		return truncateString(contentModerationDecisionMessage(decision), 120)
	}
	return contentModerationDecisionErrorCode(decision)
}
