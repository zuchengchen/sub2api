package repository

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func newContentModerationFragmentCacheTest(t *testing.T) (*contentModerationHashCache, *miniredis.Miniredis) {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	return &contentModerationHashCache{rdb: client}, server
}

func TestContentModerationFragmentCacheTTLAndProvenance(t *testing.T) {
	cache, _ := newContentModerationFragmentCacheTest(t)
	ctx := context.Background()
	sourceID := int64(42)
	entry := service.ContentModerationFragmentCacheEntry{
		Result: service.ContentModerationFragmentBlock, SourceLogID: &sourceID,
		DecisionSource: "model", ReplayOfInputHash: "abc", ModelProfile: "deepseek_v4_flash",
		PromptVersion: service.ContentModerationDeepSeekPromptVersion, Category: "cyber_abuse",
		KeywordTier: "candidate", KeywordRuleID: "layer2:test", EvidenceMode: "complete_context",
		ParserStatus: "parsed", ReviewCacheVersion: 2, DispositionApplied: true,
		Confidence: 0.93, Reason: "explicit attack",
		Label: "risk", EndpointID: "deepseek-official", ReviewOutcome: "risky",
		ReviewerDisagreement: true,
		ReviewAttempts: []service.ContentModerationReviewAttempt{{
			Reviewer: "deepseek", ChannelID: "deepseek-official", Outcome: "risk", LatencyMS: 87,
		}},
	}
	require.NoError(t, cache.PutFragmentCacheEntry(ctx, "v:test", "hash", entry, 256, 100, 1<<20, time.Minute))

	got, found, err := cache.GetFragmentCacheEntry(ctx, "v:test", "hash")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, entry.Result, got.Result)
	require.Equal(t, entry.SourceLogID, got.SourceLogID)
	require.Equal(t, entry.ReplayOfInputHash, got.ReplayOfInputHash)
	require.Equal(t, entry.DecisionSource, got.DecisionSource)
	require.Equal(t, entry.Category, got.Category)
	require.Equal(t, entry.ModelProfile, got.ModelProfile)
	require.Equal(t, entry.PromptVersion, got.PromptVersion)
	require.Equal(t, entry.KeywordTier, got.KeywordTier)
	require.Equal(t, entry.KeywordRuleID, got.KeywordRuleID)
	require.Equal(t, entry.EvidenceMode, got.EvidenceMode)
	require.Equal(t, entry.ParserStatus, got.ParserStatus)
	require.Equal(t, entry.ReviewCacheVersion, got.ReviewCacheVersion)
	require.Equal(t, entry.DispositionApplied, got.DispositionApplied)
	require.Equal(t, entry.Confidence, got.Confidence)
	require.Equal(t, entry.Reason, got.Reason)
	require.Equal(t, entry.Label, got.Label)
	require.Equal(t, entry.EndpointID, got.EndpointID)
	require.Equal(t, entry.ReviewOutcome, got.ReviewOutcome)
	require.Equal(t, entry.ReviewerDisagreement, got.ReviewerDisagreement)
	require.Equal(t, entry.ReviewAttempts, got.ReviewAttempts)
	require.False(t, got.ExpiresAt.IsZero())

	keys, ok := contentModerationFragmentKeys("v:test")
	require.True(t, ok)
	require.NoError(t, cache.rdb.HSet(ctx, keys[4], "hash", time.Now().Add(-time.Millisecond).UnixMilli()).Err())
	expired, found, err := cache.GetFragmentCacheEntry(ctx, "v:test", "hash")
	require.NoError(t, err)
	require.False(t, found)
	require.True(t, expired.Expired)
	for _, key := range []string{keys[0], keys[2], keys[4], keys[5]} {
		require.False(t, cache.rdb.HExists(ctx, key, "hash").Val())
	}
	require.ErrorIs(t, cache.rdb.ZScore(ctx, keys[1], "hash").Err(), redis.Nil)
	require.Equal(t, "0", cache.rdb.Get(ctx, keys[3]).Val())
}

