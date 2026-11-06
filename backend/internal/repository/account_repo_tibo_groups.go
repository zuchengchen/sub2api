package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/lib/pq"
)

const tiboAutoGroupSampleKey = "codex_tibo_verdict:auto_group"

var _ service.TiboAutoGroupRepository = (*accountRepository)(nil)

func (r *accountRepository) PersistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, sample service.TiboAutoGroupSample) error {
	for attempt := 0; ; attempt++ {
		err := r.persistTiboProbeResult(ctx, id, updates, sample)
		var pgerr *pq.Error
		if err == nil || attempt >= 2 || !errors.As(err, &pgerr) ||
			(pgerr.Code != "23505" && pgerr.Code != "40P01" && pgerr.Code != "40001") {
			return err
		}
		// Retry the entire transaction with the same probe identity and fresh
		// membership checks after a concurrent insert, deadlock or serialization failure.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
}

func (r *accountRepository) persistTiboProbeResult(ctx context.Context, id int64, updates map[string]any, sample service.TiboAutoGroupSample) error {
	if sample.Verdict != "healthy" && sample.Verdict != "degraded" || sample.StartedAt.IsZero() ||
		sample.CompletedAt.Before(sample.StartedAt) || sample.CompletedAt.After(time.Now()) {
		return fmt.Errorf("invalid fresh Tibo probe sample")
	}
	verdict, at := service.LatestOpenAIAstraSample(updates[service.OpenAIAstraPolicyExtraKey])
	if verdict != sample.Verdict || !at.Equal(sample.CompletedAt) {
		return fmt.Errorf("Tibo probe sample does not match its observation")
	}
	// This capability owns its transaction. It is never called by admin writes
	// or by historical sample recovery.
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	client := tx.Client()
	var platform, credentialType, status string
	var raw []byte
	err = scanSingleRow(ctx, client, `SELECT platform, type, status, extra FROM accounts
		WHERE id=$1 AND deleted_at IS NULL FOR NO KEY UPDATE`, []any{id}, &platform, &credentialType, &status, &raw)
	if err != nil {
		return err
	}
	var extra map[string]any
	if err := json.Unmarshal(raw, &extra); err != nil {
		return err
	}
	var previous struct {
		StartedAt time.Time `json:"started_at"`
	}
	if marker, ok := extra[tiboAutoGroupSampleKey]; ok {
		encoded, err := json.Marshal(marker)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(encoded, &previous); err != nil {
			return err
		}
	}
	if !previous.StartedAt.IsZero() && !sample.StartedAt.After(previous.StartedAt) {
		if err := tx.Rollback(); err != nil {
			return err
		}
		// A previous commit can have succeeded while its immediate cache
		// refresh failed. Retrying must refresh, without reapplying the move.
		return r.syncSchedulerAccountSnapshot(ctx, id)
	}
	changed := false
	var fromID, toID int64
	reason := "ineligible_account"
	if platform == service.PlatformOpenAI && credentialType == service.AccountTypeOAuth && status == service.StatusActive {
		fromID, toID, changed, reason, err = moveTiboAccountGroup(ctx, client, id, sample.Verdict)
		if err != nil {
			return err
		}
	}
	// Copy the caller's update; no shared probe state is mutated.
	write := make(map[string]any, len(updates)+1)
	for key, value := range updates {
		write[key] = value
	}
	// Business skips consume this probe too. Restoring a group or manually
	// enrolling an account must not make a historical retry move it later.
	write[tiboAutoGroupSampleKey] = map[string]any{
		"started_at":   sample.StartedAt.UTC().Format(time.RFC3339Nano),
		"completed_at": sample.CompletedAt.UTC().Format(time.RFC3339Nano), "verdict": sample.Verdict,
	}
	if err := r.UpdateExtra(dbent.NewTxContext(ctx, tx), id, write); err != nil {
		return err
	}
	if changed {
		if err := enqueueSchedulerOutbox(ctx, client, service.SchedulerOutboxEventAccountGroupsChanged,
			&id, nil, buildSchedulerGroupPayload([]int64{fromID, toID})); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		logger.LegacyPrintf("repository.account", "[TiboAutoGroup] account=%d verdict=%s from=%d to=%d", id, sample.Verdict, fromID, toID)
	} else if reason == "target_unavailable" {
		logger.LegacyPrintf("repository.account", "[TiboAutoGroup] account=%d skipped=%s", id, reason)
	}
	return r.syncSchedulerAccountSnapshot(ctx, id)
}

func moveTiboAccountGroup(ctx context.Context, client *dbent.Client, id int64, verdict string) (from, to int64, changed bool, reason string, err error) {
	rows, err := client.QueryContext(ctx, `SELECT id,name FROM groups WHERE name IN ($1,$2)
		AND platform='openai' AND status='active' AND deleted_at IS NULL ORDER BY id FOR SHARE`,
		service.TiboWelfareGroupName, service.TiboHealthyGroupName)
	if err != nil {
		return 0, 0, false, "", err
	}
	groups := map[string]int64{}
	count := 0
	for rows.Next() {
		count++
		var groupID int64
		var name string
		if err = rows.Scan(&groupID, &name); err != nil {
			rows.Close()
			return 0, 0, false, "", err
		}
		groups[name] = groupID
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, false, "", err
	}
	if len(groups) != 2 || count != 2 {
		return 0, 0, false, "target_unavailable", nil
	}
	from, to = groups[service.TiboHealthyGroupName], groups[service.TiboWelfareGroupName]
	if verdict == "healthy" {
		from, to = to, from
	}
	rows, err = client.QueryContext(ctx, `SELECT group_id FROM account_groups
		WHERE account_id=$1 AND group_id IN ($2,$3) ORDER BY group_id FOR UPDATE`, id, from, to)
	if err != nil {
		return 0, 0, false, "", err
	}
	hasSource, hasTarget := false, false
	for rows.Next() {
		var groupID int64
		if err = rows.Scan(&groupID); err != nil {
			rows.Close()
			return 0, 0, false, "", err
		}
		hasSource = hasSource || groupID == from
		hasTarget = hasTarget || groupID == to
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, 0, false, "", err
	}
	if !hasSource {
		if hasTarget {
			return from, to, false, "already_target", nil
		}
		return from, to, false, "outside_target_groups", nil
	}
	// UPDATE the existing source link instead of deleting and recreating it.
	// Concurrent manual DELETE/BindGroups uses the same row lock: if the source
	// disappeared, never insert a replacement that would pull the account back.
	query := `UPDATE account_groups SET group_id=$3 WHERE account_id=$1 AND group_id=$2`
	args := []any{id, from, to}
	if hasTarget {
		query = `DELETE FROM account_groups WHERE account_id=$1 AND group_id=$2`
		args = args[:2]
	}
	result, err := client.ExecContext(ctx, query, args...)
	if err != nil {
		return from, to, false, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return from, to, false, "", err
	}
	if affected == 0 {
		return from, to, false, "outside_target_groups", nil
	}
	return from, to, true, "moved", nil
}
