package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

// route_degraded is tri-state:
//   - NULL:  Tibo route selection did not apply (non Cookie-WS accounts, other
//     platforms, rows written before migration 261).
//   - FALSE: Tibo routing applied and a healthy/unknown route served the request.
//   - TRUE:  every route was degraded and the plain HTTP fallback served it.
type routeDegradedCase struct {
	name  string
	value *bool
	// want is the prepared insert arg; sql.NullBool.Value() yields nil/false/true.
	want sql.NullBool
}

func routeDegradedCases() []routeDegradedCase {
	healthy, degraded := false, true
	return []routeDegradedCase{
		{name: "nil_tibo_not_applied", value: nil, want: sql.NullBool{}},
		{name: "false_healthy_route", value: &healthy, want: sql.NullBool{Bool: false, Valid: true}},
		{name: "true_http_fallback", value: &degraded, want: sql.NullBool{Bool: true, Valid: true}},
	}
}

var (
	routeDegradedStaticInsertColumnsRe = regexp.MustCompile(`(?s)INSERT INTO usage_logs \((.*?)\) VALUES \(`)
	routeDegradedCTEInsertColumnsRe    = regexp.MustCompile(`(?s)INSERT INTO usage_logs \((.*?)\)\s*SELECT`)
	routeDegradedCTEInputColumnsRe     = regexp.MustCompile(`(?s)WITH input \((.*?)\) AS \(VALUES`)
	routeDegradedCTESelectColumnsRe    = regexp.MustCompile(`(?s)\)\s*SELECT(.*?)FROM input`)
)

func splitUsageLogColumnList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if col := strings.TrimSpace(part); col != "" {
			out = append(out, col)
		}
	}
	return out
}

// usageLogInsertColumnsFromSelect derives the canonical INSERT column order from
// usageLogSelectColumns ("id" + insert columns, see usageLogInsertArgTypes docs).
func usageLogInsertColumnsFromSelect(t *testing.T) []string {
	t.Helper()
	selectCols := splitUsageLogColumnList(usageLogSelectColumns)
	require.Equal(t, "id", selectCols[0])
	insertCols := selectCols[1:]
	require.Len(t, insertCols, len(usageLogInsertArgTypes),
		"usageLogSelectColumns minus id must mirror usageLogInsertArgTypes")
	return insertCols
}

func routeDegradedArgIndex(t *testing.T) int {
	t.Helper()
	cols := usageLogInsertColumnsFromSelect(t)
	for i, col := range cols {
		if col == "route_degraded" {
			return i
		}
	}
	t.Fatalf("route_degraded missing from usageLogSelectColumns")
	return -1
}

func newRouteDegradedUsageLog(value *bool) *service.UsageLog {
	return &service.UsageLog{
		UserID:        1,
		APIKeyID:      2,
		AccountID:     3,
		RequestID:     "req-route-degraded",
		Model:         "gpt-5.4",
		InputTokens:   10,
		OutputTokens:  5,
		RouteDegraded: value,
		CreatedAt:     time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
}

func TestPrepareUsageLogInsert_RouteDegradedTriState(t *testing.T) {
	idx := routeDegradedArgIndex(t)
	cols := usageLogInsertColumnsFromSelect(t)
	require.Equal(t, "upstream_model_mismatch", cols[idx-1], "route_degraded follows upstream_model_mismatch")
	require.Equal(t, "boolean", usageLogInsertArgTypes[idx-1], "upstream_model_mismatch arg type")
	require.Equal(t, "boolean", usageLogInsertArgTypes[idx], "route_degraded arg type")
	require.Equal(t, "bigint", usageLogInsertArgTypes[idx+1], "group_id arg type")

	for _, tc := range routeDegradedCases() {
		t.Run(tc.name, func(t *testing.T) {
			prepared := prepareUsageLogInsert(newRouteDegradedUsageLog(tc.value))
			require.Len(t, prepared.args, len(usageLogInsertArgTypes))
			require.Equal(t, tc.want, prepared.args[idx])
		})
	}
}

// newRouteDegradedCapturingSQLMock records every statement sent to sqlmock while
// still letting WithArgs assert exact argument values.
func newRouteDegradedCapturingSQLMock(t *testing.T, captured *[]string) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	matcher := sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
		*captured = append(*captured, actualSQL)
		return nil
	})
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(matcher))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db, mock
}

func routeDegradedExpectedArgs(idx int, want sql.NullBool) []driver.Value {
	args := make([]driver.Value, len(usageLogInsertArgTypes))
	for i := range args {
		args[i] = sqlmock.AnyArg()
	}
	args[idx] = want
	return args
}

