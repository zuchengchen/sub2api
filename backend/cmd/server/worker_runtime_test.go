//go:build unit

package main

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func TestProvideWorkerRuntimeRegistersAndStartsPilots(t *testing.T) {
	cfg := &config.Config{}
	oauthSvc := service.NewOAuthService(nil, nil)
	openAIOAuthSvc := service.NewOpenAIOAuthService(nil, nil)
	grokOAuthSvc := service.NewGrokOAuthService(nil, nil, nil)
	tokenRefresh := service.NewTokenRefreshService(nil, oauthSvc, openAIOAuthSvc, nil, nil, cfg, nil)
	usagePool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	emailQueue := service.NewEmailQueueService(nil, 1)
	opsSink := service.NewOpsSystemLogSink(nil)
	aggregator := service.NewChannelMonitorV2Aggregator(nil, nil, nil)

	runtime, err := provideWorkerRuntime(
		service.NewAccountExpiryService(nil, time.Minute),
		service.NewIdempotencyCleanupService(nil, cfg),
		usagePool,
		service.NewSubscriptionExpiryService(nil, time.Minute),
		service.NewPaymentOrderExpiryService(nil, time.Minute),
		tokenRefresh,
		oauthSvc,
		openAIOAuthSvc,
		grokOAuthSvc,
		service.NewUserMessageQueueService(nil, nil, &cfg.Gateway.UserMessageQueue),
		service.NewConcurrencyService(nil),
		emailQueue,
		opsSink,
		aggregator,
		service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
		service.NewSupportDecisionAtomicReader(30*time.Second),
		nil,
		nil,
		nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, _ = runtime.StopAll(context.Background())
	})

	names := map[string]workerruntime.Kind{}
	for _, snapshot := range runtime.Snapshot() {
		names[snapshot.Descriptor.Name] = snapshot.Descriptor.Kind
		require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
	}
	require.Equal(t, workerruntime.KindPeriodic, names["account-expiry"])
	require.Equal(t, workerruntime.KindPeriodic, names["idempotency-cleanup"])
	require.Equal(t, workerruntime.KindPool, names["usage-record-pool"])
	require.Equal(t, workerruntime.KindPeriodic, names["claude-oauth-session-cleanup"])
	require.Equal(t, workerruntime.KindPeriodic, names["openai-oauth-session-cleanup"])
	require.Equal(t, workerruntime.KindPeriodic, names["grok-oauth-session-cleanup"])
	require.Equal(t, workerruntime.KindPeriodic, names["scheduler-support-replica"])
	require.NotContains(t, names, "gemini-oauth-session-cleanup")
	require.NotContains(t, names, "antigravity-oauth-session-cleanup")
}

func TestProvideWorkerRuntimeRegistersTokenRefreshOnlyWhenEnabled(t *testing.T) {
	cfg := &config.Config{}
	oauthSvc := service.NewOAuthService(nil, nil)
	openAIOAuthSvc := service.NewOpenAIOAuthService(nil, nil)
	usagePool := service.NewUsageRecordWorkerPoolWithOptions(service.UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})

	newRuntime := func(refresh *service.TokenRefreshService) *workerruntime.Runtime {
		runtime, err := provideWorkerRuntime(
			service.NewAccountExpiryService(nil, time.Minute),
			service.NewIdempotencyCleanupService(nil, cfg),
			usagePool,
			service.NewSubscriptionExpiryService(nil, time.Minute),
			service.NewPaymentOrderExpiryService(nil, time.Minute),
			refresh,
			oauthSvc,
			openAIOAuthSvc,
			nil,
			service.NewUserMessageQueueService(nil, nil, &cfg.Gateway.UserMessageQueue),
			service.NewConcurrencyService(nil),
			service.NewEmailQueueService(nil, 1),
			service.NewOpsSystemLogSink(nil),
			service.NewChannelMonitorV2Aggregator(nil, nil, nil),
			service.NewOutboxCleanupService(nil, nil, nil, 30*24*time.Hour),
			service.NewSupportDecisionAtomicReader(30*time.Second),
			nil,
			nil,
			nil,
		)
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = runtime.StopAll(context.Background())
		})
		return runtime
	}

	disabled := newRuntime(service.NewTokenRefreshService(nil, oauthSvc, openAIOAuthSvc, nil, nil, cfg, nil))
	for _, snapshot := range disabled.Snapshot() {
		require.NotEqual(t, "token-refresh", snapshot.Descriptor.Name)
	}

	enabledCfg := &config.Config{TokenRefresh: config.TokenRefreshConfig{Enabled: true, CheckIntervalMinutes: 5}}
	enabled := newRuntime(service.NewTokenRefreshService(nil, oauthSvc, openAIOAuthSvc, nil, nil, enabledCfg, nil))
	found := false
	for _, snapshot := range enabled.Snapshot() {
		if snapshot.Descriptor.Name == "token-refresh" {
			found = true
			require.Equal(t, workerruntime.LifecycleRunning, snapshot.Lifecycle.State)
		}
	}
	require.True(t, found)
}
