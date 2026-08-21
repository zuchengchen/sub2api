package service

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	contentModerationFullReviewChunkOverlapRunes = 256
	contentModerationFullReviewMaxChunks         = 16
	contentModerationFullReviewConcurrency       = 2
	contentModerationFullReviewGlobalConcurrency = 8
	contentModerationDegradedAllowGrace          = 2 * time.Minute
)

func contentModerationFullReviewInputs(
	work contentModerationCandidateReviewWork,
	limit int,
) ([]contentModerationSecondLayerInput, bool) {
	if !work.sourceComplete || strings.TrimSpace(work.source.Text) == "" {
		return nil, false
	}
	if limit <= 0 {
		limit = contentModerationEvidenceWindowBudgetRunes
	}
	runes := []rune(work.source.Text)
	if len(runes) == 0 {
		return nil, false
	}
	matches := normalizedCandidateMatches(work.source.Text, work.matches)
	if len(matches) == 0 {
		return nil, false
	}
	overlap := contentModerationFullReviewChunkOverlapRunes
	if overlap >= limit {
		overlap = limit / 4
	}
	step := limit - overlap
	if step < 1 {
		step = limit
	}
	maxRunes := limit + (contentModerationFullReviewMaxChunks-1)*step
	if len(runes) > maxRunes {
		return nil, false
	}
	inputs := make([]contentModerationSecondLayerInput, 0, (len(runes)+step-1)/step)
	coveredMatches := make(map[string]struct{}, len(matches))
	for start := 0; start < len(runes); start += step {
		end := start + limit
		if end > len(runes) {
			end = len(runes)
		}
		rawChunk := strings.TrimSpace(string(runes[start:end]))
		if rawChunk == "" {
			return nil, false
		}
		indicators := make([]string, 0)
		chunkMatches := make([]contentModerationKeywordMatch, 0)
		seenIndicators := make(map[string]struct{})
		for _, match := range matches {
			if match.Start < start || match.End > end {
				continue
			}
			chunkMatches = append(chunkMatches, match)
			indicator := strings.TrimSpace(match.Keyword)
			if indicator == "" {
				continue
			}
			key := strings.ToLower(indicator)
			if _, exists := seenIndicators[key]; exists {
				continue
			}
			seenIndicators[key] = struct{}{}
			indicators = append(indicators, indicator)
		}
		sort.Strings(indicators)
		redacted := strings.TrimSpace(redactContentModerationEvidenceText(rawChunk))
		if redacted == "" {
			return nil, false
		}
		if len(indicators) > 0 {
			redactedMatches := newContentModerationPrefilterMatcher(indicators).MatchAll(redacted)
			retainedIndicators := make(map[string]struct{}, len(redactedMatches))
			for _, match := range redactedMatches {
				retainedIndicators[normalizedContentModerationKeywordKey(match.Keyword)] = struct{}{}
			}
			for _, match := range chunkMatches {
				if _, retained := retainedIndicators[normalizedContentModerationKeywordKey(match.Keyword)]; retained {
					coveredMatches[contentModerationSourceMatchKey(match)] = struct{}{}
				}
			}
		}
		fragment, ok := newContentModerationFragment(work.source.Role, work.source.Kind, work.source.Path, redacted)
		if !ok {
			return nil, false
		}
		fragment.ContextClass = work.source.ContextClass
		inputs = append(inputs, contentModerationSecondLayerInput{
			Fragment: fragment,
			Evidence: moderationEvidence{
				Text: redacted, Mode: "full_context_chunks", Truncated: len(runes) > limit,
				Segments: []moderationEvidenceSegment{{
					Text: redacted, Origin: redactContentModerationPath(work.source.Path), Role: work.source.Role,
					Kind: work.source.Kind, ContextClass: work.source.ContextClass,
					ExtractorVersion: ContentModerationEvidencePolicyVersion, Truncated: len(runes) > limit,
				}},
			},
			KeywordTier:       defaultContentModerationString(work.bundle.PrimaryTier, "candidate"),
			KeywordRuleID:     work.bundle.PrimaryRuleID,
			MatchedIndicators: indicators,
			Background:        work.background,
		})
		if len(inputs) > contentModerationFullReviewMaxChunks {
			return nil, false
		}
		if end == len(runes) {
			break
		}
	}
	if len(inputs) == 0 || len(coveredMatches) != len(matches) {
		return nil, false
	}
	for index := range inputs {
		inputs[index].ChunkIndex = index
		inputs[index].ChunkCount = len(inputs)
	}
	return inputs, true
}

