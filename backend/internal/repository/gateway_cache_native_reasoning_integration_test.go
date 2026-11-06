//go:build integration

package repository

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// CI=1 makes integration_harness fail if Docker cannot run. Every subtest uses
// its own prefixed client; cleanup unlinks only that namespace, never FLUSHDB.
func TestNativeReasoningReplayRedisIntegration(t *testing.T) {
	info, err := integrationRedis.Info(context.Background(), "server").Result()
	require.NoError(t, err)
	t.Logf("isolated Redis image=%s; server evidence:\n%s", redisImageTag, info)
	for name, check := range map[string]func(*testing.T, *redis.Client){"roundtrip_conflict": assertNativeReplayStoreRoundTrip, "limits": assertNativeReplayStoreLimits, "corruption_atomicity": assertNativeReplayStoreCorruption, "concurrency": assertNativeReplayStoreConcurrency, "eviction": assertNativeReplayStoreEviction, "invalidation": assertNativeReplayStoreInvalidation} {
		t.Run(name, func(t *testing.T) { check(t, testRedis(t)) })
	}
	t.Run("hooks_once_single_send_and_parent_pool", func(t *testing.T) {
		ctx := context.Background()
		client := testRedis(t)
		cache := &gatewayCache{rdb: client}
		for _, script := range []*redis.Script{putNativeReasoningReplayScript, getNativeReasoningReplayScript, invalidateNativeReasoningReplayScript} {
			require.NoError(t, script.Load(ctx, client).Err())
		}
		var hookCalls atomic.Int64
		client.AddHook(nativeReplayCountingHook{calls: &hookCalls})
		serverCalls := func() int64 {
			info, err := client.Info(ctx, "commandstats").Result()
			require.NoError(t, err)
			match := regexp.MustCompile(`cmdstat_evalsha:calls=(\d+)`).FindStringSubmatch(info)
			require.Len(t, match, 2)
			count, err := strconv.ParseInt(match[1], 10, 64)
			require.NoError(t, err)
			return count
		}
		before := serverCalls()
		v := nativeReplayStoreWrite("hook user", "hook branch", "opaque fixture")
		limits := service.DefaultNativeReasoningReplayLimits()
		require.NoError(t, cache.PutNativeReasoningReplay(ctx, v, limits))
		got, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NoError(t, cache.InvalidateNativeReasoningReplay(ctx, v.Owner, []service.NativeReasoningReplayReference{{Key: v.Key, Digest: v.Digest}}))
		require.EqualValues(t, 3, hookCalls.Load(), "original telemetry hook must run exactly once for each command")
		require.EqualValues(t, 3, serverCalls()-before, "the timeout wrapper must not send each command twice")
		require.NoError(t, client.Ping(ctx).Err(), "closing temporary Conn must keep parent pool usable")
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err = cache.GetNativeReasoningReplay(cancelled, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
		require.ErrorIs(t, err, context.Canceled)
		require.EqualValues(t, 3, hookCalls.Load(), "cancelled requests must not issue another command")
		bare, err := integrationRedis.Exists(ctx, nativeReasoningReplayDataKey).Result()
		require.NoError(t, err)
		require.Zero(t, bare, "all data remains within the harness namespace")
	})
	t.Run("concurrent_same_history_conflict", func(t *testing.T) {
		ctx := context.Background()
		client := testRedis(t)
		cache := &gatewayCache{rdb: client}
		limits := service.DefaultNativeReasoningReplayLimits()
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs <- cache.PutNativeReasoningReplay(ctx, nativeReplayStoreWrite("user", "same-history", fmt.Sprint(i)), limits)
			}(i)
		}
		wg.Wait()
		close(errs)
		accepted, conflicts := 0, 0
		for err := range errs {
			if err == nil {
				accepted++
			} else {
				require.ErrorIs(t, err, service.ErrNativeReasoningReplayAmbiguous)
				conflicts++
			}
		}
		require.Equal(t, 1, accepted)
		require.Equal(t, 1, conflicts)
		value := nativeReplayStoreWrite("user", "same-history", "0")
		got, err := cache.GetNativeReasoningReplay(ctx, value.Owner, []string{value.Key}, limits.MaxEntryBytes)
		require.NoError(t, err)
		require.Empty(t, got)
	})
	for _, scenario := range []string{"hard_user_capacity", "hard_global_capacity"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			client := testRedis(t)
			cache := &gatewayCache{rdb: client}
			limits := service.DefaultNativeReasoningReplayLimits()
			count := limits.MaxEntriesGlobal
			if scenario == "hard_user_capacity" {
				count = limits.MaxEntriesPerUser
			}
			var maxWrite time.Duration
			started := time.Now()
			for i := 0; i < count; i++ {
				owner := "one-user"
				if scenario == "hard_global_capacity" {
					owner = fmt.Sprintf("user-%d", i)
				}
				writeStarted := time.Now()
				require.NoError(t, cache.PutNativeReasoningReplay(ctx, nativeReplayStoreWrite(owner, fmt.Sprint(i), "synthetic-limit-probe"), limits))
				maxWrite = max(maxWrite, time.Since(writeStarted))
			}
			owner := "one-user"
			if scenario == "hard_global_capacity" {
				owner = "one-more-user"
			}
			require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, nativeReplayStoreWrite(owner, "over-limit", "synthetic-limit-probe"), limits), service.ErrNativeReasoningReplayCapacity)
			fields, err := nativeReplayTestFieldCount(ctx, client)
			require.NoError(t, err)
			require.EqualValues(t, count*2, fields)
			t.Logf("synthetic capacity measurement: entries=%d, fields=%d, elapsed=%s, max_write=%s, per_operation_limit=%s, max_payload_bytes=%d", count, fields, time.Since(started), maxWrite, limits.IOTimeout, count*limits.MaxEntryBytes)
		})
	}
	t.Run("expiry_reclaims_quota", func(t *testing.T) {
		ctx := context.Background()
		client := testRedis(t)
		cache := &gatewayCache{rdb: client}
		limits := service.NativeReasoningReplayLimits{TTL: 30 * time.Millisecond, MaxEntriesGlobal: 2, MaxEntriesPerUser: 1}.Normalize()
		v := nativeReplayStoreWrite("user", "expired", "p1")
		require.NoError(t, cache.PutNativeReasoningReplay(ctx, v, limits))
		keeperLimits := limits
		keeperLimits.TTL = time.Second
		keeper := nativeReplayStoreWrite("other user", "keeper", "p2")
		require.NoError(t, cache.PutNativeReasoningReplay(ctx, keeper, keeperLimits))
		require.Eventually(t, func() bool {
			got, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
			require.NoError(t, err)
			return len(got) == 0
		}, time.Second, 10*time.Millisecond)
		got, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
		require.NoError(t, err)
		require.Empty(t, got)
		next := nativeReplayStoreWrite("user", "new", "p2")
		require.NoError(t, cache.PutNativeReasoningReplay(ctx, next, limits))
		n, err := nativeReplayTestFieldCount(ctx, client)
		require.NoError(t, err)
		require.EqualValues(t, 4, n)
		got, err = cache.GetNativeReasoningReplay(ctx, keeper.Owner, []string{keeper.Key}, limits.MaxEntryBytes)
		require.NoError(t, err)
		require.Len(t, got, 1, "a short new TTL cannot expire the longer-lived entry")
	})
}
