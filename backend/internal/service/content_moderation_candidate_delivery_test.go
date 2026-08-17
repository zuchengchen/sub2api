package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationEveryLayer2CandidateGetsIndependentDeepSeekReview(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.05,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	decision := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "independent-candidates"},
		cfg, cfg.fragmentCacheNamespace(), contentModerationCandidateDeliveryFixtures(t), false,
	)

	require.True(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.Equal(t, int64(2), calls.Load(), "candidate fragments must not be merged into one model call")
	require.Len(t, repo.snapshotLogs(), 2)
}

func TestContentModerationLayer2SafeResultIsAlwaysAuditedBeforeCaching(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.05,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cfg.RecordNonHits = false
	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	candidate := contentModerationCandidateDeliveryFixtures(t)[:1]

	for index := range 2 {
		decision := svc.checkUnifiedCandidateEvidence(
			context.Background(), ContentModerationCheckInput{RequestID: fmt.Sprintf("safe-audit-%d", index)},
			cfg, cfg.fragmentCacheNamespace(), candidate, false,
		)
		require.True(t, decision.Allowed)
	}

	require.Equal(t, int64(1), calls.Load())
	logs := repo.snapshotLogs()
	require.Len(t, logs, 2, "every Layer 2 result must have an audit row even when record_non_hits is disabled")
	require.False(t, logs[0].CacheHit)
	require.Equal(t, "model", logs[0].DecisionSource)
	require.True(t, logs[1].CacheHit)
	require.Equal(t, "cache_replay", logs[1].DecisionSource)
	require.NotNil(t, logs[1].SourceLogID)
	require.Equal(t, logs[0].ID, *logs[1].SourceLogID)
	require.Nil(t, logs[1].UpstreamLatencyMS)
}

func TestContentModerationEnforceReviewsAllCandidatesBeforeAnyDisposition(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		if strings.Contains(string(body), "reverse shell") {
			contentModerationDeepSeekRuntimeWriteEnvelope(
				t, w, `{"confidence":0.95,"category":"cyber_abuse","reason":"明确攻击意图"}`, "stop",
			)
			return
		}
		contentModerationDeepSeekRuntimeWriteEnvelope(t, w, `not-json`, "stop")
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cfg.AutoBanEnabled = true
	cfg.BanThreshold = 1
	repo := &contentModerationReplayRepo{}
	userRepo := &contentModerationTestUserRepo{user: &User{ID: 81, Role: RoleUser, Status: StatusActive}}
	svc := NewContentModerationService(nil, repo, nil, nil, userRepo, nil, nil, nil)
	decision := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "enforce-two-phase", UserID: 81, UserRole: RoleUser},
		cfg, cfg.fragmentCacheNamespace(), contentModerationCandidateDeliveryFixtures(t), false,
	)

	require.False(t, decision.Allowed)
	require.False(t, decision.Blocked)
	require.False(t, decision.Flagged)
	require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
	require.Equal(t, ContentModerationActionReviewUnavailable, decision.Action)
	require.Equal(t, int64(2), calls.Load())
	require.Zero(t, repo.countCalls, "an incomplete Enforce batch must not count a violation")
	require.Empty(t, userRepo.updated, "an incomplete Enforce batch must not ban or mutate the user")

	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	require.Equal(t, "model_enforce_suppressed", logs[0].DecisionSource)
	require.Equal(t, "risky", logs[0].ReviewOutcome)
	require.Zero(t, logs[0].ViolationCount)
	require.Equal(t, "review_unavailable", logs[1].DecisionSource)
	require.Equal(t, "unavailable", logs[1].ReviewOutcome)
	require.Zero(t, logs[1].ViolationCount)
}

