package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestContentModerationShadowReviewCoalescesConcurrentEvidence(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pc"}}]}`))
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	cfg.SecondLayerStage = ContentModerationSecondLayerStageShadow
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	runtime := contextualRoutingTestRuntime(cfg, nil)
	input := contextualRoutingTestInput("请介绍恶意宏", "")

	first := svc.checkUnifiedFragments(context.Background(), input, runtime)
	require.True(t, first.Allowed)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first shadow review did not reach the model")
	}

	const duplicates = 128
	decisions := make(chan *ContentModerationDecision, duplicates)
	for range duplicates {
		go func() {
			decisions <- svc.checkUnifiedFragments(context.Background(), input, runtime)
		}()
	}
	for range duplicates {
		select {
		case decision := <-decisions:
			require.True(t, decision.Allowed)
		case <-time.After(time.Second):
			t.Fatal("coalesced shadow request waited for model capacity")
		}
	}
	otherUserInput := input
	otherUserInput.UserID++
	otherUserDecision := svc.checkUnifiedFragments(context.Background(), otherUserInput, runtime)
	require.True(t, otherUserDecision.Allowed)

	require.Equal(t, int64(2), svc.secondLayerShadowQueued.Load())
	require.Equal(t, int64(duplicates), svc.secondLayerShadowCoalesced.Load())
	require.Zero(t, svc.secondLayerShadowDropped.Load())
	require.Equal(t, int64(1), modelCalls.Load())

	releaseOnce.Do(func() { close(release) })
	require.Eventually(t, func() bool {
		return svc.secondLayerShadowDone.Load() == 2 && modelCalls.Load() == 2 && len(repo.snapshotLogs()) == 2
	}, time.Second, 10*time.Millisecond)
}

func TestContentModerationShadowWaitsBehindEnforceWithoutBusyAudit(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if modelCalls.Add(1) == 1 {
			started <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sec"}}]}`))
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	cfg.SecondLayerStage = ContentModerationSecondLayerStageShadow
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)
	enforceDone := make(chan error, 1)
	go func() {
		_, _, err := svc.scanUnifiedSecondLayer(context.Background(), cfg, "reverse shell")
		enforceDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("enforce review did not reach the model")
	}

	begin := time.Now()
	decision := svc.checkUnifiedFragments(
		context.Background(),
		contextualRoutingTestInput("请介绍恶意宏", ""),
		contextualRoutingTestRuntime(cfg, nil),
	)
	require.True(t, decision.Allowed)
	require.Less(t, time.Since(begin), 250*time.Millisecond)
	require.Eventually(t, func() bool {
		return svc.secondLayerShadowWaited.Load() == 1
	}, time.Second, time.Millisecond)
	require.Empty(t, repo.snapshotLogs(), "waiting shadow work must not emit a busy audit")
	require.Equal(t, int64(1), modelCalls.Load())

	releaseOnce.Do(func() { close(release) })
	require.NoError(t, <-enforceDone)
	require.Eventually(t, func() bool {
		return svc.secondLayerShadowDone.Load() == 1 && modelCalls.Load() == 2 && len(repo.snapshotLogs()) == 1
	}, time.Second, 10*time.Millisecond)
	logs := repo.snapshotLogs()
	require.NotEqual(t, "review_unavailable_shadow", logs[0].DecisionSource)
	require.NotEqual(t, "busy", logs[0].ParserStatus)
}

func TestContentModerationShadowQueueFullUsesAggregateDropWithoutAudit(t *testing.T) {
	var modelCalls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		modelCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"sec"}}]}`))
	}))
	defer server.Close()

	cfg := contextualRoutingTestConfig(server.URL)
	cfg.SecondLayerStage = ContentModerationSecondLayerStageShadow
	cfg.normalize()
	repo := &contentModerationReplayRepo{}
	cache := &contentModerationReplayCache{}
	svc := NewContentModerationService(nil, repo, cache, nil, nil, nil, nil, nil)

	block := make(chan struct{})
	var unblockOnce sync.Once
	t.Cleanup(func() { unblockOnce.Do(func() { close(block) }) })
	blockStarted := make(chan struct{})
	require.True(t, svc.enqueueContentModerationShadowReview(func() {
		close(blockStarted)
		<-block
	}))
	select {
	case <-blockStarted:
	case <-time.After(time.Second):
		t.Fatal("shadow queue worker did not start blocking job")
	}
	for range contentModerationShadowQueueCapacity {
		require.True(t, svc.enqueueContentModerationShadowReview(func() {}))
	}

	decision := svc.checkUnifiedFragments(
		context.Background(),
		contextualRoutingTestInput("请介绍恶意宏", ""),
		contextualRoutingTestRuntime(cfg, nil),
	)
	require.True(t, decision.Allowed)
	require.Equal(t, ContentModerationActionAllow, decision.Action)
	require.Equal(t, int64(1), svc.secondLayerShadowDropped.Load())
	require.Empty(t, repo.snapshotLogs(), "capacity drops must not create per-request unavailable audits")
	require.Zero(t, modelCalls.Load())

	unblockOnce.Do(func() { close(block) })
	require.Eventually(t, func() bool {
		return svc.secondLayerShadowDone.Load() == contentModerationShadowQueueCapacity+1
	}, time.Second, 10*time.Millisecond)
}

func TestContentModerationStatusExposesShadowCapacityAggregates(t *testing.T) {
	svc := &ContentModerationService{
		settingRepo: &contentModerationTestSettingRepo{values: map[string]string{}},
	}
	svc.secondLayerShadowQueued.Store(11)
	svc.secondLayerShadowCoalesced.Store(7)
	svc.secondLayerShadowDropped.Store(3)
	svc.secondLayerShadowWaited.Store(5)
	svc.secondLayerShadowExpired.Store(2)
	svc.secondLayerShadowDone.Store(9)

	status, err := svc.GetStatus(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(11), status.SecondLayerShadowQueued)
	require.Equal(t, int64(7), status.SecondLayerShadowCoalesced)
	require.Equal(t, int64(3), status.SecondLayerShadowDropped)
	require.Equal(t, int64(5), status.SecondLayerShadowWaited)
	require.Equal(t, int64(2), status.SecondLayerShadowWaitExpired)
	require.Equal(t, int64(9), status.SecondLayerShadowCompleted)
}
