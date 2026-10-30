//go:build integration

package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

type performanceWindowSQL struct {
	Query string          `json:"query"`
	Args  []any           `json:"args"`
	Plan  json.RawMessage `json:"plan,omitempty"`
}

type performanceWindowSQLRecorder struct {
	sqlExecutor
	statements []performanceWindowSQL
}

func (r *performanceWindowSQLRecorder) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	r.statements = append(r.statements, performanceWindowSQL{Query: query, Args: args})
	return r.sqlExecutor.QueryContext(ctx, query, args...)
}

func performanceWindowFixture(t *testing.T) *sql.Tx {
	t.Helper()
	tx := testTx(t)
	// The temporary copy uses the migrated production column types and indexes;
	// it cannot read or modify any other test's usage rows.
	_, err := tx.ExecContext(context.Background(), `CREATE TEMP TABLE usage_logs (LIKE public.usage_logs INCLUDING DEFAULTS INCLUDING INDEXES) ON COMMIT DROP`)
	require.NoError(t, err)
	return tx
}

func TestPerformanceOptimizationAccountWindowsSQL(t *testing.T) {
	tx := performanceWindowFixture(t)
	ctx := context.Background()
	anchor := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	start := anchor.Add(-5 * time.Hour)
	week := anchor.Add(-7 * 24 * time.Hour)
	timezoneStart := start.In(time.FixedZone("UTC+8", 8*60*60))
	future := anchor.Add(24 * time.Hour)
	for i, created := range []time.Time{week.Add(-time.Microsecond), week, start.Add(-time.Microsecond), start, anchor, future} {
		_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs (user_id,api_key_id,account_id,request_id,model,created_at,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,account_stats_cost,account_rate_multiplier)
  VALUES(1,1,1,$1,'fixture',$2,1,2,3,4,0.1234567891,0.0987654321,$3,$4)`, fmt.Sprint(i), created, []any{nil, 0, 0.25, nil, 1.2345678912, 0.0000000001}[i], []any{nil, 0, 0.5, 1, 2.1234, nil}[i])
		require.NoError(t, err)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs (user_id,api_key_id,account_id,request_id,model,created_at,input_tokens,total_cost,actual_cost) VALUES(1,1,2,'other','fixture',$1,999,999,999)`, anchor)
	require.NoError(t, err)
	repo := newUsageLogRepositoryWithSQL(nil, tx)
	cases := []struct {
		name      string
		primary   time.Time
		secondary *time.Time
	}{
		{"timezone_and_inclusive_bounds", timezoneStart, &week},
		{"reversed_window_order", week, &start},
		{"same_window", start, &start},
		{"future_without_upper_bound", future, nil},
		{"empty_window", future.Add(time.Microsecond), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			windows := []service.AccountWindowQuery{{AccountID: 1, StartTime: tc.primary, SecondaryStartTime: tc.secondary}, {AccountID: 3, StartTime: tc.primary, SecondaryStartTime: tc.secondary}}
			got, err := repo.GetAccountWindowStatsForAccounts(ctx, windows)
			require.NoError(t, err)
			require.Len(t, got, 2)
			resultJSON, jsonErr := json.Marshal(got)
			require.NoError(t, jsonErr)
			t.Logf("equal old/new window fields: %s", resultJSON)
			for _, w := range windows {
				old, err := repo.GetAccountWindowStats(ctx, w.AccountID, w.StartTime)
				require.NoError(t, err)
				require.Equal(t, old, got[w.AccountID].Primary, "all primary fields, including exact numeric costs")
				if w.SecondaryStartTime != nil {
					oldSecond, err := repo.GetAccountWindowStats(ctx, w.AccountID, *w.SecondaryStartTime)
					require.NoError(t, err)
					require.Equal(t, oldSecond, got[w.AccountID].Secondary, "all secondary fields")
				} else {
					require.Nil(t, got[w.AccountID].Secondary)
				}
			}
			if tc.name == "timezone_and_inclusive_bounds" {
				require.Equal(t, int64(3), got[1].Primary.Requests)
				require.Equal(t, int64(5), got[1].Secondary.Requests)
			}
		})
	}
	empty, err := repo.GetAccountWindowStatsForAccounts(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, empty)
}

