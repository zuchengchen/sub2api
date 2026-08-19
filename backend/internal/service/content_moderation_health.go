package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"
)

const contentModerationRemoteHeartbeatInterval = time.Minute

func contentModerationUnixNanoTime(value int64) *time.Time {
	if value <= 0 {
		return nil
	}
	t := time.Unix(0, value)
	return &t
}

// runStartupAPIUsabilityTests performs the one paid/real review check made by
// a process lifetime. Providers are de-duplicated so adding multiple channels
// for one provider cannot multiply startup charges.
func (s *ContentModerationService) runStartupAPIUsabilityTests(ctx context.Context) {
	checkedAt := time.Now()
	var channels []ContentModerationDeepSeekChannel
	cfg, err := s.loadConfig(ctx)
	if err == nil && cfg != nil && contentModerationRemoteReviewersEnabled(cfg) {
		seenProviders := make(map[string]struct{})
		for _, channel := range normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels) {
			if !channel.Enabled || strings.TrimSpace(channel.APIKey) == "" ||
				!isSupportedContentModerationRemoteProvider(channel.Provider) {
				continue
			}
			provider := contentModerationRemoteProvider(channel)
			if _, seen := seenProviders[provider]; seen {
				continue
			}
			seenProviders[provider] = struct{}{}
			channels = append(channels, channel)
		}
	}

	// Providers are independent. Probe them concurrently so a slow or broken
	// provider cannot delay readiness for the remaining consensus pool.
	results := make(chan bool, len(channels))
	for _, channel := range channels {
		go func(channel ContentModerationDeepSeekChannel) {
			probeTimeout := 2 * time.Duration(channel.TimeoutMS) * time.Millisecond
			if probeTimeout < time.Second {
				probeTimeout = time.Second
			}
			probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
			ok := s.probeDeepSeekChannelReview(probeCtx, channel, false)
			cancel()
			results <- ok
		}(channel)
	}
	succeeded := 0
	for range channels {
		if <-results {
			succeeded++
		}
	}
	if err != nil {
		slog.Warn("content_moderation.startup_api_usability_test_failed", "error", err)
	}
	s.startupAPIUsabilityConfigured.Store(int64(len(channels)))
	s.startupAPIUsabilitySucceeded.Store(int64(succeeded))
	s.startupAPIUsabilityAt.Store(checkedAt.UnixNano())
	s.startupAPIUsabilityTested.Store(true)
}

func (s *ContentModerationService) remoteHeartbeatWorker(ctx context.Context) {
	// Run once immediately so the admin page has data without waiting a full
	// minute after deployment.
	s.runRemoteHeartbeat(ctx)
	ticker := time.NewTicker(contentModerationRemoteHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runRemoteHeartbeat(ctx)
		}
	}
}

// runRemoteHeartbeat is transport-only. It never sends a model inference
// request and therefore does not consume moderation tokens/model quota.
func (s *ContentModerationService) runRemoteHeartbeat(ctx context.Context) {
	cfg, err := s.loadConfig(ctx)
	if err != nil || cfg == nil {
		if err != nil {
			slog.Warn("content_moderation.remote_heartbeat_config_failed", "error", err)
		}
		return
	}
	for _, channel := range normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels) {
		if ctx.Err() != nil {
			return
		}
		if !channel.Enabled || !isSupportedContentModerationRemoteProvider(channel.Provider) {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Duration(channel.TimeoutMS)*time.Millisecond)
		// The worker owns this request, so use the direct transport probe. The
		// legacy manual /test endpoint keeps its singleflight semantics, while
		// shutdown can cancel this periodic request all the way to the socket.
		result := s.probeDeepSeekChannelConnectivityOnce(probeCtx, channel)
		cancel()
		status := "unreachable"
		if result != nil && result.Reachable {
			status = "reachable"
		}
		errText := ""
		httpStatus := 0
		latency := 0
		if result != nil {
			errText, httpStatus, latency = result.Error, result.HTTPStatus, result.LatencyMS
		}
		s.deepSeekChannelState(channel).recordHeartbeat(time.Now(), status, latency, httpStatus, errText)
	}
}

