package service

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/workerruntime"
)

const (
	claudeOAuthSessionCleanupInterval = 5 * time.Minute
	claudeOAuthSessionCleanupTimeout  = 5 * time.Second
	openAIOAuthCleanupInterval        = 5 * time.Minute
	openAIOAuthCleanupTimeout         = 5 * time.Second
	grokOAuthSessionCleanupInterval   = 5 * time.Minute
	grokOAuthSessionCleanupTimeout    = 5 * time.Second
	concurrencySlotCleanupWorkerTimeout = 6 * time.Second
)

const paymentOrderExpiryWorkerTimeout = paymentOrderExpiryLockAcquireTimeout + 2*expiryCheckTimeout + time.Second

const userMessageQueueCleanupWorkerTimeout = 10*time.Second + 1000*2*time.Second + time.Second

// NewOpenAIOAuthSessionCleanupWorker adapts pending-session cleanup to the worker runtime.
func NewOpenAIOAuthSessionCleanupWorker(svc *OpenAIOAuthService) (*workerruntime.PeriodicJob, error) {
	if svc == nil || svc.sessionStore == nil {
		return nil, fmt.Errorf("OpenAI OAuth service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "openai-oauth-session-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "auth",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Removes expired OpenAI OAuth authorization sessions",
			Tags:             []string{"oauth", "openai", "session-cleanup"},
		},
		Interval:       openAIOAuthCleanupInterval,
		Timeout:        openAIOAuthCleanupTimeout,
		RunImmediately: false,
		Run:            svc.CleanupSessions,
	})
}

// NewClaudeOAuthSessionCleanupWorker adapts Claude OAuth session cleanup to the worker runtime.
func NewClaudeOAuthSessionCleanupWorker(svc *OAuthService) (*workerruntime.PeriodicJob, error) {
	if svc == nil || svc.sessionStore == nil {
		return nil, fmt.Errorf("Claude OAuth service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "claude-oauth-session-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "auth",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Removes expired Claude OAuth authorization sessions",
			Tags:             []string{"oauth", "claude", "session-cleanup"},
		},
		Interval:       claudeOAuthSessionCleanupInterval,
		Timeout:        claudeOAuthSessionCleanupTimeout,
		RunImmediately: false,
		Run:            svc.CleanupSessions,
	})
}

// NewGrokOAuthSessionCleanupWorker adapts Grok OAuth session cleanup to the worker runtime.
func NewGrokOAuthSessionCleanupWorker(svc *GrokOAuthService) (*workerruntime.PeriodicJob, error) {
	if svc == nil || svc.sessionStore == nil {
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "grok-oauth-session-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "auth",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Removes expired Grok OAuth authorization sessions",
			Tags:             []string{"oauth", "grok", "session-cleanup"},
		},
		Interval:       grokOAuthSessionCleanupInterval,
		Timeout:        grokOAuthSessionCleanupTimeout,
		RunImmediately: false,
		Run:            svc.CleanupSessions,
	})
}

// NewConcurrencySlotCleanupWorker adapts expired account-slot cleanup to the worker runtime.
func NewConcurrencySlotCleanupWorker(svc *ConcurrencyService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("concurrency service is required")
	}
	if !svc.CleanupEnabled() {
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "concurrency-slot-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Removes expired account concurrency slots",
			Tags:             []string{"concurrency", "account-slots", "cleanup"},
		},
		Interval:       svc.CleanupInterval(),
		Timeout:        concurrencySlotCleanupWorkerTimeout,
		RunImmediately: true,
		Run:            svc.RunSlotCleanup,
	})
}

