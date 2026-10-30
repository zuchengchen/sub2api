package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/stretchr/testify/require"
)

type performanceWindowRepo struct {
	mu sync.Mutex
	UsageLogRepository
	batches  [][]AccountWindowQuery
	singles  []AccountWindowQuery
	batchErr error
	single   func(context.Context, int64, time.Time) (*usagestats.AccountStats, error)
}

func (r *performanceWindowRepo) GetAccountWindowStatsForAccounts(ctx context.Context, windows []AccountWindowQuery) (map[int64]AccountWindowStats, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.batches = append(r.batches, append([]AccountWindowQuery(nil), windows...))
	if r.batchErr != nil {
		return nil, r.batchErr
	}
	result := make(map[int64]AccountWindowStats, len(windows))
	for _, w := range windows {
		result[w.AccountID] = AccountWindowStats{Primary: &usagestats.AccountStats{Requests: w.AccountID, Cost: 3, StandardCost: 5, UserCost: 2}, Secondary: &usagestats.AccountStats{Requests: w.AccountID + 100, Cost: 9}}
	}
	return result, nil
}

func (r *performanceWindowRepo) GetAccountWindowStats(ctx context.Context, id int64, start time.Time) (*usagestats.AccountStats, error) {
	r.mu.Lock()
	r.singles = append(r.singles, AccountWindowQuery{AccountID: id, StartTime: start})
	r.mu.Unlock()
	return r.single(ctx, id, start)
}

func TestPerformanceOptimizationAccountWindowBatchBoundsAndCache(t *testing.T) {
	for _, size := range []int{0, 1, 20, 100, 128, 129, 500} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			repo := &performanceWindowRepo{}
			svc := &AccountUsageService{usageLogRepo: repo, cache: NewUsageCache()}
			cache := &windowStatsCache{stats: &WindowStats{Cost: 99}, timestamp: time.Now()}
			svc.cache.windowStatsCache.Store(int64(1), cache)
			windows := make([]AccountWindowQuery, size)
			for i := range windows {
				windows[i] = AccountWindowQuery{AccountID: int64(i + 1), StartTime: time.Now().Add(-time.Duration(i) * time.Minute)}
			}
			got := svc.GetAccountWindowStatsForAccounts(context.Background(), windows)
			require.Len(t, got, size)
			require.Len(t, repo.batches, (size+127)/128)
			require.Empty(t, repo.singles)
			var flattened []AccountWindowQuery
			for _, batch := range repo.batches {
				require.LessOrEqual(t, len(batch), 128)
				flattened = append(flattened, batch...)
			}
			if size > 0 {
				require.Equal(t, windows, flattened)
			}
			after, ok := svc.cache.windowStatsCache.Load(int64(1))
			require.True(t, ok)
			require.Same(t, cache, after)
		})
	}
}

func TestPerformanceOptimizationAccountWindowPartialFailure(t *testing.T) {
	now := time.Now()
	week := now.Add(-7 * 24 * time.Hour)
	repo := &performanceWindowRepo{batchErr: errors.New("batch failure"), single: func(_ context.Context, id int64, start time.Time) (*usagestats.AccountStats, error) {
		if id == 2 || (id == 1 && start.Equal(week)) {
			return nil, errors.New("unknown window")
		}
		return &usagestats.AccountStats{Cost: 7, StandardCost: 11}, nil
	}}
	svc := &AccountUsageService{usageLogRepo: repo}
	got := svc.GetAccountWindowStatsForAccounts(context.Background(), []AccountWindowQuery{{AccountID: 1, StartTime: now, SecondaryStartTime: &week}, {AccountID: 2, StartTime: now}, {AccountID: 3, StartTime: now}})
	require.Equal(t, float64(11), got[1].Primary.StandardCost)
	require.Nil(t, got[1].Secondary)
	require.Nil(t, got[2].Primary, "failed account must remain unknown")
	require.NotNil(t, got[3].Primary, "one failed account must not hide others")
	require.Len(t, repo.batches, 1)
	require.Len(t, repo.singles, 4)

	usage := &UsageInfo{FiveHour: &UsageProgress{}, SevenDay: &UsageProgress{}}
	repo.single = func(_ context.Context, _ int64, start time.Time) (*usagestats.AccountStats, error) {
		if start.Equal(week) {
			return nil, errors.New("seven day unavailable")
		}
		return &usagestats.AccountStats{}, nil
	}
	svc.addOpenAIWindowStats(context.Background(), []openAIUsageWindows{{accountID: 1, usage: usage, now: now}})
	require.NotNil(t, usage.FiveHour.WindowStats, "successful empty window is zero")
	require.Nil(t, usage.SevenDay.WindowStats, "failed window is unknown, not zero")
}

