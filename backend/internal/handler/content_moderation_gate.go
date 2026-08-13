package handler

import (
	"crypto/sha256"
	"strings"

	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const contentModerationCompletedContextKey = "sub2api.content_moderation.completed"
const contentModerationWSTurnContextKey = "sub2api.content_moderation.ws_turn"
const contentModerationWSDedupeContextKey = "sub2api.content_moderation.ws_dedupe"

type contentModerationWSDedupeEntry struct {
	stage    string
	turn     int
	bodyHash [sha256.Size]byte
	decision service.ContentModerationDecision
}

func cachesContentModerationCompletion(stage string) bool {
	switch strings.TrimSpace(stage) {
	case "", "http":
		return true
	default:
		return false
	}
}

func isContentModerationWebSocketStage(stage string) bool {
	switch strings.TrimSpace(stage) {
	case "first_turn", "subsequent_turn":
		return true
	default:
		return false
	}
}

func (h *GatewayHandler) checkContentModeration(c *gin.Context, reqLog *zap.Logger, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol, model string, body []byte) *service.ContentModerationDecision {
	if h == nil {
		return nil
	}
	return runUnifiedContentModeration(c, reqLog, h.contentModerationService, apiKey, subject, protocol, model, body, "http")
}

func (h *OpenAIGatewayHandler) checkContentModeration(c *gin.Context, reqLog *zap.Logger, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol, model string, body []byte) *service.ContentModerationDecision {
	if h == nil {
		return nil
	}
	return runUnifiedContentModeration(c, reqLog, h.contentModerationService, apiKey, subject, protocol, model, body, "http")
}

func (h *OpenAIGatewayHandler) checkContentModerationStage(c *gin.Context, reqLog *zap.Logger, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol, model string, body []byte, stage string) *service.ContentModerationDecision {
	if h == nil {
		return nil
	}
	return runUnifiedContentModeration(c, reqLog, h.contentModerationService, apiKey, subject, protocol, model, body, stage)
}

func runUnifiedContentModeration(c *gin.Context, reqLog *zap.Logger, unified *service.ContentModerationService, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol, model string, body []byte, stage string) *service.ContentModerationDecision {
	if c == nil || c.Request == nil {
		return nil
	}
	cacheCompletion := cachesContentModerationCompletion(stage)
	if cacheCompletion {
		if completed, exists := c.Get(contentModerationCompletedContextKey); exists && completed == true {
			return nil
		}
	}
	stage = strings.TrimSpace(stage)
	if stage == "" {
		stage = "http"
	}
	if isContentModerationWebSocketStage(stage) {
		if turnNo, ok := contentModerationWSTurn(c); ok {
			bodyHash := sha256.Sum256(body)
			if cached, exists := c.Get(contentModerationWSDedupeContextKey); exists {
				if entry, ok := cached.(contentModerationWSDedupeEntry); ok &&
					entry.stage == stage && entry.turn == turnNo && entry.bodyHash == bodyHash {
					decision := entry.decision
					return &decision
				}
			}
			decision := runContentModerationStage(c, reqLog, unified, apiKey, subject, protocol, model, body, stage)
			if decision == nil {
				return nil
			}
			if decision.Allowed && !decision.Flagged {
				c.Set(contentModerationWSDedupeContextKey, contentModerationWSDedupeEntry{
					stage: stage, turn: turnNo, bodyHash: bodyHash, decision: *decision,
				})
			}
			return decision
		}
	}
	decision := runContentModerationStage(c, reqLog, unified, apiKey, subject, protocol, model, body, stage)
	if decision == nil {
		return nil
	}
	if decision.Allowed && cacheCompletion {
		c.Set(contentModerationCompletedContextKey, true)
	}
	return decision
}

func contentModerationWSTurn(c *gin.Context) (int, bool) {
	turn, exists := c.Get(contentModerationWSTurnContextKey)
	if !exists {
		return 0, false
	}
	turnNo, ok := turn.(int)
	return turnNo, ok
}
