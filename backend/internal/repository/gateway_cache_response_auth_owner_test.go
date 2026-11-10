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

func TestGatewayCacheResponseAuthOwnerFirstWriterAndExpiry(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer client.Close()
	writer := NewGatewayCache(client).(service.OpenAIResponseAuthOwnerCache)
	reader := NewGatewayCache(client).(service.OpenAIResponseAuthOwnerCache)
	ctx := context.Background()
	missing, err := reader.GetOpenAIResponseAuthOwner(ctx, "group:missing")
	require.NoError(t, err)
	require.Empty(t, missing)
	applied, err := writer.SetOpenAIResponseAuthOwner(ctx, "group:response", []byte(`{"owner":"first"}`), time.Minute)
	require.NoError(t, err)
	require.True(t, applied)
	applied, err = reader.SetOpenAIResponseAuthOwner(ctx, "group:response", []byte(`{"owner":"replacement"}`), time.Minute)
	require.NoError(t, err)
	require.False(t, applied)
	got, err := reader.GetOpenAIResponseAuthOwner(ctx, "group:response")
	require.NoError(t, err)
	require.JSONEq(t, `{"owner":"first"}`, string(got))
	mr.FastForward(2 * time.Minute)
	got, err = reader.GetOpenAIResponseAuthOwner(ctx, "group:response")
	require.NoError(t, err)
	require.Empty(t, got)
	mr.Close()
	_, err = reader.GetOpenAIResponseAuthOwner(ctx, "group:response")
	require.Error(t, err)
}