func TestContentModerationFragmentCacheSupportedResults(t *testing.T) {
	cache, _ := newContentModerationFragmentCacheTest(t)
	ctx := context.Background()
	for _, result := range []string{
		service.ContentModerationFragmentAllow,
		service.ContentModerationFragmentBlock,
		service.ContentModerationFragmentRestricted,
	} {
		t.Run(result, func(t *testing.T) {
			require.NoError(t, cache.PutFragmentCacheEntry(
				ctx, "supported-results", result,
				service.ContentModerationFragmentCacheEntry{Result: result},
				128, 100, 1<<20, time.Minute,
			))

			entry, found, err := cache.GetFragmentCacheEntry(ctx, "supported-results", result)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, result, entry.Result)
		})
	}

	err := cache.PutFragmentCacheEntry(
		ctx, "supported-results", "invalid",
		service.ContentModerationFragmentCacheEntry{Result: "invalid"},
		128, 100, 1<<20, time.Minute,
	)
	require.EqualError(t, err, "invalid content moderation fragment result")

	require.NoError(t, cache.PutFragmentResult(
		ctx, "supported-results", "normalized-restricted", " ReStRiCtEd ",
		128, 100, 1<<20,
	))
	entry, found, err := cache.GetFragmentCacheEntry(ctx, "supported-results", "normalized-restricted")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, service.ContentModerationFragmentRestricted, entry.Result)
}

func TestContentModerationFragmentCacheLegacyEntryWithoutExpiryIsRemoved(t *testing.T) {
	cache, _ := newContentModerationFragmentCacheTest(t)
	ctx := context.Background()
	keys, ok := contentModerationFragmentKeys("legacy")
	require.True(t, ok)
	require.NoError(t, cache.rdb.HSet(ctx, keys[0], "old", service.ContentModerationFragmentBlock).Err())
	require.NoError(t, cache.rdb.HSet(ctx, keys[2], "old", 64).Err())

	entry, found, err := cache.GetFragmentCacheEntry(ctx, "legacy", "old")
	require.NoError(t, err)
	require.False(t, found)
	require.True(t, entry.Expired)
	require.False(t, cache.rdb.HExists(ctx, keys[0], "old").Val())
	require.False(t, cache.rdb.HExists(ctx, keys[2], "old").Val())
	require.Equal(t, "0", cache.rdb.Get(ctx, keys[3]).Val())
}

func TestContentModerationFragmentCacheDeleteAliasesAndClearMetadata(t *testing.T) {
	cache, _ := newContentModerationFragmentCacheTest(t)
	ctx := context.Background()
	for _, namespace := range []string{"old:v1", "new:v2"} {
		require.NoError(t, cache.PutFragmentCacheEntry(ctx, namespace, "same", service.ContentModerationFragmentCacheEntry{Result: service.ContentModerationFragmentBlock}, 128, 100, 1<<20, time.Minute))
	}
	deleted, err := cache.DeleteFragmentResultAliases(ctx, "same")
	require.NoError(t, err)
	require.Equal(t, int64(2), deleted)
	for _, namespace := range []string{"old:v1", "new:v2"} {
		_, found, getErr := cache.GetFragmentCacheEntry(ctx, namespace, "same")
		require.NoError(t, getErr)
		require.False(t, found)
	}

	require.NoError(t, cache.PutFragmentCacheEntry(ctx, "old:v1", "a", service.ContentModerationFragmentCacheEntry{Result: service.ContentModerationFragmentAllow}, 128, 100, 1<<20, time.Minute))
	require.NoError(t, cache.PutFragmentCacheEntry(ctx, "new:v2", "b", service.ContentModerationFragmentCacheEntry{Result: service.ContentModerationFragmentAllow}, 128, 100, 1<<20, time.Minute))
	clearedNamespace, err := cache.ClearFragmentResults(ctx, "old:v1")
	require.NoError(t, err)
	require.Equal(t, int64(1), clearedNamespace)
	require.False(t, cache.rdb.SIsMember(ctx, contentModerationFragmentNamespacesSetKey, "old:v1").Val())
	cleared, err := cache.ClearAllFragmentResults(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleared)
}

func TestContentModerationFragmentCacheConcurrentGetPutAndUnavailable(t *testing.T) {
	cache, server := newContentModerationFragmentCacheTest(t)
	ctx := context.Background()
	var workers sync.WaitGroup
	errors := make(chan error, 32)
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := cache.PutFragmentCacheEntry(ctx, "concurrent", "same", service.ContentModerationFragmentCacheEntry{Result: service.ContentModerationFragmentAllow}, 128, 100, 1<<20, time.Minute); err != nil {
				errors <- err
				return
			}
			_, _, _ = cache.GetFragmentCacheEntry(ctx, "concurrent", "same")
		}()
	}
	workers.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	entry, found, err := cache.GetFragmentCacheEntry(ctx, "concurrent", "same")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, service.ContentModerationFragmentAllow, entry.Result)

	server.Close()
	_, _, err = cache.GetFragmentCacheEntry(ctx, "concurrent", "same")
	require.Error(t, err)
}
