package repository

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

var _ service.OpenAIOAuthRefreshSuccessRepository = (*accountRepository)(nil)

func (r *accountRepository) UpdateOpenAIOAuthCredentialsIfUnchanged(ctx context.Context, id int64, expectedCredentials map[string]any, expectedProxyID *int64, credentials map[string]any) (bool, error) {
	if r == nil || r.sql == nil {
		return false, errors.New("account repository SQL executor is not configured")
	}
	if scoped := r.accountTransactionScope(ctx); scoped != r {
		applied, err := scoped.UpdateOpenAIOAuthCredentialsIfUnchanged(ctx, id, expectedCredentials, expectedProxyID, credentials)
		if applied && err == nil {
			service.AfterAccountCommit(ctx, func(committed context.Context) { r.syncSchedulerAccountSnapshotDetached(committed, id) })
		}
		return applied, err
	}
	expectedJSON, err := json.Marshal(normalizeJSONMap(expectedCredentials))
	if err != nil {
		return false, err
	}
	credentialsJSON, err := json.Marshal(normalizeJSONMap(credentials))
	if err != nil {
		return false, err
	}
	result, err := r.sql.ExecContext(ctx, `
		WITH updated AS (
		UPDATE accounts AS a
		SET credentials = $1::jsonb,
			updated_at = NOW()
		WHERE a.id = $2
			AND a.deleted_at IS NULL
			AND a.platform = $3
			AND a.type = $4
			AND a.credentials = $5::jsonb
			AND a.proxy_id IS NOT DISTINCT FROM $6
		RETURNING a.id
		)
		INSERT INTO scheduler_outbox (event_type, account_id, group_id, payload)
		SELECT $7, updated.id, NULL, NULL FROM updated
	`, string(credentialsJSON), id, service.PlatformOpenAI, service.AccountTypeOAuth,
		string(expectedJSON), expectedProxyID, service.SchedulerOutboxEventAccountChanged)
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
