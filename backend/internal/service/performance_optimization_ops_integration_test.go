//go:build integration

package service

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

const performanceOldUsageCounts = `SELECT COALESCE(COUNT(*),0), COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens),0) FROM usage_logs WHERE created_at >= $1 AND created_at < $2`
const performanceOldDuration = `SELECT percentile_cont(0.50) WITHIN GROUP (ORDER BY duration_ms), percentile_cont(0.90) WITHIN GROUP (ORDER BY duration_ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY duration_ms), percentile_cont(0.99) WITHIN GROUP (ORDER BY duration_ms), AVG(duration_ms), MAX(duration_ms) FROM usage_logs WHERE created_at >= $1 AND created_at < $2 AND duration_ms IS NOT NULL`
const performanceOldTTFT = `SELECT percentile_cont(0.50) WITHIN GROUP (ORDER BY first_token_ms), percentile_cont(0.90) WITHIN GROUP (ORDER BY first_token_ms), percentile_cont(0.95) WITHIN GROUP (ORDER BY first_token_ms), percentile_cont(0.99) WITHIN GROUP (ORDER BY first_token_ms), AVG(first_token_ms), MAX(first_token_ms) FROM usage_logs WHERE created_at >= $1 AND created_at < $2 AND first_token_ms IS NOT NULL`

type performanceOpsStatement struct {
	Query string          `json:"query"`
	Args  []any           `json:"args"`
	Plan  json.RawMessage `json:"plan,omitempty"`
}

type performanceOpsConnector struct {
	driver.Connector
	statements []performanceOpsStatement
}
type performanceOpsConn struct {
	driver.Conn
	recorder *performanceOpsConnector
}

func (c *performanceOpsConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return &performanceOpsConn{Conn: conn, recorder: c}, nil
}
func (c *performanceOpsConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	values := make([]any, len(args))
	for i, arg := range args {
		values[i] = arg.Value
	}
	c.recorder.statements = append(c.recorder.statements, performanceOpsStatement{Query: query, Args: values})
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
}

