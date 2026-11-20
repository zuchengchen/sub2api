package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type passiveUsageRecoveryCounter struct{ calls atomic.Int32 }

func (r *passiveUsageRecoveryCounter) ConsiderOpenAI429Recovery(*Account) { r.calls.Add(1) }

func TestAccountUsageService_PassiveSnapshotsReadLocalDataOnly(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sampled := now.Add(-time.Hour)
	rateLimitedUntil := now.Add(time.Hour)
	repo := &stubOpenAIAccountRepo{accounts: []Account{
		{
			ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, RateLimitResetAt: &rateLimitedUntil,
			Extra: map[string]any{
				"codex_usage_updated_at": sampled.Format(time.RFC3339), "codex_5h_used_percent": 73.0,
				"codex_5h_reset_at": rateLimitedUntil.Format(time.RFC3339),
			},
		},
		{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth},
		{ID: 3, Platform: PlatformGrok, Type: AccountTypeOAuth},
		{ID: 4, Platform: PlatformAnthropic, Type: AccountTypeSetupToken, Extra: map[string]any{"passive_usage_7d_utilization": 0.25}},
		{ID: 5, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{"codex_7d_used_percent": 0.0}},
	}}
	for _, mode := range []string{"single", "batch"} {
		t.Run(mode, func(t *testing.T) {
			logs := &performanceWindowRepo{single: func(context.Context, int64, time.Time) (*usagestats.AccountStats, error) {
				return &usagestats.AccountStats{}, nil
			}}
			recovery := &passiveUsageRecoveryCounter{}
			svc := &AccountUsageService{accountRepo: repo, usageLogRepo: logs, cache: NewUsageCache(), openAI429Recovery: recovery}
			usage := map[int64]*UsageInfo{}
			if mode == "batch" {
				var err error
				var failures map[int64]string
				usage, failures, err = svc.GetPassiveUsageBatch(t.Context(), []int64{1, 2, 3, 4, 5, 5, 99})
				require.NoError(t, err)
				require.Len(t, failures, 2)
				require.Contains(t, failures[3], "passive usage")
				require.Equal(t, ErrAccountNotFound.Error(), failures[99])
				require.Len(t, logs.batches, 1)
				require.Len(t, logs.batches[0], 2)
				for _, window := range logs.batches[0] {
					require.Equal(t, window.AccountID == 5, window.SecondaryStartTime != nil)
				}
			} else {
				for _, id := range []int64{1, 2, 4, 5} {
					var err error
					usage[id], err = svc.GetPassiveUsage(t.Context(), id)
					require.NoError(t, err)
				}
				_, err := svc.GetPassiveUsage(t.Context(), 3)
				require.Equal(t, "PASSIVE_USAGE_UNSUPPORTED", infraerrors.Reason(err))
			}

			require.Equal(t, "passive", usage[1].Source)
			require.Equal(t, &sampled, usage[1].UpdatedAt)
			require.Equal(t, 73.0, usage[1].FiveHour.Utilization)
			require.Equal(t, &rateLimitedUntil, usage[1].FiveHour.ResetsAt)
			require.EqualValues(t, 1, usage[1].FiveHour.WindowStats.Requests)
			require.Nil(t, usage[1].SevenDay)
			require.Nil(t, usage[2].UpdatedAt)
			require.Nil(t, usage[2].FiveHour)
			require.Nil(t, usage[2].SevenDay)
			require.Equal(t, "passive", usage[4].Source)
			require.Equal(t, 25.0, usage[4].SevenDay.Utilization)
			require.Nil(t, usage[5].FiveHour)
			require.Equal(t, 0.0, usage[5].SevenDay.Utilization)
			require.EqualValues(t, 105, usage[5].SevenDay.WindowStats.Requests)
			require.Zero(t, recovery.calls.Load())
			for _, id := range []int64{1, 2, 5} {
				_, attempted := svc.cache.openAIProbeCache.Load(id)
				require.False(t, attempted, "passive lookup must not start a quota probe")
			}
		})
	}
}