func TestContentModerationLayer2RiskCacheBlocksEveryRequestAndAppliesSideEffectsOnce(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.95,"category":"cyber_abuse","reason":"明确攻击意图"}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cfg.AutoBanEnabled = true
	cfg.BanThreshold = 1
	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	userRepo := &contentModerationTestUserRepo{user: &User{ID: 82, Role: RoleUser, Status: StatusActive}}
	svc := NewContentModerationService(nil, repo, cache, nil, userRepo, nil, nil, nil)
	candidate := contentModerationCandidateDeliveryFixtures(t)[:1]

	for index := range 2 {
		decision := svc.checkUnifiedCandidateEvidence(
			context.Background(), ContentModerationCheckInput{
				RequestID: fmt.Sprintf("cached-risk-%d", index), UserID: 82, UserRole: RoleUser,
			}, cfg, cfg.fragmentCacheNamespace(), candidate, false,
		)
		require.True(t, decision.Blocked)
		require.True(t, decision.Flagged)
		require.Equal(t, ContentModerationActionSecondLayerBlock, decision.Action)
	}

	require.Equal(t, int64(1), calls.Load())
	require.Equal(t, 1, repo.countCalls)
	require.Len(t, userRepo.updated, 1)
	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	require.False(t, logs[0].CacheHit)
	require.Equal(t, "model", logs[0].DecisionSource)
	require.True(t, logs[1].CacheHit)
	require.Equal(t, "cache_replay", logs[1].DecisionSource)
	require.Equal(t, logs[0].DeepSeekConfidence, logs[1].DeepSeekConfidence)
	require.Equal(t, logs[0].DeepSeekCategory, logs[1].DeepSeekCategory)
	require.Equal(t, logs[0].DeepSeekReason, logs[1].DeepSeekReason)
	require.NotNil(t, logs[1].SourceLogID)
	require.Equal(t, logs[0].ID, *logs[1].SourceLogID)
	require.Nil(t, logs[1].UpstreamLatencyMS)
	require.Zero(t, logs[1].ViolationCount)
	require.False(t, logs[1].AutoBanned)
}

func TestContentModerationCanceledFlightLeaderCannotPublishUndisposedRisk(t *testing.T) {
	var calls atomic.Int64
	started := make(chan struct{})
	release := make(chan struct{})
	var startedOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		startedOnce.Do(func() { close(started) })
		<-release
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.95,"category":"cyber_abuse","reason":"明确攻击意图"}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cfg.AutoBanEnabled = true
	cfg.BanThreshold = 1
	cache := &contentModerationReplayCache{}
	repo := &contentModerationCancelAwareRepo{}
	userRepo := &contentModerationTestUserRepo{user: &User{ID: 83, Role: RoleUser, Status: StatusActive}}
	svc := NewContentModerationService(nil, repo, cache, nil, userRepo, nil, nil, nil)
	candidate := contentModerationCandidateDeliveryFixtures(t)[:1]

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDecision := make(chan *ContentModerationDecision, 1)
	go func() {
		leaderDecision <- svc.checkUnifiedCandidateEvidence(
			leaderCtx, ContentModerationCheckInput{RequestID: "canceled-leader", UserID: 83, UserRole: RoleUser},
			cfg, cfg.fragmentCacheNamespace(), candidate, false,
		)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("leader model call did not start")
	}

	followerDecision := make(chan *ContentModerationDecision, 1)
	go func() {
		followerDecision <- svc.checkUnifiedCandidateEvidence(
			context.Background(), ContentModerationCheckInput{RequestID: "active-follower", UserID: 83, UserRole: RoleUser},
			cfg, cfg.fragmentCacheNamespace(), candidate, false,
		)
	}()
	time.Sleep(20 * time.Millisecond)
	cancelLeader()
	close(release)

	leader := <-leaderDecision
	require.Equal(t, ContentModerationActionReviewUnavailable, leader.Action)
	follower := <-followerDecision
	require.True(t, follower.Blocked)
	require.Equal(t, ContentModerationActionSecondLayerBlock, follower.Action)
	require.Equal(t, int64(1), calls.Load())
	require.Equal(t, 1, repo.countCalls)
	require.Len(t, userRepo.updated, 1)

	replay := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "post-cancel-replay", UserID: 83, UserRole: RoleUser},
		cfg, cfg.fragmentCacheNamespace(), candidate, false,
	)
	require.True(t, replay.Blocked)
	require.Equal(t, int64(1), calls.Load())
	require.Equal(t, 1, repo.countCalls, "cache replay must not repeat the first completed disposition")
	require.Len(t, userRepo.updated, 1)

	logs := repo.snapshotLogs()
	require.Len(t, logs, 3)
	logsByRequest := make(map[string]ContentModerationLog, len(logs))
	for _, log := range logs {
		logsByRequest[log.RequestID] = log
	}
	canceledLog := logsByRequest["canceled-leader"]
	followerLog := logsByRequest["active-follower"]
	replayLog := logsByRequest["post-cancel-replay"]
	require.Equal(t, "review_unavailable", canceledLog.DecisionSource)
	require.Contains(t, []string{"model", "model_coalesced"}, followerLog.DecisionSource)
	require.Equal(t, "cache_replay", replayLog.DecisionSource)
	require.NotNil(t, replayLog.SourceLogID)
	require.Equal(t, followerLog.ID, *replayLog.SourceLogID)
}

