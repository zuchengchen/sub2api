package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func nativeReplayTestHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
func nativeReplayStoreWrite(owner, key, content string) service.NativeReasoningReplayWrite {
	return service.NativeReasoningReplayWrite{Owner: nativeReplayTestHash(owner), Key: nativeReplayTestHash(key), NativeReasoningReplayValue: service.NativeReasoningReplayValue{Digest: nativeReplayTestHash(content), Ciphertext: "synthetic-ciphertext/" + content}}
}

// integration_harness intentionally prefixes EVAL KEYS but does not recognize
// every hash command. Use explicit scripts for assertions too, so they inspect
// exactly the isolated namespace rather than accidentally reading the bare key.
func nativeReplayTestFieldCount(ctx context.Context, client *redis.Client) (int64, error) {
	return client.Eval(ctx, `return redis.call('HLEN',KEYS[1])`, []string{nativeReasoningReplayDataKey}).Int64()
}
func nativeReplayTestHasValue(ctx context.Context, client *redis.Client, field string) (bool, error) {
	n, err := client.Eval(ctx, `return redis.call('HEXISTS',KEYS[1],ARGV[1])`, []string{nativeReasoningReplayDataKey}, field).Int64()
	return n == 1, err
}

func nativeReplayUnitRedis(t *testing.T) (*gatewayCache, *redis.Client, *miniredis.Miniredis) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return &gatewayCache{rdb: client}, client, mr
}

func assertNativeReplayStoreRoundTrip(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx := context.Background()
	cache := &gatewayCache{rdb: client}
	v := nativeReplayStoreWrite("u1", "branch1", "payload1")
	limits := service.DefaultNativeReasoningReplayLimits()
	missing, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Empty(t, missing)
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, v, limits))
	got, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Equal(t, v.NativeReasoningReplayValue, got[v.Key])
	ttl, err := client.PTTL(ctx, nativeReasoningReplayDataKey).Result()
	require.NoError(t, err)
	require.Greater(t, ttl, 59*time.Minute)
	require.LessOrEqual(t, ttl, time.Hour)
	// A new nonce changes ciphertext but keeps the semantic content digest.
	duplicate := v
	duplicate.Ciphertext = "different-synthetic-encryption-nonce"
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, duplicate, limits))
	got, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Equal(t, v.Ciphertext, got[v.Key].Ciphertext)
	otherOwner := nativeReplayTestHash("u2")
	got, err = cache.GetNativeReasoningReplay(ctx, otherOwner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Empty(t, got)
	// The old reverse reasoning cache cannot supply a native record.
	require.NoError(t, cache.SetReasoningContent(ctx, v.Key, "synthetic plaintext", time.Hour))
	got, err = cache.GetNativeReasoningReplay(ctx, otherOwner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Empty(t, got)
	conflict := v
	conflict.Digest = nativeReplayTestHash("different native output")
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, conflict, limits), service.ErrNativeReasoningReplayAmbiguous)
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v, limits), service.ErrNativeReasoningReplayAmbiguous)
	got, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Empty(t, got)
	n, err := nativeReplayTestFieldCount(ctx, client)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
}

func assertNativeReplayStoreLimits(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx := context.Background()
	cache := &gatewayCache{rdb: client}
	limits := service.NativeReasoningReplayLimits{MaxEntriesPerUser: 1, MaxEntriesGlobal: 2, MaxEntryBytes: 128}.Normalize()
	v1 := nativeReplayStoreWrite("u1", "b1", "p1")
	v2 := nativeReplayStoreWrite("u1", "b2", "p2")
	v3 := nativeReplayStoreWrite("u2", "b3", "p3")
	v4 := nativeReplayStoreWrite("u3", "b4", "p4")
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, v1, limits))
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v2, limits), service.ErrNativeReasoningReplayCapacity)
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, v3, limits))
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v4, limits), service.ErrNativeReasoningReplayCapacity)
	v4.Ciphertext = strings.Repeat("x", 129)
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v4, limits), service.ErrNativeReasoningReplayCapacity)
	for _, v := range []service.NativeReasoningReplayWrite{v2, v4} {
		n, err := nativeReplayTestHasValue(ctx, client, nativeReasoningReplayKey(v.Owner, v.Key))
		require.NoError(t, err)
		require.False(t, n)
	}
	n, err := nativeReplayTestFieldCount(ctx, client)
	require.NoError(t, err)
	require.EqualValues(t, 4, n)
}

