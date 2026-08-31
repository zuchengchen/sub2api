package repository

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// SumActiveAccountWaitingCounts 走活跃索引而非遍历全部可调度账号：
// 旧实现对每个账号发 5 条命令（约 2.2 万条），在预算内必然超时，
// 导致 concurrency_queue_depth 长期为 NULL、依赖它的告警规则永不触发。
//
// 这里覆盖三类成员——有等待数、无等待数、score 已过期——
// 断言只有"未过期且有等待数"的成员参与求和。
func TestSumActiveAccountWaitingCountsUsesActiveIndex(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	cache := NewConcurrencyCache(rdb, 5, 300)

	now, err := rdb.Time(ctx).Result()
	require.NoError(t, err)
	future := now.Add(5 * time.Minute).Unix()
	past := now.Add(-5 * time.Minute).Unix()

	// 未过期 + 有等待数 → 计入
	require.NoError(t, rdb.ZAdd(ctx, accountActiveIndexKey,
		redis.Z{Score: float64(future), Member: "101"}).Err())
	require.NoError(t, rdb.Set(ctx, accountWaitKey(101), "3", 0).Err())

	require.NoError(t, rdb.ZAdd(ctx, accountActiveIndexKey,
		redis.Z{Score: float64(future), Member: "102"}).Err())
	require.NoError(t, rdb.Set(ctx, accountWaitKey(102), "4", 0).Err())

	// 未过期 + 无等待计数键（已随 TTL 失效）→ 贡献 0，且不得报错
	require.NoError(t, rdb.ZAdd(ctx, accountActiveIndexKey,
		redis.Z{Score: float64(future), Member: "103"}).Err())

	// score 已过期 → 跳过，即使等待计数键仍残留
	require.NoError(t, rdb.ZAdd(ctx, accountActiveIndexKey,
		redis.Z{Score: float64(past), Member: "104"}).Err())
	require.NoError(t, rdb.Set(ctx, accountWaitKey(104), "99", 0).Err())

	// 不在索引内但有等待计数键 → 不应被读到（索引是唯一入口）
	require.NoError(t, rdb.Set(ctx, accountWaitKey(105), "50", 0).Err())

	got, err := cache.SumActiveAccountWaitingCounts(ctx)
	require.NoError(t, err)
	require.Equal(t, 7, got, "只应累加 101(3) 与 102(4)")
}

// 无积压时必须返回 0 而非报错：0 与采集失败（返回 error → 指标写 NULL）
// 是不同语义，把无积压写成 NULL 会让"健康"伪装成"无数据"，反之亦然。
func TestSumActiveAccountWaitingCountsEmptyIndexReturnsZero(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	cache := NewConcurrencyCache(rdb, 5, 300)

	got, err := cache.SumActiveAccountWaitingCounts(context.Background())
	require.NoError(t, err)
	require.Equal(t, 0, got)
}

// 索引里的非法 member 不得中断求和：跳过该项，其余照常累加。
func TestSumActiveAccountWaitingCountsSkipsMalformedMembers(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	defer func() { _ = rdb.Close() }()

	ctx := context.Background()
	cache := NewConcurrencyCache(rdb, 5, 300)

	now, err := rdb.Time(ctx).Result()
	require.NoError(t, err)
	future := float64(now.Add(5 * time.Minute).Unix())

	require.NoError(t, rdb.ZAdd(ctx, accountActiveIndexKey,
		redis.Z{Score: future, Member: "not-a-number"}).Err())
	require.NoError(t, rdb.ZAdd(ctx, accountActiveIndexKey,
		redis.Z{Score: future, Member: "0"}).Err())
	require.NoError(t, rdb.ZAdd(ctx, accountActiveIndexKey,
		redis.Z{Score: future, Member: strconv.Itoa(201)}).Err())
	require.NoError(t, rdb.Set(ctx, accountWaitKey(201), "6", 0).Err())

	got, err := cache.SumActiveAccountWaitingCounts(ctx)
	require.NoError(t, err)
	require.Equal(t, 6, got)
}
