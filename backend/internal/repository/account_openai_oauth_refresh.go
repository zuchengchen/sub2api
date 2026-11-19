package repository

import (
	"context"
	"encoding/json"
	"errors"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbaccount "github.com/Wei-Shaw/sub2api/ent/account"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.OpenAIOAuthRefreshSuccessRepository = (*accountRepository)(nil)
var _ service.OpenAIOAuthRefreshErrorRepository = (*accountRepository)(nil)

// Error accounts are excluded from the ordinary background refresh pager.
func (r *accountRepository) ListOpenAIOAuthRefreshErrors(ctx context.Context, afterID int64, limit int, message string) ([]service.Account, error) {
	rows, err := r.client.Account.Query().Where(
		dbaccount.IDGT(afterID), dbaccount.DeletedAtIsNil(), dbaccount.PlatformEQ(service.PlatformOpenAI),
		dbaccount.TypeEQ(service.AccountTypeOAuth), dbaccount.SchedulableEQ(true),
		dbaccount.StatusEQ(service.StatusError), dbaccount.ErrorMessageEQ(message),
	).Order(dbent.Asc(dbaccount.FieldID)).Limit(limit).All(ctx)
	if err != nil {
		return nil, err
	}
	return r.accountsToService(ctx, rows)
}

const openAIRefreshAccountMatch = `
		WHERE a.id = $2
			AND a.deleted_at IS NULL
			AND a.platform = $3
			AND a.type = $4
			AND a.credentials = $5::jsonb
			AND a.proxy_id IS NOT DISTINCT FROM $6`

const openAIRefreshAccountChangedOutbox = `
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $7, updated.id, NULL, NULL FROM updated`

func (r *accountRepository) UpdateOpenAIOAuthCredentialsIfUnchanged(ctx context.Context, id int64, expectedCredentials map[string]any, expectedProxyID *int64, credentials map[string]any) (bool, error) {
	credentialsJSON, err := json.Marshal(normalizeJSONMap(credentials))
	if err != nil {
		return false, err
	}
	const query = `
		WITH updated AS (
		UPDATE accounts AS a
		SET credentials = $1::jsonb, updated_at = NOW()` + openAIRefreshAccountMatch + openAIRefreshAccountChangedOutbox
	return r.applyOpenAIRefreshUpdate(ctx, id, expectedCredentials, expectedProxyID, query, string(credentialsJSON))
}

func (r *accountRepository) SetOpenAIOAuthRefreshErrorIfCredentialsUnchanged(ctx context.Context, id int64, expectedCredentials map[string]any, expectedProxyID *int64, errorMsg string) (bool, error) {
	// Status excludes this account. Preserve schedulable so ClearError after
	// a successful access test restores only accounts the administrator enabled.
	const query = `
		WITH updated AS (
		UPDATE accounts AS a
		SET error_message = $1, status = $8, updated_at = NOW()` + openAIRefreshAccountMatch + `
			AND a.status = $9` + openAIRefreshAccountChangedOutbox
	return r.applyOpenAIRefreshUpdate(ctx, id, expectedCredentials, expectedProxyID, query, errorMsg, service.StatusError, service.StatusActive)
}

// The worker passes the error produced by this credential's rejected refresh.
// Match it again at write time so an administrator's stop or a later failure wins.
func (r *accountRepository) ClearOpenAIOAuthRefreshErrorIfCredentialsUnchanged(ctx context.Context, id int64, expectedCredentials map[string]any, expectedProxyID *int64, expectedErrorMessage string) (bool, error) {
	const query = `
		WITH updated AS (
		UPDATE accounts AS a
		SET error_message = '', status = $8, updated_at = NOW()` + openAIRefreshAccountMatch + `
			AND a.status = $9 AND a.schedulable AND a.error_message = $1` + openAIRefreshAccountChangedOutbox
	return r.applyOpenAIRefreshUpdate(ctx, id, expectedCredentials, expectedProxyID, query, expectedErrorMessage, service.StatusActive, service.StatusError)
}

func (r *accountRepository) applyOpenAIRefreshUpdate(ctx context.Context, id int64, expectedCredentials map[string]any, expectedProxyID *int64, query, value string, extraArgs ...any) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	if scoped := r.accountTransactionScope(ctx); scoped != r {
		applied, err := scoped.applyOpenAIRefreshUpdate(ctx, id, expectedCredentials, expectedProxyID, query, value, extraArgs...)
		if applied && err == nil {
			service.AfterAccountCommit(ctx, func(committed context.Context) { r.syncSchedulerAccountSnapshotDetached(committed, id) })
		}
		return applied, err
	}
	expectedJSON, err := json.Marshal(normalizeJSONMap(expectedCredentials))
	if err != nil {
		return false, err
	}
	args := []any{value, id, service.PlatformOpenAI, service.AccountTypeOAuth, string(expectedJSON), expectedProxyID, service.SchedulerOutboxEventAccountChanged}
	result, err := r.sql.ExecContext(ctx, query, append(args, extraArgs...)...)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 0 {
		return false, err
	}
	r.syncSchedulerAccountSnapshotDetached(ctx, id)
	return true, nil
}