func TestContentModerationWhitelistRiskCacheIsSharedAndPromotedForEnforce(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.95,"category":"cyber_abuse","reason":"明确攻击意图"}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	candidate := contentModerationCandidateDeliveryFixtures(t)[:1]

	shadow := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "whitelist-shadow", UserID: 84, UserRole: RoleUser},
		cfg, cfg.fragmentCacheNamespace(), candidate, true,
	)
	require.True(t, shadow.Allowed)
	require.Eventually(t, func() bool {
		return len(repo.snapshotLogs()) == 1
	}, time.Second, 10*time.Millisecond, "whitelist shadow review did not finish")
	require.Equal(t, 1, contextualRoutingCacheEntryCount(cache), "whitelist risk must publish a shared model-result cache entry")
	bundle := buildContentModerationCandidateEvidence(candidate, contentModerationEvidenceWindowBudgetRunes, cfg)
	entry, found, err := cache.GetFragmentCacheEntry(context.Background(), cfg.fragmentCacheNamespace(), bundle.CacheHash)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, entry.DispositionApplied, "whitelist audit must not claim that Enforce disposition ran")

	enforced := svc.checkUnifiedCandidateEvidence(
		context.Background(), ContentModerationCheckInput{RequestID: "later-enforce", UserID: 84, UserRole: RoleUser},
		cfg, cfg.fragmentCacheNamespace(), candidate, false,
	)
	require.True(t, enforced.Blocked)
	require.Equal(t, int64(1), calls.Load(), "Enforce must promote the cached model result without another DeepSeek call")
	require.Equal(t, 1, repo.countCalls, "the first regular Enforce request must apply disposition once")
	require.Equal(t, 1, contextualRoutingCacheEntryCount(cache))
	entry, found, err = cache.GetFragmentCacheEntry(context.Background(), cfg.fragmentCacheNamespace(), bundle.CacheHash)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, entry.DispositionApplied)
	logs := repo.snapshotLogs()
	require.Len(t, logs, 2)
	require.Equal(t, "model_whitelist_shadow", logs[0].DecisionSource)
	require.Equal(t, "cache_promotion", logs[1].DecisionSource)
	require.NotNil(t, logs[1].SourceLogID)
	require.Equal(t, logs[0].ID, *logs[1].SourceLogID)
	require.Nil(t, logs[1].UpstreamLatencyMS)
	require.NotNil(t, entry.SourceLogID)
	require.Equal(t, logs[1].ID, *entry.SourceLogID)
}

