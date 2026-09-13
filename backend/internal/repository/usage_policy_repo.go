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
		count, countErr := r.countByUser(ctx, v.UserID)
		return 0, false, count, countErr
	}
	if err != nil {
		return 0, false, 0, fmt.Errorf("insert usage policy violation: %w", err)
	}
	count, err := r.countByUser(ctx, v.UserID)
	if err != nil {
		return id, true, 0, err
	}
	return id, true, count, nil
}

func (r *usagePolicyRepository) countByUser(ctx context.Context, userID int64) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM usage_policy_violations WHERE user_id = $1`, userID).Scan(&count)
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

func (r *usagePolicyRepository) ListUserStats(ctx context.Context) ([]service.UsagePolicyUserStat, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("usage policy repository is unavailable")
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT
    v.user_id,
    COALESCE(u.email, ''),
    COALESCE(u.username, ''),
    COALESCE(u.role, ''),
    COALESCE(u.status, ''),
    COUNT(*)::int,
    BOOL_OR(v.auto_banned),
    MAX(v.created_at)
FROM usage_policy_violations v
LEFT JOIN users u ON u.id = v.user_id
GROUP BY v.user_id, u.email, u.username, u.role, u.status
ORDER BY COUNT(*) DESC, MAX(v.created_at) DESC`)
	if err != nil {
		return nil, fmt.Errorf("list usage policy stats: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []service.UsagePolicyUserStat
	for rows.Next() {
		var row service.UsagePolicyUserStat
		if err := rows.Scan(
			&row.UserID,
			&row.Email,
			&row.Username,
			&row.Role,
			&row.Status,
			&row.Count,
			&row.AutoBanned,
			&row.LastAt,
		); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (r *usagePolicyRepository) DisableUserIfActive(ctx context.Context, userID int64) (bool, error) {
	if r == nil || r.db == nil {
		return false, errors.New("usage policy repository is unavailable")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE users SET status = 'disabled', updated_at = NOW()
WHERE id = $1 AND status = 'active' AND deleted_at IS NULL`, userID)
	if err != nil {
		return false, fmt.Errorf("disable usage policy user: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}
