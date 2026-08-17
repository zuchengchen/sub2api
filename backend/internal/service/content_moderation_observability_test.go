package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/stretchr/testify/require"
)

func TestSummarizeContentModerationReviewFailure(t *testing.T) {
	tests := []struct {
		name      string
		reviewErr error
		attempts  []ContentModerationReviewAttempt
		want      contentModerationReviewFailureSummary
	}{
		{
			name:      "response body deadline is classified separately",
			reviewErr: context.DeadlineExceeded,
			attempts:  []ContentModerationReviewAttempt{{Reviewer: "deepseek", Outcome: "error", Error: "response_read"}},
			want:      contentModerationReviewFailureSummary{responseReadTimeouts: 1},
		},
		{
			name:     "explicit response body timeout remains classified without raw error",
			attempts: []ContentModerationReviewAttempt{{Reviewer: "deepseek", Outcome: "error", Error: "response_read_timeout"}},
			want:     contentModerationReviewFailureSummary{responseReadTimeouts: 1},
		},
		{
			name: "breaker skips are split by state",
			attempts: []ContentModerationReviewAttempt{
				{Reviewer: "deepseek", Outcome: "skipped", Error: "cooldown"},
				{Reviewer: "deepseek", Outcome: "skipped", Error: "half_open_busy"},
				{Reviewer: "yufeng", Outcome: "skipped", Error: "cooldown"},
			},
			want: contentModerationReviewFailureSummary{breakerSkips: 2, cooldownSkips: 1, halfOpenBusySkips: 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require.Equal(t, test.want, summarizeContentModerationReviewFailure(test.reviewErr, test.attempts))
		})
	}
}

func TestContentModerationReviewObservabilityStatusAndStructuredAlert(t *testing.T) {
	sink, releaseLogCapture := captureStructuredLog(t)
	t.Cleanup(releaseLogCapture)
	require.NoError(t, logger.SetLevel("error"))
	t.Cleanup(func() { require.NoError(t, logger.SetLevel("debug")) })

	svc := &ContentModerationService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{}},
	}
	attempts := []ContentModerationReviewAttempt{
		{Reviewer: "deepseek", ChannelID: "primary", Outcome: "error", Error: "response_read_timeout"},
		{Reviewer: "deepseek", ChannelID: "primary", Outcome: "skipped", Error: "cooldown"},
		{Reviewer: "deepseek", ChannelID: "primary", Outcome: "skipped", Error: "half_open_busy"},
	}
	svc.recordContentModerationReviewAttempts(attempts, context.DeadlineExceeded)
	svc.recordContentModerationReviewUnavailable(
		context.Background(),
		"request-observability-test",
		&ContentModerationConfig{SecondLayerStage: ContentModerationSecondLayerStageEnforce},
		"review_unavailable",
		"timeout",
		errors.New("must-not-log sk-sensitive-api-key or private evidence"),
		attempts,
	)

	status, err := svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), status.DeepSeekResponseReadTimeoutCount)
	require.Equal(t, int64(2), status.DeepSeekBreakerSkipCount)
	require.Equal(t, int64(1), status.DeepSeekCooldownSkipCount)
	require.Equal(t, int64(1), status.DeepSeekHalfOpenBusySkipCount)
	require.Equal(t, int64(1), status.ReviewUnavailableCount)
	require.Equal(t, int64(1), status.ReviewUnavailableEnforcedCount)
	require.NotNil(t, status.LastReviewUnavailableAt)

	sink.mu.Lock()
	defer sink.mu.Unlock()
	var eventText string
	for _, event := range sink.events {
		if event != nil && event.Message == "content_moderation.review_unavailable" {
			eventText = event.Message + " " + event.Level + " " + fmt.Sprint(event.Fields)
			break
		}
	}
	require.NotEmpty(t, eventText)
	require.Contains(t, eventText, "error")
	require.Contains(t, eventText, "response_read_timeout")
	require.Contains(t, eventText, "request-observability-test")
	require.NotContains(t, strings.ToLower(eventText), "sensitive-api-key")
	require.NotContains(t, strings.ToLower(eventText), "private evidence")
}
