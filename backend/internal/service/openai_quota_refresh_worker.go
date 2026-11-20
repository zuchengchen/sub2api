package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
)

const (
	openAICreditRefreshInterval   = 5 * time.Minute
	openAIQuotaRefreshPageSize    = 100
	openAIQuotaRefreshConcurrency = 4
	openAIQuotaRefreshLockKey     = "jobs:openai-quota-credit-refresh"
	openAIQuotaRefreshLease       = time.Minute
)

type openAICreditSnapshotRefresher interface {
	RefreshCreditSnapshots(ctx context.Context, accountID int64) error
}

type openAIQuotaRefreshLock interface {
	LeaderLockCache
	RenewLeaderLock(ctx context.Context, key, owner string, ttl time.Duration) (bool, error)
}

// OpenAIQuotaRefreshWorker refreshes display snapshots independently of the
// assignment of gateway traffic and of permission to spend reset cards or points.
type OpenAIQuotaRefreshWorker struct {
	pager  OAuthRefreshCandidatePager
	quota  openAICreditSnapshotRefresher
	locks  openAIQuotaRefreshLock
	cancel context.CancelFunc
	done   chan struct{}
}

func ProvideOpenAIQuotaRefreshWorker(accounts AccountRepository, quota *OpenAIQuotaService, locks LeaderLockCache) (*OpenAIQuotaRefreshWorker, error) {
	pager, ok := accounts.(OAuthRefreshCandidatePager)
	if !ok {
		return nil, errors.New("OpenAI quota refresh requires a paged account repository")
	}
	lease, ok := locks.(openAIQuotaRefreshLock)
	if !ok {
		return nil, errors.New("OpenAI quota refresh requires a renewable leader lock")
	}
	w := &OpenAIQuotaRefreshWorker{pager: pager, quota: quota, locks: lease}
	w.start()
	return w, nil
}

func (w *OpenAIQuotaRefreshWorker) start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.done = make(chan struct{})
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(openAICreditRefreshInterval)
		defer ticker.Stop()
		for ctx.Err() == nil {
			if err := w.run(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("openai_quota_refresh_scan_failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (w *OpenAIQuotaRefreshWorker) Stop() {
	w.cancel()
	<-w.done
}

func (w *OpenAIQuotaRefreshWorker) run(ctx context.Context) error {
	owner := uuid.NewString()
	locked, err := w.locks.TryAcquireLeaderLock(ctx, openAIQuotaRefreshLockKey, owner, openAIQuotaRefreshLease)
	if err != nil || !locked {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = w.locks.ReleaseLeaderLock(releaseCtx, openAIQuotaRefreshLockKey, owner)
	}()

	// The slot outlives a short scan, so a peer whose ticker started later
	// cannot repeat it. The renewable run lock also covers scans spanning slots.
	slotKey := fmt.Sprintf("%s:%d", openAIQuotaRefreshLockKey, time.Now().Truncate(openAICreditRefreshInterval).Unix())
	locked, err = w.locks.TryAcquireLeaderLock(ctx, slotKey, owner, openAICreditRefreshInterval)
	if err != nil || !locked {
		return err
	}

	scanCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.refreshAll(scanCtx) }()
	ticker := time.NewTicker(openAIQuotaRefreshLease / 3)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case <-ticker.C:
			renewCtx, stop := context.WithTimeout(ctx, 3*time.Second)
			ok, err := w.locks.RenewLeaderLock(renewCtx, openAIQuotaRefreshLockKey, owner, openAIQuotaRefreshLease)
			stop()
			if err != nil || !ok {
				cancel()
				<-done
				return errors.New("lost OpenAI quota refresh lease")
			}
		}
	}
}

func (w *OpenAIQuotaRefreshWorker) refreshAll(ctx context.Context) error {
	ids := make(chan int64)
	var workers sync.WaitGroup
	for range openAIQuotaRefreshConcurrency {
		workers.Go(func() {
			for id := range ids {
				if ctx.Err() != nil {
					return
				}
				callCtx, cancel := context.WithTimeout(ctx, time.Minute)
				err := w.quota.RefreshCreditSnapshots(callCtx, id)
				cancel()
				if err != nil && ctx.Err() == nil {
					slog.Warn("openai_quota_refresh_account_failed", "account_id", id, "error_code", infraerrors.Reason(err))
				}
			}
		})
	}
	defer func() {
		close(ids)
		workers.Wait()
	}()

	options := OAuthRefreshPageOptions{
		Platforms: []string{PlatformOpenAI}, ActiveOnly: true, Limit: openAIQuotaRefreshPageSize,
	}
	for ctx.Err() == nil {
		page, err := w.pager.ListOAuthRefreshCandidatePage(ctx, options)
		if err != nil {
			return err
		}
		if page == nil || (page.HasMore && page.NextAfterID <= options.AfterID) {
			return errors.New("invalid OpenAI quota refresh account page")
		}
		for i := range page.Accounts {
			a := &page.Accounts[i]
			if !a.IsOpenAIOAuth() || !a.IsActive() || a.IsSyntheticUITest() ||
				(a.AutoPauseOnExpired && a.ExpiresAt != nil && !time.Now().Before(*a.ExpiresAt)) {
				continue
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ids <- a.ID:
			}
		}
		if !page.HasMore {
			return nil
		}
		options.AfterID = page.NextAfterID
	}
	return ctx.Err()
}