// TestContentModerationChannel performs an explicit, real API usability test.
// The payload is a fixed harmless health-check sentence and only parsed
// verdict metadata is returned to the caller.
func (s *ContentModerationService) testDeepSeekChannelReview(ctx context.Context, channelID string) (*TestContentModerationDeepSeekChannelResult, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return nil, errors.New("审核渠道 ID 不能为空")
	}
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	var channel *ContentModerationDeepSeekChannel
	for i := range cfg.DeepSeekChannels {
		if cfg.DeepSeekChannels[i].ID == channelID {
			channel = &cfg.DeepSeekChannels[i]
			break
		}
	}
	if channel == nil {
		return nil, errors.New("审核渠道不存在")
	}
	if strings.TrimSpace(channel.APIKey) == "" {
		return nil, errors.New("审核渠道 API Key 未配置")
	}
	probeTimeout := 2 * time.Duration(channel.TimeoutMS) * time.Millisecond
	if probeTimeout < time.Second {
		probeTimeout = time.Second
	}
	requestCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	input := contentModerationDeepSeekReviewProbeInput()
	var result contentModerationSecondLayerResult
	var attempt ContentModerationReviewAttempt
	if contentModerationRemoteProvider(*channel) == ContentModerationRemoteProviderDeepSeek {
		result, attempt, err = s.callContentModerationDeepSeekChannel(requestCtx, *channel, input, true)
	} else {
		result, attempt, err = s.callContentModerationRemoteChannel(requestCtx, *channel, input, true)
	}
	s.recordContentModerationReviewAttempts([]ContentModerationReviewAttempt{attempt}, err)
	checkedAt := time.Now()
	out := &TestContentModerationDeepSeekChannelResult{
		ChannelID:  channel.ID,
		Provider:   contentModerationRemoteProvider(*channel),
		Model:      channel.Model,
		TestType:   "api_usability",
		LatencyMS:  attempt.LatencyMS,
		HTTPStatus: attempt.HTTPStatus,
		CheckedAt:  &checkedAt,
	}
	if err != nil {
		out.Error = trimRunes(redactContentModerationSecrets(err.Error()), 240)
		return out, nil
	}
	out.Reachable = true
	out.HealthValid = true
	out.Category = result.Category
	out.Confidence = result.Confidence
	switch result.normalizedDisposition() {
	case ContentModerationReviewDispositionViolation:
		out.Verdict = "violation"
	case ContentModerationReviewDispositionRestricted:
		out.Verdict = "restricted"
	default:
		out.Verdict = "safe"
	}
	// The explicit admin test is the second allowed way to establish
	// readiness (the other is the one-time startup probe).
	s.deepSeekChannelState(*channel).markReviewHealthy(checkedAt, contentModerationDeepSeekChannelDigest(*channel))
	return out, nil
}

func (s *ContentModerationService) remoteHeartbeatViews(cfg *ContentModerationConfig) []ContentModerationChannelHeartbeatView {
	if cfg == nil {
		return nil
	}
	channels := normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels)
	out := make([]ContentModerationChannelHeartbeatView, 0, len(channels))
	for _, channel := range channels {
		if !channel.Enabled || !isSupportedContentModerationRemoteProvider(channel.Provider) {
			continue
		}
		status, checkedAt, latency, httpStatus, lastError := s.deepSeekChannelState(channel).heartbeatSnapshot()
		if status == "" {
			status = "untested"
		}
		out = append(out, ContentModerationChannelHeartbeatView{
			ChannelID: channel.ID, Provider: contentModerationRemoteProvider(channel), Status: status,
			CheckedAt: checkedAt, LatencyMS: latency, HTTPStatus: httpStatus, Error: lastError,
		})
	}
	return out
}