func assertNativeReplayStoreCorruption(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx := context.Background()
	cache := &gatewayCache{rdb: client}
	limits := service.NativeReasoningReplayLimits{MaxEntryBytes: 128}.Normalize()
	v := nativeReplayStoreWrite("u1", "b1", "p1")
	field := nativeReasoningReplayKey(v.Owner, v.Key)
	meta := "m:" + v.Owner + ":" + v.Key
	// A corrupt namespace must fail before publishing any partial entry.
	require.NoError(t, client.Set(ctx, nativeReasoningReplayDataKey, "corrupt", time.Hour).Err())
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v, limits), service.ErrNativeReasoningReplayCorrupt)
	original, err := client.Get(ctx, nativeReasoningReplayDataKey).Result()
	require.NoError(t, err)
	require.Equal(t, "corrupt", original)
	require.NoError(t, client.Del(ctx, nativeReasoningReplayDataKey).Err())
	// Never download oversized corrupted values in order to validate them.
	require.NoError(t, client.HSet(ctx, nativeReasoningReplayDataKey, meta, time.Now().Add(time.Hour).UnixMilli(), field, strings.Repeat("x", 1024)).Err())
	_, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, 128)
	require.ErrorIs(t, err, service.ErrNativeReasoningReplayCapacity)
	require.NoError(t, client.HSet(ctx, nativeReasoningReplayDataKey, field, "invalid envelope").Err())
	_, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, 128)
	require.ErrorIs(t, err, service.ErrNativeReasoningReplayCorrupt)
	require.NoError(t, client.HSet(ctx, nativeReasoningReplayDataKey, meta, "invalid expiry").Err())
	_, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, 128)
	require.ErrorIs(t, err, service.ErrNativeReasoningReplayCorrupt)
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v, limits), service.ErrNativeReasoningReplayCorrupt)
	require.NoError(t, client.HDel(ctx, nativeReasoningReplayDataKey, meta).Err())
	_, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, 128)
	require.ErrorIs(t, err, service.ErrNativeReasoningReplayCorrupt)
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v, limits), service.ErrNativeReasoningReplayCorrupt)
}

func assertNativeReplayStoreConcurrency(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx := context.Background()
	cache := &gatewayCache{rdb: client}
	limits := service.NativeReasoningReplayLimits{MaxEntriesPerUser: 4, MaxEntriesGlobal: 4}.Normalize()
	// This test measures atomic admission, not handshake/script cold-start
	// contention. Timeout behavior has its own blackhole and context tests.
	require.NoError(t, putNativeReasoningReplayScript.Load(ctx, client).Err())
	var warm sync.WaitGroup
	warmErrors := make(chan error, 20)
	for i := 0; i < 20; i++ {
		warm.Add(1)
		go func() { defer warm.Done(); warmErrors <- client.Ping(ctx).Err() }()
	}
	warm.Wait()
	close(warmErrors)
	for err := range warmErrors {
		require.NoError(t, err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := nativeReplayStoreWrite("one-user", fmt.Sprint(i), fmt.Sprint(i))
			errs <- cache.PutNativeReasoningReplay(ctx, v, limits)
		}(i)
	}
	wg.Wait()
	close(errs)
	accepted := 0
	for err := range errs {
		if err == nil {
			accepted++
		} else {
			require.ErrorIs(t, err, service.ErrNativeReasoningReplayCapacity)
		}
	}
	require.Equal(t, 4, accepted)
	n, err := nativeReplayTestFieldCount(ctx, client)
	require.NoError(t, err)
	require.EqualValues(t, accepted*2, n)
	// Every published quota member must resolve to a complete value.
	members, err := client.Eval(ctx, `return redis.call('HKEYS',KEYS[1])`, []string{nativeReasoningReplayDataKey}).StringSlice()
	require.NoError(t, err)
	for _, key := range members {
		if !strings.HasPrefix(key, "v:") {
			continue
		}
		value, err := client.HGet(ctx, nativeReasoningReplayDataKey, key).Result()
		require.NoError(t, err)
		require.Greater(t, len(value), 65)
		require.Equal(t, byte('\n'), value[64])
	}
}

