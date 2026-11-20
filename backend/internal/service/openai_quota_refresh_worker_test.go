//go:build unit

package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

type quotaRefreshTestPager func(context.Context, OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error)

func (f quotaRefreshTestPager) ListOAuthRefreshCandidatePage(ctx context.Context, options OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
	return f(ctx, options)
}

type quotaRefreshTestQuery func(context.Context, int64) error

func (f quotaRefreshTestQuery) RefreshQuotaSnapshots(ctx context.Context, id int64) error {
	return f(ctx, id)
}

type quotaRefreshTestLock struct {
	fakeLeaderLockCache
	renewals atomic.Int32
	loseLock bool
}

func (l *quotaRefreshTestLock) RenewLeaderLock(_ context.Context, key, owner string, _ time.Duration) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renewals.Add(1)
	return !l.loseLock && l.owners[key] == owner, nil
}

func TestOpenAIQuotaRefreshWorkerPaginatesAndLimitsQueries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opts := []OAuthRefreshPageOptions{}
		now := time.Now()
		accounts := make([]Account, 14)
		for i := range accounts {
			accounts[i] = Account{
				ID: int64(i + 1), Status: StatusActive, Platform: PlatformOpenAI,
				Type: AccountTypeOAuth, Schedulable: true,
			}
		}
		accounts[0].Schedulable = false
		reset := now.Add(time.Hour)
		accounts[1].RateLimitResetAt = &reset
		accounts[8].Status = StatusError
		accounts[9].Type = AccountTypeAPIKey
		accounts[10].Platform = PlatformAnthropic
		accounts[11].ID, accounts[12].ID, accounts[13].ID = 15, 16, 17
		accounts[11].AutoPauseOnExpired = true
		accounts[11].ExpiresAt = &now
		accounts[12].Extra = map[string]any{"synthetic_ui_test": true}
		accounts[13].Extra = map[string]any{
			OpenAIAutoResetCreditEnabledExtraKey:      true,
			OpenAIAutoResetCreditOptInVersionExtraKey: 1,
			OpenAICreditsEnabledExtraKey:              true,
		}

		pager := quotaRefreshTestPager(func(_ context.Context, option OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
			opts = append(opts, option)
			switch option.AfterID {
			case 0:
				return &OAuthRefreshCandidatePage{Accounts: accounts[:11], NextAfterID: 11, HasMore: true}, nil
			case 11:
				// Rows deleted between the ID query and hydration still advance the cursor.
				return &OAuthRefreshCandidatePage{NextAfterID: 14, HasMore: true}, nil
			default:
				return &OAuthRefreshCandidatePage{Accounts: accounts[11:]}, nil
			}
		})
		var mu sync.Mutex
		var ids []int64
		active, peak := 0, 0
		query := quotaRefreshTestQuery(func(_ context.Context, id int64) error {
			mu.Lock()
			active++
			peak = max(peak, active)
			ids = append(ids, id)
			mu.Unlock()
			time.Sleep(time.Second)
			mu.Lock()
			active--
			mu.Unlock()
			if id == 1 {
				return errors.New("upstream unavailable")
			}
			return nil
		})
		w := OpenAIQuotaRefreshWorker{pager: pager, quota: query}
		require.NoError(t, w.refreshAll(t.Context()))
		require.Equal(t, 4, peak)
		require.ElementsMatch(t, []int64{1, 2, 3, 4, 5, 6, 7, 8, 17}, ids)
		var cursors []int64
		for _, opt := range opts {
			cursors = append(cursors, opt.AfterID)
			require.Equal(t, OAuthRefreshPageOptions{
				Platforms: []string{PlatformOpenAI}, ActiveOnly: true, Limit: openAIQuotaRefreshPageSize, AfterID: opt.AfterID,
			}, opt)
		}
		require.Equal(t, []int64{0, 11, 14}, cursors)
	})
}

func TestOpenAIQuotaRefreshWorkerRunsOncePerFiveMinuteSlotAcrossReplicas(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var scans atomic.Int32
		pager := quotaRefreshTestPager(func(context.Context, OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
			scans.Add(1)
			return &OAuthRefreshCandidatePage{}, nil
		})
		locks := &quotaRefreshTestLock{}
		first := OpenAIQuotaRefreshWorker{pager: pager, locks: locks}
		first.start()
		defer first.Stop()
		synctest.Wait()
		require.EqualValues(t, 1, scans.Load())

		time.Sleep(time.Minute)
		peer := OpenAIQuotaRefreshWorker{pager: pager, locks: locks}
		peer.start()
		defer peer.Stop()
		synctest.Wait()
		require.EqualValues(t, 1, scans.Load())
		time.Sleep(4*time.Minute - time.Nanosecond)
		synctest.Wait()
		require.EqualValues(t, 1, scans.Load())
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		require.EqualValues(t, 2, scans.Load())
	})
}

func TestOpenAIQuotaRefreshWorkerKeepsOwnershipAcrossSlots(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var scans atomic.Int32
		pager := quotaRefreshTestPager(func(ctx context.Context, _ OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
			scans.Add(1)
			<-ctx.Done()
			return nil, ctx.Err()
		})
		locks := &quotaRefreshTestLock{}
		first := OpenAIQuotaRefreshWorker{pager: pager, locks: locks}
		first.start()
		defer first.Stop()
		synctest.Wait()
		time.Sleep(2 * openAIQuotaSnapshotRefreshInterval)
		synctest.Wait()
		require.Greater(t, locks.renewals.Load(), int32(1))
		peer := OpenAIQuotaRefreshWorker{
			locks: locks,
			pager: quotaRefreshTestPager(func(context.Context, OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
				scans.Add(1)
				return &OAuthRefreshCandidatePage{}, nil
			}),
		}
		require.NoError(t, peer.run(t.Context()))
		require.EqualValues(t, 1, scans.Load())
	})
}

func TestOpenAIQuotaRefreshWorkerCancelsInFlightOnStopOrLeaseLoss(t *testing.T) {
	for _, lose := range []bool{false, true} {
		name := "shutdown"
		if lose {
			name = "lease loss"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				locks := &quotaRefreshTestLock{loseLock: lose}
				var canceled atomic.Int32
				pager := quotaRefreshTestPager(func(context.Context, OAuthRefreshPageOptions) (*OAuthRefreshCandidatePage, error) {
					return &OAuthRefreshCandidatePage{Accounts: []Account{{
						ID: 1, Status: StatusActive, Platform: PlatformOpenAI, Type: AccountTypeOAuth,
					}}}, nil
				})
				query := quotaRefreshTestQuery(func(ctx context.Context, _ int64) error {
					<-ctx.Done()
					canceled.Add(1)
					return ctx.Err()
				})
				w := OpenAIQuotaRefreshWorker{pager: pager, quota: query, locks: locks}
				w.start()
				defer w.Stop()
				synctest.Wait()
				if lose {
					time.Sleep(openAIQuotaRefreshLease / 3)
					synctest.Wait()
					require.EqualValues(t, 1, locks.renewals.Load())
				} else {
					w.Stop()
				}
				require.EqualValues(t, 1, canceled.Load())
			})
		})
	}
}
