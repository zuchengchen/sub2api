package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

func (r *affiliateRepository) EnsureUnusedInviteCode(ctx context.Context, userID int64) (string, error) {
	if userID <= 0 {
		return "", service.ErrUserNotFound
	}
	var code string
	err := r.withTx(ctx, func(txCtx context.Context, txClient *dbent.Client) error {
		if _, err := ensureUserAffiliateWithClient(txCtx, txClient, userID); err != nil {
			return err
		}
		unused, err := ensureUnusedInviteCodeWithClient(txCtx, txClient, userID, false)
		if err != nil {
			return err
		}
		code = unused
		return nil
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

func (r *affiliateRepository) RotateUnusedInviteCode(ctx context.Context, userID int64) (string, error) {
	if userID <= 0 {
		return "", service.ErrUserNotFound
	}
	var code string
	err := r.withTx(ctx, func(txCtx context.Context, txClient *dbent.Client) error {
		if _, err := ensureUserAffiliateWithClient(txCtx, txClient, userID); err != nil {
			return err
		}
		unused, err := ensureUnusedInviteCodeWithClient(txCtx, txClient, userID, true)
		if err != nil {
			return err
		}
		code = unused
		return nil
	})
	if err != nil {
		return "", err
	}
	return code, nil
}

func (r *affiliateRepository) BindInviterByInviteCode(ctx context.Context, userID int64, rawCode string) error {
	code := strings.ToUpper(strings.TrimSpace(rawCode))
	if code == "" || !serviceAffiliateCodeFormat(code) {
		return service.ErrAffiliateCodeInvalid
	}
	if userID <= 0 {
		return service.ErrUserNotFound
	}

	return r.withTx(ctx, func(txCtx context.Context, txClient *dbent.Client) error {
		if _, err := ensureUserAffiliateWithClient(txCtx, txClient, userID); err != nil {
			return err
		}

		inviterID, consume, err := resolveInviteCodeForBind(txCtx, txClient, code, userID)
		if err != nil {
			return err
		}
		if inviterID <= 0 || inviterID == userID {
			return service.ErrAffiliateCodeInvalid
		}
		if _, err := ensureUserAffiliateWithClient(txCtx, txClient, inviterID); err != nil {
			return err
		}

		res, err := txClient.ExecContext(txCtx,
			"UPDATE user_affiliates SET inviter_id = $1, updated_at = NOW() WHERE user_id = $2 AND inviter_id IS NULL",
			inviterID, userID,
		)
		if err != nil {
			return fmt.Errorf("bind inviter: %w", err)
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			return service.ErrAffiliateAlreadyBound
		}

		if err := consume(); err != nil {
			return err
		}

		if _, err = txClient.ExecContext(txCtx,
			"UPDATE user_affiliates SET aff_count = aff_count + 1, updated_at = NOW() WHERE user_id = $1",
			inviterID,
		); err != nil {
			return fmt.Errorf("increment inviter aff_count: %w", err)
		}

		if _, err := ensureUnusedInviteCodeWithClient(txCtx, txClient, inviterID, false); err != nil {
			return err
		}
		return nil
	})
}

func serviceAffiliateCodeFormat(code string) bool {
	return isValidAffiliateCodeFormatProxy(code)
}

func isValidAffiliateCodeFormatProxy(code string) bool {
	if len(code) < service.AffiliateCodeMinLength || len(code) > service.AffiliateCodeMaxLength {
		return false
	}
	for i := 0; i < len(code); i++ {
		c := code[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func resolveInviteCodeForBind(ctx context.Context, client affiliateQueryExecer, code string, usedByUserID int64) (int64, func() error, error) {
	rows, err := client.QueryContext(ctx, `
SELECT id, inviter_id, used_by
FROM user_affiliate_invite_codes
WHERE code = $1
FOR UPDATE`, code)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = rows.Close() }()

	if rows.Next() {
		var id, inviterID int64
		var usedBy sql.NullInt64
		if err := rows.Scan(&id, &inviterID, &usedBy); err != nil {
			return 0, nil, err
		}
		if usedBy.Valid {
			return 0, nil, service.ErrAffiliateCodeInvalid
		}
		consume := func() error {
			res, execErr := client.ExecContext(ctx, `
UPDATE user_affiliate_invite_codes
SET used_by = $1, used_at = NOW()
WHERE id = $2 AND used_by IS NULL`, usedByUserID, id)
			if execErr != nil {
				if isAffiliateUniqueViolation(execErr) {
					return service.ErrAffiliateCodeInvalid
				}
				return fmt.Errorf("consume invite code: %w", execErr)
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return service.ErrAffiliateCodeInvalid
			}
			return nil
		}
		return inviterID, consume, rows.Err()
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	_ = rows.Close()

	identity, err := queryAffiliateByIdentityCode(ctx, client, code)
	if err != nil {
		if errors.Is(err, service.ErrAffiliateProfileNotFound) {
			return 0, nil, service.ErrAffiliateCodeInvalid
		}
		return 0, nil, err
	}
	inviterID := identity.UserID
	consume := func() error {
		_, execErr := client.ExecContext(ctx, `
INSERT INTO user_affiliate_invite_codes (inviter_id, code, used_by, used_at, created_at)
VALUES ($1, $2, $3, NOW(), NOW())`, inviterID, code, usedByUserID)
		if execErr != nil {
			if isAffiliateUniqueViolation(execErr) {
				return service.ErrAffiliateCodeInvalid
			}
			return fmt.Errorf("record legacy invite code: %w", execErr)
		}
		return nil
	}
	return inviterID, consume, nil
}

func ensureUnusedInviteCodeWithClient(ctx context.Context, client affiliateQueryExecer, userID int64, forceNew bool) (string, error) {
	if !forceNew {
		if code, err := queryLatestUnusedInviteCode(ctx, client, userID); err != nil {
			return "", err
		} else if code != "" {
			return code, nil
		}

		identity, err := queryAffiliateByUserID(ctx, client, userID)
		if err != nil {
			return "", err
		}
		if identity.AffCode != "" {
			if _, err := client.ExecContext(ctx, `
INSERT INTO user_affiliate_invite_codes (inviter_id, code, created_at)
VALUES ($1, $2, NOW())
ON CONFLICT (code) DO NOTHING`, userID, identity.AffCode); err != nil {
				return "", fmt.Errorf("seed identity invite code: %w", err)
			}
			if code, err := queryLatestUnusedInviteCode(ctx, client, userID); err != nil {
				return "", err
			} else if code != "" {
				return code, nil
			}
		}
	}

	for i := 0; i < affiliateCodeMaxAttempts; i++ {
		candidate, err := generateAffiliateCode()
		if err != nil {
			return "", err
		}
		_, err = client.ExecContext(ctx, `
INSERT INTO user_affiliate_invite_codes (inviter_id, code, created_at)
VALUES ($1, $2, NOW())`, userID, candidate)
		if err == nil {
			return candidate, nil
		}
		if isAffiliateUniqueViolation(err) {
			continue
		}
		return "", fmt.Errorf("mint invite code: %w", err)
	}
	return "", fmt.Errorf("mint invite code: exhausted attempts")
}

func queryLatestUnusedInviteCode(ctx context.Context, client affiliateQueryExecer, userID int64) (string, error) {
	rows, err := client.QueryContext(ctx, `
SELECT code
FROM user_affiliate_invite_codes
WHERE inviter_id = $1 AND used_by IS NULL
ORDER BY id DESC
LIMIT 1`, userID)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", rows.Err()
	}
	var code string
	if err := rows.Scan(&code); err != nil {
		return "", err
	}
	return code, rows.Err()
}

func replaceUnusedInviteCodes(ctx context.Context, client affiliateQueryExecer, userID int64, newCode string) error {
	if _, err := client.ExecContext(ctx, `
DELETE FROM user_affiliate_invite_codes
WHERE inviter_id = $1 AND used_by IS NULL`, userID); err != nil {
		return fmt.Errorf("revoke unused invite codes: %w", err)
	}
	_, err := client.ExecContext(ctx, `
INSERT INTO user_affiliate_invite_codes (inviter_id, code, created_at)
VALUES ($1, $2, NOW())`, userID, newCode)
	if err != nil {
		if isAffiliateUniqueViolation(err) {
			return service.ErrAffiliateCodeTaken
		}
		return fmt.Errorf("insert unused invite code: %w", err)
	}
	return nil
}

func queryAffiliateByIdentityCode(ctx context.Context, client affiliateQueryExecer, code string) (*service.AffiliateSummary, error) {
	return scanAffiliateByQuery(ctx, client, `
SELECT user_id,
       aff_code,
       aff_code_custom,
       aff_rebate_rate_percent,
       inviter_id,
       aff_count,
       aff_quota::double precision,
       aff_frozen_quota::double precision,
       aff_history_quota::double precision,
       created_at,
       updated_at
FROM user_affiliates
WHERE aff_code = $1
LIMIT 1`, strings.ToUpper(strings.TrimSpace(code)))
}

func scanAffiliateByQuery(ctx context.Context, client affiliateQueryExecer, query string, args ...any) (*service.AffiliateSummary, error) {
	rows, err := client.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, service.ErrAffiliateProfileNotFound
	}

	var out service.AffiliateSummary
	var inviterID sql.NullInt64
	var rebateRate sql.NullFloat64
	if err := rows.Scan(
		&out.UserID,
		&out.AffCode,
		&out.AffCodeCustom,
		&rebateRate,
		&inviterID,
		&out.AffCount,
		&out.AffQuota,
		&out.AffFrozenQuota,
		&out.AffHistoryQuota,
		&out.CreatedAt,
		&out.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if inviterID.Valid {
		out.InviterID = &inviterID.Int64
	}
	if rebateRate.Valid {
		v := rebateRate.Float64
		out.AffRebateRatePercent = &v
	}
	return &out, rows.Err()
}