func assertNativeReplayStoreEviction(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx := context.Background()
	cache := &gatewayCache{rdb: client}
	limits := service.NativeReasoningReplayLimits{MaxEntriesGlobal: 1, MaxEntriesPerUser: 1}.Normalize()
	first := nativeReplayStoreWrite("user", "first", "payload")
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, first, limits))
	// Simulate Redis evicting the namespace key. No detached payload or quota
	// index may survive and make the next accepted write exceed the hard cap.
	require.NoError(t, client.Del(ctx, nativeReasoningReplayDataKey).Err())
	got, err := cache.GetNativeReasoningReplay(ctx, first.Owner, []string{first.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Empty(t, got)
	second := nativeReplayStoreWrite("user", "second", "new payload")
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, second, limits))
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, first, limits), service.ErrNativeReasoningReplayCapacity)
	n, err := nativeReplayTestFieldCount(ctx, client)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
}

func assertNativeReplayStoreInvalidation(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx := context.Background()
	cache := &gatewayCache{rdb: client}
	limits := service.DefaultNativeReasoningReplayLimits()
	v := nativeReplayStoreWrite("user", "rejected-history", "synthetic record")
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, v, limits))
	ref := service.NativeReasoningReplayReference{Key: v.Key, Digest: nativeReplayTestHash("stale record")}
	require.NoError(t, cache.InvalidateNativeReasoningReplay(ctx, v.Owner, []service.NativeReasoningReplayReference{ref}))
	got, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Len(t, got, 1)
	ref.Digest = v.Digest
	require.NoError(t, cache.InvalidateNativeReasoningReplay(ctx, nativeReplayTestHash("another user"), []service.NativeReasoningReplayReference{ref}))
	got, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Len(t, got, 1)
	before, err := client.PTTL(ctx, nativeReasoningReplayDataKey).Result()
	require.NoError(t, err)
	require.NoError(t, cache.InvalidateNativeReasoningReplay(ctx, v.Owner, []service.NativeReasoningReplayReference{ref}))
	got, err = cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Empty(t, got)
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v, limits), service.ErrNativeReasoningReplayAmbiguous)
	after, err := client.PTTL(ctx, nativeReasoningReplayDataKey).Result()
	require.NoError(t, err)
	require.LessOrEqual(t, after, before)
	count, err := nativeReplayTestFieldCount(ctx, client)
	require.NoError(t, err)
	require.EqualValues(t, 2, count, "poison must keep its quota reservation")
}

func TestNativeReasoningReplayStoreUnit(t *testing.T) {
	for name, check := range map[string]func(*testing.T, *redis.Client){"roundtrip_conflict": assertNativeReplayStoreRoundTrip, "limits": assertNativeReplayStoreLimits, "corruption_atomicity": assertNativeReplayStoreCorruption, "concurrency": assertNativeReplayStoreConcurrency, "eviction": assertNativeReplayStoreEviction, "invalidation": assertNativeReplayStoreInvalidation} {
		t.Run(name, func(t *testing.T) { _, client, _ := nativeReplayUnitRedis(t); check(t, client) })
	}
}

func TestNativeReasoningReplayStoreExpiryAndValidation(t *testing.T) {
	cache, client, mr := nativeReplayUnitRedis(t)
	ctx := context.Background()
	limits := service.NativeReasoningReplayLimits{TTL: time.Second, MaxEntriesGlobal: 1, MaxEntriesPerUser: 1}.Normalize()
	v := nativeReplayStoreWrite("u1", "expired", "p1")
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, v, limits))
	mr.FastForward(2 * time.Second)
	mr.SetTime(time.Now().Add(2 * time.Second))
	got, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Empty(t, got)
	next := nativeReplayStoreWrite("u1", "new", "p2")
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, next, limits))
	n, err := nativeReplayTestFieldCount(ctx, client)
	require.NoError(t, err)
	require.EqualValues(t, 2, n)
	_, err = cache.GetNativeReasoningReplay(ctx, "untrusted/raw/owner", []string{v.Key}, 128)
	require.ErrorIs(t, err, service.ErrNativeReasoningReplayInvalid)
	_, err = cache.GetNativeReasoningReplay(ctx, v.Owner, make([]string, 33), 128)
	require.ErrorIs(t, err, service.ErrNativeReasoningReplayInvalid)
	var nilCache *gatewayCache
	require.ErrorIs(t, nilCache.PutNativeReasoningReplay(ctx, v, limits), service.ErrNativeReasoningReplayUnavailable)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	require.ErrorIs(t, cache.PutNativeReasoningReplay(ctx, v, limits), context.Canceled)
}

