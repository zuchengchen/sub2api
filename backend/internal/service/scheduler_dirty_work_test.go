//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSchedulerDirtyWorkApplyResultKeepsUnknownKinds(t *testing.T) {
	svc := &SchedulerSnapshotService{}
	results := svc.ApplyDirtyWorkBatch(context.Background(), []SchedulerDirtyWork{
		{Kind: 99, EntityID: 1, Generation: 1},
	})
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	require.Equal(t, int16(99), results[0].Work.Kind)
}

func TestSchedulerDirtyWorkAccountRefreshWithNilRepoIsNoop(t *testing.T) {
	svc := &SchedulerSnapshotService{}
	results := svc.ApplyDirtyWorkBatch(context.Background(), []SchedulerDirtyWork{
		{Kind: SchedulerDirtyWorkAccount, EntityID: 42, Generation: 1},
		{Kind: SchedulerDirtyWorkGroup, EntityID: 0, Generation: 1},
	})
	require.Len(t, results, 2)
	require.NoError(t, results[0].Err)
	require.NoError(t, results[1].Err)
}

type dirtyWorkRepoStub struct {
	promoted     int
	work         []SchedulerDirtyWork
	acknowledged []SchedulerDirtyWork
	failures     []SchedulerDirtyWork
}

func (r *dirtyWorkRepoStub) Promote(context.Context, SchedulerOwnership, int) (int, error) {
	n := r.promoted
	r.promoted = 0
	return n, nil
}
func (r *dirtyWorkRepoStub) RequestFullRebuild(context.Context) error { return nil }
func (r *dirtyWorkRepoStub) List(context.Context, int) ([]SchedulerDirtyWork, error) {
	return r.work, nil
}
func (r *dirtyWorkRepoStub) RecordFailure(_ context.Context, _ SchedulerOwnership, work SchedulerDirtyWork, _ error) (bool, error) {
	r.failures = append(r.failures, work)
	return true, nil
}
func (r *dirtyWorkRepoStub) Acknowledge(_ context.Context, _ SchedulerOwnership, work SchedulerDirtyWork) (bool, error) {
	r.acknowledged = append(r.acknowledged, work)
	return true, nil
}
func (r *dirtyWorkRepoStub) PendingStats(context.Context) (SchedulerDirtyWorkStats, error) {
	return SchedulerDirtyWorkStats{}, nil
}

type dirtyWorkProcessorStub struct {
	applied []SchedulerDirtyWork
}

func (p *dirtyWorkProcessorStub) ApplyDirtyWorkBatch(_ context.Context, batch []SchedulerDirtyWork) []SchedulerDirtyWorkApplyResult {
	p.applied = append([]SchedulerDirtyWork(nil), batch...)
	results := make([]SchedulerDirtyWorkApplyResult, 0, len(batch))
	for _, work := range batch {
		results = append(results, SchedulerDirtyWorkApplyResult{Work: work})
	}
	return results
}

func TestSchedulerDirtyWorkProcessPromotesAppliesAndAcknowledges(t *testing.T) {
	item := SchedulerDirtyWork{Kind: SchedulerDirtyWorkAccount, EntityID: 42, Generation: 7}
	repo := &dirtyWorkRepoStub{promoted: 1, work: []SchedulerDirtyWork{item}}
	processor := &dirtyWorkProcessorStub{}
	require.NoError(t, ProcessSchedulerDirtyWork(context.Background(), repo, nil, processor))
	require.Equal(t, []SchedulerDirtyWork{item}, processor.applied)
	require.Equal(t, []SchedulerDirtyWork{item}, repo.acknowledged)
	require.Empty(t, repo.failures)
}
