package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// TokenRefresher 定义平台特定的token刷新策略接口
// 通过此接口可以扩展支持不同平台（Anthropic/OpenAI/Gemini）
type TokenRefresher interface {
	// CanRefresh 检查此刷新器是否能处理指定账号
	CanRefresh(account *Account) bool

	// NeedsRefresh 检查账号的token是否需要刷新
	NeedsRefresh(account *Account, refreshWindow time.Duration) bool

	// Refresh 执行token刷新，返回更新后的credentials
	// 注意：返回的map应该保留原有credentials中的所有字段，只更新token相关字段
	Refresh(ctx context.Context, account *Account) (map[string]any, error)
}

// ClaudeTokenRefresher 处理Anthropic/Claude OAuth token刷新
type ClaudeTokenRefresher struct {
	oauthService *OAuthService
}

// NewClaudeTokenRefresher 创建Claude token刷新器
func NewClaudeTokenRefresher(oauthService *OAuthService) *ClaudeTokenRefresher {
	return &ClaudeTokenRefresher{
		oauthService: oauthService,
	}
}

// CacheKey 返回用于分布式锁的缓存键
func (r *ClaudeTokenRefresher) CacheKey(account *Account) string {
	return ClaudeTokenCacheKey(account)
}

// CanRefresh 检查是否能处理此账号
// 处理 anthropic 平台的 oauth 与 setup-token 类型账号。
// 两者的 access_token 均为短期令牌（expires_in=28800，即 8h），到期都需刷新；
// setup-token 之前被排除会导致其 access_token 过期后请求 401。
// 此处与手动刷新入口（account.IsOAuth()）保持一致，实际是否刷新由 NeedsRefresh
// 基于 expires_at 门控，并在分布式锁保护下执行，不会造成过度刷新。
func (r *ClaudeTokenRefresher) CanRefresh(account *Account) bool {
	return account.Platform == PlatformAnthropic && account.IsOAuth()
}

// NeedsRefresh 检查token是否需要刷新
// 基于 expires_at 字段判断是否在刷新窗口内
func (r *ClaudeTokenRefresher) NeedsRefresh(account *Account, refreshWindow time.Duration) bool {
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return false
	}
	return time.Until(*expiresAt) < refreshWindow
}

// Refresh 执行token刷新
// 保留原有credentials中的所有字段，只更新token相关字段
func (r *ClaudeTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	tokenInfo, err := r.oauthService.RefreshAccountToken(ctx, account)
	if err != nil {
		return nil, err
	}

	newCredentials := BuildClaudeAccountCredentials(tokenInfo)
	newCredentials = MergeCredentials(account.Credentials, newCredentials)

	return newCredentials, nil
}

// OpenAITokenRefresher 处理 OpenAI OAuth token刷新
type OpenAITokenRefresher struct {
	openaiOAuthService *OpenAIOAuthService
	accountRepo        AccountRepository
}

// NewOpenAITokenRefresher 创建 OpenAI token刷新器
func NewOpenAITokenRefresher(openaiOAuthService *OpenAIOAuthService, accountRepo AccountRepository) *OpenAITokenRefresher {
	return &OpenAITokenRefresher{
		openaiOAuthService: openaiOAuthService,
		accountRepo:        accountRepo,
	}
}

// CacheKey 返回用于分布式锁的缓存键
func (r *OpenAITokenRefresher) CacheKey(account *Account) string {
	return OpenAITokenCacheKey(account)
}

// CanRefresh 检查是否能处理此账号
func (r *OpenAITokenRefresher) CanRefresh(account *Account) bool {
	return account.Platform == PlatformOpenAI && account.Type == AccountTypeOAuth
}

// Managed GPT auth uses the CLI's five-minute window, independent of the
// cross-provider background window (which also serves short-lived Google tokens).
func (r *OpenAITokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if account.IsOpenAIPersonalAccessToken() || account.IsOpenAIAgentIdentity() || strings.HasPrefix(strings.TrimSpace(account.GetOpenAIAccessToken()), "at-") {
		return false
	}
	if strings.TrimSpace(account.GetOpenAIRefreshToken()) == "" {
		return false
	}
	return codexAutomaticRefreshReason(account, time.Now()) != ""
}

// Refresh 执行token刷新
// 保留原有credentials中的所有字段，只更新token相关字段
func (r *OpenAITokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if openai.CodexRefreshReasonFromContext(ctx) == "" {
		reason := codexRefreshReasonForAccount(account, time.Now())
		ctx = openai.WithCodexRefreshReason(ctx, reason)
	}
	tokenInfo, err := r.openaiOAuthService.RefreshAccountToken(ctx, account)
	if err != nil {
		return nil, err
	}

	// 使用服务提供的方法构建新凭证，并保留原有字段
	newCredentials := r.openaiOAuthService.BuildAccountCredentials(tokenInfo)
	newCredentials = MergeCredentials(account.Credentials, newCredentials)
	newCredentials = NormalizeOpenAIPersonalAccessTokenCredentials(account, tokenInfo, newCredentials)

	return newCredentials, nil
}

func (r *OpenAITokenRefresher) AuthCredentialsPersisted(ctx context.Context, previous *Account) {
	r.openaiOAuthService.AuthCredentialsPersisted(ctx, previous)
}

func codexRefreshReasonForAccount(account *Account, now time.Time) string {
	if reason := codexAutomaticRefreshReason(account, now); reason != "" {
		return reason
	}
	return "forced"
}

// Official managed auth prioritizes JWT exp. Only absent/unparseable exp uses
// the eight-day last_refresh fallback; row-level expires_at is not JWT evidence.
func codexAutomaticRefreshReason(account *Account, now time.Time) string {
	parts := strings.Split(account.GetOpenAIAccessToken(), ".")
	if len(parts) == 3 {
		if raw, err := base64.RawURLEncoding.DecodeString(parts[1]); err == nil {
			var claims struct {
				Exp *int64 `json:"exp"`
			}
			if json.Unmarshal(raw, &claims) == nil && claims.Exp != nil {
				if *claims.Exp <= now.Add(5*time.Minute).Unix() {
					return "proactive_expiry"
				}
				return ""
			}
		}
	}
	if last := account.GetCredentialAsTime("last_refresh"); last != nil && last.Before(now.Add(-8*24*time.Hour)) {
		return "scheduled"
	}
	return ""
}
