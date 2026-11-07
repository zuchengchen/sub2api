package repository

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

// Co-locate payload and quota/expiry metadata in one bounded hash. Redis may
// evict keys independently; separate quota indexes can disappear while payloads
// remain and silently defeat a hard global limit. Eviction of this hash removes
// both together. The two fields per entry are published by the same HSET.
const nativeReasoningReplayPrefix = "native_reasoning_replay:{native-reasoning-v1}:"
const nativeReasoningReplayDataKey = nativeReasoningReplayPrefix + "entries"

var _ service.NativeReasoningReplayStore = (*gatewayCache)(nil)
var _ service.NativeReasoningReplayInvalidationStore = (*gatewayCache)(nil)

// At most 1024 short metadata records are examined, never all payload bytes.
// Validate every structural invariant before any write: Lua runtime errors do
// not roll back earlier writes. Expired records are removed atomically before
// counting user/global quotas. Per-entry expiry is also enforced on every read.
var putNativeReasoningReplayScript = redis.NewScript(`
local kind = redis.call('TYPE', KEYS[1])
if type(kind) == 'table' then kind = kind.ok end
if kind ~= 'none' and kind ~= 'hash' then return -3 end
local ttl = tonumber(ARGV[5])
local peruser = tonumber(ARGV[6])
local global = tonumber(ARGV[7])
local maxbytes = tonumber(ARGV[8])
if not ttl or ttl < 1 or ttl > 3600000 or not peruser or peruser < 1 or peruser > 64 or
   not global or global < 1 or global > 1024 or not maxbytes or maxbytes < 1 or maxbytes > 262144 or
   string.len(ARGV[3]) ~= 64 or string.len(ARGV[4]) + 65 > maxbytes then return -2 end
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local totalfields = redis.call('HLEN', KEYS[1])
if totalfields > 2048 or totalfields % 2 ~= 0 then return -3 end
local fields = redis.call('HKEYS', KEYS[1])
local expired = {}
local userprefix = 'm:' .. ARGV[1] .. ':'
local live, userlive = 0, 0
for _, field in ipairs(fields) do
  if string.len(field) ~= 131 or (string.sub(field,1,2) ~= 'm:' and string.sub(field,1,2) ~= 'v:') then return -3 end
  if string.sub(field,1,2) == 'm:' then
    if redis.call('HSTRLEN', KEYS[1], field) > 16 then return -3 end
    local expiry = tonumber(redis.call('HGET', KEYS[1], field))
    if not expiry or redis.call('HEXISTS', KEYS[1], 'v:' .. string.sub(field,3)) ~= 1 then return -3 end
    if expiry <= nowms then
      table.insert(expired, field)
      table.insert(expired, 'v:' .. string.sub(field,3))
    else
      live = live + 1
      if string.sub(field,1,string.len(userprefix)) == userprefix then userlive = userlive + 1 end
    end
  end
end
if live * 2 + #expired ~= totalfields then return -3 end
local member = ARGV[1] .. ':' .. ARGV[2]
local mfield, vfield = 'm:' .. member, 'v:' .. member
if redis.call('HSTRLEN', KEYS[1], vfield) > maxbytes then return -3 end
local expiry = tonumber(redis.call('HGET', KEYS[1], mfield))
local old = false
if expiry and expiry > nowms then old = redis.call('HGET', KEYS[1], vfield) end
if old then
  if old == '!' then return -1 end
  if string.len(old) < 66 or string.sub(old,65,65) ~= '\n' then return -3 end
  if string.sub(old,1,64) == ARGV[3] then return 2 end
  redis.call('HSET', KEYS[1], vfield, '!')
  return -1
end
for _, field in ipairs(expired) do redis.call('HDEL', KEYS[1], field) end
if userlive >= peruser or live >= global then return 0 end
redis.call('HSET', KEYS[1], mfield, string.format('%.0f',nowms+ttl), vfield, ARGV[3] .. '\n' .. ARGV[4])
-- A shorter configured TTL must not truncate older still-live entries.
if redis.call('PTTL', KEYS[1]) < ttl then redis.call('PEXPIRE', KEYS[1], ttl) end
return 1
`)