// NewAccountExpiryWorker adapts account expiry maintenance to the worker runtime.
func NewAccountExpiryWorker(svc *AccountExpiryService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("account expiry service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "account-expiry",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval:       svc.Interval(),
		Timeout:        5 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

// NewSubscriptionExpiryWorker adapts subscription expiry maintenance to the worker runtime.
func NewSubscriptionExpiryWorker(svc *SubscriptionExpiryService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("subscription expiry service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "subscription-expiry",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Periodically expires subscriptions and sends configured reminders",
			Tags:             []string{"subscriptions", "expiry", "reminders"},
		},
		Interval:       svc.Interval(),
		Timeout:        10 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

// NewPaymentOrderExpiryWorker adapts payment-order expiry maintenance to the worker runtime.
func NewPaymentOrderExpiryWorker(svc *PaymentOrderExpiryService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("payment order expiry service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "payment-order-expiry",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationSingletonRun,
			Description:      "Reconciles and expires timed-out payment orders",
			Tags:             []string{"payments", "expiry", "reconciliation"},
		},
		Interval:       svc.Interval(),
		Timeout:        paymentOrderExpiryWorkerTimeout,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

// NewTokenRefreshWorker adapts configured OAuth token refresh checks to the worker runtime.
func NewTokenRefreshWorker(svc *TokenRefreshService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("token refresh service is required")
	}
	if !svc.Enabled() {
		slog.Info("token_refresh.service_disabled")
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "token-refresh",
			Kind:             workerruntime.KindPeriodic,
			Group:            "auth",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Refreshes eligible OAuth tokens before expiry",
			Tags:             []string{"oauth", "token-refresh"},
		},
		Interval: svc.Interval(),
		Timeout:  100 * 365 * 24 * time.Hour,
		RunImmediately: true,
		Run:            svc.Run,
		OnStart: func() {
			slog.Info("token_refresh.service_started",
				"check_interval_minutes", svc.cfg.CheckIntervalMinutes,
				"refresh_before_expiry_hours", svc.cfg.RefreshBeforeExpiryHours,
			)
		},
		OnStop: func() {
			slog.Info("token_refresh.service_stopped")
		},
	})
}

// NewUserMessageQueueCleanupWorker adapts orphan-lock cleanup to the worker runtime.
func NewUserMessageQueueCleanupWorker(svc *UserMessageQueueService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("user message queue service is required")
	}
	if !svc.CleanupEnabled() {
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "user-message-queue-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Releases orphaned user-message queue locks",
			Tags:             []string{"user-message-queue", "orphan-lock-cleanup"},
		},
		Interval:       svc.CleanupInterval(),
		Timeout:        userMessageQueueCleanupWorkerTimeout,
		RunImmediately: false,
		Run:            svc.RunCleanup,
	})
}

// NewIdempotencyCleanupWorker adapts idempotency cleanup maintenance to the worker runtime.
func NewOutboxCleanupWorker(svc *OutboxCleanupService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("outbox cleanup service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "outbox-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationSingletonRun,
		},
		Interval:       svc.Interval(),
		Timeout:        outboxCleanupCycleTimeout,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

func NewSchedulerDirtyWorkWorker(dirty SchedulerDirtyWorkRepository, ownership SchedulerOwnershipRepository, processor SchedulerSnapshotDirtyProcessor) (*workerruntime.PeriodicJob, error) {
	if dirty == nil || ownership == nil || processor == nil {
		return nil, nil
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "scheduler-dirty-work",
			Kind:             workerruntime.KindPeriodic,
			Group:            "scheduler",
			CoordinationMode: workerruntime.CoordinationSingletonRun,
			Description:      "Promotes and applies scheduler dirty work behind ownership epoch",
			Tags:             []string{"scheduler", "dirty-work"},
		},
		Interval:       time.Second,
		Timeout:        20 * time.Second,
		RunImmediately: true,
		Run: func(ctx context.Context) error {
			owner, acquired, err := ownership.TryAcquire(ctx)
			if err != nil {
				return err
			}
			if !acquired || owner == nil {
				return nil
			}
			defer func() { _ = owner.Close() }()
			return ProcessSchedulerDirtyWork(ctx, dirty, owner, processor)
		},
	})
}

