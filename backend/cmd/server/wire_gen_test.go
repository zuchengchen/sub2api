package main

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestProvideServiceBuildInfo(t *testing.T) {
	in := handler.BuildInfo{
		Version:    "v-test",
		Commit:     "abcdef123456",
		BuildType:  "release",
		ReleaseURL: "https://github.com/zuchengchen/sub2api/releases/tag/test",
	}
	out := provideServiceBuildInfo(in)
	require.Equal(t, in.Version, out.Version)
	require.Equal(t, in.Commit, out.Commit)
	require.Equal(t, in.BuildType, out.BuildType)
	require.Equal(t, in.ReleaseURL, out.ReleaseURL)
}

func TestProvideCleanup_WithMinimalDependencies_NoPanic(t *testing.T) {
	cfg := &config.Config{}

	oauthSvc := service.NewOAuthService(nil, nil)
	openAIOAuthSvc := service.NewOpenAIOAuthService(nil, nil)

	tokenRefreshSvc := service.NewTokenRefreshService(
		nil,
		oauthSvc,
		openAIOAuthSvc,
		nil,
		nil,
		cfg,
		nil,
	)
	codexVersionSyncSvc := service.NewOpenAICodexVersionSyncService(nil, nil, nil, time.Second)
	claudeCodeVersionSyncSvc := service.NewClaudeCodeVersionSyncService(nil, nil, nil, time.Second)
	proxyExpirySvc := service.NewProxyExpiryService(nil, time.Second)
	pricingSvc := service.NewPricingService(cfg, nil)
	billingCacheSvc := service.NewBillingCacheService(nil, nil, nil, nil, nil, nil, cfg, nil)
	schedulerSnapshotSvc := service.NewSchedulerSnapshotService(nil, nil, nil, nil, cfg)

	cleanup := provideCleanup(
		nil, // entClient
		nil, // redis
		&service.OpsMetricsCollector{},
		&service.OpsAggregationService{},
		&service.OpsAlertEvaluatorService{},
		&service.OpsCleanupService{},
		&service.OpsScheduledReportService{},
		nil, // opsService
		nil, // opsIngressRejectAggregator
		nil, // apiKeyService
		nil, // authCacheInvalidationWorker
		schedulerSnapshotSvc,
		tokenRefreshSvc,
		nil, // cnProviderBalanceCheck
		codexVersionSyncSvc,
		claudeCodeVersionSyncSvc,
		proxyExpirySvc,
		&service.UsageCleanupService{},
		pricingSvc,
		billingCacheSvc,
		&service.SubscriptionService{},
		oauthSvc,
		openAIOAuthSvc,
		nil, // grokOAuth
		nil, // openAIGateway
		nil, // scheduledTestRunner
		nil, // backupSvc
		nil, // channelMonitorRunner
		nil, // quotaFlusher
		nil, // upstreamBillingProbe
		nil, // ollamaCloudUsage
		nil, // opencodeGoUsage
		nil, // auditLog
		nil, // contentModeration
		nil, // pluginManager
		nil, // usagePolicyService
		nil, // billingOutboxWorker
		nil, // runtime
	)

	require.NotPanics(t, func() {
		cleanup()
	})
}
