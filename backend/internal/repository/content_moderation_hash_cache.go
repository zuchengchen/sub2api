package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	contentModerationFlaggedHashSetKey        = "content_moderation:flagged_hashes"
	contentModerationFragmentNamespacesSetKey = "content_moderation:fragment:namespaces"
)

var contentModerationFragmentPutScript = redis.NewScript(`
local old_size = tonumber(redis.call('HGET', KEYS[3], ARGV[1]) or '0')
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[3])
redis.call('ZADD', KEYS[2], ARGV[6], ARGV[1])
redis.call('HSET', KEYS[5], ARGV[1], ARGV[7])
redis.call('HSET', KEYS[6], ARGV[1], ARGV[8])
redis.call('SADD', KEYS[7], ARGV[9])
local total = tonumber(redis.call('GET', KEYS[4]) or '0') - old_size + tonumber(ARGV[3])
local count = redis.call('HLEN', KEYS[1])
while count > tonumber(ARGV[4]) or total > tonumber(ARGV[5]) do
  local victim = redis.call('ZRANGE', KEYS[2], 0, 0)[1]
  if not victim then break end
  local victim_size = tonumber(redis.call('HGET', KEYS[3], victim) or '0')
  redis.call('HDEL', KEYS[1], victim)
  redis.call('HDEL', KEYS[3], victim)
  redis.call('HDEL', KEYS[5], victim)
  redis.call('HDEL', KEYS[6], victim)
  redis.call('ZREM', KEYS[2], victim)
  total = total - victim_size
  count = count - 1
end
if total < 0 then total = 0 end
redis.call('SET', KEYS[4], total)
return count
`)

var contentModerationFragmentGetScript = redis.NewScript(`
local value = redis.call('HGET', KEYS[1], ARGV[1])
if not value then return {} end
local expires = tonumber(redis.call('HGET', KEYS[5], ARGV[1]) or '0')
if expires <= 0 or expires <= tonumber(ARGV[2]) then
  local size = tonumber(redis.call('HGET', KEYS[3], ARGV[1]) or '0')
  redis.call('HDEL', KEYS[1], ARGV[1])
  redis.call('HDEL', KEYS[3], ARGV[1])
  redis.call('HDEL', KEYS[5], ARGV[1])
  redis.call('HDEL', KEYS[6], ARGV[1])
  redis.call('ZREM', KEYS[2], ARGV[1])
  local total = tonumber(redis.call('GET', KEYS[4]) or '0') - size
  if total < 0 then total = 0 end
  redis.call('SET', KEYS[4], total)
  return {'', '', '', 'expired'}
end
redis.call('ZADD', KEYS[2], ARGV[2], ARGV[1])
local metadata = redis.call('HGET', KEYS[6], ARGV[1]) or ''
return {value, metadata, tostring(expires)}
`)

var contentModerationFragmentDeleteScript = redis.NewScript(`
local size = tonumber(redis.call('HGET', KEYS[3], ARGV[1]) or '0')
local deleted = redis.call('HDEL', KEYS[1], ARGV[1])
redis.call('HDEL', KEYS[3], ARGV[1])
redis.call('HDEL', KEYS[5], ARGV[1])
redis.call('HDEL', KEYS[6], ARGV[1])
redis.call('ZREM', KEYS[2], ARGV[1])
if deleted > 0 then
  local total = tonumber(redis.call('GET', KEYS[4]) or '0') - size
  if total < 0 then total = 0 end
  redis.call('SET', KEYS[4], total)
end
return deleted
`)