// HSTRLEN bounds each reply before HGET; corrupt oversized values cannot force
// an unbounded client allocation. A poisoned or expired entry is a cache miss.
var getNativeReasoningReplayScript = redis.NewScript(`
local kind = redis.call('TYPE', KEYS[1])
if type(kind) == 'table' then kind = kind.ok end
if kind ~= 'none' and kind ~= 'hash' then return redis.error_reply('NATIVE_REPLAY_CORRUPT') end
if redis.call('HLEN', KEYS[1]) > 2048 then return redis.error_reply('NATIVE_REPLAY_CORRUPT') end
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local maxbytes = tonumber(ARGV[1])
local result = {}
for i=3,#ARGV do
  local member = ARGV[2] .. ':' .. ARGV[i]
  local mfield, vfield = 'm:' .. member, 'v:' .. member
  if redis.call('HSTRLEN', KEYS[1], mfield) > 16 then return redis.error_reply('NATIVE_REPLAY_CORRUPT') end
  local metadata = redis.call('HGET', KEYS[1], mfield)
  if not metadata then
    if redis.call('HEXISTS', KEYS[1], vfield) == 1 then return redis.error_reply('NATIVE_REPLAY_CORRUPT') end
  else
    local expiry = tonumber(metadata)
    if not expiry then return redis.error_reply('NATIVE_REPLAY_CORRUPT') end
    if expiry <= nowms then
      redis.call('HDEL', KEYS[1], mfield, vfield)
    else
      if redis.call('HSTRLEN', KEYS[1], vfield) > maxbytes then return redis.error_reply('NATIVE_REPLAY_OVERSIZE') end
      local value = redis.call('HGET', KEYS[1], vfield)
      if not value then return redis.error_reply('NATIVE_REPLAY_CORRUPT') end
      if value ~= '!' then table.insert(result,i-2); table.insert(result,value) end
    end
  end
end
return result
`)

var invalidateNativeReasoningReplayScript = redis.NewScript(`
local kind = redis.call('TYPE', KEYS[1])
if type(kind) == 'table' then kind = kind.ok end
if kind ~= 'none' and kind ~= 'hash' then return -3 end
if redis.call('HLEN', KEYS[1]) > 2048 then return -3 end
local now = redis.call('TIME')
local nowms = tonumber(now[1]) * 1000 + math.floor(tonumber(now[2]) / 1000)
local targets = {}
-- Validate every target before mutating any. Poison only the exact digest that
-- was sent upstream, preserving TTL and accounting and refusing stale rejects.
for i=2,#ARGV,2 do
  local member = ARGV[1] .. ':' .. ARGV[i]
  local mfield, vfield = 'm:' .. member, 'v:' .. member
  if redis.call('HSTRLEN', KEYS[1], mfield) > 16 or redis.call('HSTRLEN', KEYS[1], vfield) > 262144 then return -3 end
  local metadata = redis.call('HGET', KEYS[1], mfield)
  if metadata then
    local expiry = tonumber(metadata)
    if not expiry then return -3 end
    if expiry > nowms then
      local value = redis.call('HGET', KEYS[1], vfield)
      if not value then return -3 end
      if value ~= '!' then
        if string.len(value) < 66 or string.sub(value,65,65) ~= '\n' then return -3 end
        if string.sub(value,1,64) == ARGV[i+1] then table.insert(targets,vfield) end
      end
    end
  elseif redis.call('HEXISTS', KEYS[1], vfield) == 1 then return -3 end
end
for _, field in ipairs(targets) do redis.call('HSET', KEYS[1], field, '!') end
return 1
`)

func nativeReasoningReplayHashValid(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && s == strings.ToLower(s)
}

func nativeReasoningReplayKey(owner, key string) string {
	return "v:" + owner + ":" + key
}

// go-redis WithTimeout currently drops the original client's hooks. A temporary
// Conn inherits those hooks correctly; its final hook sends the already-processed
// command through a bounded clone. This preserves namespace/telemetry hooks and
// shared-pool ownership without changing global options or adding shared hooks.
type nativeReasoningReplayTimeoutHook struct{ client *redis.Client }

func (h nativeReasoningReplayTimeoutHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h nativeReasoningReplayTimeoutHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h nativeReasoningReplayTimeoutHook) ProcessHook(_ redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := service.DefaultNativeReasoningReplayLimits().IOTimeout
		if deadline, ok := ctx.Deadline(); ok {
			timeout = min(timeout, time.Until(deadline))
		}
		if timeout <= 0 {
			return context.DeadlineExceeded
		}
		return h.client.WithTimeout(timeout).Process(ctx, cmd)
	}
}