// TestUsageLogStaticInserts_PassRouteDegraded covers the two hand-written INSERTs
// (createSingle via Create, execUsageLogInsertNoResult): the exact tri-state value
// is bound at the route_degraded column position.
func TestUsageLogStaticInserts_PassRouteDegraded(t *testing.T) {
	idx := routeDegradedArgIndex(t)
	wantCols := usageLogInsertColumnsFromSelect(t)

	for _, tc := range routeDegradedCases() {
		t.Run(tc.name, func(t *testing.T) {
			var captured []string
			db, mock := newRouteDegradedCapturingSQLMock(t, &captured)
			repo := &usageLogRepository{sql: db}
			log := newRouteDegradedUsageLog(tc.value)

			mock.ExpectQuery("INSERT INTO usage_logs").
				WithArgs(routeDegradedExpectedArgs(idx, tc.want)...).
				WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), log.CreatedAt))
			inserted, err := repo.Create(context.Background(), log)
			require.NoError(t, err)
			require.True(t, inserted)

			mock.ExpectExec("INSERT INTO usage_logs").
				WithArgs(routeDegradedExpectedArgs(idx, tc.want)...).
				WillReturnResult(sqlmock.NewResult(0, 1))
			require.NoError(t, execUsageLogInsertNoResult(context.Background(), db, prepareUsageLogInsert(newRouteDegradedUsageLog(tc.value))))

			require.NoError(t, mock.ExpectationsWereMet())
			require.Len(t, captured, 2)
			for _, query := range captured {
				m := routeDegradedStaticInsertColumnsRe.FindStringSubmatch(query)
				require.Len(t, m, 2, "unrecognised INSERT shape:\n%s", query)
				require.Equal(t, wantCols, splitUsageLogColumnList(m[1]))
			}
		})
	}
}

// TestUsageLogBatchInserts_AlignRouteDegraded covers the CTE-based batch and
// best-effort builders: every column list carries route_degraded in the same
// position as its bound argument.
func TestUsageLogBatchInserts_AlignRouteDegraded(t *testing.T) {
	idx := routeDegradedArgIndex(t)
	wantCols := usageLogInsertColumnsFromSelect(t)

	for _, tc := range routeDegradedCases() {
		t.Run(tc.name, func(t *testing.T) {
			log := newRouteDegradedUsageLog(tc.value)
			prepared := prepareUsageLogInsert(log)

			key := usageLogBatchKey(log.RequestID, log.APIKeyID)
			batchQuery, batchArgs := buildUsageLogBatchInsertQuery([]string{key}, map[string]usageLogInsertPrepared{key: prepared})
			require.Len(t, batchArgs, len(usageLogInsertArgTypes)+1)
			// batch args prepend the synthetic input_idx.
			require.Equal(t, tc.want, batchArgs[idx+1])
			requireRouteDegradedCTEColumns(t, batchQuery, append([]string{"input_idx"}, wantCols...), wantCols)

			bestEffortQuery, bestEffortArgs := buildUsageLogBestEffortInsertQuery([]usageLogInsertPrepared{prepared})
			require.Len(t, bestEffortArgs, len(usageLogInsertArgTypes))
			require.Equal(t, tc.want, bestEffortArgs[idx])
			requireRouteDegradedCTEColumns(t, bestEffortQuery, wantCols, wantCols)
		})
	}
}

func requireRouteDegradedCTEColumns(t *testing.T, query string, wantInputCols, wantCols []string) {
	t.Helper()
	input := routeDegradedCTEInputColumnsRe.FindStringSubmatch(query)
	require.Len(t, input, 2, "missing WITH input (...) column list")
	require.Equal(t, wantInputCols, splitUsageLogColumnList(input[1]))

	insert := routeDegradedCTEInsertColumnsRe.FindStringSubmatch(query)
	require.Len(t, insert, 2, "missing INSERT INTO usage_logs (...) SELECT column list")
	require.Equal(t, wantCols, splitUsageLogColumnList(insert[1]))

	selectFromInput := routeDegradedCTESelectColumnsRe.FindStringSubmatch(query)
	require.Len(t, selectFromInput, 2, "missing SELECT ... FROM input column list")
	require.Equal(t, wantCols, splitUsageLogColumnList(selectFromInput[1]))
}

// routeDegradedScannerStub fills scan destinations by usageLogSelectColumns name,
// using zero values for everything not explicitly provided.
type routeDegradedScannerStub struct {
	values map[string]any
}

func (s routeDegradedScannerStub) Scan(dest ...any) error {
	cols := splitUsageLogColumnList(usageLogSelectColumns)
	if len(dest) != len(cols) {
		return fmt.Errorf("scan arg count mismatch: got %d want %d", len(dest), len(cols))
	}
	for i, col := range cols {
		target := reflect.ValueOf(dest[i])
		if target.Kind() != reflect.Pointer {
			return fmt.Errorf("dest[%d] (%s) is not a pointer", i, col)
		}
		value, ok := s.values[col]
		if !ok {
			target.Elem().Set(reflect.Zero(target.Elem().Type()))
			continue
		}
		target.Elem().Set(reflect.ValueOf(value))
	}
	return nil
}

