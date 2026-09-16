package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestGroupDetailStatsRejectsInvertedRange(t *testing.T) {
	from := time.Now()
	to := from.Add(-time.Hour)
	_, err := (*DashboardService)(nil).GetGroupDetailStats(context.Background(), 1, &from, &to)
	require.Error(t, err)
}

func TestGroupStatisticsReadOnlyShape(t *testing.T) {
	stats := GroupDetailStats{TotalCost: 10, TotalActualCost: 4, TotalAccountCost: 3, BalanceCost: 2, SubscriptionCost: 2}
	require.Equal(t, 4.0, stats.TotalActualCost)
	require.Equal(t, 3.0, stats.TotalAccountCost)
	require.Equal(t, 2.0, stats.BalanceCost)
	require.Equal(t, 2.0, stats.SubscriptionCost)
}