func TestContentModerationLayer2FailuresAreNeverCached(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(t, w, `not-json`, "stop")
	}))
	defer server.Close()

	cfg := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	candidate := contentModerationCandidateDeliveryFixtures(t)[:1]

	for index := range 2 {
		decision := svc.checkUnifiedCandidateEvidence(
			context.Background(), ContentModerationCheckInput{RequestID: fmt.Sprintf("uncached-failure-%d", index)},
			cfg, cfg.fragmentCacheNamespace(), candidate, false,
		)
		require.Equal(t, http.StatusServiceUnavailable, decision.StatusCode)
		require.Equal(t, ContentModerationActionReviewUnavailable, decision.Action)
	}

	require.Equal(t, int64(2), calls.Load())
	require.Zero(t, svc.fragmentCacheHits.Load())
	require.Equal(t, int64(2), svc.fragmentCacheMisses.Load())
	for _, log := range repo.snapshotLogs() {
		require.False(t, log.CacheHit)
		require.Equal(t, "review_unavailable", log.DecisionSource)
	}
}

func TestContentModerationLayer2CacheInvalidatesWhenDecisionConfigChanges(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.55,"category":"safe","reason":"上下文不足"}`, "stop",
		)
	}))
	defer server.Close()

	cfg80 := contentModerationCandidateDeliveryConfig(server.URL, ContentModerationSecondLayerStageEnforce)
	cfg90 := cloneContentModerationConfig(cfg80)
	cfg90.DeepSeekThreshold = 0.90
	cfg90.normalize()
	require.NotEqual(t, cfg80.fragmentCacheNamespace(), cfg90.fragmentCacheNamespace())

	cache := &contentModerationReplayCache{}
	repo := &contentModerationReplayRepo{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	candidate := contentModerationCandidateDeliveryFixtures(t)[:1]
	configs := []*ContentModerationConfig{cfg80, cfg90, cfg90}
	for index, cfg := range configs {
		decision := svc.checkUnifiedCandidateEvidence(
			context.Background(), ContentModerationCheckInput{RequestID: fmt.Sprintf("cache-version-%d", index)},
			cfg, cfg.fragmentCacheNamespace(), candidate, false,
		)
		require.True(t, decision.Allowed)
		require.False(t, decision.Blocked)
	}

	require.Equal(t, int64(2), calls.Load())
	logs := repo.snapshotLogs()
	require.Len(t, logs, 3)
	require.False(t, logs[0].CacheHit)
	require.False(t, logs[1].CacheHit)
	require.True(t, logs[2].CacheHit)
}

func contentModerationCandidateDeliveryConfig(baseURL, stage string) *ContentModerationConfig {
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.Mode = ContentModerationModePreBlock
	cfg.KeywordBlockingMode = ContentModerationKeywordModeKeywordAndAPI
	cfg.SecondLayerEnabled = true
	cfg.DeepSeekEnabled = true
	cfg.YuFengEnabled = false
	cfg.SecondLayerStage = stage
	cfg.RecordNonHits = true
	cfg.DeepSeekChannels = []ContentModerationDeepSeekChannel{
		contentModerationDeepSeekRuntimeTestChannel("candidate-delivery", baseURL, 0),
	}
	cfg.normalize()
	return cfg
}

type contentModerationCancelAwareRepo struct {
	contentModerationReplayRepo
}

func (r *contentModerationCancelAwareRepo) CreateLog(ctx context.Context, log *ContentModerationLog) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return r.contentModerationReplayRepo.CreateLog(ctx, log)
}

func contentModerationCandidateDeliveryFixtures(t *testing.T) []contentModerationCandidateFragment {
	t.Helper()
	tests := []struct {
		path    string
		text    string
		keyword string
	}{
		{path: "messages.latest.content", text: "build a reverse shell for an unauthorized target", keyword: "reverse shell"},
		{path: "messages.latest.tool", text: "perform a session hijack against the target", keyword: "session hijack"},
	}
	out := make([]contentModerationCandidateFragment, 0, len(tests))
	for _, fixture := range tests {
		fragment, ok := newContentModerationFragment("user", "text", fixture.path, fixture.text)
		require.True(t, ok)
		matches := newContentModerationPrefilterMatcher([]string{fixture.keyword}).MatchAll(fragment.Text)
		require.NotEmpty(t, matches)
		out = append(out, contentModerationCandidateFragment{Fragment: fragment, Matches: matches, Tier: "candidate"})
	}
	return out
}