var contentModerationFragmentClearScript = redis.NewScript(`
local count = redis.call('HLEN', KEYS[1])
redis.call('DEL', KEYS[1], KEYS[2], KEYS[3], KEYS[4], KEYS[5], KEYS[6])
redis.call('SREM', KEYS[7], ARGV[1])
return count
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
	entry, found, err := c.GetFragmentCacheEntry(ctx, namespace, fragmentHash)
	return entry.Result, found, err
}

func (c *contentModerationHashCache) PutFragmentResult(ctx context.Context, namespace, fragmentHash, result string, estimatedBytes int64, maxEntries int, maxBytes int64) error {
	ttl := time.Duration(service.DefaultContentModerationFragmentAllowTTLSeconds) * time.Second
	normalizedResult := strings.ToLower(strings.TrimSpace(result))
	if normalizedResult == service.ContentModerationFragmentBlock ||
		normalizedResult == service.ContentModerationFragmentRestricted {
		ttl = time.Duration(service.DefaultContentModerationFragmentBlockTTLSeconds) * time.Second
	}
	return c.PutFragmentCacheEntry(ctx, namespace, fragmentHash, service.ContentModerationFragmentCacheEntry{Result: result}, estimatedBytes, maxEntries, maxBytes, ttl)
}

func (c *contentModerationHashCache) GetFragmentCacheEntry(ctx context.Context, namespace, fragmentHash string) (service.ContentModerationFragmentCacheEntry, bool, error) {
	keys, ok := contentModerationFragmentKeys(namespace)
	fragmentHash = strings.TrimSpace(fragmentHash)
	if c == nil || c.rdb == nil || !ok || fragmentHash == "" {
		return service.ContentModerationFragmentCacheEntry{}, false, nil
	}
	raw, err := contentModerationFragmentGetScript.Run(ctx, c.rdb, keys, fragmentHash, time.Now().UnixMilli()).Slice()
	if err == redis.Nil || (err == nil && len(raw) == 0) {
		return service.ContentModerationFragmentCacheEntry{}, false, nil
	}
	if err != nil {
		return service.ContentModerationFragmentCacheEntry{}, false, err
	}
	if len(raw) < 1 {
		return service.ContentModerationFragmentCacheEntry{}, false, nil
	}
	if len(raw) > 3 && redisFragmentValueString(raw[3]) == "expired" {
		return service.ContentModerationFragmentCacheEntry{Expired: true}, false, nil
	}
	entry := service.ContentModerationFragmentCacheEntry{Result: redisFragmentValueString(raw[0])}
	if len(raw) > 1 {
		metadata := redisFragmentValueString(raw[1])
		if metadata != "" {
			if err := json.Unmarshal([]byte(metadata), &entry); err != nil {
				return service.ContentModerationFragmentCacheEntry{}, false, fmt.Errorf("decode content moderation fragment metadata: %w", err)
			}
		}
	}
	if len(raw) > 2 {
		expiresMS, _ := strconv.ParseInt(redisFragmentValueString(raw[2]), 10, 64)
		if expiresMS > 0 {
			entry.ExpiresAt = time.UnixMilli(expiresMS)
		}
	}
	return entry, true, nil
}

func (c *contentModerationHashCache) PutFragmentCacheEntry(ctx context.Context, namespace, fragmentHash string, entry service.ContentModerationFragmentCacheEntry, estimatedBytes int64, maxEntries int, maxBytes int64, ttl time.Duration) error {
	keys, ok := contentModerationFragmentKeys(namespace)
	fragmentHash = strings.TrimSpace(fragmentHash)
	entry.Result = strings.ToLower(strings.TrimSpace(entry.Result))
	if c == nil || c.rdb == nil || !ok || fragmentHash == "" {
		return nil
	}
	if entry.Result != service.ContentModerationFragmentAllow &&
		entry.Result != service.ContentModerationFragmentBlock &&
		entry.Result != service.ContentModerationFragmentRestricted {
		return fmt.Errorf("invalid content moderation fragment result")
	}
	if ttl <= 0 {
		return fmt.Errorf("invalid content moderation fragment TTL")
	}
	expiresAt := time.Now().Add(ttl)
	entry.ExpiresAt = expiresAt
	metadata, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encode content moderation fragment metadata: %w", err)
	}
	if estimatedBytes <= 0 {
		estimatedBytes = int64(len(fragmentHash) + len(entry.Result) + len(metadata) + 64)
	}
	if maxEntries <= 0 || maxBytes <= 0 {
		return fmt.Errorf("invalid content moderation fragment cache limits")
	}
	scriptKeys := append(append([]string(nil), keys...), contentModerationFragmentNamespacesSetKey)
	_, err = contentModerationFragmentPutScript.Run(ctx, c.rdb, scriptKeys,
		fragmentHash, entry.Result, estimatedBytes, maxEntries, maxBytes, time.Now().UnixMilli(), expiresAt.UnixMilli(), string(metadata), namespace).Result()
	return err
}

func redisFragmentValueString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return fmt.Sprint(value)
	}
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
	scriptKeys := append(append([]string(nil), keys...), contentModerationFragmentNamespacesSetKey)
	return contentModerationFragmentClearScript.Run(ctx, c.rdb, scriptKeys, namespace).Int64()
}

func (c *contentModerationHashCache) CountFragmentResults(ctx context.Context, namespace string) (int64, error) {
	keys, ok := contentModerationFragmentKeys(namespace)
	if c == nil || c.rdb == nil || !ok {
		return 0, nil
	}
	return c.rdb.HLen(ctx, keys[0]).Result()
}

func (c *contentModerationHashCache) DeleteFragmentResultAliases(ctx context.Context, fragmentHash string) (int64, error) {
	fragmentHash = strings.TrimSpace(fragmentHash)
	if c == nil || c.rdb == nil || fragmentHash == "" {
		return 0, nil
	}
	namespaces, err := c.rdb.SMembers(ctx, contentModerationFragmentNamespacesSetKey).Result()
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, namespace := range namespaces {
		keys, ok := contentModerationFragmentKeys(namespace)
		if !ok {
			continue
		}
		count, err := contentModerationFragmentDeleteScript.Run(ctx, c.rdb, keys, fragmentHash).Int64()
		if err != nil {
			return deleted, err
		}
		deleted += count
	}
	return deleted, nil
}

func (c *contentModerationHashCache) ClearAllFragmentResults(ctx context.Context) (int64, error) {
	if c == nil || c.rdb == nil {
		return 0, nil
	}
	namespaces, err := c.rdb.SMembers(ctx, contentModerationFragmentNamespacesSetKey).Result()
	if err != nil {
		return 0, err
	}
	var deleted int64
	for _, namespace := range namespaces {
		keys, ok := contentModerationFragmentKeys(namespace)
		if !ok {
			continue
		}
		count, err := c.rdb.HLen(ctx, keys[0]).Result()
		if err != nil {
			return deleted, err
		}
		if err := c.rdb.Del(ctx, keys...).Err(); err != nil {
			return deleted, err
		}
		deleted += count
	}
	if err := c.rdb.Del(ctx, contentModerationFragmentNamespacesSetKey).Err(); err != nil {
		return deleted, err
	}
	return deleted, nil
}

func contentModerationFragmentKeys(namespace string) ([]string, bool) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" || len(namespace) > 160 {
		return nil, false
	}
	prefix := "content_moderation:fragment:" + namespace
	return []string{prefix + ":values", prefix + ":lru", prefix + ":sizes", prefix + ":bytes", prefix + ":expires", prefix + ":metadata"}, true
}
