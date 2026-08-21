package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func contentModerationResilienceCandidate(t *testing.T, text, keyword, tier string, whole bool) contentModerationCandidateFragment {
	t.Helper()
	fragment, ok := newContentModerationFragment("user", "text", "messages.latest.content", text)
	require.True(t, ok)
	matches := newContentModerationPrefilterMatcher([]string{keyword}).MatchAll(fragment.Text)
	require.NotEmpty(t, matches)
	return contentModerationCandidateFragment{
		Fragment: fragment, Matches: matches, Tier: tier, WholeFragment: whole,
	}
}

func TestContentModerationFullContextChunksReviewTailRiskAndNeverCachePartialEvidence(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		calls.Add(1)
		if strings.Contains(string(body), "TAIL_RISK") {
			contentModerationDeepSeekRuntimeWriteEnvelope(
				t, w, `{"disposition":"violation","confidence":0.97,"category":"cyber_abuse","reason":"尾块风险"}`, "stop",
			)
			return
		}
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"disposition":"allow","confidence":0.04,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	contentModerationCandidateDeliveryMarkReviewReady(svc, cfg)
	prefix := "exploit " + strings.Repeat("ordinary context ", 560)
	safeCandidate := contentModerationResilienceCandidate(t, prefix+"SAFE_TAIL", "exploit", "candidate", true)
	riskyCandidate := contentModerationResilienceCandidate(t, prefix+"TAIL_RISK", "exploit", "candidate", true)

	safe := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "chunk-safe"},
		cfg, cfg.fragmentCacheNamespace(), []contentModerationCandidateFragment{safeCandidate}, false,
	)
	require.True(t, safe.Allowed)
	require.False(t, safe.Blocked)
	require.Zero(t, contextualRoutingCacheEntryCount(cache), "a full-chunk verdict must not use the bounded evidence cache key")
	safeCalls := calls.Load()
	require.GreaterOrEqual(t, safeCalls, int64(3), "a safe result requires every source chunk")

	risky := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "chunk-risk"},
		cfg, cfg.fragmentCacheNamespace(), []contentModerationCandidateFragment{riskyCandidate}, false,
	)
	require.True(t, risky.Blocked)
	require.Equal(t, ContentModerationActionSecondLayerBlock, risky.Action)
	require.Greater(t, calls.Load(), safeCalls, "the tail-risk request must reach a later source chunk")
	require.Zero(t, contextualRoutingCacheEntryCount(cache))

	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	require.Equal(t, "full_context_chunks", logs[0].EvidenceMode)
	require.Equal(t, "full_context_chunks", logs[1].EvidenceMode)
}

func TestContentModerationFullContextChunksFailClosedWhenAnyChunkIsUnavailable(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		calls.Add(1)
		if strings.Contains(string(body), "CHUNK_FAIL") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"disposition":"allow","confidence":0.04,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	contentModerationCandidateDeliveryMarkReviewReady(svc, cfg)
	candidate := contentModerationResilienceCandidate(
		t, "exploit "+strings.Repeat("ordinary context ", 560)+"CHUNK_FAIL", "exploit", "candidate", true,
	)

	decision := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "chunk-unavailable"},
		cfg, cfg.fragmentCacheNamespace(), []contentModerationCandidateFragment{candidate}, false,
	)
	require.False(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
	require.Equal(t, ContentModerationActionReviewUnavailable, decision.Action)
	require.GreaterOrEqual(t, calls.Load(), int64(3))
	require.Zero(t, contextualRoutingCacheEntryCount(cache))
}

func TestContentModerationRiskTieredTransientOutageAllowsAndAuditsWithoutCaching(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cfg.RemoteUnavailablePolicy = ContentModerationRemoteUnavailableRiskTiered
	cfg.normalize()
	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	contentModerationCandidateDeliveryMarkReviewReady(svc, cfg)

	decision := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "degraded-transient"},
		cfg, cfg.fragmentCacheNamespace(), contentModerationCandidateDeliveryFixtures(t)[:1], false,
	)
	require.True(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.Equal(t, ContentModerationActionDegradedAllow, decision.Action)
	require.Zero(t, contextualRoutingCacheEntryCount(cache))
	require.Equal(t, int64(1), svc.reviewObservability.reviewUnavailableCount.Load())
	require.Zero(t, svc.reviewObservability.reviewUnavailableEnforcedCount.Load())
	require.Equal(t, int64(1), svc.reviewObservability.reviewUnavailableDegradedCount.Load())

	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, ContentModerationActionDegradedAllow, logs[0].Action)
	require.Equal(t, "review_unavailable_degraded_allow", logs[0].DecisionSource)
	require.Equal(t, "degraded_allow", logs[0].ReviewOutcome)
	require.NotEmpty(t, logs[0].Error)
	require.False(t, logs[0].Flagged)
}

