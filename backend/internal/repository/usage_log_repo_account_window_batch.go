package repository

import (
	"context"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

// GetAccountWindowStatsForAccounts batches independently anchored account
// windows. The lateral aggregate scans each account from its earliest requested
// start once, including when the primary window is older than the secondary.
func (r *usageLogRepository) GetAccountWindowStatsForAccounts(ctx context.Context, windows []service.AccountWindowQuery) (map[int64]service.AccountWindowStats, error) {
	result := make(map[int64]service.AccountWindowStats, len(windows))
	for offset := 0; offset < len(windows); offset += service.AccountWindowStatsBatchSize {
		batch := windows[offset:min(offset+service.AccountWindowStatsBatchSize, len(windows))]
		query, args := accountWindowStatsQuery(batch)
		rows, err := r.sql.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			primary, secondary := &usagestats.AccountStats{}, &usagestats.AccountStats{}
			if err := rows.Scan(&id,
				&primary.Requests, &primary.Tokens, &primary.Cost, &primary.StandardCost, &primary.UserCost,
				&secondary.Requests, &secondary.Tokens, &secondary.Cost, &secondary.StandardCost, &secondary.UserCost,
			); err != nil {
				_ = rows.Close()
				return nil, err
			}
			result[id] = service.AccountWindowStats{Primary: primary, Secondary: secondary}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		for _, window := range batch {
			if window.SecondaryStartTime == nil {
				stats := result[window.AccountID]
				stats.Secondary = nil
				result[window.AccountID] = stats
			}
		}
	}
	return result, nil
}

func accountWindowStatsQuery(windows []service.AccountWindowQuery) (string, []any) {
	values := make([]string, 0, len(windows))
	args := make([]any, 0, len(windows)*3)
	for _, window := range windows {
		n := len(args)
		values = append(values, fmt.Sprintf("($%d::bigint, $%d::timestamptz, $%d::timestamptz)", n+1, n+2, n+3))
		args = append(args, window.AccountID, window.StartTime, window.SecondaryStartTime)
	}
	query := `SELECT w.account_id, stats.*
FROM (VALUES ` + strings.Join(values, ",") + `) AS w(account_id, start_time, secondary_start_time)
CROSS JOIN LATERAL (
  SELECT
    COUNT(*) FILTER (WHERE created_at >= w.start_time),
    COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens) FILTER (WHERE created_at >= w.start_time), 0),
    COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)) FILTER (WHERE created_at >= w.start_time), 0),
    COALESCE(SUM(total_cost) FILTER (WHERE created_at >= w.start_time), 0),
    COALESCE(SUM(actual_cost) FILTER (WHERE created_at >= w.start_time), 0),
    COUNT(*) FILTER (WHERE created_at >= w.secondary_start_time),
    COALESCE(SUM(input_tokens + output_tokens + cache_creation_tokens + cache_read_tokens) FILTER (WHERE created_at >= w.secondary_start_time), 0),
    COALESCE(SUM(COALESCE(account_stats_cost, total_cost) * COALESCE(account_rate_multiplier, 1)) FILTER (WHERE created_at >= w.secondary_start_time), 0),
    COALESCE(SUM(total_cost) FILTER (WHERE created_at >= w.secondary_start_time), 0),
    COALESCE(SUM(actual_cost) FILTER (WHERE created_at >= w.secondary_start_time), 0)
  FROM usage_logs
  WHERE account_id = w.account_id
    AND created_at >= LEAST(w.start_time, w.secondary_start_time)
) AS stats`
	return query, args
}
