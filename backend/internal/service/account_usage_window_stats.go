package service

import (
	"context"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"golang.org/x/sync/errgroup"
)

// AccountWindowStatsBatchSize bounds query parameters, result size and failure
// recovery work. Each account may request two windows sharing the same scan.
const AccountWindowStatsBatchSize = 128

// AccountWindowQuery describes inclusive lower bounds, with no upper bound,
// matching GetAccountWindowStats. SecondaryStartTime is optional.
type AccountWindowQuery struct {
	AccountID          int64
	StartTime          time.Time
	SecondaryStartTime *time.Time
}

type AccountWindowStats struct {
	Primary   *usagestats.AccountStats
	Secondary *usagestats.AccountStats
}

type accountWindowStatsReader interface {
	GetAccountWindowStatsForAccounts(context.Context, []AccountWindowQuery) (map[int64]AccountWindowStats, error)
}

// GetAccountWindowStatsForAccounts reads independent account windows without
// changing the Anthropic usage cache or the list's refresh timing. A nil result
// for a window means unknown; only a successful query may produce zero stats.
func (s *AccountUsageService) GetAccountWindowStatsForAccounts(ctx context.Context, windows []AccountWindowQuery) map[int64]AccountWindowStats {
	result := make(map[int64]AccountWindowStats, len(windows))
	if s.usageLogRepo == nil {
		return result
	}
	reader, canBatch := s.usageLogRepo.(accountWindowStatsReader)
	for offset := 0; offset < len(windows); offset += AccountWindowStatsBatchSize {
		batch := windows[offset:min(offset+AccountWindowStatsBatchSize, len(windows))]
		if canBatch {
			if stats, err := reader.GetAccountWindowStatsForAccounts(ctx, batch); err == nil {
				for id, value := range stats {
					result[id] = value
				}
				continue
			}
		}
		// Retain partial success if one account/window cannot be read. This is
		// also the compatibility path for repositories without the batch reader.
		var workers errgroup.Group
		workers.SetLimit(6)
		var mu sync.Mutex
		for _, window := range batch {
			if ctx.Err() != nil {
				break
			}
			workers.Go(func() error {
				if ctx.Err() != nil {
					return nil
				}
				var stats AccountWindowStats
				if value, err := s.usageLogRepo.GetAccountWindowStats(ctx, window.AccountID, window.StartTime); err == nil {
					stats.Primary = value
				}
				if window.SecondaryStartTime != nil && ctx.Err() == nil {
					if value, err := s.usageLogRepo.GetAccountWindowStats(ctx, window.AccountID, *window.SecondaryStartTime); err == nil {
						stats.Secondary = value
					}
				}
				mu.Lock()
				result[window.AccountID] = stats
				mu.Unlock()
				return nil
			})
		}
		_ = workers.Wait()
		if ctx.Err() != nil {
			return result
		}
	}
	return result
}

type openAIUsageWindows struct {
	accountID int64
	usage     *UsageInfo
	now       time.Time
}

func (s *AccountUsageService) addOpenAIWindowStats(ctx context.Context, pending []openAIUsageWindows) {
	windows := make([]AccountWindowQuery, 0, len(pending))
	for _, item := range pending {
		sevenDayStart := codexWindowStatsStart(item.usage.SevenDay, 7*24*time.Hour, item.now)
		windows = append(windows, AccountWindowQuery{
			AccountID: item.accountID, StartTime: codexWindowStatsStart(item.usage.FiveHour, 5*time.Hour, item.now),
			SecondaryStartTime: &sevenDayStart,
		})
	}
	stats := s.GetAccountWindowStatsForAccounts(ctx, windows)
	for _, item := range pending {
		value := stats[item.accountID]
		if value.Primary != nil {
			if item.usage.FiveHour == nil {
				item.usage.FiveHour = &UsageProgress{Utilization: 0}
			}
			item.usage.FiveHour.WindowStats = windowStatsFromAccountStats(value.Primary)
		}
		if value.Secondary != nil {
			if item.usage.SevenDay == nil {
				item.usage.SevenDay = &UsageProgress{Utilization: 0}
			}
			item.usage.SevenDay.WindowStats = windowStatsFromAccountStats(value.Secondary)
		}
	}
}
