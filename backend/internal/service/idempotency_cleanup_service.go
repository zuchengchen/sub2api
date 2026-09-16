package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

// IdempotencyCleanupService 定期清理已过期的幂等记录，避免表无限增长。
type IdempotencyCleanupService struct {
	repo     IdempotencyRepository
	interval time.Duration
	batch    int
}

func NewIdempotencyCleanupService(repo IdempotencyRepository, cfg *config.Config) *IdempotencyCleanupService {
	interval := 60 * time.Second
	batch := 500
	if cfg != nil {
		if cfg.Idempotency.CleanupIntervalSeconds > 0 {
			interval = time.Duration(cfg.Idempotency.CleanupIntervalSeconds) * time.Second
		}
		if cfg.Idempotency.CleanupBatchSize > 0 {
			batch = cfg.Idempotency.CleanupBatchSize
		}
	}
	return &IdempotencyCleanupService{
		repo:     repo,
		interval: interval,
		batch:    batch,
	}
}

// Interval returns the configured worker interval.
func (s *IdempotencyCleanupService) Interval() time.Duration {
	if s == nil {
		return 0
	}
	return s.interval
}

// Start is retained as a no-op while runtime lifecycle ownership is introduced.
func (s *IdempotencyCleanupService) Start() {}

// Stop is retained for compatibility; runtime owns this worker's lifecycle.
func (s *IdempotencyCleanupService) Stop() {}

// Run deletes one batch of expired idempotency records.
func (s *IdempotencyCleanupService) Run(ctx context.Context) error {
	if s == nil || s.repo == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	deleted, err := s.repo.DeleteExpired(ctx, time.Now(), s.batch)
	if err != nil {
		logger.LegacyPrintf("service.idempotency_cleanup", "[IdempotencyCleanup] cleanup failed err=%v", err)
		return err
	}
	if deleted > 0 {
		logger.LegacyPrintf("service.idempotency_cleanup", "[IdempotencyCleanup] cleaned expired records count=%d", deleted)
	}
	return nil
}
