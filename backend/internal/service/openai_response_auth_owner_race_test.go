package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type responseAuthOwnerControlledCache struct {
	responseAuthOwnerTestCache
	entered chan struct{}
	release chan struct{}
	setErr  error
	lose    bool
}

func (c *responseAuthOwnerControlledCache) SetOpenAIResponseAuthOwner(ctx context.Context, key string, payload []byte, ttl time.Duration) (bool, error) {
	if c.entered != nil {
		close(c.entered)
		<-c.release
	}
	if c.setErr != nil {
		return false, c.setErr
	}
	if c.lose {
		return false, nil
	}
	return c.responseAuthOwnerTestCache.SetOpenAIResponseAuthOwner(ctx, key, payload, ttl)
}
func TestOpenAIResponseAuthOwnerPublishesOnlyCanonicalRedisWinner(t *testing.T) {
	for _, winnerExists := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending_new_origin", true: "existing_winner"}[winnerExists], func(t *testing.T) {
			account := responseAuthOwnerAccount()
			winner := snapshotOAuthRefreshAccount(account)
			winner.Credentials["chatgpt_user_id"] = "canonical-user"
			cache := &responseAuthOwnerControlledCache{entered: make(chan struct{}), release: make(chan struct{})}
			c := responseAuthOwnerContext(101, 201)
			binding := openAIResponseAuthOwnerBinding{Owner: openAIResponseAuthOwnerFingerprint(account), UserID: 101, APIKeyID: 201, ExpiresUnixNano: time.Now().Add(time.Minute).UnixNano()}
			if winnerExists {
				canonical := binding
				canonical.Owner = openAIResponseAuthOwnerFingerprint(winner)
				raw, err := json.Marshal(canonical)
				require.NoError(t, err)
				_, err = cache.responseAuthOwnerTestCache.SetOpenAIResponseAuthOwner(context.Background(), openAIResponseAuthOwnerCacheKey(0, "resp_race"), raw, time.Minute)
				require.NoError(t, err)
			}
			store := NewOpenAIWSStateStore(cache).(*defaultOpenAIWSStateStore)
			svc := &OpenAIGatewayService{openaiWSStateStore: store}
			done := make(chan error, 1)
			go func() {
				done <- store.bindResponseAuthOwner(context.Background(), 0, "resp_race", binding, time.Minute)
			}()
			<-cache.entered
			store.responseOwnerMu.RLock()
			_, published := store.responseAuthOwners[openAIWSResponseAccountMapKey(0, "resp_race")]
			store.responseOwnerMu.RUnlock()
			require.False(t, published, "unconfirmed SET NX must not publish the attempted owner locally")
			err := svc.validateOpenAIResponseAuthOwner(context.Background(), c, account, "resp_race")
			require.Error(t, err, "pending origin must never become unknown-native permission")
			if winnerExists {
				require.ErrorIs(t, err, errOpenAIResponseAuthOwnerMismatch)
			}
			close(cache.release)
			require.NoError(t, <-done)
			if winnerExists {
				require.NoError(t, svc.validateOpenAIResponseAuthOwner(context.Background(), c, winner, "resp_race"))
				require.ErrorIs(t, svc.validateOpenAIResponseAuthOwner(context.Background(), c, account, "resp_race"), errOpenAIResponseAuthOwnerMismatch)
			} else {
				require.NoError(t, svc.validateOpenAIResponseAuthOwner(context.Background(), c, account, "resp_race"))
			}
		})
	}
}

func TestOpenAIResponseAuthOwnerFailedPublishRemainsUnavailableUntilCanonicalRead(t *testing.T) {
	for _, mode := range []string{"set_error", "winner_read_error", "missing_winner", "malformed_winner"} {
		t.Run(mode, func(t *testing.T) {
			account := responseAuthOwnerAccount()
			cache := &responseAuthOwnerControlledCache{lose: true}
			key := openAIResponseAuthOwnerCacheKey(0, "resp_failed")
			switch mode {
			case "set_error":
				cache.setErr = errors.New("synthetic SET failure")
			case "winner_read_error":
				cache.readErr = errors.New("synthetic GET failure")
			case "malformed_winner":
				cache.values = map[string][]byte{key: []byte(`{"owner":`)}
			}
			store := NewOpenAIWSStateStore(cache).(*defaultOpenAIWSStateStore)
			svc := &OpenAIGatewayService{openaiWSStateStore: store}
			c := responseAuthOwnerContext(101, 201)
			binding := openAIResponseAuthOwnerBinding{Owner: openAIResponseAuthOwnerFingerprint(account), UserID: 101, APIKeyID: 201, ExpiresUnixNano: time.Now().Add(time.Minute).UnixNano()}
			require.Error(t, store.bindResponseAuthOwner(context.Background(), 0, "resp_failed", binding, time.Minute))
			require.Empty(t, store.responseAuthOwners, "failed remote confirmation cannot publish an owner")
			require.Error(t, svc.validateOpenAIResponseAuthOwner(context.Background(), c, account, "resp_failed"))
			raw, err := json.Marshal(binding)
			require.NoError(t, err)
			cache.mu.Lock()
			cache.readErr = nil
			if cache.values == nil {
				cache.values = map[string][]byte{}
			}
			cache.values[key] = raw
			cache.mu.Unlock()
			require.NoError(t, svc.validateOpenAIResponseAuthOwner(context.Background(), c, account, "resp_failed"))
			require.Empty(t, store.responseAuthOwnerPending)
		})
	}
}

func BenchmarkOpenAIResponseAuthOwnerGuard(b *testing.B) {
	account := responseAuthOwnerAccount()
	c := responseAuthOwnerContext(101, 201)
	ctx := context.Background()
	svc := &OpenAIGatewayService{openaiWSStateStore: NewOpenAIWSStateStore(nil)}
	svc.bindOpenAIResponseAuthOwner(ctx, c, account, "resp_local")
	for _, scenario := range []struct{ name, id string }{{"EmptyID", ""}, {"SameOwnerLocalHit", "resp_local"}} {
		b.Run(scenario.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := svc.validateOpenAIResponseAuthOwner(ctx, c, account, scenario.id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