func NewIdempotencyCleanupWorker(svc *IdempotencyCleanupService) (*workerruntime.PeriodicJob, error) {
	if svc == nil {
		return nil, fmt.Errorf("idempotency cleanup service is required")
	}
	return workerruntime.NewPeriodicJob(workerruntime.PeriodicJobSpec{
		Descriptor: workerruntime.Descriptor{
			Name:             "idempotency-cleanup",
			Kind:             workerruntime.KindPeriodic,
			Group:            "maintenance",
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		Interval:       svc.Interval(),
		Timeout:        10 * time.Second,
		RunImmediately: true,
		Run:            svc.Run,
	})
}

// startStopPoolWorker adapts an existing Start/Stop service to the worker runtime.
type startStopPoolWorker struct {
	descriptor workerruntime.Descriptor
	start      func() error
	stop       func()
	status     func() workerruntime.PoolStatus

	mu        sync.RWMutex
	lifecycle workerruntime.LifecycleSnapshot
	started   bool
	stopping  bool
	stopDone  chan struct{}
}

func newStartStopPoolWorker(descriptor workerruntime.Descriptor, start func() error, stop func(), status func() workerruntime.PoolStatus) *startStopPoolWorker {
	return &startStopPoolWorker{
		descriptor: descriptor,
		start:      start,
		stop:       stop,
		status:     status,
		lifecycle:  workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()},
	}
}

func (w *startStopPoolWorker) Descriptor() workerruntime.Descriptor {
	if w == nil {
		return workerruntime.Descriptor{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	descriptor := w.descriptor
	descriptor.Tags = append([]string(nil), descriptor.Tags...)
	return descriptor
}

func (w *startStopPoolWorker) Start(ctx context.Context) error {
	if w == nil {
		return fmt.Errorf("worker is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopping {
		return fmt.Errorf("%s is stopping", w.descriptor.Name)
	}
	if w.started {
		return nil
	}
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStarting, UpdatedAt: time.Now()}
	if w.start != nil {
		if err := w.start(); err != nil {
			w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleFailed, UpdatedAt: time.Now(), LastError: err.Error()}
			return err
		}
	}
	w.started = true
	w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleRunning, UpdatedAt: time.Now()}
	return nil
}

func (w *startStopPoolWorker) Stop(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	if !w.started && !w.stopping {
		w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()}
		w.mu.Unlock()
		return nil
	}
	if w.stopDone == nil {
		w.stopping = true
		w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopping, UpdatedAt: time.Now()}
		w.stopDone = make(chan struct{})
		done := w.stopDone
		go func() {
			if w.stop != nil {
				w.stop()
			}
			w.mu.Lock()
			w.started = false
			w.stopping = false
			w.lifecycle = workerruntime.LifecycleSnapshot{State: workerruntime.LifecycleStopped, UpdatedAt: time.Now()}
			w.mu.Unlock()
			close(done)
		}()
	}
	done := w.stopDone
	w.mu.Unlock()

	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		select {
		case <-done:
			return nil
		default:
			return ctx.Err()
		}
	}
}

func (w *startStopPoolWorker) Snapshot() workerruntime.Snapshot {
	if w == nil {
		return workerruntime.Snapshot{}
	}
	w.mu.RLock()
	descriptor := w.descriptor
	descriptor.Tags = append([]string(nil), descriptor.Tags...)
	lifecycle := w.lifecycle
	stopping := w.stopping
	w.mu.RUnlock()
	status := workerruntime.PoolStatus{}
	if w.status != nil {
		status = w.status()
	}
	if stopping {
		status.StillRunning = true
		status.Accepting = false
	}
	return workerruntime.Snapshot{Descriptor: descriptor, Lifecycle: lifecycle, Status: status}
}

// NewOpsSystemLogSinkWorker returns the runtime component for buffered operational logs.
func NewOpsSystemLogSinkWorker(sink *OpsSystemLogSink) (*startStopPoolWorker, error) {
	if sink == nil {
		return nil, fmt.Errorf("Ops system log sink is required")
	}
	return newStartStopPoolWorker(
		workerruntime.Descriptor{
			Name:             "ops-system-log-sink",
			Kind:             workerruntime.KindPool,
			Group:            "ops",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Persists buffered operational log events",
			Tags:             []string{"ops", "system-logs", "ingestion"},
		},
		func() error {
			sink.Start()
			return nil
		},
		sink.Stop,
		func() workerruntime.PoolStatus {
			health := sink.Health()
			waiting := uint64(0)
			if health.QueueDepth > 0 {
				waiting = uint64(health.QueueDepth)
			}
			completed := health.WrittenCount + health.WriteFailed
			if completed < health.WrittenCount {
				completed = math.MaxUint64
			}
			return workerruntime.PoolStatus{
				Accepting:        true,
				StillRunning:     health.QueueDepth > 0,
				MaxConcurrency:   1,
				RunningWorkers:   1,
				WaitingTasks:     waiting,
				CompletedTasks:   completed,
				SuccessfulTasks:  health.WrittenCount,
				FailedTasks:      health.WriteFailed,
				DroppedTasks:     health.DroppedCount,
				DroppedQueueFull: health.DroppedCount,
			}
		},
	), nil
}