// Both context and socket I/O are bounded even when the original client was
// created without ContextTimeoutEnabled. Only single script commands use this
// connection; its empty sticky pool never borrows or closes a parent connection.
func (c *gatewayCache) nativeReasoningReplayClient(ctx context.Context) (*redis.Conn, context.Context, context.CancelFunc, error) {
	if c == nil || c.rdb == nil {
		return nil, ctx, func() {}, service.ErrNativeReasoningReplayUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, ctx, func() {}, err
	}
	timeout := service.DefaultNativeReasoningReplayLimits().IOTimeout
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < timeout {
		timeout = time.Until(deadline)
	}
	if timeout <= 0 {
		return nil, ctx, func() {}, context.DeadlineExceeded
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	conn := c.rdb.Conn()
	conn.AddHook(nativeReasoningReplayTimeoutHook{client: c.rdb})
	return conn, bounded, func() { cancel(); _ = conn.Close() }, nil
}

func (c *gatewayCache) GetNativeReasoningReplay(ctx context.Context, owner string, keys []string, maxBytes int) (map[string]service.NativeReasoningReplayValue, error) {
	hard := service.DefaultNativeReasoningReplayLimits()
	if !nativeReasoningReplayHashValid(owner) || len(keys) > hard.MaxLookups || maxBytes <= 0 || maxBytes > hard.MaxEntryBytes {
		return nil, service.ErrNativeReasoningReplayInvalid
	}
	result := make(map[string]service.NativeReasoningReplayValue)
	if len(keys) == 0 {
		return result, nil
	}
	args := make([]any, 0, len(keys)+2)
	args = append(args, maxBytes, owner)
	for _, key := range keys {
		if !nativeReasoningReplayHashValid(key) {
			return nil, service.ErrNativeReasoningReplayInvalid
		}
		args = append(args, key)
	}
	client, bounded, cancel, err := c.nativeReasoningReplayClient(ctx)
	if err != nil {
		return nil, err
	}
	defer cancel()
	values, err := getNativeReasoningReplayScript.Run(bounded, client, []string{nativeReasoningReplayDataKey}, args...).Slice()
	if err != nil {
		if strings.Contains(err.Error(), "NATIVE_REPLAY_CORRUPT") {
			return nil, service.ErrNativeReasoningReplayCorrupt
		}
		if strings.Contains(err.Error(), "NATIVE_REPLAY_OVERSIZE") {
			return nil, service.ErrNativeReasoningReplayCapacity
		}
		return nil, service.ErrNativeReasoningReplayUnavailable
	}
	if len(values)%2 != 0 {
		return nil, service.ErrNativeReasoningReplayCorrupt
	}
	for i := 0; i < len(values); i += 2 {
		index, ok := values[i].(int64)
		value, vok := values[i+1].(string)
		if !ok || !vok || index < 1 || index > int64(len(keys)) || len(value) > maxBytes || len(value) < 66 || value[64] != '\n' || !nativeReasoningReplayHashValid(value[:64]) {
			return nil, service.ErrNativeReasoningReplayCorrupt
		}
		result[keys[index-1]] = service.NativeReasoningReplayValue{Digest: value[:64], Ciphertext: value[65:]}
	}
	return result, nil
}

func (c *gatewayCache) PutNativeReasoningReplay(ctx context.Context, value service.NativeReasoningReplayWrite, limits service.NativeReasoningReplayLimits) error {
	limits = limits.Normalize()
	if !nativeReasoningReplayHashValid(value.Owner) || !nativeReasoningReplayHashValid(value.Key) || !nativeReasoningReplayHashValid(value.Digest) || value.Ciphertext == "" {
		return service.ErrNativeReasoningReplayInvalid
	}
	if len(value.Ciphertext)+65 > limits.MaxEntryBytes {
		return service.ErrNativeReasoningReplayCapacity
	}
	client, bounded, cancel, err := c.nativeReasoningReplayClient(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	n, err := putNativeReasoningReplayScript.Run(bounded, client, []string{nativeReasoningReplayDataKey}, value.Owner, value.Key, value.Digest, value.Ciphertext, limits.TTL.Milliseconds(), limits.MaxEntriesPerUser, limits.MaxEntriesGlobal, limits.MaxEntryBytes).Int()
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return err
		}
		return service.ErrNativeReasoningReplayUnavailable
	}
	switch n {
	case 1, 2:
		return nil
	case 0:
		return service.ErrNativeReasoningReplayCapacity
	case -1:
		return service.ErrNativeReasoningReplayAmbiguous
	case -2:
		return service.ErrNativeReasoningReplayInvalid
	default:
		return service.ErrNativeReasoningReplayCorrupt
	}
}

func (c *gatewayCache) InvalidateNativeReasoningReplay(ctx context.Context, owner string, refs []service.NativeReasoningReplayReference) error {
	if !nativeReasoningReplayHashValid(owner) || len(refs) > service.DefaultNativeReasoningReplayLimits().MaxLookups {
		return service.ErrNativeReasoningReplayInvalid
	}
	if len(refs) == 0 {
		return nil
	}
	args := make([]any, 0, 1+len(refs)*2)
	args = append(args, owner)
	for _, ref := range refs {
		if !nativeReasoningReplayHashValid(ref.Key) || !nativeReasoningReplayHashValid(ref.Digest) {
			return service.ErrNativeReasoningReplayInvalid
		}
		args = append(args, ref.Key, ref.Digest)
	}
	client, bounded, cancel, err := c.nativeReasoningReplayClient(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	n, err := invalidateNativeReasoningReplayScript.Run(bounded, client, []string{nativeReasoningReplayDataKey}, args...).Int()
	if err != nil {
		return service.ErrNativeReasoningReplayUnavailable
	}
	if n != 1 {
		return service.ErrNativeReasoningReplayCorrupt
	}
	return nil
}
