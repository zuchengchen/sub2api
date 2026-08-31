package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 采集器停摆时 GetLatestSystemMetrics 会永久返回同一行冻结数据。
// 若不设陈旧性判定，CPU/内存/队列深度规则会基于几小时前的值反复越限并触发告警。
//
// 阈值取采集器最小间隔的 3 倍而非 2 倍：60s 节拍下 2 倍（120s）会被正常抖动突破，
// 导致持续计数被反复清零、规则 3-6 永不发火。两侧断言即校准。
func TestSystemMetricsStalenessThreshold(t *testing.T) {
	require.Equal(t, 3*opsMetricsCollectorMinInterval, opsSystemMetricsStaleAfter)
	require.Equal(t, 180*time.Second, opsSystemMetricsStaleAfter)

	// 必须严格长于采集间隔，否则每个样本在下一次采集前就会过期。
	require.Greater(t, opsSystemMetricsStaleAfter, opsMetricsCollectorMinInterval,
		"阈值不得短于采集间隔，否则正常样本会被误判为陈旧")
}

// 陈旧性判定的边界行为：新鲜样本保留、超期样本置 nil。
// computeRuleMetric 对 nil 快照返回 ok=false，评估器随后 resetRuleState，
// 因此"置 nil"等价于"该指标不可用"，而非"值为 0"。
func TestStaleSystemMetricsAreTreatedAsUnavailable(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	cpu := 99.0

	cases := []struct {
		name        string
		age         time.Duration
		wantDiscard bool
	}{
		{"刚采集", 0, false},
		{"一个采集周期内", 59 * time.Second, false},
		{"两倍间隔——正常抖动范围，必须保留", 121 * time.Second, false},
		{"恰好等于阈值", opsSystemMetricsStaleAfter, false},
		{"刚超过阈值", opsSystemMetricsStaleAfter + time.Second, true},
		{"采集器停摆一小时", time.Hour, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &OpsSystemMetricsSnapshot{
				CreatedAt:       now.Add(-tc.age),
				CPUUsagePercent: &cpu,
			}
			require.Equal(t, tc.wantDiscard, isOpsSystemMetricsStale(snapshot, now))
		})
	}

	// nil 快照本身就代表"无数据"，不应被再判为陈旧（否则语义重复且掩盖来源）。
	require.False(t, isOpsSystemMetricsStale(nil, now))
}
