package service

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

// 去重标记的保留期必须独立于 ops 数据的保留期：最长去重窗口是订阅到期提醒的
// 7 天（提醒键为 7d/3d/1d，不含日期），沿用 ops 现行的 3 天会在窗口内删掉标记
// 而导致重复发信。这里断言常量本身，避免日后被"统一保留期"的重构悄悄改小。
func TestNotificationDeliveryRetentionExceedsLongestDedupWindow(t *testing.T) {
	const longestDedupWindowDays = 7
	require.Greater(t, opsNotificationDeliveryRetentionDays, longestDedupWindowDays,
		"保留期必须长于最长去重窗口，否则会在去重窗口内删除标记导致重复发信")
	require.Equal(t, 30, opsNotificationDeliveryRetentionDays)
}

// 只删除 key 前缀匹配且 updated_at 早于 cutoff 的行。
// 两侧断言即校准：确认 SQL 同时带前缀过滤与时间过滤，
// 缺少前缀过滤会把真正的配置项一并删除。
func TestDeleteExpiredNotificationDeliveryMarkersFiltersByPrefixAndAge(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	// 第一批删除 2 行，第二批 0 行（循环终止条件）。
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM settings")).
		WithArgs(notificationEmailDeliveryKeyPrefix+"%", cutoff, opsCleanupBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM settings")).
		WithArgs(notificationEmailDeliveryKeyPrefix+"%", cutoff, opsCleanupBatchSize).
		WillReturnResult(sqlmock.NewResult(0, 0))

	got, err := deleteExpiredNotificationDeliveryMarkers(context.Background(), db, cutoff, opsCleanupBatchSize)
	require.NoError(t, err)
	require.Equal(t, int64(2), got)
	require.NoError(t, mock.ExpectationsWereMet())
}

// 表不存在时静默返回，与其他 ops 清理目标的行为一致，
// 不能让缺表把整个清理任务判为失败。
func TestDeleteExpiredNotificationDeliveryMarkersToleratesMissingTable(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM settings")).
		WillReturnError(errMissingRelationForTest{})

	got, err := deleteExpiredNotificationDeliveryMarkers(
		context.Background(), db, time.Now().UTC(), opsCleanupBatchSize)
	require.NoError(t, err)
	require.Equal(t, int64(0), got)
}

type errMissingRelationForTest struct{}

func (errMissingRelationForTest) Error() string {
	return `pq: relation "settings" does not exist`
}