func TestPerformanceOptimizationOpenAIUsageBatchSnapshots(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	accounts := make([]Account, 0, 20)
	ids := make([]int64, 0, 22)
	for i := 1; i <= 20; i++ {
		accounts = append(accounts, Account{ID: int64(i), Platform: PlatformOpenAI, Type: AccountTypeOAuth, Status: StatusError, ErrorMessage: "refresh_token_invalidated", Extra: map[string]any{
			"codex_5h_used_percent": float64(i), "codex_7d_used_percent": float64(i * 2),
			"codex_5h_reset_at": now.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			"codex_7d_reset_at": now.Add(time.Duration(i) * time.Hour).Format(time.RFC3339),
		}})
		ids = append(ids, int64(i))
	}
	ids = append(ids, 1, 999)
	logs := &performanceWindowRepo{}
	repo := &usageErrorAccountRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: accounts}}
	svc := &AccountUsageService{accountRepo: repo, usageLogRepo: logs, cache: NewUsageCache()}
	usage, failures, err := svc.GetUsageBatch(context.Background(), ids, false)
	require.NoError(t, err)
	require.Len(t, usage, 20)
	require.Equal(t, map[int64]string{999: ErrAccountNotFound.Error()}, failures)
	require.Len(t, logs.batches, 1)
	require.Empty(t, logs.singles)
	for _, w := range logs.batches[0] {
		require.Equal(t, now.Add(time.Duration(w.AccountID)*time.Minute-5*time.Hour), w.StartTime)
		require.Equal(t, now.Add(time.Duration(w.AccountID)*time.Hour-7*24*time.Hour), *w.SecondaryStartTime)
		require.Equal(t, w.AccountID, usage[w.AccountID].FiveHour.WindowStats.Requests)
		require.Equal(t, w.AccountID+100, usage[w.AccountID].SevenDay.WindowStats.Requests)
	}
	require.Zero(t, repo.clearCalls)
	for _, account := range repo.accounts {
		require.Equal(t, StatusError, account.Status)
		require.Equal(t, "refresh_token_invalidated", account.ErrorMessage)
	}
}

func TestPerformanceOptimizationOpenAIExpiredWindowsAndCancellation(t *testing.T) {
	now := time.Now()
	expired := now.Add(-time.Minute)
	reversed := now.Add(10 * 24 * time.Hour)
	logs := &performanceWindowRepo{}
	svc := &AccountUsageService{usageLogRepo: logs}
	usage := &UsageInfo{FiveHour: &UsageProgress{ResetsAt: &expired}, SevenDay: &UsageProgress{ResetsAt: &reversed}}
	svc.addOpenAIWindowStats(context.Background(), []openAIUsageWindows{{accountID: 1, usage: usage, now: now}})
	require.Equal(t, now.Add(-5*time.Hour), logs.batches[0][0].StartTime)
	require.Equal(t, reversed.Add(-7*24*time.Hour), *logs.batches[0][0].SecondaryStartTime)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := svc.GetAccountWindowStatsForAccounts(ctx, []AccountWindowQuery{{AccountID: 1, StartTime: now}})
	require.Empty(t, got)
	require.Empty(t, logs.singles)
}

type performanceCodexProbe struct{ calls int }

func (p *performanceCodexProbe) doOpenAICodexProbe(_ *http.Request, _ string, _ *Account) (*http.Response, error) {
	p.calls++
	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "25")
	headers.Set("x-codex-primary-reset-after-seconds", "3600")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-secondary-used-percent", "50")
	headers.Set("x-codex-secondary-reset-after-seconds", "86400")
	headers.Set("x-codex-secondary-window-minutes", "10080")
	return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader(""))}, nil
}
func TestPerformanceOptimizationOpenAIForceRefreshWindowStarts(t *testing.T) {
	oldReset := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	repo := &accountUsageCodexProbeRepo{stubOpenAIAccountRepo: stubOpenAIAccountRepo{accounts: []Account{{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"access_token": "synthetic-local-test"}, Extra: map[string]any{
		"codex_5h_used_percent": 1.0, "codex_5h_reset_at": oldReset.Format(time.RFC3339), "codex_7d_used_percent": 2.0, "codex_7d_reset_at": oldReset.Format(time.RFC3339),
	}}}}, updateExtraCh: make(chan map[string]any, 1)}
	logs := &performanceWindowRepo{}
	probe := &performanceCodexProbe{}
	svc := &AccountUsageService{accountRepo: repo, usageLogRepo: logs, cache: NewUsageCache(), codexProbeSender: probe}
	got, failures, err := svc.GetUsageBatch(context.Background(), []int64{1}, true)
	require.NoError(t, err)
	require.Empty(t, failures)
	require.Equal(t, 1, probe.calls)
	require.Len(t, logs.batches, 1)
	require.Equal(t, 25.0, got[1].FiveHour.Utilization)
	require.Equal(t, 50.0, got[1].SevenDay.Utilization)
	query := logs.batches[0][0]
	require.Equal(t, got[1].FiveHour.ResetsAt.Add(-5*time.Hour), query.StartTime)
	require.Equal(t, got[1].SevenDay.ResetsAt.Add(-7*24*time.Hour), *query.SecondaryStartTime)
	require.NotEqual(t, oldReset.Add(-5*time.Hour), query.StartTime)
	select {
	case <-repo.updateExtraCh:
	case <-time.After(time.Second):
		t.Fatal("refreshed snapshot was not persisted")
	}
}

func TestPerformanceOptimizationWindowFallbackConcurrencyAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 20)
	var active, peak, calls atomic.Int32
	repo := &performanceWindowRepo{batchErr: errors.New("batch failed"), single: func(ctx context.Context, _ int64, _ time.Time) (*usagestats.AccountStats, error) {
		calls.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	svc := &AccountUsageService{usageLogRepo: repo}
	windows := make([]AccountWindowQuery, 20)
	now := time.Now()
	for i := range windows {
		windows[i] = AccountWindowQuery{AccountID: int64(i + 1), StartTime: now, SecondaryStartTime: &now}
	}
	done := make(chan map[int64]AccountWindowStats, 1)
	go func() { done <- svc.GetAccountWindowStatsForAccounts(ctx, windows) }()
	for i := 0; i < 6; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("fallback did not allow six concurrent accounts")
		}
	}
	require.Equal(t, int32(6), active.Load())
	cancel()
	select {
	case result := <-done:
		for _, stats := range result {
			require.Nil(t, stats.Primary)
			require.Nil(t, stats.Secondary)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback did not stop after context cancellation")
	}
	require.Equal(t, int32(6), peak.Load())
	require.Equal(t, int32(6), calls.Load(), "cancelled windows and second reads must not start")
}
