package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func assertLiveSidebandRoutePersistence(t *testing.T, client *redis.Client) {
	t.Helper()
	ctx := context.Background()
	cache := NewGatewayCache(client).(service.LiveCallStore)
	other := NewGatewayCache(client).(service.LiveCallStore)
	record := &service.LiveCallRecord{CallID: "synthetic_route_call", CallHash: HashLiveCallID("synthetic_route_call"),
		AccountID: 11, UserID: 12, APIKeyID: 13, SidebandRoute: "openai_v1", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Minute),
		Controller: service.LiveControllerPending, AttestationCiphertext: "synthetic-attestation", UpstreamContextCiphertext: "synthetic-context"}
	require.NoError(t, cache.SaveLiveCall(ctx, record, time.Minute))
	stored, err := client.HGet(ctx, liveCallKey(record.CallHash), "sideband_route").Result()
	require.NoError(t, err)
	require.Equal(t, "openai_v1", stored)
	loaded, err := other.GetLiveCall(ctx, record.CallHash)
	require.NoError(t, err)
	require.Equal(t, "openai_v1", loaded.SidebandRoute)
	require.Equal(t, record.UpstreamContextCiphertext, loaded.UpstreamContextCiphertext)
	// A pre-upgrade hash has no route field. A new process must keep it legacy.
	require.NoError(t, client.HDel(ctx, liveCallKey(record.CallHash), "sideband_route").Err())
	legacy, err := other.GetLiveCall(ctx, record.CallHash)
	require.NoError(t, err)
	require.Empty(t, legacy.SidebandRoute)
	require.Equal(t, record.AccountID, legacy.AccountID)
	require.Equal(t, record.UpstreamContextCiphertext, legacy.UpstreamContextCiphertext)
}

func TestGatewayCacheLiveSidebandRouteRoundTrip(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	assertLiveSidebandRoutePersistence(t, client)
}