func performanceOpsDatabase(t *testing.T) (*sql.DB, *performanceOpsConnector) {
	t.Helper()
	ctx := context.Background()
	image := "postgres:18.1-alpine3.23"
	if override := strings.TrimSpace(os.Getenv("SUB2API_TEST_POSTGRES_IMAGE")); override != "" {
		image = override
	}
	// Deliberately no external DSN or skip path: required integration checks
	// create their own database and fail if Docker cannot supply it.
	pg, err := tcpostgres.Run(ctx, image, tcpostgres.WithDatabase("performance_ops"), tcpostgres.WithUsername("postgres"), tcpostgres.WithPassword("postgres"), tcpostgres.BasicWaitStrategies())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, pg.Terminate(context.Background())) })
	dsn, err := pg.ConnectionString(ctx, "sslmode=disable", "TimeZone=UTC")
	require.NoError(t, err)
	connector, err := pq.NewConnector(dsn)
	require.NoError(t, err)
	recorder := &performanceOpsConnector{Connector: connector}
	db := sql.OpenDB(recorder)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(ctx))
	var version string
	require.NoError(t, db.QueryRowContext(ctx, "SELECT version()").Scan(&version))
	t.Logf("isolated PostgreSQL: %s image=%s", version, image)
	_, err = db.ExecContext(ctx, `CREATE TABLE usage_logs (created_at timestamptz NOT NULL,input_tokens integer,output_tokens integer,cache_creation_tokens integer,cache_read_tokens integer,duration_ms integer,first_token_ms integer)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `CREATE INDEX idx_perf_usage_created_at ON usage_logs(created_at)`)
	require.NoError(t, err)
	return db, recorder
}

func performanceOldPercentiles(t *testing.T, db *sql.DB, query string, start, end time.Time) opsCollectedPercentiles {
	t.Helper()
	var p50, p90, p95, p99, avg sql.NullFloat64
	var max sql.NullInt64
	require.NoError(t, db.QueryRowContext(context.Background(), query, start, end).Scan(&p50, &p90, &p95, &p99, &avg, &max))
	// Independent copy of the previous conversion, not the candidate helper.
	convert := func(v sql.NullFloat64) *int {
		if !v.Valid {
			return nil
		}
		n := int(math.Round(v.Float64))
		return &n
	}
	out := opsCollectedPercentiles{p50: convert(p50), p90: convert(p90), p95: convert(p95), p99: convert(p99)}
	if avg.Valid {
		v := math.Round(avg.Float64*10) / 10
		out.avg = &v
	}
	if max.Valid {
		v := int(max.Int64)
		out.max = &v
	}
	return out
}

func TestPerformanceOptimizationOpsMinuteSQL(t *testing.T) {
	db, recorder := performanceOpsDatabase(t)
	collector := &OpsMetricsCollector{db: db}
	start := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	end := start.Add(time.Minute)
	cases := []struct {
		name           string
		duration, ttft []any
	}{
		{"empty", nil, nil},
		{"all_null", []any{nil, nil, nil}, []any{nil, nil, nil}},
		{"mixed_null_odd", []any{nil, -5, 0, 1, 8, nil, 2147483647}, []any{10, nil, 0, 101, nil, 999, 2147483647}},
		{"even_fractional_rounding", []any{0, 1, 2, 3}, []any{-2, -1, 0, 1}},
		{"independent_nulls", []any{nil, nil, nil}, []any{3, 10, 20}},
		{"single_negative", []any{-3}, []any{-1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec("TRUNCATE usage_logs")
			require.NoError(t, err)
			for i, duration := range tc.duration {
				_, err = db.Exec(`INSERT INTO usage_logs VALUES($1,$2,2,3,4,$3,$4)`, start.Add(time.Duration(i)*time.Microsecond), 1000000000, duration, tc.ttft[i])
				require.NoError(t, err)
			}
			// Half-open minute window: exactly start is in; end and adjacent rows out.
			for _, outside := range []time.Time{start.Add(-time.Microsecond), end, end.Add(time.Microsecond)} {
				_, err = db.Exec(`INSERT INTO usage_logs VALUES($1,2000000000,0,0,0,2147483647,2147483647)`, outside)
				require.NoError(t, err)
			}
			recorder.statements = nil
			var oldCount, oldTokens int64
			require.NoError(t, db.QueryRow(performanceOldUsageCounts, start, end).Scan(&oldCount, &oldTokens))
			oldDuration := performanceOldPercentiles(t, db, performanceOldDuration, start, end)
			oldTTFT := performanceOldPercentiles(t, db, performanceOldTTFT, start, end)
			baseline := recorder.statements
			recorder.statements = nil
			count, tokens, duration, ttft, err := collector.queryUsageMetrics(context.Background(), start, end)
			require.NoError(t, err)
			require.Equal(t, int64(len(tc.duration)), count)
			require.Equal(t, oldCount, count)
			require.Equal(t, oldTokens, tokens)
			require.Equal(t, oldDuration, duration, "p50/p90/p95/p99/avg/max including NULL and rounding")
			require.Equal(t, oldTTFT, ttft)
			candidate := recorder.statements
			require.Len(t, baseline, 3)
			require.Len(t, candidate, 1)
			results, marshalErr := json.Marshal(map[string]any{"count": count, "tokens": tokens, "duration": performanceOpsResult(duration), "ttft": performanceOpsResult(ttft)})
			require.NoError(t, marshalErr)
			t.Logf("equal old/new fields: %s", results)
			t.Logf("actual executed usage SQL baseline=%d candidate=%d count=%d tokens=%d duration=%+v ttft=%+v", len(baseline), len(candidate), count, tokens, duration, ttft)
		})
	}
	t.Run("query_error_is_not_successful_zero", func(t *testing.T) {
		_, err := db.Exec("ALTER TABLE usage_logs RENAME TO unavailable_usage_logs")
		require.NoError(t, err)
		_, _, _, _, err = collector.queryUsageMetrics(context.Background(), start, end)
		require.Error(t, err)
		_, err = db.Exec("ALTER TABLE unavailable_usage_logs RENAME TO usage_logs")
		require.NoError(t, err)
	})
	t.Run("plans_6000_rows", func(t *testing.T) {
		_, err := db.Exec(`TRUNCATE usage_logs;`)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO usage_logs SELECT $1::timestamptz+n*interval '10 milliseconds',100,200,30,40,CASE WHEN n%5=0 THEN NULL ELSE n*11 END,CASE WHEN n%3=0 THEN NULL ELSE n END FROM generate_series(0,5999) n`, start)
		require.NoError(t, err)
		_, err = db.Exec("ANALYZE usage_logs")
		require.NoError(t, err)
		recorder.statements = nil
		var oldCount, oldTokens int64
		require.NoError(t, db.QueryRow(performanceOldUsageCounts, start, end).Scan(&oldCount, &oldTokens))
		oldDuration := performanceOldPercentiles(t, db, performanceOldDuration, start, end)
		oldTTFT := performanceOldPercentiles(t, db, performanceOldTTFT, start, end)
		baseline := recorder.statements
		recorder.statements = nil
		count, tokens, duration, ttft, err := collector.queryUsageMetrics(context.Background(), start, end)
		require.NoError(t, err)
		candidate := recorder.statements
		require.Equal(t, oldCount, count)
		require.Equal(t, oldTokens, tokens)
		require.Equal(t, oldDuration, duration)
		require.Equal(t, oldTTFT, ttft)
		require.Len(t, baseline, 3)
		require.Len(t, candidate, 1)
		performanceOpsPlans(t, db, baseline, candidate)
	})
}

func performanceOpsPlans(t *testing.T, db *sql.DB, baseline, candidate []performanceOpsStatement) {
	t.Helper()
	report := map[string][]performanceOpsStatement{"baseline": baseline, "candidate": candidate}
	for _, name := range []string{"baseline", "candidate"} {
		statements := report[name]
		var elapsed float64
		var hits int64
		for i := range statements {
			var plan string
			require.NoError(t, db.QueryRow("EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+statements[i].Query, statements[i].Args...).Scan(&plan))
			statements[i].Plan = json.RawMessage(plan)
			var data []struct {
				ExecutionMS float64 `json:"Execution Time"`
				Plan        struct {
					SharedHit int64 `json:"Shared Hit Blocks"`
				}
			}
			require.NoError(t, json.Unmarshal([]byte(plan), &data))
			require.Len(t, data, 1)
			elapsed += data[0].ExecutionMS
			hits += data[0].Plan.SharedHit
		}
		t.Logf("Ops EXPLAIN %s: statements=%d execution_ms=%.3f shared_hit_blocks=%d", name, len(statements), elapsed, hits)
	}
	if dir := os.Getenv("PERF_RUN_DIR"); dir != "" {
		require.True(t, filepath.IsAbs(dir), "PERF_RUN_DIR must be absolute")
		data, err := json.MarshalIndent(report, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.MkdirAll(dir, 0755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "sql-ops-plans.json"), data, 0644))
	}
}

var _ driver.QueryerContext = (*performanceOpsConn)(nil)
var _ driver.Connector = (*performanceOpsConnector)(nil)

func performanceOpsResult(p opsCollectedPercentiles) map[string]any {
	return map[string]any{"p50": p.p50, "p90": p.p90, "p95": p.p95, "p99": p.p99, "avg": p.avg, "max": p.max}
}