// NewUsageRecordWorkerPoolWorker returns the runtime component for pool.
func NewUsageRecordWorkerPoolWorker(pool *UsageRecordWorkerPool) workerruntime.Component {
	return newStartStopPoolWorker(
		workerruntime.Descriptor{
			Name:             "usage-record-pool",
			Kind:             workerruntime.KindPool,
			Group:            "usage",
			CoordinationMode: workerruntime.CoordinationPerInstance,
		},
		func() error { return nil },
		func() {
			if pool != nil {
				pool.Stop()
			}
		},
		func() workerruntime.PoolStatus {
			if pool == nil {
				return workerruntime.PoolStatus{}
			}
			stats := pool.Stats()
			return workerruntime.PoolStatus{
				Accepting:          true,
				MaxConcurrency:     stats.MaxConcurrency,
				RunningWorkers:     stats.RunningWorkers,
				WaitingTasks:       stats.WaitingTasks,
				SubmittedTasks:     stats.SubmittedTasks,
				CompletedTasks:     stats.CompletedTasks,
				SuccessfulTasks:    stats.SuccessfulTasks,
				FailedTasks:        stats.FailedTasks,
				DroppedTasks:       stats.DroppedTasks,
				DroppedQueueFull:   stats.DroppedQueueFull,
				DroppedPoolStopped: stats.DroppedPoolStopped,
				SyncFallbackTasks:  stats.SyncFallbackTasks,
			}
		},
	)
}

// NewEmailQueueWorker returns the runtime component for asynchronous email delivery.
func NewEmailQueueWorker(queue *EmailQueueService) workerruntime.Component {
	return newStartStopPoolWorker(
		workerruntime.Descriptor{
			Name:             "email-queue",
			Kind:             workerruntime.KindPool,
			Group:            "notifications",
			CoordinationMode: workerruntime.CoordinationPerInstance,
			Description:      "Delivers queued email messages asynchronously",
			Tags:             []string{"email", "notifications", "asynchronous-delivery"},
		},
		func() error { return nil },
		func() {
			if queue != nil {
				queue.Stop()
			}
		},
		func() workerruntime.PoolStatus {
			workers := 0
			if queue != nil {
				workers = queue.workers
			}
			return workerruntime.PoolStatus{
				Accepting:      true,
				MaxConcurrency: workers,
				RunningWorkers: int64(workers),
			}
		},
	)
}

// NewChannelMonitorV2AggregationWorker adapts the existing aggregator loop to the unified runtime.
func NewChannelMonitorV2AggregationWorker(svc *ChannelMonitorV2Aggregator) (workerruntime.Component, error) {
	if svc == nil {
		return nil, fmt.Errorf("channel monitor v2 aggregator is required")
	}
	return newStartStopPoolWorker(
		workerruntime.Descriptor{
			Name:             "channel-monitor-v2-aggregation",
			Kind:             workerruntime.KindPool,
			Group:            "monitoring",
			CoordinationMode: workerruntime.CoordinationSingletonRun,
			Description:      "Aggregates passive channel health metrics",
			Tags:             []string{"channel-monitor", "passive", "aggregation"},
		},
		func() error {
			svc.Start()
			return nil
		},
		svc.Stop,
		func() workerruntime.PoolStatus {
			return workerruntime.PoolStatus{Accepting: true, MaxConcurrency: 1, RunningWorkers: 1}
		},
	), nil
}
