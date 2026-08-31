package repository

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

// 断言插入语句真的为量表列传了"有效的 0"，而不只是辅助函数本身行为正确。
//
// 单测辅助函数不足以保护这个修复：把调用点换回 opsNullInt 仍能编译、
// 函数级测试也仍会通过，缺陷会悄悄回归。因此这里直接检查落到驱动的实参。
//
// 列顺序取自 INSERT 语句：db_conn_waiting 是 $38，concurrency_queue_depth 是 $40。
// 若日后列顺序变动，本测试会失败——这正是期望的行为。
func TestInsertSystemMetricsPersistsZeroGauges(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := &opsRepository{db: db}

	zero := 0
	goroutines := 140

	const totalArgs = 40
	expected := make([]driver.Value, totalArgs)
	for i := range expected {
		expected[i] = anyArgMatcher{}
	}
	// $38 = db_conn_waiting，$40 = concurrency_queue_depth（1 基 → 索引 37 / 39）
	expected[37] = validZeroMatcher{}
	expected[39] = validZeroMatcher{}

	mock.ExpectExec("INSERT INTO ops_system_metrics").
		WithArgs(expected...).
		WillReturnResult(sqlmock.NewResult(1, 1))

	err = repo.InsertSystemMetrics(context.Background(), &service.OpsInsertSystemMetricsInput{
		CreatedAt:             time.Now().UTC(),
		WindowMinutes:         1,
		GoroutineCount:        &goroutines,
		DBConnWaiting:         &zero,
		ConcurrencyQueueDepth: &zero,
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

type anyArgMatcher struct{}

func (anyArgMatcher) Match(driver.Value) bool { return true }

// validZeroMatcher 只接受"有效且为 0"的值，
// 从而把 NULL（Valid=false）与观测到的 0 区分开。
type validZeroMatcher struct{}

func (validZeroMatcher) Match(v driver.Value) bool {
	switch n := v.(type) {
	case sql.NullInt64:
		return n.Valid && n.Int64 == 0
	case int64:
		return n == 0
	default:
		return false
	}
}
