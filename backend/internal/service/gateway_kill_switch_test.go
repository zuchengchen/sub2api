package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGatewayKillSwitchRegistry_CancelIsolatesUsers(t *testing.T) {
	reg := NewGatewayKillSwitchRegistry()

	ctxA1, cancelA1 := context.WithCancel(context.Background())
	defer cancelA1()
	ctxA2, cancelA2 := context.WithCancel(context.Background())
	defer cancelA2()
	ctxB1, cancelB1 := context.WithCancel(context.Background())
	defer cancelB1()

	reg.Register(7, "req-a1", cancelA1)
	reg.Register(7, "req-a2", cancelA2)
	reg.Register(8, "req-b1", cancelB1)
	require.Equal(t, 2, reg.CountForUser(7))
	require.Equal(t, 1, reg.CountForUser(8))

	killed := reg.CancelAllForUser(7)
	require.Equal(t, 2, killed)
	require.Equal(t, 0, reg.CountForUser(7))
	require.Equal(t, context.Canceled, ctxA1.Err())
	require.Equal(t, context.Canceled, ctxA2.Err())
	require.NoError(t, ctxB1.Err(), "其他用户的在途请求不得被取消")
	require.Equal(t, 0, reg.CancelAllForUser(7), "取消后重复调用应为空")

	// Unregister 只移除登记，不触发取消。
	reg.Register(9, "req-c1", func() {})
	reg.Unregister(9, "req-c1")
	require.Equal(t, 0, reg.CountForUser(9))
}

func TestGatewayKillSwitchRegistry_NilAndInvalidInputSafe(t *testing.T) {
	var reg *GatewayKillSwitchRegistry
	require.NotPanics(t, func() { reg.Register(1, "x", func() {}) })
	require.NotPanics(t, func() { reg.Unregister(1, "x") })
	require.Equal(t, 0, reg.CancelAllForUser(1))
	require.Equal(t, 0, reg.CountForUser(1))

	real := NewGatewayKillSwitchRegistry()
	require.NotPanics(t, func() {
		real.Register(0, "x", func() {}) // 非法 userID
		real.Register(1, "", nil)        // 空 requestID / nil cancel
	})
	require.Equal(t, 0, real.CountForUser(1))
}