func TestPerformanceOptimizationAccountWindowQueryCountsAndPlans(t *testing.T) {
	tx := performanceWindowFixture(t)
	ctx := context.Background()
	anchor := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	// 84,000 rows, 168 hourly samples per account, and production indexes.
	_, err := tx.ExecContext(ctx, `INSERT INTO usage_logs(user_id,api_key_id,account_id,request_id,model,created_at,input_tokens,output_tokens,cache_creation_tokens,cache_read_tokens,total_cost,actual_cost,account_stats_cost,account_rate_multiplier)
 SELECT 1,1,a,'perf-'||a||'-'||n,'fixture',$1::timestamptz-n*interval '1 hour',100,200,20,30,0.1234567891,0.1,
 CASE WHEN n%3=0 THEN NULL ELSE 0.2 END,CASE WHEN n%5=0 THEN NULL ELSE 1.2345 END
 FROM generate_series(1,500) a CROSS JOIN generate_series(0,167) n`, anchor)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "ANALYZE usage_logs")
	require.NoError(t, err)
	for _, size := range []int{20, 100, 500} {
		for _, pair := range []bool{false, true} {
			name := fmt.Sprintf("accounts_%d_pair_%t", size, pair)
			t.Run(name, func(t *testing.T) {
				recorder := &performanceWindowSQLRecorder{sqlExecutor: tx}
				repo := newUsageLogRepositoryWithSQL(nil, recorder)
				windows := make([]service.AccountWindowQuery, size)
				expected := make(map[int64]service.AccountWindowStats, size)
				for i := range windows {
					id := int64(i + 1)
					// Every account has its own reset times; both bounds vary.
					w := service.AccountWindowQuery{AccountID: id, StartTime: anchor.Add(-time.Duration(1+i%5) * time.Hour)}
					old, err := repo.GetAccountWindowStats(ctx, id, w.StartTime)
					require.NoError(t, err)
					expected[id] = service.AccountWindowStats{Primary: old}
					if pair {
						second := anchor.Add(-time.Duration(24+i%144) * time.Hour)
						w.SecondaryStartTime = &second
						oldSecond, err := repo.GetAccountWindowStats(ctx, id, second)
						require.NoError(t, err)
						expected[id] = service.AccountWindowStats{Primary: old, Secondary: oldSecond}
					}
					windows[i] = w
				}
				baseline := recorder.statements
				recorder.statements = nil
				got, err := repo.GetAccountWindowStatsForAccounts(ctx, windows)
				require.NoError(t, err)
				require.Equal(t, expected, got)
				candidate := recorder.statements
				baselineCount := size
				if pair {
					baselineCount *= 2
				}
				require.Len(t, baseline, baselineCount)
				require.Len(t, candidate, (size+127)/128)
				t.Logf("actual executed statistics SQL: baseline=%d candidate=%d accounts=%d windows_per_account=%d", len(baseline), len(candidate), size, baselineCount/size)
				performanceWindowPlans(t, tx, name, baseline, candidate)
			})
		}
	}
}

func performanceWindowPlans(t *testing.T, tx *sql.Tx, name string, baseline, candidate []performanceWindowSQL) {
	t.Helper()
	type summary struct {
		Queries     int     `json:"queries"`
		ExecutionMS float64 `json:"execution_ms"`
		PlanningMS  float64 `json:"planning_ms"`
		SharedHit   int64   `json:"shared_hit_blocks"`
		LocalHit    int64   `json:"local_hit_blocks"`
	}
	report := struct {
		Name                              string `json:"name"`
		Baseline, Candidate               []performanceWindowSQL
		BaselineSummary, CandidateSummary summary
	}{Name: name, Baseline: baseline, Candidate: candidate}
	explain := func(statements []performanceWindowSQL) summary {
		result := summary{Queries: len(statements)}
		for i := range statements {
			var plan string
			require.NoError(t, tx.QueryRowContext(context.Background(), "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statements[i].Query, statements[i].Args...).Scan(&plan))
			statements[i].Plan = json.RawMessage(plan)
			var data []struct {
				ExecutionMS float64 `json:"Execution Time"`
				PlanningMS  float64 `json:"Planning Time"`
				Plan        struct {
					SharedHit int64 `json:"Shared Hit Blocks"`
					LocalHit  int64 `json:"Local Hit Blocks"`
				}
			}
			require.NoError(t, json.Unmarshal([]byte(plan), &data))
			require.Len(t, data, 1)
			result.ExecutionMS += data[0].ExecutionMS
			result.PlanningMS += data[0].PlanningMS
			result.SharedHit += data[0].Plan.SharedHit
			result.LocalHit += data[0].Plan.LocalHit
		}
		return result
	}
	report.BaselineSummary = explain(report.Baseline)
	report.CandidateSummary = explain(report.Candidate)
	t.Logf("EXPLAIN totals baseline=%+v candidate=%+v", report.BaselineSummary, report.CandidateSummary)
	if dir := os.Getenv("PERF_RUN_DIR"); dir != "" {
		require.True(t, filepath.IsAbs(dir), "PERF_RUN_DIR must be absolute")
		data, err := json.MarshalIndent(report, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sql-windows-"+name+".json"), data, 0644))
	}
}