type nativeReplayTestPrefixHook struct{ prefix string }

func (h nativeReplayTestPrefixHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h nativeReplayTestPrefixHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

type nativeReplayCountingHook struct{ calls *atomic.Int64 }

func (h nativeReplayCountingHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h nativeReplayCountingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}
func (h nativeReplayCountingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "evalsha" || cmd.Name() == "eval" {
			h.calls.Add(1)
		}
		return next(ctx, cmd)
	}
}
func (h nativeReplayTestPrefixHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if (cmd.Name() == "eval" || cmd.Name() == "evalsha") && len(cmd.Args()) > 3 {
			if key, ok := cmd.Args()[3].(string); ok && !strings.HasPrefix(key, h.prefix) {
				cmd.Args()[3] = h.prefix + key
			}
		}
		return next(ctx, cmd)
	}
}

func TestNativeReasoningReplayStorePreservesClientHooks(t *testing.T) {
	cache, client, mr := nativeReplayUnitRedis(t)
	client.AddHook(nativeReplayTestPrefixHook{prefix: "isolated-native-test:"})
	ctx := context.Background()
	v := nativeReplayStoreWrite("user", "branch", "opaque payload")
	limits := service.DefaultNativeReasoningReplayLimits()
	require.NoError(t, cache.PutNativeReasoningReplay(ctx, v, limits))
	require.True(t, mr.Exists("isolated-native-test:"+nativeReasoningReplayDataKey))
	require.False(t, mr.Exists(nativeReasoningReplayDataKey), "bounded I/O must not bypass namespace hooks")
	got, err := cache.GetNativeReasoningReplay(ctx, v.Owner, []string{v.Key}, limits.MaxEntryBytes)
	require.NoError(t, err)
	require.Equal(t, v.NativeReasoningReplayValue, got[v.Key])
	// The temporary client must not close the shared parent pool.
	require.NoError(t, client.Ping(ctx).Err())
}

func TestNativeReasoningReplayStoreBoundsNetworkIO(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := make(chan struct{})
	var wg sync.WaitGroup
	var connectionsMu sync.Mutex
	var connections []net.Conn
	go func() {
		defer close(closed)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connectionsMu.Lock()
			connections = append(connections, conn)
			connectionsMu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				buffer := make([]byte, 1024)
				for {
					if _, err := conn.Read(buffer); err != nil {
						return
					}
				}
			}()
		}
	}()
	client := redis.NewClient(&redis.Options{Addr: listener.Addr().String(), ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, MaxRetries: -1})
	t.Cleanup(func() {
		_ = client.Close()
		_ = listener.Close()
		<-closed
		// This blackhole owns its accepted sockets, including connections the
		// client dialer may have abandoned before they entered its shared pool.
		connectionsMu.Lock()
		for _, conn := range connections {
			_ = conn.Close()
		}
		connectionsMu.Unlock()
		wg.Wait()
	})
	cache := &gatewayCache{rdb: client}
	v := nativeReplayStoreWrite("u", "k", "p")
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = cache.PutNativeReasoningReplay(ctx, v, service.NativeReasoningReplayLimits{})
	require.Error(t, err)
	require.True(t, errors.Is(err, service.ErrNativeReasoningReplayUnavailable) || errors.Is(err, context.DeadlineExceeded))
	require.Less(t, time.Since(start), 500*time.Millisecond, "ordinary Redis socket timeout must not control replay I/O")
}