func TestContentModerationRiskTieredEligibilityMatrix(t *testing.T) {
	now := time.Now()
	baseCfg := contentModerationCandidateDeliveryConfig("https://review.example", ContentModerationSecondLayerStageEnforce)
	baseCfg.RemoteUnavailablePolicy = ContentModerationRemoteUnavailableRiskTiered
	baseCfg.normalize()
	candidate := contentModerationResilienceCandidate(t, "explain exploit detection", "exploit", "candidate", true)
	bundle := buildContentModerationCandidateEvidence(
		[]contentModerationCandidateFragment{candidate}, contentModerationEvidenceWindowBudgetRunes, baseCfg,
	)
	require.False(t, bundle.CoverageIncomplete)
	require.False(t, bundle.ContextIncomplete)
	baseWork := contentModerationCandidateReviewWork{
		bundle: bundle, primary: candidate.Fragment, source: candidate.Fragment,
		matches: candidate.Matches, sourceComplete: true, reviewRequired: true, requireHealthyReviewer: true,
	}
	baseOutcome := contentModerationCandidateReviewOutcome{
		parserStatus: "error", err: errors.New("temporary upstream failure"),
		result: contentModerationSecondLayerResult{ReviewAttempts: []ContentModerationReviewAttempt{{
			Reviewer: "deepseek", Provider: ContentModerationRemoteProviderDeepSeek,
			Outcome: "error", Error: "http_503", HTTPStatus: http.StatusServiceUnavailable,
		}}},
	}

	tests := []struct {
		name       string
		mutateCfg  func(*ContentModerationConfig)
		mutateWork func(*contentModerationCandidateReviewWork)
		mutate     func(*contentModerationCandidateReviewOutcome)
		successAt  time.Time
		want       bool
	}{
		{name: "recent transient HTTP failure", successAt: now.Add(-time.Minute), want: true},
		{name: "network failure", successAt: now.Add(-time.Minute), want: true, mutate: func(outcome *contentModerationCandidateReviewOutcome) {
			outcome.result.ReviewAttempts[0].Error = "network"
		}},
		{name: "expired grace", successAt: now.Add(-contentModerationDegradedAllowGrace - time.Second)},
		{name: "cold start has no success"},
		{name: "parser failure", successAt: now.Add(-time.Minute), mutate: func(outcome *contentModerationCandidateReviewOutcome) {
			outcome.parserStatus = "parse_error"
			outcome.result.ReviewAttempts[0].Error = "invalid_json"
		}},
		{name: "authentication failure", successAt: now.Add(-time.Minute), mutate: func(outcome *contentModerationCandidateReviewOutcome) {
			outcome.result.ReviewAttempts[0].Error = "http_401"
			outcome.result.ReviewAttempts[0].HTTPStatus = http.StatusUnauthorized
		}},
		{name: "canceled request", successAt: now.Add(-time.Minute), mutate: func(outcome *contentModerationCandidateReviewOutcome) {
			outcome.result.ReviewAttempts[0].Error = "canceled"
		}},
		{name: "policy restricted tier", successAt: now.Add(-time.Minute), mutateWork: func(work *contentModerationCandidateReviewWork) {
			work.bundle.PrimaryTier = contentModerationKeywordTierPolicyRestrictedReview
		}},
		{name: "contextual tier", successAt: now.Add(-time.Minute), mutateWork: func(work *contentModerationCandidateReviewWork) {
			work.bundle.PrimaryTier = contentModerationKeywordTierContextualReview
		}},
		{name: "incomplete evidence", successAt: now.Add(-time.Minute), mutateWork: func(work *contentModerationCandidateReviewWork) {
			work.bundle.CoverageIncomplete = true
		}},
		{name: "fail closed policy", successAt: now.Add(-time.Minute), mutateCfg: func(cfg *ContentModerationConfig) {
			cfg.RemoteUnavailablePolicy = ContentModerationRemoteUnavailableFailClosed
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloneContentModerationConfig(baseCfg)
			work := baseWork
			outcome := baseOutcome
			outcome.result.ReviewAttempts = append([]ContentModerationReviewAttempt(nil), baseOutcome.result.ReviewAttempts...)
			if tc.mutateCfg != nil {
				tc.mutateCfg(cfg)
			}
			if tc.mutateWork != nil {
				tc.mutateWork(&work)
			}
			if tc.mutate != nil {
				tc.mutate(&outcome)
			}
			svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
			if !tc.successAt.IsZero() {
				svc.deepSeekChannelState(cfg.DeepSeekChannels[0]).markReviewHealthy(
					tc.successAt, contentModerationDeepSeekChannelDigest(cfg.DeepSeekChannels[0]),
				)
			}
			require.Equal(t, tc.want, svc.contentModerationReviewCanDegrade(cfg, work, outcome, now))
		})
	}
}

