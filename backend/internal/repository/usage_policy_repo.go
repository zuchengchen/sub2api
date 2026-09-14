package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

type usagePolicyRepository struct {
	db *sql.DB
}

func NewUsagePolicyRepository(db *sql.DB) service.UsagePolicyRepository {
	return &usagePolicyRepository{db: db}
}

func (r *usagePolicyRepository) InsertViolation(ctx context.Context, v *service.UsagePolicyViolation) (int64, bool, int, error) {
	if r == nil || r.db == nil || v == nil {
		return 0, false, 0, errors.New("usage policy repository is unavailable")
	}
	createdAt := v.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	var id int64
	err := r.db.QueryRowContext(ctx, `
INSERT INTO usage_policy_violations (
    user_id, request_id, client_request_id, api_key_id, account_id, group_id,
    platform, model, inbound_endpoint, status_code, upstream_status_code,
    error_message, auto_banned, skip_reason, created_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
ON CONFLICT (request_id) WHERE request_id <> '' DO NOTHING
RETURNING id`,
		v.UserID,
		strings.TrimSpace(v.RequestID),
		strings.TrimSpace(v.ClientRequestID),
		v.APIKeyID,
		v.AccountID,
		v.GroupID,
		v.Platform,
		v.Model,
		v.InboundEndpoint,
		v.StatusCode,
		v.UpstreamStatusCode,
		v.ErrorMessage,
		v.AutoBanned,
		v.SkipReason,
		createdAt,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		count, countErr := r.countForBan(ctx, v)
		return 0, false, count, countErr
	}
	if err != nil {
		return 0, false, 0, fmt.Errorf("insert usage policy violation: %w", err)
	}
	count, err := r.countForBan(ctx, v)
	if err != nil {
		return id, true, 0, err
	}
	return id, true, count, nil
}

func (r *usagePolicyRepository) countForBan(ctx context.Context, v *service.UsagePolicyViolation) (int, error) {
	var count int
	var err error
	if v != nil && v.APIKeyID != nil && *v.APIKeyID > 0 {
		err = r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM usage_policy_violations WHERE api_key_id = $1`, *v.APIKeyID).Scan(&count)
	} else {
		err = r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM usage_policy_violations WHERE user_id = $1`, v.UserID).Scan(&count)
	}
	if err != nil {
		return 0, fmt.Errorf("count usage policy violations: %w", err)
	}
	return count, nil
}

func (r *usagePolicyRepository) UpdateViolationDisposition(ctx context.Context, id int64, autoBanned bool, skipReason string) error {
	if r == nil || r.db == nil {
		return errors.New("usage policy repository is unavailable")
	}
	_, err := r.db.ExecContext(ctx, `
UPDATE usage_policy_violations
SET auto_banned = $2, skip_reason = $3
WHERE id = $1`, id, autoBanned, skipReason)
	if err != nil {
		return fmt.Errorf("update usage policy disposition: %w", err)
	}
	return nil
}

func (r *usagePolicyRepository) ListKeyStats(ctx context.Context) ([]service.UsagePolicyKeyStat, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("usage policy repository is unavailable")
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT
    v.user_id,
    COALESCE(u.email, ''),
    COALESCE(u.username, ''),
    COALESCE(u.role, ''),
    v.api_key_id,
    COALESCE(k.name, ''),
    COALESCE(k.status, ''),
    COUNT(*)::int,
    BOOL_OR(v.auto_banned),
    MAX(v.created_at)
FROM usage_policy_violations v
LEFT JOIN users u ON u.id = v.user_id
LEFT JOIN api_keys k ON k.id = v.api_key_id
GROUP BY v.user_id, u.email, u.username, u.role, v.api_key_id, k.name, k.status
ORDER BY COUNT(*) DESC, MAX(v.created_at) DESC`)
	if err != nil {
		return nil, fmt.Errorf("list usage policy stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []service.UsagePolicyKeyStat
	for rows.Next() {
		var row service.UsagePolicyKeyStat
		var apiKeyID sql.NullInt64
		if err := rows.Scan(
			&row.UserID,
			&row.Email,
			&row.Username,
			&row.Role,
			&apiKeyID,
			&row.APIKeyName,
			&row.APIKeyStatus,
			&row.Count,
			&row.AutoBanned,
			&row.LastAt,
		); err != nil {
			return nil, err
		}
		if apiKeyID.Valid {
			id := apiKeyID.Int64
			row.APIKeyID = &id
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *usagePolicyRepository) DisableUserForUsagePolicy(ctx context.Context, userID int64, until time.Time) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("usage policy repository is unavailable")
	}
	if userID <= 0 {
		return false, nil
	}
	if until.IsZero() {
		until = time.Now().UTC().Add(15 * time.Minute)
	}
	var id int64
	err := r.db.QueryRowContext(ctx, `
UPDATE users
SET status = 'disabled', usage_policy_unban_at = $2, updated_at = NOW()
WHERE id = $1 AND deleted_at IS NULL AND status = 'active'
RETURNING id`, userID, until).Scan(&id)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("disable usage policy user: %w", err)
	}
	err = r.db.QueryRowContext(ctx, `
UPDATE users
SET usage_policy_unban_at = GREATEST(usage_policy_unban_at, $2), updated_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL
  AND status = 'disabled'
  AND usage_policy_unban_at IS NOT NULL
RETURNING id`, userID, until).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("extend usage policy user ban: %w", err)
	}
	return true, nil
}

func (r *usagePolicyRepository) UnbanDueUsers(ctx context.Context, limit int) ([]int64, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("usage policy repository is unavailable")
	}
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `
WITH due AS (
    SELECT id
    FROM users
    WHERE deleted_at IS NULL
      AND status = 'disabled'
      AND usage_policy_unban_at IS NOT NULL
      AND usage_policy_unban_at <= NOW()
    ORDER BY usage_policy_unban_at ASC
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE users AS u
SET status = 'active', usage_policy_unban_at = NULL, updated_at = NOW()
FROM due
WHERE u.id = due.id
RETURNING u.id`, limit)
	if err != nil {
		return nil, fmt.Errorf("unban due usage policy users: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
