package service

import (
	"context"
	"time"
)

// TokenCache stores short-lived access tokens and coordinates refresh to avoid stampedes.
//
// 该缓存与具体平台无关：Claude、OpenAI、Grok 与 Vertex 服务账号的 token
// provider 都通过类型别名指向它。Redis 键前缀为中性的 oauth:token: /
// oauth:refresh_lock:，与平台无关。
type TokenCache interface {
	// cacheKey should be stable for the token scope.
	GetAccessToken(ctx context.Context, cacheKey string) (string, error)
	SetAccessToken(ctx context.Context, cacheKey string, token string, ttl time.Duration) error
	DeleteAccessToken(ctx context.Context, cacheKey string) error

	AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (bool, error)
	ReleaseRefreshLock(ctx context.Context, cacheKey string) error
}
