package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

// This predicate is shared by list, detail, history and image reads. Visibility
// is checked from current settings at request time, never from a cached snapshot.
const publicIntelligentAccessSQL = `a.deleted_at IS NULL AND s.user_visible
 AND EXISTS(SELECT 1 FROM users u WHERE u.id=$1 AND u.status='active' AND u.deleted_at IS NULL
 AND EXISTS(SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
 WHERE ag.account_id=a.id AND g.deleted_at IS NULL AND g.status='active'
 AND ((NOT g.is_exclusive AND NOT u.restrict_public_groups) OR EXISTS(SELECT 1 FROM user_allowed_groups ug WHERE ug.user_id=u.id AND ug.group_id=g.id))
 AND (g.subscription_type<>'subscription' OR EXISTS(SELECT 1 FROM user_subscriptions us WHERE us.user_id=u.id AND us.group_id=g.id AND us.status='active' AND us.expires_at>NOW() AND us.deleted_at IS NULL))))`
const publicIntelligentColumns = `t.id,t.account_id,t.test_type,t.status,t.score,t.result,t.result_image,t.duration_ms,t.model,t.created_at,t.finished_at,t.evaluation`

func scanPublicIntelligent(row intelligentScanner) (*service.PublicAccountTest, error) {
	p := &service.PublicAccountTest{}
	var raw []byte
	err := row.Scan(&p.ID, &p.AccountID, &p.TestType, &p.Status, &p.Score, &p.Result, &p.ResultImage, &p.DurationMS, &p.Model, &p.CreatedAt, &p.FinishedAt, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, service.ErrIntelligentTestNotFound
	}
	if err == nil {
		var evaluation map[string]any
		if err = json.Unmarshal(raw, &evaluation); err == nil {
			p.Evaluation = service.PublicIntelligentAssessment(evaluation)
		}
	}
	return p, err
}
func (r *intelligentTestRepository) PublicGet(ctx context.Context, user, id int64) (*service.PublicAccountTest, error) {
	return scanPublicIntelligent(r.db.QueryRowContext(ctx, `SELECT `+publicIntelligentColumns+` FROM account_tests t JOIN accounts a ON a.id=t.account_id JOIN test_settings s ON s.test_type=t.test_type WHERE `+publicIntelligentAccessSQL+` AND t.id=$2 AND t.status NOT IN ('queued','running')`, user, id))
}
func (r *intelligentTestRepository) PublicRecords(ctx context.Context, user int64, f service.IntelligentTestFilter) (*service.PublicAccountTests, error) {
	out := &service.PublicAccountTests{Items: []service.PublicAccountTest{}, Page: f.Page, PageSize: f.PageSize}
	w := &intelligentWhere{parts: []string{publicIntelligentAccessSQL, `t.status NOT IN ('queued','running','cancelled')`}, args: []any{user}}
	if f.AccountID > 0 {
		w.add("t.account_id=$%d", f.AccountID)
	}
	if f.TestType != "" {
		w.add("t.test_type=$%d", f.TestType)
	}
	if f.Status != "" {
		w.add("t.status=$%d", f.Status)
	}
	if f.From != nil {
		w.add("t.created_at >= $%d", *f.From)
	}
	if f.To != nil {
		w.add("t.created_at <= $%d", *f.To)
	}
	base := ` FROM account_tests t JOIN accounts a ON a.id=t.account_id JOIN test_settings s ON s.test_type=t.test_type WHERE ` + w.sql()
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*)`+base, w.args...).Scan(&out.Total); err != nil {
		return nil, err
	}
	args := append(append([]any{}, w.args...), f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT `+publicIntelligentColumns+base+fmt.Sprintf(` ORDER BY t.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanPublicIntelligent(rows)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, *p)
	}
	return out, rows.Err()
}
func (r *intelligentTestRepository) Capabilities(ctx context.Context, user int64, f service.IntelligentTestFilter) ([]service.AccountCapability, int64, error) {
	out := []service.AccountCapability{}
	w := &intelligentWhere{parts: []string{publicIntelligentAccessSQL, `t.status NOT IN ('queued','running','cancelled')`}, args: []any{user}}
	if f.AccountID > 0 {
		w.add("a.id=$%d", f.AccountID)
	}
	base := ` FROM account_tests t JOIN accounts a ON a.id=t.account_id JOIN test_settings s ON s.test_type=t.test_type WHERE ` + w.sql()
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT a.id)`+base, w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args := append(append([]any{}, w.args...), f.PageSize, (f.Page-1)*f.PageSize)
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT a.id,a.platform,a.type`+base+fmt.Sprintf(` ORDER BY a.id DESC LIMIT $%d OFFSET $%d`, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	for rows.Next() {
		var item service.AccountCapability
		if err := rows.Scan(&item.AccountID, &item.Platform, &item.AccountType); err != nil {
			rows.Close()
			return nil, 0, err
		}
		item.Tests = []service.PublicAccountTest{}
		out = append(out, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, err
	}
	for i := range out {
		query := `SELECT DISTINCT ON(t.test_type) ` + publicIntelligentColumns + base + fmt.Sprintf(` AND a.id=$%d ORDER BY t.test_type,t.id DESC`, len(w.args)+1)
		queryArgs := append(append([]any{}, w.args...), out[i].AccountID)
		tests, err := r.db.QueryContext(ctx, query, queryArgs...)
		if err != nil {
			return nil, 0, err
		}
		for tests.Next() {
			p, err := scanPublicIntelligent(tests)
			if err != nil {
				tests.Close()
				return nil, 0, err
			}
			out[i].Tests = append(out[i].Tests, *p)
		}
		err = tests.Err()
		tests.Close()
		if err != nil {
			return nil, 0, err
		}
	}
	return out, total, nil
}

func (r *intelligentTestRepository) FirstAdminUserID(ctx context.Context) (int64, error) {
	var id int64
	err := r.db.QueryRowContext(ctx, `SELECT id FROM users WHERE role=$1 AND status=$2 AND deleted_at IS NULL ORDER BY id LIMIT 1`, service.RoleAdmin, service.StatusActive).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func (r *intelligentTestRepository) ListGPTProOpenAIAccountIDs(ctx context.Context) ([]int64, error) {
	// Only accounts with a still-valid 292 gpt-6-astra ticket. Scheduled pelican
	// tests inject that ticket; picking an expired/missing ticket fails fail-closed.
	const astraTicket = `codex_turn_ticket:gpt-6-astra`
	rows, err := r.db.QueryContext(ctx, `
SELECT DISTINCT a.id
FROM accounts a
JOIN account_groups ag ON ag.account_id=a.id
JOIN groups g ON g.id=ag.group_id
WHERE a.deleted_at IS NULL AND a.status=$1 AND a.platform=$2 AND a.type=ANY($3)
  AND a.parent_account_id IS NULL AND a.schedulable=true
  AND g.deleted_at IS NULL AND g.status=$1 AND lower(g.name)=$4
  AND jsonb_typeof(a.extra->$5) = 'object'
  AND COALESCE((a.extra->$5->>'length')::int, 0) = 292
  AND length(COALESCE(a.extra->$5->>'state','')) = 292
  AND COALESCE(a.extra->$5->>'state','') LIKE 'gAAAAA%'
  AND NULLIF(btrim(a.extra->$5->>'expires_at'),'') IS NOT NULL
  AND (a.extra->$5->>'expires_at')::timestamptz > NOW()
ORDER BY a.id`, service.StatusActive, service.PlatformOpenAI, pq.Array([]string{service.AccountTypeOAuth, service.AccountTypeSetupToken}), service.VipDiscountedGroupName, astraTicket)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *intelligentTestRepository) UserPelicanTests(ctx context.Context, f service.IntelligentTestFilter) (*service.UserPelicanTests, error) {
	out := &service.UserPelicanTests{Items: []service.UserPelicanTest{}, Page: f.Page, PageSize: f.PageSize}
	base := `
FROM account_tests t
JOIN accounts a ON a.id=t.account_id
JOIN test_settings s ON s.test_type=t.test_type
WHERE t.test_type='pelican' AND t.status NOT IN ('queued','running','cancelled')
  AND s.user_visible
  AND a.deleted_at IS NULL
  AND t.result ILIKE '%<svg%'
  AND COALESCE(t.finished_at, t.created_at) >= NOW() - INTERVAL '24 hours'
  AND EXISTS (
    SELECT 1 FROM account_groups ag JOIN groups g ON g.id=ag.group_id
    WHERE ag.account_id=a.id AND g.deleted_at IS NULL AND lower(g.name)=$1
  )`
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) `+base, service.VipDiscountedGroupName).Scan(&out.Total); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT t.id,t.status,t.result,t.model,t.created_at,t.finished_at,t.duration_ms,
COALESCE(NULLIF(t.config_snapshot#>>'{execution,group_name}',''),'GPT-PRO'),
COALESCE(NULLIF(t.config_snapshot#>>'{execution,reasoning_effort}',''),'low'),
COALESCE(NULLIF(t.input,''),NULLIF(t.config_snapshot->>'prompt',''),'')
`+base+` ORDER BY t.id DESC LIMIT $2 OFFSET $3`, service.VipDiscountedGroupName, f.PageSize, (f.Page-1)*f.PageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item := service.UserPelicanTest{}
		var raw string
		if err := rows.Scan(&item.ID, &item.Status, &raw, &item.Model, &item.CreatedAt, &item.FinishedAt, &item.DurationMS, &item.GroupName, &item.ReasoningEffort, &item.Prompt); err != nil {
			return nil, err
		}
		if html, err := service.SanitizeIntelligentTestHTML(raw); err == nil {
			item.HTML = html
		}
		out.Items = append(out.Items, item)
	}
	return out, rows.Err()
}

func (r *intelligentTestRepository) DeleteStalePelicanTests(ctx context.Context) (int64, error) {
	res, err := r.db.ExecContext(ctx, `
DELETE FROM account_tests
WHERE test_type='pelican'
  AND status NOT IN ('queued','running')
  AND COALESCE(finished_at, created_at) < NOW() - INTERVAL '24 hours'`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := r.db.ExecContext(ctx, `
DELETE FROM intelligent_test_requests
WHERE (request_key LIKE 'pelican-slot-%' OR request_key LIKE 'pelican-hourly-%')
  AND created_at < NOW() - INTERVAL '24 hours'`); err != nil {
		return n, err
	}
	return n, nil
}