type contentModerationChunkReviewOutcome struct {
	index     int
	result    contentModerationSecondLayerResult
	attempted bool
	err       error
}

func (s *ContentModerationService) acquireContentModerationFullReviewSlot(ctx context.Context) (func(), bool) {
	s.fullReviewSlotsOnce.Do(func() {
		s.fullReviewSlots = make(chan struct{}, contentModerationFullReviewGlobalConcurrency)
	})
	select {
	case s.fullReviewSlots <- struct{}{}:
		return func() { <-s.fullReviewSlots }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (s *ContentModerationService) scanContentModerationFullReviewChunks(
	ctx context.Context,
	cfg *ContentModerationConfig,
	inputs []contentModerationSecondLayerInput,
) (contentModerationSecondLayerResult, bool, error) {
	if len(inputs) == 0 {
		return contentModerationSecondLayerResult{}, false, errors.New("full-context review has no chunks")
	}
	reviewCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan contentModerationChunkReviewOutcome, len(inputs))
	semaphore := make(chan struct{}, contentModerationFullReviewConcurrency)
	for index, input := range inputs {
		index := index
		input := input
		go func() {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-reviewCtx.Done():
				results <- contentModerationChunkReviewOutcome{index: index, err: reviewCtx.Err()}
				return
			}
			releaseGlobal, acquired := s.acquireContentModerationFullReviewSlot(reviewCtx)
			if !acquired {
				results <- contentModerationChunkReviewOutcome{index: index, err: reviewCtx.Err()}
				return
			}
			defer releaseGlobal()
			result, attempted, err := s.scanUnifiedSecondLayerPrepared(reviewCtx, cfg, input)
			results <- contentModerationChunkReviewOutcome{index: index, result: result, attempted: attempted, err: err}
		}()
	}

	outcomes := make([]contentModerationChunkReviewOutcome, len(inputs))
	var failures []error
	attempted := false
	var risky *contentModerationSecondLayerResult
	var safe *contentModerationSecondLayerResult
	for range inputs {
		outcome := <-results
		outcomes[outcome.index] = outcome
		attempted = attempted || outcome.attempted
		if outcome.result.Blocked {
			candidate := outcome.result
			if risky == nil || (candidate.normalizedDisposition() == ContentModerationReviewDispositionViolation &&
				risky.normalizedDisposition() != ContentModerationReviewDispositionViolation) {
				risky = &candidate
			}
			cancel()
			continue
		}
		if outcome.err != nil {
			failures = append(failures, outcome.err)
			continue
		}
		candidate := outcome.result
		if safe == nil || candidate.Confidence > safe.Confidence {
			safe = &candidate
		}
	}
	allAttempts := make([]ContentModerationReviewAttempt, 0, len(inputs))
	for _, outcome := range outcomes {
		allAttempts = append(allAttempts, outcome.result.ReviewAttempts...)
	}
	if risky != nil {
		risky.ReviewAttempts = allAttempts
		risky.EvidenceMode = "full_context_chunks"
		risky.EvidenceTruncated = len(inputs) > 1
		return *risky, true, nil
	}
	if len(failures) > 0 {
		return contentModerationSecondLayerResult{
			ReviewAttempts: allAttempts, ConsensusStatus: "unavailable", EvidenceMode: "full_context_chunks",
			EvidenceTruncated: len(inputs) > 1,
		}, attempted, errors.Join(failures...)
	}
	if safe == nil {
		return contentModerationSecondLayerResult{ReviewAttempts: allAttempts}, attempted, errors.New("full-context review produced no verdict")
	}
	safe.ReviewAttempts = allAttempts
	safe.EvidenceMode = "full_context_chunks"
	safe.EvidenceTruncated = len(inputs) > 1
	return *safe, true, nil
}

func (s *ContentModerationService) latestContentModerationReviewSuccess(
	cfg *ContentModerationConfig,
) time.Time {
	var latest time.Time
	if cfg == nil {
		return latest
	}
	for _, channel := range normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels) {
		if !channel.Enabled || strings.TrimSpace(channel.APIKey) == "" {
			continue
		}
		if succeededAt := s.deepSeekChannelState(channel).lastReviewSuccess(); succeededAt.After(latest) {
			latest = succeededAt
		}
	}
	return latest
}

