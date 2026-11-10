package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const openAIResponseAuthOwnerPrefix = "openai:response:auth-owner:v1:"

var _ service.OpenAIResponseAuthOwnerCache = (*gatewayCache)(nil)

func (c *gatewayCache) SetOpenAIResponseAuthOwner(ctx context.Context, key string, payload []byte, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return c.rdb.SetNX(ctx, openAIResponseAuthOwnerPrefix+key, payload, ttl).Result()
}
func (c *gatewayCache) GetOpenAIResponseAuthOwner(ctx context.Context, key string) ([]byte, error) {
	payload, err := c.rdb.Get(ctx, openAIResponseAuthOwnerPrefix+key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	return payload, err
}