func TestScanUsageLog_RouteDegradedTriState(t *testing.T) {
	for _, tc := range routeDegradedCases() {
		t.Run(tc.name, func(t *testing.T) {
			log, err := scanUsageLog(routeDegradedScannerStub{values: map[string]any{
				"id":             int64(9),
				"model":          "gpt-5.4",
				"route_degraded": tc.want,
				// A neighbouring bool column with a different value catches a swapped
				// scan order between upstream_model_mismatch and route_degraded.
				"upstream_model_mismatch": sql.NullBool{Bool: true, Valid: true},
			}})
			require.NoError(t, err)
			require.Equal(t, tc.value, log.RouteDegraded)
			require.NotNil(t, log.UpstreamModelMismatch)
			require.True(t, *log.UpstreamModelMismatch)
		})
	}
}

func TestUsageLogRepositoryListWithFiltersRouteDegraded(t *testing.T) {
	start := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		degraded bool
		cond     string
	}{
		{name: "true_keeps_http_fallback_rows", degraded: true, cond: "route_degraded IS TRUE"},
		{name: "false_keeps_healthy_route_rows", degraded: false, cond: "route_degraded IS FALSE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newSQLMock(t)
			repo := &usageLogRepository{sql: db}
			degraded := tc.degraded

			// Fast pagination path (no user/api key/account filter): no COUNT query.
			mock.ExpectQuery("SELECT .* FROM usage_logs WHERE "+regexp.QuoteMeta(tc.cond)+" ORDER BY id DESC LIMIT \\$1 OFFSET \\$2").
				WithArgs(21, 0).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			logs, page, err := repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 20},
				usagestats.UsageLogFilters{RouteDegraded: &degraded})
			require.NoError(t, err)
			require.Empty(t, logs)
			require.NotNil(t, page)

			// Combined with placeholder filters: the condition takes no bind arg and
			// the following placeholders keep their positions.
			where := "WHERE user_id = \\$1 AND " + regexp.QuoteMeta(tc.cond) + " AND created_at >= \\$2"
			mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM usage_logs "+where+"$").
				WithArgs(int64(7), start).
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
			mock.ExpectQuery("SELECT .* FROM usage_logs "+where+" ORDER BY id DESC LIMIT \\$3 OFFSET \\$4").
				WithArgs(int64(7), start, 20, 0).
				WillReturnRows(sqlmock.NewRows([]string{"id"}))
			logs, page, err = repo.ListWithFilters(context.Background(), pagination.PaginationParams{Page: 1, PageSize: 20},
				usagestats.UsageLogFilters{UserID: 7, RouteDegraded: &degraded, StartTime: &start})
			require.NoError(t, err)
			require.Empty(t, logs)
			require.NotNil(t, page)

			require.NoError(t, mock.ExpectationsWereMet())
		})
	}
}

// The usage page's stats cards apply the same tri-state filter as the list.
func TestUsageLogRepositoryGetStatsWithFiltersRouteDegraded(t *testing.T) {
	for _, degraded := range []bool{true, false} {
		db, mock := newSQLMock(t)
		repo := &usageLogRepository{sql: db}
		cond := "route_degraded IS FALSE"
		if degraded {
			cond = "route_degraded IS TRUE"
		}
		mock.ExpectQuery("(?s)FROM usage_logs\\s+WHERE model = \\$1 AND " + regexp.QuoteMeta(cond) + ".*GROUP BY GROUPING SETS").
			WithArgs("gpt-6-astra").
			WillReturnRows(sqlmock.NewRows([]string{
				"inbound_grouped", "upstream_grouped", "inbound_endpoint", "upstream_endpoint",
				"requests", "input_tokens", "output_tokens", "cache_creation_tokens", "cache_read_tokens",
				"cost", "actual_cost", "account_cost", "avg_duration_ms",
			}).AddRow(1, 1, nil, nil, int64(3), int64(2), int64(3), int64(1), int64(3), 1.2, 1.0, 1.2, 20.0))
		stats, err := repo.GetStatsWithFilters(context.Background(), usagestats.UsageLogFilters{Model: "gpt-6-astra", RouteDegraded: &degraded})
		require.NoError(t, err)
		require.Equal(t, int64(3), stats.TotalRequests)
		require.NoError(t, mock.ExpectationsWereMet())
	}
}

func TestUsageLogRouteDegradedMigrationsRegistered(t *testing.T) {
	const columnMigration = "261_add_usage_log_route_degraded.sql"
	require.Less(t, columnMigration, usageLogsRouteDegradedIndexMigration, "column must exist before its index is built")

	columnSQL, err := migrations.FS.ReadFile(columnMigration)
	require.NoError(t, err)
	nonTx, err := validateMigrationExecutionMode(columnMigration, string(columnSQL))
	require.NoError(t, err)
	require.False(t, nonTx, "ADD COLUMN runs in a transaction")

	indexSQL, err := migrations.FS.ReadFile(usageLogsRouteDegradedIndexMigration)
	require.NoError(t, err)
	nonTx, err = validateMigrationExecutionMode(usageLogsRouteDegradedIndexMigration, string(indexSQL))
	require.NoError(t, err)
	require.True(t, nonTx, "CREATE INDEX CONCURRENTLY must run outside a transaction")
	require.Contains(t, string(indexSQL), usageLogsRouteDegradedIndex)
	require.Contains(t, string(indexSQL), "WHERE route_degraded IS TRUE")
}
