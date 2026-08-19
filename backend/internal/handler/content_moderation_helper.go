package handler

import (
	"context"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	contentModerationScopeContextKey       = "sub2api.content_moderation.scope"
	contentModerationReservationContextKey = "sub2api.content_moderation.reservation"
	contentModerationInputContextKey       = "sub2api.content_moderation.current_input"
)

func contentModerationStatus(decision *service.ContentModerationDecision) int {
	if decision == nil || decision.StatusCode < 400 || decision.StatusCode > 599 {
		return http.StatusForbidden
	}
	return decision.StatusCode
}

func contentModerationErrorCode(decision *service.ContentModerationDecision) string {
	return "content_policy_violation"
}

func clientRequestedModel(c *gin.Context, fallback string) string {
	fallback = strings.TrimSpace(fallback)
	if c == nil || c.Request == nil {
		return fallback
	}
	if model, ok := service.RequestedPublicModelFromContext(c.Request.Context()); ok {
		return model
	}
	return fallback
}

func clientRequestedUsageFields(c *gin.Context, mapping service.ChannelMappingResult, fallbackModel, upstreamModel string) service.ChannelUsageFields {
	return mapping.ToUsageFields(clientRequestedModel(c, fallbackModel), upstreamModel)
}

func runContentModerationStage(c *gin.Context, reqLog *zap.Logger, svc *service.ContentModerationService, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol string, model string, body []byte, stage string) *service.ContentModerationDecision {
	if svc == nil || c == nil || c.Request == nil {
		return nil
	}
	input := buildContentModerationInputForStage(c, apiKey, subject, protocol, model, body, stage)
	inModerationScope := svc.IsContentModerationRequestInScope(c.Request.Context(), input.Scope, input.Model)
	if inModerationScope {
		reservation, ok := contentModerationReservation(c)
		if !ok {
			reservation, ok = svc.ReservePendingRequestBody(int64(len(body)))
			if !ok {
				return &service.ContentModerationDecision{
					Allowed:    false,
					Blocked:    false,
					Message:    "Risk-control capacity is temporarily exhausted; retry later",
					StatusCode: http.StatusServiceUnavailable,
					Action:     service.ContentModerationActionBudgetRejected,
					RetryAfter: 1,
				}
			}
			c.Set(contentModerationReservationContextKey, reservation)
		}
		input.Reservation = reservation
	}
	c.Set(contentModerationInputContextKey, input)
	if reqLog != nil {
		reqLog.Info("content_moderation.gateway_check_start",
			zap.String("request_id", input.RequestID),
			zap.Int64("user_id", input.UserID),
			zap.Int64("api_key_id", input.APIKeyID),
			zap.String("api_key_name", input.APIKeyName),
			zap.Int64p("group_id", input.GroupID),
			zap.String("group_name", input.GroupName),
			zap.String("endpoint", input.Endpoint),
			zap.String("provider", input.Provider),
			zap.String("protocol", input.Protocol),
			zap.String("model", input.Model),
			zap.Int("body_bytes", len(body)),
			zap.Bool("cached", false),
		)
	}
	decision, err := svc.Check(c.Request.Context(), input)
	if err != nil {
		if reqLog != nil {
			reqLog.Warn("content_moderation.check_failed", zap.Error(err))
		}
		return nil
	}
	if reqLog != nil && decision != nil {
		reqLog.Info("content_moderation.gateway_check_done",
			zap.String("request_id", input.RequestID),
			zap.Bool("allowed", decision.Allowed),
			zap.Bool("blocked", decision.Blocked),
			zap.Bool("flagged", decision.Flagged),
			zap.String("action", decision.Action),
			zap.Int("status_code", decision.StatusCode),
			zap.String("highest_category", decision.HighestCategory),
			zap.Float64("highest_score", decision.HighestScore),
			zap.Bool("cached", false),
		)
	}
	return decision
}

func currentContentModerationInput(c *gin.Context) (service.ContentModerationCheckInput, bool) {
	if c == nil {
		return service.ContentModerationCheckInput{}, false
	}
	value, exists := c.Get(contentModerationInputContextKey)
	input, ok := value.(service.ContentModerationCheckInput)
	return input, exists && ok
}

func buildContentModerationInput(c *gin.Context, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol string, model string, body []byte) service.ContentModerationCheckInput {
	return buildContentModerationInputForStage(c, apiKey, subject, protocol, model, body, "http")
}

