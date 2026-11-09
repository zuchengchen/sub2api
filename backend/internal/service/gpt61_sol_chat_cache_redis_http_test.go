package service_test

// These tests join real HTTP/SSE, the normal settings/cache constructors and
// Redis protocol failures. Miniredis is a private TCP fixture, not evidence of
// real Redis Lua/atomicity semantics; the repository integration suite supplies
// that separate evidence against its own PostgreSQL and Redis containers.
import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPT61SolChatCacheRedisHTTPFallback(t *testing.T) {
	for _, scenario := range []string{"ttl_expiry", "evicted_namespace", "connection_closed"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream_%t", scenario, stream), func(t *testing.T) {
				mr := miniredis.RunT(t)
				client := redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
				t.Cleanup(func() { require.NoError(t, client.Close()) })
				require.NoError(t, client.Ping(context.Background()).Err())
				settings := &nativeWiringSettingsRepository{values: map[string]string{
					nativeWiringSettingKey: `{"mode":"replay","user_ids":[],"all_users_models":["gpt-6.1-sol"]}`,
				}}
				cfg := nativeWiringConfig("synthetic-gpt61-redis-http-fallback")
				if scenario == "ttl_expiry" {
					cfg.Gateway.NativeReasoningReplay.TTLSeconds = 1
				}
				h, upstream := newNativeConstructorWiringHarness(t)
				h.mu.Lock()
				h.modelOverride = "gpt-6.1-sol"
				h.mu.Unlock()
				userID := int64(101)
				if stream {
					userID = 102
				}
				nativeWiringStartService(t, h, upstream, client, settings, cfg, userID)
				history := nativeTrafficInitialHistory()
				first, err := h.call(history, stream, "")
				require.NoError(t, err)
				require.Equal(t, 1, nativeWiringStoredEntries(t, client))
				history = nativeTrafficContinue(history, first)
				second, err := h.call(history, stream, "")
				require.NoError(t, err)
				rows := nativeWiringRows(h)
				require.Len(t, rows, 2)
				require.Zero(t, nativeTrafficComparisonExit(rows[1].Body, rows[0].Output), "positive control must restore before introducing the failure")
				require.Equal(t, 2, nativeWiringStoredEntries(t, client))
				history = nativeTrafficContinue(history, second)

				switch scenario {
				case "ttl_expiry":
					// Application entry expiry uses real Unix time in the Lua
					// arguments. Advancing miniredis's key clock alone would
					// only simulate removing the whole key, not this check.
					time.Sleep(1100 * time.Millisecond)
				case "evicted_namespace":
					keys, err := client.Keys(context.Background(), "native_reasoning_replay:*").Result()
					require.NoError(t, err)
					require.NotEmpty(t, keys)
					require.NoError(t, client.Del(context.Background(), keys...).Err())
				case "connection_closed":
					mr.Close()
				}
				third, err := h.call(history, stream, "")
				require.NoError(t, err, "loss of optional state must preserve ordinary HTTP forwarding")
				rows = nativeWiringRows(h)
				require.Len(t, rows, 3)
				require.Zero(t, gpt61BodyReasoningCount(rows[2].Body))
				require.NotZero(t, nativeTrafficComparisonExit(rows[2].Body, rows[0].Output))
				require.NotZero(t, nativeTrafficComparisonExit(rows[2].Body, rows[1].Output))
				history = nativeTrafficContinue(history, third)
				_, err = h.call(history, stream, "")
				require.NoError(t, err)
				rows = nativeWiringRows(h)
				require.Len(t, rows, 4)
				if scenario == "connection_closed" {
					require.Zero(t, gpt61BodyReasoningCount(rows[3].Body))
				} else {
					require.Equal(t, 1, gpt61BodyReasoningCount(rows[3].Body))
					require.Zero(t, nativeTrafficComparisonExit(rows[3].Body, rows[2].Output), "a fresh complete branch may recover after the old records disappear")
				}
				for _, row := range rows {
					require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(row.Body, "model").String())
				}
				clients := h.clientEvidence()
				require.Len(t, clients, 4)
				require.Len(t, gjson.GetBytes(clients[3].Request, "messages").Array(), len(history), "the fallback retains the entire client history")
				h.mu.Lock()
				errorsSeen := append([]string(nil), h.errors...)
				results := append(h.results[:0:0], h.results...)
				h.mu.Unlock()
				require.Empty(t, errorsSeen)
				require.Len(t, results, 4)
				for _, result := range results {
					require.NotNil(t, result)
					require.Equal(t, 101, result.Usage.InputTokens)
					require.Equal(t, 31, result.Usage.CacheReadInputTokens)
					require.Equal(t, 17, result.Usage.OutputTokens)
				}
			})
		}
	}
}
