package service

import (
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testHealthCfg() AccountHealthSettings {
	return AccountHealthSettings{
		Enabled: true, WindowMinutes: 10, MinSamples: 10,
		IsolateErrRate: 0.5, RecoverErrRate: 0.2,
		CooldownMinutes: 30, IntervalSeconds: 60,
	}
}

func TestDecideAccountHealth(t *testing.T) {
	now := time.Now()
	cfg := testHealthCfg()

	// 无样本：健康满分
	snap := decideAccountHealth(accountHealthRow{id: 1, name: "a", platform: "openai"}, cfg, now)
	require.Equal(t, 100, snap.Score)
	require.Equal(t, "healthy", snap.State)

	// 样本不足：不隔离
	snap = decideAccountHealth(accountHealthRow{id: 1, ok: 3, err: 3}, cfg, now)
	require.Equal(t, "healthy", snap.State)
	require.Equal(t, 50, snap.Score)

	// 错误率达标：隔离
	snap = decideAccountHealth(accountHealthRow{id: 1, ok: 4, err: 6}, cfg, now)
	require.Equal(t, "isolated", snap.State)
	require.Equal(t, 40, snap.Score)
	require.True(t, snap.Isolated)

	// 中间态：degraded
	snap = decideAccountHealth(accountHealthRow{id: 1, ok: 7, err: 3}, cfg, now)
	require.Equal(t, "degraded", snap.State)
	require.Equal(t, 70, snap.Score)

	// 已隔离但窗口无流量：保持 Isolated 标记（不误恢复）
	snap = decideAccountHealth(accountHealthRow{
		id: 1, reason: sql.NullString{String: "health:auto", Valid: true},
		until: sql.NullTime{Time: now.Add(time.Hour), Valid: true},
	}, cfg, now)
	require.True(t, snap.Isolated)
}

func TestAccountHealthRowPrefix(t *testing.T) {
	now := time.Now()
	health := accountHealthRow{
		reason: sql.NullString{String: "health:auto err_rate=80.0%", Valid: true},
		until:  sql.NullTime{Time: now.Add(time.Hour), Valid: true},
	}
	require.True(t, health.isHealthIsolated(now))

	other := accountHealthRow{
		reason: sql.NullString{String: "ratelimit:429", Valid: true},
		until:  sql.NullTime{Time: now.Add(time.Hour), Valid: true},
	}
	require.False(t, other.isHealthIsolated(now))

	expired := accountHealthRow{
		reason: sql.NullString{String: "health:auto", Valid: true},
		until:  sql.NullTime{Time: now.Add(-time.Hour), Valid: true},
	}
	require.False(t, expired.isHealthIsolated(now))
}

func TestNormalizeAccountHealthSettings(t *testing.T) {
	norm := normalizeAccountHealthSettings(AccountHealthSettings{})
	require.True(t, norm.WindowMinutes > 0)
	require.True(t, norm.IsolateErrRate > norm.RecoverErrRate)

	// 恢复阈值不得 >= 隔离阈值
	norm = normalizeAccountHealthSettings(AccountHealthSettings{RecoverErrRate: 0.9, IsolateErrRate: 0.5})
	require.True(t, norm.RecoverErrRate < norm.IsolateErrRate)

	require.Equal(t, "50.0%", formatRate(0.5))
}

func TestAccountHealthCapacityErrorPattern(t *testing.T) {
	re := accountHealthCapacityErrorRE
	require.True(t, re.MatchString("Upstream service temporarily unavailable"))
	require.True(t, re.MatchString("Upstream API request failed."))
	require.True(t, re.MatchString(`dial tcp 127.0.0.1:8992: connect: connection refused`))
	require.True(t, re.MatchString(`{"reason":"MODEL_TEMPORARILY_UNAVAILABLE"}`))
	require.True(t, re.MatchString("Upstream model is temporarily overloaded. Retry later."))
	require.True(t, re.MatchString("overloaded_error"))
	require.False(t, re.MatchString("invalid_request_error"))
	require.False(t, re.MatchString("Context window is full"))
}

func TestShouldSkipHealthIsolateLastLive(t *testing.T) {
	require.Equal(t, "", healthIsolateSkipReason(accountHealthRow{minGroupLive: 0}))
	require.Equal(t, "last_live_pool", healthIsolateSkipReason(accountHealthRow{minGroupLive: 1}))
	require.Equal(t, "", healthIsolateSkipReason(accountHealthRow{minGroupLive: 2}))
	require.Equal(t, "pool_mode", healthIsolateSkipReason(accountHealthRow{minGroupLive: 2, poolMode: true}))
	require.True(t, shouldSkipHealthIsolateLastLive(accountHealthRow{poolMode: true, minGroupLive: 5}))
	require.False(t, shouldSkipHealthIsolateLastLive(accountHealthRow{minGroupLive: 2}))

	now := time.Now()
	cfg := testHealthCfg()
	r := accountHealthRow{id: 22173, ok: 4, err: 8, minGroupLive: 1}
	snap := decideAccountHealth(r, cfg, now)
	require.Equal(t, "isolated", snap.State)
	require.True(t, shouldSkipHealthIsolateLastLive(r))
}
