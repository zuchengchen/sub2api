//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateAccountOveragesRepoStub struct {
	mockAccountRepoForTest
	account     *Account
	updateCalls int
}

func (r *updateAccountOveragesRepoStub) GetByID(ctx context.Context, id int64) (*Account, error) {
	return r.account, nil
}

func (r *updateAccountOveragesRepoStub) Update(ctx context.Context, account *Account) error {
	r.updateCalls++
	r.account = account
	return nil
}

func TestUpdateAccount_EmptyExtraPayloadCanClearQuotaLimits(t *testing.T) {
	accountID := int64(103)
	repo := &updateAccountOveragesRepoStub{
		account: &Account{
			ID:       accountID,
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra: map[string]any{
				"quota_limit":        100.0,
				"quota_daily_limit":  10.0,
				"quota_weekly_limit": 40.0,
			},
		},
	}

	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		// 显式空对象：语义是“清空 extra 中的可配置键”（例如关闭配额限制）
		Extra: map[string]any{},
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 1, repo.updateCalls)
	require.NotNil(t, repo.account.Extra)
	require.NotContains(t, repo.account.Extra, "quota_limit")
	require.NotContains(t, repo.account.Extra, "quota_daily_limit")
	require.NotContains(t, repo.account.Extra, "quota_weekly_limit")
	require.Len(t, repo.account.Extra, 0)
}

func TestUpdateAccount_FixedWeeklyResetClearsLegacyRollingUsage(t *testing.T) {
	now := time.Now().UTC()
	daysSinceMonday := (int(now.Weekday()) + 6) % 7
	currentWeekStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -daysSinceMonday)
	legacyRollingStart := currentWeekStart.Add(-24 * time.Hour)
	accountID := int64(104)
	repo := &updateAccountOveragesRepoStub{
		account: &Account{
			ID:       accountID,
			Platform: PlatformAnthropic,
			Type:     AccountTypeAPIKey,
			Status:   StatusActive,
			Extra: map[string]any{
				"quota_weekly_limit": 40.0,
				"quota_weekly_used":  12.5,
				"quota_weekly_start": legacyRollingStart.Format(time.RFC3339),
			},
		},
	}

	svc := &adminServiceImpl{accountRepo: repo}
	updated, err := svc.UpdateAccount(context.Background(), accountID, &UpdateAccountInput{
		Extra: map[string]any{
			"quota_weekly_limit":      40.0,
			"quota_weekly_reset_mode": "fixed",
			"quota_weekly_reset_day":  float64(1),
			"quota_weekly_reset_hour": float64(0),
			"quota_reset_timezone":    "UTC",
		},
	})

	require.NoError(t, err)
	require.NotNil(t, updated)
	require.Equal(t, 1, repo.updateCalls)
	require.InDelta(t, 0.0, updated.GetQuotaWeeklyUsed(), 1e-9)
	require.Equal(t, currentWeekStart.Format(time.RFC3339), updated.Extra["quota_weekly_start"])
	require.False(t, updated.IsWeeklyQuotaPeriodExpired())
}
