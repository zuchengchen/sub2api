package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationShadowReviewLaunchesEveryJobWithoutCapacityLimit(t *testing.T) {
	const jobs = 129
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var started atomic.Int64
	svc := &ContentModerationService{}

	for range jobs {
		require.True(t, svc.launchContentModerationShadowReview(func() {
			started.Add(1)
			<-release
		}))
	}

	require.Eventually(t, func() bool {
		return started.Load() == jobs && svc.secondLayerShadowInFlight.Load() == jobs
	}, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, int64(jobs), svc.secondLayerShadowSubmitted.Load())

	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, func() bool {
		return svc.secondLayerShadowCompleted.Load() == jobs && svc.secondLayerShadowInFlight.Load() == 0
	}, 5*time.Second, 5*time.Millisecond)
}

func TestContentModerationSecondLayerDoesNotSerializeConcurrentRequests(t *testing.T) {
	const requests = 32
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sec"}}]}`))
	}))
	defer server.Close()

	cfg := secondLayerGateTestConfig(server.URL)
	svc := &ContentModerationService{}
	errors := make(chan error, requests)
	for range requests {
		go func() {
			_, _, err := svc.scanUnifiedSecondLayer(context.Background(), cfg, "reverse shell")
			errors <- err
		}()
	}

	require.Eventually(t, func() bool { return calls.Load() == requests }, 5*time.Second, 5*time.Millisecond)
	releaseOnce.Do(func() { close(release) })
	for range requests {
		require.NoError(t, <-errors)
	}
}

func TestContentModerationShadowIdenticalCandidatesShareReviewAndKeepEveryAudit(t *testing.T) {
	const requests = 80
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		<-release
		contentModerationDeepSeekRuntimeWriteEnvelope(
			t, w, `{"confidence":0.05,"category":"safe","reason":""}`, "stop",
		)
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	cfg.DeepSeekEnabled = true
	cfg.YuFengEnabled = false
	cfg.SecondLayerEndpoints = nil
	cfg.SecondLayerStage = ContentModerationSecondLayerStageShadow
	cfg.RecordNonHits = true
	cfg.DeepSeekChannels = []ContentModerationDeepSeekChannel{
		contentModerationDeepSeekRuntimeTestChannel("primary", server.URL, 0),
	}
	cfg.DeepSeekChannels[0].APIKey = "test-key"
	cfg.normalize()

	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	runtime := contextualRoutingTestRuntime(cfg, nil)
	input := contextualRoutingTestInput("请介绍恶意宏", "")
	for index := range requests {
		input.RequestID = fmt.Sprintf("cached-shadow-%03d", index)
		decision := svc.checkUnifiedFragments(context.Background(), input, runtime)
		require.True(t, decision.Allowed)
	}

	require.Eventually(t, func() bool { return modelCalls.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, int64(requests), svc.secondLayerShadowSubmitted.Load())
	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, func() bool {
		return svc.secondLayerShadowCompleted.Load() == requests && len(repo.snapshotLogs()) == requests
	}, 5*time.Second, 5*time.Millisecond)
	require.Equal(t, int64(1), modelCalls.Load())
	require.Equal(t, int64(requests-1), svc.fragmentCacheHits.Load())
	require.Equal(t, int64(requests-1), svc.fragmentCacheReplays.Load())

	logs := repo.snapshotLogs()
	cacheHits := 0
	requestIDs := make(map[string]struct{}, requests)
	for _, log := range logs {
		requestIDs[log.RequestID] = struct{}{}
		if log.CacheHit {
			cacheHits++
			require.Equal(t, "cache_replay", log.DecisionSource)
		}
	}
	require.Len(t, requestIDs, requests)
	require.Equal(t, requests-1, cacheHits)
}

func TestContentModerationStatusExposesShadowExecutionCounts(t *testing.T) {
	svc := &ContentModerationService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{}},
	}
	svc.secondLayerShadowSubmitted.Store(11)
	svc.secondLayerShadowCompleted.Store(9)
	svc.secondLayerShadowInFlight.Store(2)
	svc.secondLayerCacheHits.Store(17)
	svc.secondLayerCacheMisses.Store(4)
	svc.secondLayerCacheWrites.Store(3)
	svc.secondLayerCacheErrors.Store(1)

	status, err := svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(11), status.SecondLayerShadowSubmitted)
	require.Equal(t, int64(9), status.SecondLayerShadowCompleted)
	require.Equal(t, int64(2), status.SecondLayerShadowInFlight)
	require.Equal(t, int64(17), status.SecondLayerCacheHits)
	require.Equal(t, int64(4), status.SecondLayerCacheMisses)
	require.Equal(t, int64(3), status.SecondLayerCacheWrites)
	require.Equal(t, int64(1), status.SecondLayerCacheErrors)
}
