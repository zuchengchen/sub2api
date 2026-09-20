package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type busyThenOKRunner struct {
	calls int
}

func (r *busyThenOKRunner) RunIntelligentTest(_ context.Context, rec *IntelligentTestRecord) error {
	r.calls++
	if r.calls == 1 {
		return &TestAdmissionWaitError{Until: time.Now().Add(15 * time.Millisecond), Reason: "rpm"}
	}
	rec.Result = "<svg viewBox=\"0 0 10 10\"></svg>"
	return nil
}

type alwaysBusyRunner struct{}

func (alwaysBusyRunner) RunIntelligentTest(_ context.Context, _ *IntelligentTestRecord) error {
	return &TestAdmissionWaitError{Until: time.Now().Add(time.Hour), Reason: "rpm"}
}

func TestIntelligentTestRunRetriesBusyWithoutRequeue(t *testing.T) {
	t.Parallel()
	runner := &busyThenOKRunner{}
	svc := &IntelligentTestService{runner: runner, evaluators: DefaultIntelligentTestEvaluators()}
	rec := &IntelligentTestRecord{
		Status: "running",
		ConfigSnapshot: &IntelligentTestConfig{
			Prompt:         "x",
			Evaluator:      "svg_structure",
			TimeoutSeconds: 2,
		},
	}
	svc.run(context.Background(), rec)
	require.NotEqual(t, "queued", rec.Status)
	require.Equal(t, 2, runner.calls)
}

func TestIntelligentTestRunBusyTimeoutIsRateLimitedNotQueued(t *testing.T) {
	t.Parallel()
	svc := &IntelligentTestService{runner: alwaysBusyRunner{}, evaluators: DefaultIntelligentTestEvaluators()}
	rec := &IntelligentTestRecord{
		Status: "running",
		ConfigSnapshot: &IntelligentTestConfig{
			Prompt:         "x",
			Evaluator:      "svg_structure",
			TimeoutSeconds: 1,
		},
	}
	svc.run(context.Background(), rec)
	require.Equal(t, "rate_limited", rec.Status)
	require.NotEqual(t, "queued", rec.Status)
	require.Contains(t, rec.ErrorMessage, "账号繁忙")
}

func TestIntelligentTestStartEnqueuedClaimsQueuedRecords(t *testing.T) {
	t.Parallel()
	repo := &startEnqueuedRepo{
		byID: map[int64]*IntelligentTestRecord{
			7: {ID: 7, Status: "queued", LeaseToken: "tok", ConfigSnapshot: &IntelligentTestConfig{TimeoutSeconds: 30, Evaluator: "svg_structure"}},
		},
	}
	svc := &IntelligentTestService{repo: repo, runner: &busyThenOKRunner{}, evaluators: DefaultIntelligentTestEvaluators()}
	out := &IntelligentTestEnqueued{Records: []*IntelligentTestRecord{{ID: 7, Status: "queued"}}}
	svc.startEnqueued(context.Background(), out)
	require.Equal(t, []int64{7}, repo.claimed)
	require.Equal(t, "running", out.Records[0].Status)
	require.Nil(t, out.Records[0].ConfigSnapshot)
}

type startEnqueuedRepo struct {
	IntelligentTestRepository
	byID    map[int64]*IntelligentTestRecord
	claimed []int64
}

func (r *startEnqueuedRepo) ClaimID(_ context.Context, id int64) (*IntelligentTestRecord, error) {
	r.claimed = append(r.claimed, id)
	rec := r.byID[id]
	if rec == nil {
		return nil, nil
	}
	out := *rec
	out.Status = "running"
	if rec.ConfigSnapshot != nil {
		cfg := *rec.ConfigSnapshot
		out.ConfigSnapshot = &cfg
	}
	return &out, nil
}

func (r *startEnqueuedRepo) Finish(context.Context, *IntelligentTestRecord) error { return nil }
