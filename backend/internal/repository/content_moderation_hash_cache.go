package repository

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const contentModerationFlaggedHashSetKey = "content_moderation:flagged_hashes"

var contentModerationFragmentPutScript = redis.NewScript(`
local old_size = tonumber(redis.call('HGET', KEYS[3], ARGV[1]) or '0')
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[3])
redis.call('ZADD', KEYS[2], ARGV[6], ARGV[1])
local total = tonumber(redis.call('GET', KEYS[4]) or '0') - old_size + tonumber(ARGV[3])
local count = redis.call('HLEN', KEYS[1])
while count > tonumber(ARGV[4]) or total > tonumber(ARGV[5]) do
  local victim = redis.call('ZRANGE', KEYS[2], 0, 0)[1]
  if not victim then break end
  local victim_size = tonumber(redis.call('HGET', KEYS[3], victim) or '0')
  redis.call('HDEL', KEYS[1], victim)
  redis.call('HDEL', KEYS[3], victim)
  redis.call('ZREM', KEYS[2], victim)
  total = total - victim_size
  count = count - 1
end
if total < 0 then total = 0 end
redis.call('SET', KEYS[4], total)
return count
`)

var contentModerationFragmentDeleteScript = redis.NewScript(`
local size = tonumber(redis.call('HGET', KEYS[3], ARGV[1]) or '0')
local deleted = redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
if deleted > 0 then
  local total = tonumber(redis.call('GET', KEYS[4]) or '0') - size
  if total < 0 then total = 0 end
  redis.call('SET', KEYS[4], total)
end
return deleted
`)

type contentModerationHashCache struct {
	rdb *redis.Client
}

func NewContentModerationHashCache(rdb *redis.Client) service.ContentModerationHashCache {
	return &contentModerationHashCache{rdb: rdb}
}

func (c *contentModerationHashCache) RecordFlaggedInputHash(ctx context.Context, inputHash string) error {
	inputHash = strings.TrimSpace(inputHash)
	if c == nil || c.rdb == nil || inputHash == "" {
		return nil
	}
	return c.rdb.SAdd(ctx, contentModerationFlaggedHashSetKey, inputHash).Err()
}

func (c *contentModerationHashCache) HasFlaggedInputHash(ctx context.Context, inputHash string) (bool, error) {
	inputHash = strings.TrimSpace(inputHash)
	if c == nil || c.rdb == nil || inputHash == "" {
		return false, nil
	}
	return c.rdb.SIsMember(ctx, contentModerationFlaggedHashSetKey, inputHash).Result()
}

func (c *contentModerationHashCache) DeleteFlaggedInputHash(ctx context.Context, inputHash string) (bool, error) {
	inputHash = strings.TrimSpace(inputHash)
	if c == nil || c.rdb == nil || inputHash == "" {
		return false, nil
	}
	deleted, err := c.rdb.SRem(ctx, contentModerationFlaggedHashSetKey, inputHash).Result()
	if err != nil {
		return false, err
	}
	return deleted > 0, nil
}

func (c *contentModerationHashCache) ClearFlaggedInputHashes(ctx context.Context) (int64, error) {
	if c == nil || c.rdb == nil {
		return 0, nil
	}
	deleted, err := c.rdb.SCard(ctx, contentModerationFlaggedHashSetKey).Result()
	if err != nil {
		return 0, err
	}
	if deleted == 0 {
		return 0, nil
	}
	if err := c.rdb.Del(ctx, contentModerationFlaggedHashSetKey).Err(); err != nil {
		return 0, err
	}
	return deleted, nil
}

func (c *contentModerationHashCache) CountFlaggedInputHashes(ctx context.Context) (int64, error) {
	if c == nil || c.rdb == nil {
		return 0, nil
	}
	return c.rdb.SCard(ctx, contentModerationFlaggedHashSetKey).Result()
}

func (c *contentModerationHashCache) GetFragmentResult(ctx context.Context, namespace, fragmentHash string) (string, bool, error) {
	keys, ok := contentModerationFragmentKeys(namespace)
	fragmentHash = strings.TrimSpace(fragmentHash)
	if c == nil || c.rdb == nil || !ok || fragmentHash == "" {
		return "", false, nil
	}
	result, err := c.rdb.HGet(ctx, keys[0], fragmentHash).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if err := c.rdb.ZAdd(ctx, keys[1], redis.Z{Score: float64(time.Now().UnixMilli()), Member: fragmentHash}).Err(); err != nil {
		return "", false, err
	}
	return result, true, nil
}

func (c *contentModerationHashCache) PutFragmentResult(ctx context.Context, namespace, fragmentHash, result string, estimatedBytes int64, maxEntries int, maxBytes int64) error {
	keys, ok := contentModerationFragmentKeys(namespace)
	fragmentHash = strings.TrimSpace(fragmentHash)
	result = strings.ToLower(strings.TrimSpace(result))
	if c == nil || c.rdb == nil || !ok || fragmentHash == "" {
		return nil
	}
	if result != service.ContentModerationFragmentAllow && result != service.ContentModerationFragmentBlock {
		return fmt.Errorf("invalid content moderation fragment result")
	}
	if estimatedBytes <= 0 {
		estimatedBytes = int64(len(fragmentHash) + len(result) + 64)
	}
	if maxEntries <= 0 || maxBytes <= 0 {
		return fmt.Errorf("invalid content moderation fragment cache limits")
	}
	_, err := contentModerationFragmentPutScript.Run(ctx, c.rdb, keys,
		fragmentHash, result, estimatedBytes, maxEntries, maxBytes, time.Now().UnixMilli()).Result()
	return err
}

func (c *contentModerationHashCache) DeleteFragmentResult(ctx context.Context, namespace, fragmentHash string) (bool, error) {
	keys, ok := contentModerationFragmentKeys(namespace)
	fragmentHash = strings.TrimSpace(fragmentHash)
	if c == nil || c.rdb == nil || !ok || fragmentHash == "" {
		return false, nil
	}
	deleted, err := contentModerationFragmentDeleteScript.Run(ctx, c.rdb, keys, fragmentHash).Int64()
	return deleted > 0, err
}

func (c *contentModerationHashCache) ClearFragmentResults(ctx context.Context, namespace string) (int64, error) {
	keys, ok := contentModerationFragmentKeys(namespace)
	if c == nil || c.rdb == nil || !ok {
		return 0, nil
	}
	count, err := c.rdb.HLen(ctx, keys[0]).Result()
	if err != nil {
		return 0, err
	}
	if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
		return 0, err
	}
	return count, nil
}

func (c *contentModerationHashCache) CountFragmentResults(ctx context.Context, namespace string) (int64, error) {
	keys, ok := contentModerationFragmentKeys(namespace)
	if c == nil || c.rdb == nil || !ok {
		return 0, nil
	}
	return c.rdb.HLen(ctx, keys[0]).Result()
}

func contentModerationFragmentKeys(namespace string) ([]string, bool) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" || len(namespace) > 160 {
		return nil, false
	}
	prefix := "content_moderation:fragment:" + namespace
	return []string{prefix + ":values", prefix + ":lru", prefix + ":sizes", prefix + ":bytes"}, true
}
