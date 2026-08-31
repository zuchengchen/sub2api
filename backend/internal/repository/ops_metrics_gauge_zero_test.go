package repository

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
)

// 量表型指标（队列深度、DB 连接等待数）必须区分"确实为 0"与"无数据"。
//
// 这两列曾长期不可见：opsNullInt 把 0 折叠成 NULL，导致
// db_conn_waiting 在 1154 行样本中全为 NULL，
// concurrency_queue_depth 只在非零的 7 次出现过值——
// 于是"无积压"（健康）与"采集失败"（无数据）在库里无法区分，
// 依赖队列深度的告警规则也永远无法评估。
//
// 这里对比两个辅助函数在 0 上的行为差异，锁定量表列必须用哪一个。
func TestGaugeMetricsPreserveObservedZero(t *testing.T) {
	zero := 0

	// 量表列使用的函数：0 必须落库为有效的 0。
	gauge := opsNullableIntPointer(&zero).(sql.NullInt64)
	require.True(t, gauge.Valid, "0 是有效观测，不能写成 NULL")
	require.Zero(t, gauge.Int64)

	// 对照：通用函数会把 0 折叠成 NULL。
	// 该行为对延迟分位数等"无请求即无数据"的列是合理的，
	// 因此不修改通用函数本身，只为量表列换用上面的函数。
	folded := opsNullInt(&zero).(sql.NullInt64)
	require.False(t, folded.Valid, "通用函数的既有行为应保持不变")

	// 两者对 nil 的处理必须一致：缺失值都是 NULL。
	require.False(t, opsNullableIntPointer(nil).(sql.NullInt64).Valid)
	require.False(t, opsNullInt(nil).(sql.NullInt64).Valid)

	// 非零值两者都应保留。
	depth := 7
	require.EqualValues(t, 7, opsNullableIntPointer(&depth).(sql.NullInt64).Int64)
	require.EqualValues(t, 7, opsNullInt(&depth).(sql.NullInt64).Int64)
}