func TestContentModerationRiskTieredMixedFailuresNeverHideHardFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if strings.Contains(string(body), "reverse shell") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		contentModerationDeepSeekRuntimeWriteEnvelope(t, w, `not-json`, "stop")
	}))
	defer server.Close()

	for _, reverse := range []bool{false, true} {
		cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
		cfg.RemoteUnavailablePolicy = ContentModerationRemoteUnavailableRiskTiered
		cfg.normalize()
		repo := &contentModerationReplayRepo{}
		svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
		contentModerationCandidateDeliveryMarkReviewReady(svc, cfg)
		candidates := contentModerationCandidateDeliveryFixtures(t)
		if reverse {
			candidates[0], candidates[1] = candidates[1], candidates[0]
		}
		decision := svc.checkUnifiedCandidateEvidence(
			context.Background(), ContentModerationCheckInput{RequestID: "mixed-failures"},
			cfg, cfg.fragmentCacheNamespace(), candidates, false,
		)
		require.False(t, decision.Allowed)
		require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
		require.Equal(t, ContentModerationActionReviewUnavailable, decision.Action)
		for _, log := range repo.snapshotLogs() {
			require.NotEqual(t, ContentModerationActionDegradedAllow, log.Action)
		}
	}
}

func TestContentModerationRiskTieredPolicyFloorWinsOverDegradedCandidateInAnyOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	for _, reverse := range []bool{false, true} {
		cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
		cfg.RemoteUnavailablePolicy = ContentModerationRemoteUnavailableRiskTiered
		cfg.normalize()
		repo := &contentModerationReplayRepo{}
		svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
		contentModerationCandidateDeliveryMarkReviewReady(svc, cfg)
		candidates := contentModerationCandidateDeliveryFixtures(t)
		candidates[1].Tier = contentModerationKeywordTierPolicyRestrictedReview
		if reverse {
			candidates[0], candidates[1] = candidates[1], candidates[0]
		}
		decision := svc.checkUnifiedCandidateEvidence(
			context.Background(), ContentModerationCheckInput{RequestID: "policy-floor-mixed"},
			cfg, cfg.fragmentCacheNamespace(), candidates, false,
		)
		require.True(t, decision.Blocked)
		require.False(t, decision.Flagged)
		require.Equal(t, ContentModerationActionRestrictedBlock, decision.Action)
		for _, log := range repo.snapshotLogs() {
			require.NotEqual(t, ContentModerationActionDegradedAllow, log.Action)
		}
	}
}

func TestContentModerationReviewRetryAfterTracksEarliestCooldown(t *testing.T) {
	now := time.Now()
	cfg := contentModerationCandidateDeliveryConfig("https://review.example", ContentModerationSecondLayerStageEnforce)
	svc := NewContentModerationService(nil, nil, nil, nil, nil, nil, nil, nil)
	state := svc.deepSeekChannelState(cfg.DeepSeekChannels[0])
	state.markReviewHealthy(now, contentModerationDeepSeekChannelDigest(cfg.DeepSeekChannels[0]))
	state.mu.Lock()
	state.cooldownUntil = now.Add(17200 * time.Millisecond)
	state.mu.Unlock()

	require.Equal(t, 18, svc.contentModerationReviewRetryAfter(cfg, now))
	require.Equal(t, 1, svc.contentModerationReviewRetryAfter(cfg, now.Add(18*time.Second)))
}
