//go:build unit

package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func astraPolicyCacheAccount(verdict string, at time.Time) service.Account {
	return service.Account{ID: 78001, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth, Status: service.StatusActive, Schedulable: true,
		Extra: map[string]any{service.OpenAIAstraPolicyExtraKey: map[string]any{"model": "gpt-6-astra", "recent_samples": []map[string]any{{"verdict": verdict, "at": at.Format(time.RFC3339Nano)}}}}}
}

func TestAstraOAuthModelPolicyCacheRejectsOlderSnapshot(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	older := astraPolicyCacheAccount("degraded", time.Now().Add(-time.Hour))
	latest := astraPolicyCacheAccount("healthy", time.Now())
	for _, account := range []*service.Account{&older, &latest, &older} {
		require.NoError(t, cache.SetAccount(ctx, account))
	}
	full, err := cache.GetAccount(ctx, latest.ID)
	require.NoError(t, err)
	require.Equal(t, "healthy", full.OpenAIOAuthModelPolicy().Verdict)
	raw, err := cache.rdb.Get(ctx, schedulerAccountMetaKey(fmt.Sprint(latest.ID))).Result()
	require.NoError(t, err)
	meta, err := decodeCachedAccount(raw)
	require.NoError(t, err)
	require.Equal(t, "healthy", meta.OpenAIOAuthModelPolicy().Verdict)
	require.True(t, meta.IsModelSupported("gpt-6-astra"))
	require.False(t, meta.IsModelSupported("gpt-6-luna"))
}

func TestAstraOAuthModelPolicyConcurrentCacheReads(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	latest := astraPolicyCacheAccount("healthy", time.Now())
	older := astraPolicyCacheAccount("degraded", time.Now().Add(-time.Hour))
	require.NoError(t, cache.SetAccount(ctx, &latest))
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 10; n++ {
				if err := cache.SetAccount(ctx, &older); err != nil {
					t.Error(err)
					return
				}
				account, err := cache.GetAccount(ctx, latest.ID)
				if err != nil {
					t.Error(err)
					return
				}
				if account == nil || !account.IsModelSupported("gpt-6-astra") {
					t.Error("older snapshot replaced latest policy")
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestAstraOAuthModelPolicyMetadataSurvivesFullKeyEviction(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	latest := astraPolicyCacheAccount("healthy", time.Now().Add(-time.Minute))
	older := astraPolicyCacheAccount("degraded", time.Now().Add(-time.Hour))
	require.NoError(t, cache.SetAccount(ctx, &latest))
	require.NoError(t, cache.rdb.Del(ctx, schedulerAccountKey(fmt.Sprint(latest.ID))).Err())
	require.NoError(t, cache.SetAccount(ctx, &older))
	raw, err := cache.rdb.Get(ctx, schedulerAccountMetaKey(fmt.Sprint(latest.ID))).Result()
	require.NoError(t, err)
	meta, err := decodeCachedAccount(raw)
	require.NoError(t, err)
	require.Equal(t, "healthy", meta.OpenAIOAuthModelPolicy().Verdict)
	// The authoritative account read can refill the missing full key with the same version.
	require.NoError(t, cache.SetAccount(ctx, &latest))
	restored, err := cache.GetAccount(ctx, latest.ID)
	require.NoError(t, err)
	require.Equal(t, "healthy", restored.OpenAIOAuthModelPolicy().Verdict)
}

func TestAstraOAuthModelPolicyRefreshesAllBucketMemos(t *testing.T) {
	ctx := context.Background()
	cache := newSchedulerCacheUnit(t)
	older := astraPolicyCacheAccount("degraded", time.Now().Add(-time.Hour))
	latest := astraPolicyCacheAccount("healthy", time.Now().Add(-time.Minute))
	buckets := []service.SchedulerBucket{
		{GroupID: 21, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeSingle},
		{GroupID: 21, Platform: service.PlatformOpenAI, Mode: service.SchedulerModeForced},
	}
	for _, bucket := range buckets {
		token, err := cache.CaptureBucketWriteToken(ctx, bucket)
		require.NoError(t, err)
		require.NoError(t, cache.SetSnapshot(ctx, bucket, token, []service.Account{older}))
		snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
		require.NoError(t, err)
		require.True(t, hit)
		require.Len(t, snapshot, 1)
		require.Equal(t, "degraded", snapshot[0].OpenAIOAuthModelPolicy().Verdict)
	}
	require.NoError(t, cache.SetAccount(ctx, &latest))
	// An in-flight rebuild may publish the older account into one bucket.
	token, err := cache.CaptureBucketWriteToken(ctx, buckets[0])
	require.NoError(t, err)
	require.NoError(t, cache.SetSnapshot(ctx, buckets[0], token, []service.Account{older}))
	for _, bucket := range buckets {
		snapshot, hit, err := cache.GetSnapshot(ctx, bucket)
		require.NoError(t, err)
		require.True(t, hit)
		require.Len(t, snapshot, 1)
		require.Equal(t, "healthy", snapshot[0].OpenAIOAuthModelPolicy().Verdict, bucket.String())
	}
}
