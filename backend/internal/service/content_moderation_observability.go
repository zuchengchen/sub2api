package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

type contentModerationReviewObservability struct {
	deepSeekResponseReadTimeoutCount atomic.Int64
	deepSeekBreakerSkipCount         atomic.Int64
	deepSeekCooldownSkipCount        atomic.Int64
	deepSeekHalfOpenBusySkipCount    atomic.Int64
	reviewUnavailableCount           atomic.Int64
	reviewUnavailableEnforcedCount   atomic.Int64
	lastReviewUnavailableUnixMilli   atomic.Int64
}

type contentModerationReviewFailureSummary struct {
	responseReadTimeouts int
	breakerSkips         int
	cooldownSkips        int
	halfOpenBusySkips    int
}

func summarizeContentModerationReviewFailure(reviewErr error, attempts []ContentModerationReviewAttempt) contentModerationReviewFailureSummary {
	var summary contentModerationReviewFailureSummary
	for _, attempt := range attempts {
		provider := strings.TrimSpace(attempt.Provider)
		if provider == "" {
			provider = strings.TrimSpace(attempt.Reviewer)
		}
		if !isSupportedContentModerationRemoteProvider(provider) {
			continue
		}
		errorClass := strings.ToLower(strings.TrimSpace(attempt.Error))
		switch errorClass {
		case "response_read_timeout", "response_body_timeout":
			summary.responseReadTimeouts++
		case "response_read":
			if errors.Is(reviewErr, context.DeadlineExceeded) || isContentModerationSecondLayerTimeout(reviewErr) {
				summary.responseReadTimeouts++
			}
		}
		if !strings.EqualFold(strings.TrimSpace(attempt.Outcome), "skipped") {
			continue
		}
		switch errorClass {
		case "cooldown":
			summary.breakerSkips++
			summary.cooldownSkips++
		case "half_open_busy":
			summary.breakerSkips++
			summary.halfOpenBusySkips++
		}
	}
	return summary
}

func contentModerationReviewFailureClass(reviewErr error, attempts []ContentModerationReviewAttempt) string {
	summary := summarizeContentModerationReviewFailure(reviewErr, attempts)
	switch {
	case summary.responseReadTimeouts > 0:
		return "response_read_timeout"
	case summary.halfOpenBusySkips > 0:
		return "half_open_busy"
	case summary.cooldownSkips > 0:
		return "cooldown"
	case errors.Is(reviewErr, context.DeadlineExceeded) || isContentModerationSecondLayerTimeout(reviewErr):
		return "timeout"
	case len(attempts) == 0:
		return "not_attempted"
	default:
		return "review_failed"
	}
}

func (s *ContentModerationService) recordContentModerationReviewAttempts(attempts []ContentModerationReviewAttempt, reviewErr error) {
	if s == nil {
		return
	}
	summary := summarizeContentModerationReviewFailure(reviewErr, attempts)
	s.reviewObservability.deepSeekResponseReadTimeoutCount.Add(int64(summary.responseReadTimeouts))
	s.reviewObservability.deepSeekBreakerSkipCount.Add(int64(summary.breakerSkips))
	s.reviewObservability.deepSeekCooldownSkipCount.Add(int64(summary.cooldownSkips))
	s.reviewObservability.deepSeekHalfOpenBusySkipCount.Add(int64(summary.halfOpenBusySkips))
}

func (s *ContentModerationService) recordContentModerationReviewUnavailable(
	ctx context.Context,
	requestID string,
	cfg *ContentModerationConfig,
	decisionSource string,
	parserStatus string,
	reviewErr error,
	attempts []ContentModerationReviewAttempt,
) {
	if s == nil {
		return
	}
	s.reviewObservability.reviewUnavailableCount.Add(1)
	enforced := strings.TrimSpace(decisionSource) == "review_unavailable"
	if enforced {
		s.reviewObservability.reviewUnavailableEnforcedCount.Add(1)
	}
	s.reviewObservability.lastReviewUnavailableUnixMilli.Store(time.Now().UnixMilli())

	summary := summarizeContentModerationReviewFailure(reviewErr, attempts)
	stage := ""
	if cfg != nil {
		stage = cfg.SecondLayerStage
	}
	fields := []zap.Field{
		zap.String("component", "service.content_moderation"),
		zap.String("failure_class", contentModerationReviewFailureClass(reviewErr, attempts)),
		zap.String("parser_status", strings.TrimSpace(parserStatus)),
		zap.String("decision_source", strings.TrimSpace(decisionSource)),
		zap.String("second_layer_stage", strings.TrimSpace(stage)),
		zap.Bool("enforced", enforced),
		zap.Int("review_attempt_count", len(attempts)),
		zap.Int("response_read_timeout_count", summary.responseReadTimeouts),
		zap.Int("breaker_skip_count", summary.breakerSkips),
		zap.Int("cooldown_skip_count", summary.cooldownSkips),
		zap.Int("half_open_busy_skip_count", summary.halfOpenBusySkips),
	}
	if requestID = strings.TrimSpace(requestID); requestID != "" {
		fields = append(fields, zap.String("moderation_request_id", requestID))
	}
	logger.FromContext(ctx).Error("content_moderation.review_unavailable", fields...)
}
