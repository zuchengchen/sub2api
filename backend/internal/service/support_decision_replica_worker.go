package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

func NewSupportDecisionReplicaWorker(reader *SupportDecisionAtomicReader) (*workerruntime.PeriodicJob, error) {
	if reader == nil {
		return nil, fmt.Errorf("support decision reader is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "scheduler-support-replica",
			Kind:             workerruntime.KindPeriodic,
			Group:            "scheduler",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Refreshes the in-process support decision replica",
			Tags:             []string{"scheduler", "support-decision"},
		},
		Interval:       time.Second,
		Timeout:        5 * time.Second,
		RunImmediately: true,
		Run: func(context.Context) error {
			reader.Install(BuildSupportDecisionTable(time.Now()))
			return nil
		},
	})
}