func buildContentModerationInputForStage(c *gin.Context, apiKey *service.APIKey, subject middleware2.AuthSubject, protocol string, model string, body []byte, stage string) service.ContentModerationCheckInput {
	scope := contentModerationScopeSnapshot(c, apiKey)
	requestContext := context.Background()
	if c != nil && c.Request != nil {
		requestContext = c.Request.Context()
	}
	input := service.ContentModerationCheckInput{
		RequestID: contentModerationRequestID(requestContext),
		UserID:    subject.UserID,
		Endpoint:  GetInboundEndpoint(c),
		Provider:  contentModerationProvider(apiKey),
		Model:     clientRequestedModel(c, model),
		Protocol:  protocol,
		Body:      body,
		Scope:     &scope,
		RawRequest: service.ContentModerationRawRequest{
			Body:      body,
			Transport: "http",
			Stage:     strings.TrimSpace(stage),
		},
	}
	if input.RawRequest.Stage == "" {
		input.RawRequest.Stage = "http"
	}
	if c != nil && c.Request != nil {
		input.RawRequest.Method = c.Request.Method
		input.RawRequest.Headers = c.Request.Header.Clone()
		if c.Request.URL != nil {
			input.RawRequest.Target = c.Request.URL.RequestURI()
		}
	}
	if input.RawRequest.Stage != "http" {
		input.RawRequest.Transport = "websocket"
	}
	if resolvedPlatform, ok := service.ResolvedTargetPlatformFromContext(requestContext); ok {
		input.Provider = resolvedPlatform
	}
	if forcedPlatform, ok := middleware2.GetForcePlatformFromContext(c); ok {
		input.Provider = strings.TrimSpace(forcedPlatform)
	}
	if apiKey != nil {
		input.APIKeyID = apiKey.ID
		input.APIKeyName = apiKey.Name
		if apiKey.User != nil {
			input.UserEmail = apiKey.User.Email
			input.UserRole = apiKey.User.Role
		}
		if apiKey.GroupID != nil {
			groupID := *apiKey.GroupID
			input.GroupID = &groupID
		}
		if apiKey.Group != nil {
			input.GroupName = apiKey.Group.Name
		}
	}
	if input.Endpoint == "" && c.Request != nil && c.Request.URL != nil {
		input.Endpoint = c.Request.URL.Path
	}
	return input
}

func contentModerationScopeSnapshot(c *gin.Context, apiKey *service.APIKey) service.ContentModerationScopeSnapshot {
	if c != nil {
		if stored, ok := c.Get(contentModerationScopeContextKey); ok {
			if snapshot, ok := stored.(service.ContentModerationScopeSnapshot); ok {
				return snapshot
			}
		}
	}
	var groupID *int64
	groupName := ""
	if apiKey != nil {
		groupID = apiKey.GroupID
		if apiKey.Group != nil {
			groupName = apiKey.Group.Name
		}
	}
	snapshot := service.NewContentModerationScopeSnapshot(groupID, groupName)
	if c != nil {
		c.Set(contentModerationScopeContextKey, snapshot)
	}
	return snapshot
}

func contentModerationReservation(c *gin.Context) (*service.ContentModerationPendingReservation, bool) {
	if c == nil {
		return nil, false
	}
	value, ok := c.Get(contentModerationReservationContextKey)
	if !ok {
		return nil, false
	}
	reservation, ok := value.(*service.ContentModerationPendingReservation)
	return reservation, ok && reservation != nil
}

func releaseContentModerationReservation(c *gin.Context) {
	reservation, ok := contentModerationReservation(c)
	if !ok {
		return
	}
	c.Set(contentModerationReservationContextKey, (*service.ContentModerationPendingReservation)(nil))
	reservation.Release()
}

// ContentModerationRequestLifecycle releases the body-budget reservation only
// after the gateway handler has completed, so an upstream severe response can
// still archive the exact bytes received by the application.
func ContentModerationRequestLifecycle() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer releaseContentModerationReservation(c)
		c.Next()
	}
}

func contentModerationProvider(apiKey *service.APIKey) string {
	if apiKey == nil || apiKey.Group == nil {
		return ""
	}
	return strings.TrimSpace(apiKey.Group.Platform)
}

func contentModerationRequestID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if requestID, ok := ctx.Value(ctxkey.RequestID).(string); ok {
		return strings.TrimSpace(requestID)
	}
	return ""
}
