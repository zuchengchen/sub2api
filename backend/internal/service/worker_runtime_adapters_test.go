//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
	"github.com/stretchr/testify/require"
)

func TestWorkerRuntimeAccountExpiryAdapterLifecycle(t *testing.T) {
	svc := NewAccountExpiryService(nil, time.Second)
	worker, err := NewAccountExpiryWorker(svc)
	require.NoError(t, err)
	require.Equal(t, "account-expiry", worker.Descriptor().Name)
	require.Equal(t, workerruntime.KindPeriodic, worker.Descriptor().Kind)

	require.NoError(t, worker.Start(context.Background()))
	require.Equal(t, workerruntime.LifecycleRunning, worker.Snapshot().Lifecycle.State)
	require.NoError(t, worker.Stop(context.Background()))
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
}

func TestWorkerRuntimeIdempotencyCleanupAdapterLifecycle(t *testing.T) {
	svc := NewIdempotencyCleanupService(nil, nil)
	worker, err := NewIdempotencyCleanupWorker(svc)
	require.NoError(t, err)
	require.Equal(t, "idempotency-cleanup", worker.Descriptor().Name)
	require.NoError(t, worker.Start(context.Background()))
	require.NoError(t, worker.Stop(context.Background()))
}

func TestWorkerRuntimeSchedulerDirtyWorkAdapterLifecycle(t *testing.T) {
	item := SchedulerDirtyWork{Kind: SchedulerDirtyWorkAccount, EntityID: 9, Generation: 1}
	repo := &dirtyWorkRepoStub{work: []SchedulerDirtyWork{item}}
	processor := &dirtyWorkProcessorStub{}
	ownership := dirtyWorkOwnershipRepoStub{}
	worker, err := NewSchedulerDirtyWorkWorker(repo, ownership, processor)
	require.NoError(t, err)
	require.Equal(t, "scheduler-dirty-work", worker.Descriptor().Name)
	require.Equal(t, workerruntime.KindPeriodic, worker.Descriptor().Kind)
	require.NoError(t, worker.Start(context.Background()))
	require.Eventually(t, func() bool { return len(processor.applied) > 0 }, time.Second, 10*time.Millisecond)
	require.NoError(t, worker.Stop(context.Background()))
	require.Equal(t, workerruntime.LifecycleStopped, worker.Snapshot().Lifecycle.State)
}

type dirtyWorkOwnershipRepoStub struct{}

func (dirtyWorkOwnershipRepoStub) TryAcquire(context.Context) (SchedulerOwnership, bool, error) {
	return dirtyWorkOwnershipStub{}, true, nil
}

type dirtyWorkOwnershipStub struct{}

func (dirtyWorkOwnershipStub) Epoch() int64             { return 1 }
func (dirtyWorkOwnershipStub) Context() context.Context { return context.Background() }
func (dirtyWorkOwnershipStub) Lost() <-chan struct{}    { return make(chan struct{}) }
func (dirtyWorkOwnershipStub) Err() error               { return nil }
func (dirtyWorkOwnershipStub) Close() error             { return nil }

func TestWorkerRuntimeUsageRecordPoolAdapterLifecycle(t *testing.T) {
	pool := NewUsageRecordWorkerPoolWithOptions(UsageRecordWorkerPoolOptions{WorkerCount: 1, QueueSize: 1})
	worker := NewUsageRecordWorkerPoolWorker(pool)
	require.Equal(t, "usage-record-pool", worker.Descriptor().Name)
	require.Equal(t, workerruntime.KindPool, worker.Descriptor().Kind)
	require.NoError(t, worker.Start(context.Background()))
	require.NoError(t, worker.Stop(context.Background()))
}
