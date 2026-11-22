package service

import (
	"context"
	"log/slog"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/alitto/pond/v2"
)

// OpenAIAccountImportChecks schedules a quota refresh and a Tibo probe for
// newly created OAuth accounts. It limits upstream concurrency during bulk
// imports.
type OpenAIAccountImportChecks struct {
	accounts  AccountRepository
	quota     openAIQuotaSnapshotRefresher
	probeTibo func(context.Context, *Account)
	pool      pond.Pool
	ctx       context.Context
	cancel    context.CancelFunc
}

func NewOpenAIAccountImportChecks(accounts AccountRepository, quota *OpenAIQuotaService, gateway *OpenAIGatewayService) *OpenAIAccountImportChecks {
	ctx, cancel := context.WithCancel(context.Background())
	return &OpenAIAccountImportChecks{
		accounts:  accounts,
		quota:     quota,
		probeTibo: gateway.probeNewOpenAIOAuthAccount,
		pool:      pond.NewPool(openAIQuotaRefreshConcurrency),
		ctx:       ctx,
		cancel:    cancel,
	}
}

func (s *OpenAIAccountImportChecks) schedule(ctx context.Context, account *Account) {
	if s == nil || !shouldRefreshOpenAIQuotaSnapshots(account, time.Now()) || account.ID <= 0 {
		return
	}
	id := account.ID
	AfterAccountCommit(ctx, func(context.Context) {
		_ = s.pool.Go(func() { s.check(id) })
	})
}

func (s *OpenAIAccountImportChecks) check(accountID int64) {
	if s.ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, openAIQuotaRefreshTimeout)
	// A queued account can be paused, edited, or deleted before its turn.
	account, err := s.accounts.GetByID(ctx, accountID)
	if err != nil || !shouldRefreshOpenAIQuotaSnapshots(account, time.Now()) {
		cancel()
		return
	}
	err = s.quota.RefreshQuotaSnapshots(ctx, accountID)
	cancel()
	if err != nil && s.ctx.Err() == nil {
		slog.Warn("openai_import_quota_refresh_failed", "account_id", accountID, "error_code", infraerrors.Reason(err))
	}
	// Tibo can still run when the quota call failed or its deadline passed.
	s.probeTibo(s.ctx, account)
}

func (s *OpenAIAccountImportChecks) Stop() {
	s.cancel()
	s.pool.StopAndWait()
}
