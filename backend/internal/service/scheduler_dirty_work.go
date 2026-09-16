package service

import (
	"context"
	"errors"
	"time"
)

var (
	errDirtyWorkResultCount     = errors.New("snapshot processor returned invalid result count")
	errDirtyWorkIdentityChanged = errors.New("snapshot processor changed dirty work identity")
)

const (
	SchedulerDirtyWorkAccount int16 = 1
	SchedulerDirtyWorkGroup   int16 = 2
	SchedulerDirtyWorkGlobal  int16 = 3
)

type SchedulerDirtyWork struct {
	Kind           int16
	EntityID       int64
	Generation     int64
	RebuildBuckets bool
	UpdatedAt      time.Time
	FailureCount   int
	LastFailureAt  *time.Time
	LastError      string
	RetryAt        time.Time
}

type SchedulerDirtyWorkStats struct {
	Count           int64
	OldestUpdatedAt *time.Time
	FailedCount     int64
	OldestFailureAt *time.Time
}

type SchedulerOwnership interface {
	Epoch() int64
	Context() context.Context
	Lost() <-chan struct{}
	Err() error
	Close() error
}

type SchedulerOwnershipRepository interface {
	TryAcquire(ctx context.Context) (SchedulerOwnership, bool, error)
}

type SchedulerDirtyWorkRepository interface {
	Promote(ctx context.Context, ownership SchedulerOwnership, limit int) (int, error)
	RequestFullRebuild(ctx context.Context) error
	List(ctx context.Context, limit int) ([]SchedulerDirtyWork, error)
	RecordFailure(ctx context.Context, ownership SchedulerOwnership, work SchedulerDirtyWork, failure error) (bool, error)
	Acknowledge(ctx context.Context, ownership SchedulerOwnership, work SchedulerDirtyWork) (bool, error)
	PendingStats(ctx context.Context) (SchedulerDirtyWorkStats, error)
}

type SchedulerDirtyWorkApplyResult struct {
	Work SchedulerDirtyWork
	Err  error
}

const schedulerDirtyWorkBatchSize = 100

// SchedulerSnapshotDirtyProcessor applies one incremental dirty-work batch.
type SchedulerSnapshotDirtyProcessor interface {
	ApplyDirtyWorkBatch(ctx context.Context, batch []SchedulerDirtyWork) []SchedulerDirtyWorkApplyResult
}

func ProvideSchedulerSnapshotDirtyProcessor(svc *SchedulerSnapshotService) SchedulerSnapshotDirtyProcessor {
	return svc
}

// ProcessSchedulerDirtyWork promotes source evidence, applies one canonical
// batch, and acknowledges successful items. It is the default-on consume path.
func ProcessSchedulerDirtyWork(ctx context.Context, dirty SchedulerDirtyWorkRepository, ownership SchedulerOwnership, processor SchedulerSnapshotDirtyProcessor) error {
	if dirty == nil || processor == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for range schedulerDirtyWorkBatchSize {
		n, err := dirty.Promote(ctx, ownership, schedulerDirtyWorkBatchSize)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	work, err := dirty.List(ctx, schedulerDirtyWorkBatchSize)
	if err != nil {
		return err
	}
	if len(work) == 0 {
		return nil
	}
	results := processor.ApplyDirtyWorkBatch(ctx, work)
	for i, item := range work {
		if err := ctx.Err(); err != nil {
			return err
		}
		if i >= len(results) {
			_, _ = dirty.RecordFailure(ctx, ownership, item, errDirtyWorkResultCount)
			continue
		}
		if results[i].Work.Kind != item.Kind || results[i].Work.EntityID != item.EntityID || results[i].Work.Generation != item.Generation {
			_, _ = dirty.RecordFailure(ctx, ownership, item, errDirtyWorkIdentityChanged)
			continue
		}
		if results[i].Err != nil {
			if _, err := dirty.RecordFailure(ctx, ownership, item, results[i].Err); err != nil {
				return err
			}
			continue
		}
		if _, err := dirty.Acknowledge(ctx, ownership, item); err != nil {
			return err
		}
	}
	return nil
}

func (s *SchedulerSnapshotService) ApplyDirtyWorkBatch(ctx context.Context, batch []SchedulerDirtyWork) []SchedulerDirtyWorkApplyResult {
	results := make([]SchedulerDirtyWorkApplyResult, 0, len(batch))
	for _, work := range batch {
		results = append(results, SchedulerDirtyWorkApplyResult{Work: work, Err: s.applyDirtyWork(ctx, work)})
	}
	return results
}

func (s *SchedulerSnapshotService) applyDirtyWork(ctx context.Context, work SchedulerDirtyWork) error {
	if s == nil {
		return nil
	}
	switch work.Kind {
	case SchedulerDirtyWorkAccount:
		if work.EntityID <= 0 {
			return nil
		}
		return s.refreshDirtyAccount(ctx, work.EntityID)
	case SchedulerDirtyWorkGroup:
		if s.cache == nil {
			return nil
		}
		return nil
	case SchedulerDirtyWorkGlobal:
		return s.triggerFullRebuild("dirty-work")
	default:
		return nil
	}
}

func (s *SchedulerSnapshotService) refreshDirtyAccount(ctx context.Context, accountID int64) error {
	if s == nil || s.accountRepo == nil {
		return nil
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		if s.cache != nil {
			_ = s.cache.DeleteAccount(ctx, accountID)
		}
		return nil
	}
	if s.cache != nil && account != nil {
		_ = s.cache.SetAccount(ctx, account)
	}
	return nil
}
