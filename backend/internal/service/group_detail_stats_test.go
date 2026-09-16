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

type readOnlyGroupStatsRepo struct {
	UsageLogRepository
}

func (readOnlyGroupStatsRepo) GetGroupDetailStats(_ context.Context, id int64, from, to *time.Time) (*GroupDetailStats, error) {
	return &GroupDetailStats{
		GroupID:          id,
		TotalCost:        32,
		TotalActualCost:  16,
		TotalAccountCost: 13,
		BalanceCost:      9,
		SubscriptionCost: 7,
		From:             from,
		To:               to,
	}, nil
}

func TestGroupStatisticsReadOnlyShape(t *testing.T) {
	stats, err := NewDashboardService(readOnlyGroupStatsRepo{}, nil, nil, nil).GetGroupDetailStats(context.Background(), 7, nil, nil)
	require.NoError(t, err)
	require.Equal(t, int64(7), stats.GroupID)
	require.Equal(t, 32.0, stats.TotalCost)
	require.Equal(t, 16.0, stats.TotalActualCost)
	require.Equal(t, 13.0, stats.TotalAccountCost)
	require.Equal(t, 9.0, stats.BalanceCost)
	require.Equal(t, 7.0, stats.SubscriptionCost)
}
