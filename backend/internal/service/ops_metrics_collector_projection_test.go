package service

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// opsMetricsWaitSumCache 只实现采集队列深度所需的那一个方法。
// 其余方法留给嵌入的 nil 接口：一旦采集路径回退到遍历账号的老实现，
// 就会 panic 而非静默退化，从而暴露回归。
type opsMetricsWaitSumCache struct {
	ConcurrencyCache
	sum   int
	err   error
	calls int
}

func (c *opsMetricsWaitSumCache) SumActiveAccountWaitingCounts(context.Context) (int, error) {
	c.calls++
	if c.err != nil {
		return 0, c.err
	}
	return c.sum, nil
}

func newWaitSumCollector(cache ConcurrencyCache) *OpsMetricsCollector {
	concurrency := NewConcurrencyService(cache)
	return &OpsMetricsCollector{concurrencyService: concurrency}
}

// 队列深度改走活跃索引汇总：采集器只发一次调用，直接采用其结果。
// 旧实现遍历全部可调度账号（每账号 5 条命令，约 2.2 万条），在预算内必然超时，
// 使该指标长期为 NULL、依赖它的告警规则永不触发。
func TestCollectConcurrencyQueueDepthUsesActiveIndexSum(t *testing.T) {
	cache := &opsMetricsWaitSumCache{sum: 5}
	collector := newWaitSumCollector(cache)

	depth := collector.collectConcurrencyQueueDepth(context.Background())

	require.NotNil(t, depth)
	require.Equal(t, 5, *depth)
	require.Equal(t, 1, cache.calls, "每次采集只应调用一次汇总")
}

// 0 与采集失败必须区分：
//   - 无积压 → &0，指标写 0（有效观测）
//   - 采集失败 → nil，指标写 NULL（无数据）
//
// 把失败写成 0 会让"无数据"伪装成"健康"，这正是此前该指标长期为 NULL
// 却无人察觉的反面：两种状态一旦混淆，指标就失去意义。
func TestCollectConcurrencyQueueDepthDistinguishesZeroFromFailure(t *testing.T) {
	t.Run("无积压返回 0 而非 nil", func(t *testing.T) {
		cache := &opsMetricsWaitSumCache{sum: 0}
		depth := newWaitSumCollector(cache).collectConcurrencyQueueDepth(context.Background())

		require.NotNil(t, depth, "无积压是有效观测，必须写 0 而非 NULL")
		require.Equal(t, 0, *depth)
	})

	t.Run("采集失败返回 nil", func(t *testing.T) {
		cache := &opsMetricsWaitSumCache{err: errors.New("redis unavailable")}
		depth := newWaitSumCollector(cache).collectConcurrencyQueueDepth(context.Background())

		require.Nil(t, depth, "采集失败必须写 NULL，不得伪装成 0")
		require.Equal(t, 1, cache.calls)
	})
}

// 并发服务缺失时不得 panic，只返回 nil（该指标不可用），
// 采集器的其他指标照常写入。
func TestCollectConcurrencyQueueDepthWithoutConcurrencyService(t *testing.T) {
	collector := &OpsMetricsCollector{}
	require.Nil(t, collector.collectConcurrencyQueueDepth(context.Background()))
}

func BenchmarkOpsMetricsCollectorCollectConcurrencyQueueDepth(b *testing.B) {
	collector := newWaitSumCollector(&opsMetricsWaitSumCache{sum: 0})

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if depth := collector.collectConcurrencyQueueDepth(context.Background()); depth == nil || *depth != 0 {
			b.Fatalf("unexpected queue depth: %v", depth)
		}
	}
}