func contentModerationTransientAttempt(attempt ContentModerationReviewAttempt) bool {
	if attempt.Outcome == "success" {
		return attempt.Verdict == "" || attempt.Verdict == "safe"
	}
	kind := strings.ToLower(strings.TrimSpace(attempt.Error))
	switch kind {
	case "network", "timeout", "response_read", "response_read_timeout", "cooldown", "half_open_busy":
		return true
	case "", "canceled", "invalid_json", "response_too_large", "payload", "base_url", "request",
		"key_unavailable", "auth_disabled":
		return false
	}
	if strings.HasSuffix(kind, "_timeout") {
		return true
	}
	if !strings.HasPrefix(kind, "http_") {
		return false
	}
	status, err := strconv.Atoi(strings.TrimPrefix(kind, "http_"))
	return err == nil && (status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500)
}

func (s *ContentModerationService) contentModerationTransientReadinessOutage(
	cfg *ContentModerationConfig,
	now time.Time,
) bool {
	if cfg == nil {
		return false
	}
	for _, channel := range normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels) {
		if !channel.Enabled || strings.TrimSpace(channel.APIKey) == "" {
			continue
		}
		state := s.deepSeekChannelState(channel)
		usable, reason := state.reviewIsUsable(now)
		if !usable && reason == "cooldown" && now.Sub(state.lastReviewSuccess()) <= contentModerationDegradedAllowGrace {
			return true
		}
	}
	return false
}

func (s *ContentModerationService) contentModerationReviewCanDegrade(
	cfg *ContentModerationConfig,
	work contentModerationCandidateReviewWork,
	outcome contentModerationCandidateReviewOutcome,
	now time.Time,
) bool {
	if cfg == nil || cfg.RemoteUnavailablePolicy != ContentModerationRemoteUnavailableRiskTiered ||
		cfg.SecondLayerStage != ContentModerationSecondLayerStageEnforce || work.bundle.PrimaryTier != "candidate" ||
		!work.sourceComplete || work.bundle.CoverageIncomplete || outcome.result.Blocked {
		return false
	}
	if outcome.parserStatus == "evidence_truncated" {
		return work.bundle.ContextIncomplete && contentModerationReviewResultIsConclusiveSafe(outcome.result)
	}
	if work.bundle.ContextIncomplete {
		return false
	}
	if _, complete := contentModerationFullReviewInputs(work, contentModerationReviewInputLimit(cfg)); !complete {
		return false
	}
	for _, attempt := range outcome.result.ReviewAttempts {
		if !contentModerationTransientAttempt(attempt) {
			return false
		}
	}
	latestSuccess := s.latestContentModerationReviewSuccess(cfg)
	if latestSuccess.IsZero() || now.Before(latestSuccess) || now.Sub(latestSuccess) > contentModerationDegradedAllowGrace {
		return false
	}
	if outcome.parserStatus == "health_not_ready" {
		return s.contentModerationTransientReadinessOutage(cfg, now)
	}
	if outcome.parserStatus != "timeout" && outcome.parserStatus != "error" {
		return false
	}
	return len(outcome.result.ReviewAttempts) > 0
}

func contentModerationReviewResultIsConclusiveSafe(result contentModerationSecondLayerResult) bool {
	if result.Blocked || result.normalizedDisposition() != ContentModerationReviewDispositionAllow {
		return false
	}
	for _, attempt := range result.ReviewAttempts {
		if attempt.Outcome == "success" && attempt.Verdict == "safe" {
			return true
		}
	}
	return false
}

func (s *ContentModerationService) contentModerationReviewRetryAfter(cfg *ContentModerationConfig, now time.Time) int {
	retryAfter := 2
	soonest := time.Duration(0)
	if cfg != nil {
		for _, channel := range normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels) {
			if !channel.Enabled || strings.TrimSpace(channel.APIKey) == "" {
				continue
			}
			_, breaker, _, _, cooldownUntil, _, _ := s.deepSeekChannelState(channel).snapshot(now)
			if breaker == "half_open" {
				return 1
			}
			if cooldownUntil == nil || !cooldownUntil.After(now) {
				continue
			}
			remaining := cooldownUntil.Sub(now)
			if soonest == 0 || remaining < soonest {
				soonest = remaining
			}
		}
	}
	if soonest > 0 {
		retryAfter = int(math.Ceil(soonest.Seconds()))
	}
	if retryAfter < 1 {
		return 1
	}
	if retryAfter > 60 {
		return 60
	}
	return retryAfter
}
