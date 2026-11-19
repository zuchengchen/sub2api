package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// A restart may occur between a background refresh persisting its refusal and
// the access check completing. Recover only that specific kind of error.
type openAIRefreshErrorLister interface {
	ListOpenAIOAuthRefreshErrors(ctx context.Context, afterID int64, limit int, message string) ([]Account, error)
}

func (api *OAuthRefreshAPI) restoreOpenAIAccessProbes(ctx context.Context) {
	repo, ok := api.accountRepo.(openAIRefreshErrorLister)
	if !ok {
		return
	}
	const pageSize = 64
	var afterID int64
	cause := &openai.RefreshTokenError{Permanent: true}
	message := nonRetryableTokenRefreshMessage(cause)
	for ctx.Err() == nil {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		accounts, err := repo.ListOpenAIOAuthRefreshErrors(readCtx, afterID, pageSize, message)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("openai_access_probe_startup_read_failed", "after_account_id", afterID)
			}
			return
		}
		for i := range accounts {
			account := &accounts[i]
			r := &openAIRefreshRejection{credentials: openAIRefreshFingerprint(account), err: cause}
			if account.Status == StatusError && isOpenAIRefreshProbeAccount(account, r) {
				r.probe = &openAIAccessProbeSchedule{next: time.Now()}
				// A newer live rejection or verification always takes precedence.
				api.openAIFailures.LoadOrStore(account.ID, r)
			}
		}
		if len(accounts) < pageSize {
			return
		}
		afterID = accounts[len(accounts)-1].ID
	}
}
