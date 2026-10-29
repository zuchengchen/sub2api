package admin

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/handler/dto"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestEnrichShadowParentInfo(t *testing.T) {
	pid := int64(100)
	parent := &service.Account{
		ID:       100,
		Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{
			"email":                      "owner@example.com",
			"plan_type":                  "pro",
			"subscription_expires_at":    "2026-12-31T00:00:00Z",
			"subscription_is_delinquent": false,
			"chatgpt_account_id":         "acct_123",
		},
		Extra: map[string]any{"privacy_mode": "training_off"},
	}
	parents := map[int64]*service.Account{100: parent}

	shadow := AccountWithConcurrency{Account: &dto.Account{ID: 200, ParentAccountID: &pid}}
	normal := AccountWithConcurrency{Account: &dto.Account{ID: 1}}
	orphan := AccountWithConcurrency{Account: &dto.Account{ID: 201, ParentAccountID: ptrInt64(999)}}
	items := []AccountWithConcurrency{shadow, normal, orphan}

	enrichShadowParentInfo(items, parents)

	require.Equal(t, "owner@example.com", items[0].ParentEmail, "影子回填母账号邮箱")
	require.Equal(t, "pro", items[0].ParentPlanType)
	require.Equal(t, "training_off", items[0].ParentPrivacyMode)
	require.Equal(t, "2026-12-31T00:00:00Z", items[0].ParentSubscriptionExpiresAt)
	require.NotNil(t, items[0].ParentSubscriptionIsDelinquent)
	require.False(t, *items[0].ParentSubscriptionIsDelinquent)
	require.NotNil(t, dto.AccountListItemFromAccount(items[0].Account).ParentSubscriptionIsDelinquent)
	require.False(t, *dto.AccountListItemFromAccount(items[0].Account).ParentSubscriptionIsDelinquent)
	require.Equal(t, "acct_123", items[0].ParentChatGPTAccountID)

	require.Empty(t, items[1].ParentEmail, "非影子不回填")
	require.Empty(t, items[2].ParentEmail, "母账号缺失时优雅留空")
}

func ptrInt64(v int64) *int64 { return &v }

func TestEnrichShadowParentInfo_SubscriptionExpiryFromIDToken(t *testing.T) {
	const expiry = "2026-11-20T15:01:57Z"
	idToken := buildCodexImportTestJWT(t, time.Now().Add(-time.Hour), map[string]any{
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-1", "chatgpt_plan_type": "prolite",
			"chatgpt_subscription_active_until": expiry,
		},
	})
	parent := &service.Account{ID: 100, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth,
		Credentials: map[string]any{"id_token": idToken, "plan_type": "prolite", "chatgpt_account_id": "acct-1"},
	}
	items := []AccountWithConcurrency{{Account: &dto.Account{ParentAccountID: ptrInt64(100)}}}
	enrichShadowParentInfo(items, map[int64]*service.Account{100: parent})
	require.Equal(t, expiry, items[0].ParentSubscriptionExpiresAt)
	require.NotContains(t, parent.Credentials, "subscription_expires_at")
}
