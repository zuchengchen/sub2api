package repository

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	codexCreditsSnapshotKey     = "codex_credits_snapshot"
	codexResetCreditSnapshotKey = "codex_reset_credit_snapshot"
	codexCreditOwnerChangedKey  = "codex_reset_credit_owner_changed_at_us"
	codexUsageObservedAtKey     = "codex_usage_observed_at_us"
)

var codexWindowKeys = []string{
	"codex_usage_updated_at", codexUsageObservedAtKey,
	"codex_primary_used_percent", "codex_primary_reset_after_seconds", "codex_primary_reset_at", "codex_primary_window_minutes",
	"codex_secondary_used_percent", "codex_secondary_reset_after_seconds", "codex_secondary_reset_at", "codex_secondary_window_minutes",
	"codex_primary_over_secondary_percent",
	"codex_5h_used_percent", "codex_5h_reset_after_seconds", "codex_5h_reset_at", "codex_5h_window_minutes",
	"codex_7d_used_percent", "codex_7d_reset_after_seconds", "codex_7d_reset_at", "codex_7d_window_minutes",
}

func hasCodexWindowFields(extra map[string]any) bool {
	for _, key := range codexWindowKeys {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

// Older callers supply only the RFC3339 sample time or a directly observed
// subset of window fields. Give those writes the same ordering as new probes.
func versionCodexWindowUpdates(updates map[string]any) map[string]any {
	if _, ok := updates[codexUsageObservedAtKey]; ok || !hasCodexWindowFields(updates) {
		return updates
	}
	raw, _ := updates["codex_usage_updated_at"].(string)
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		at = time.Now()
	}
	versioned := maps.Clone(updates)
	versioned[codexUsageObservedAtKey] = at.UnixMicro()
	return versioned
}

// A refresh may complete after another refresh, a redemption, or an account
// reauthorization. Apply the comparison to each field on the locked UPDATE row
// so the windows, points and reset credits can advance independently.
var codexQuotaSnapshotPatchSQL = `COALESCE((
	SELECT jsonb_object_agg(patch.key, patch.value)
	FROM jsonb_each($1::jsonb) AS patch
	WHERE CASE
		WHEN patch.key IN ('` + codexCreditsSnapshotKey + `', '` + codexResetCreditSnapshotKey + `') THEN
			patch.value = 'null'::jsonb OR (platform = 'openai' AND type = 'oauth'
			AND NOT EXISTS (
				SELECT 1
				FROM (VALUES (extra -> patch.key -> 'query_started_at_us'),
				             (extra -> '` + codexCreditOwnerChangedKey + `')) AS version(started_at)
				WHERE jsonb_typeof(version.started_at) = 'number'
					AND (jsonb_typeof(patch.value -> 'query_started_at_us') IS DISTINCT FROM 'number'
						OR version.started_at > (patch.value -> 'query_started_at_us'))
			))
		WHEN patch.key IN ('` + strings.Join(codexWindowKeys, "', '") + `') THEN
			(NOT ($1::jsonb ?| ARRAY['` + codexCreditsSnapshotKey + `','` + codexResetCreditSnapshotKey + `'])
			 OR (platform = 'openai' AND type = 'oauth'))
			AND jsonb_typeof($1::jsonb -> '` + codexUsageObservedAtKey + `') = 'number'
			AND ($1::jsonb -> '` + codexUsageObservedAtKey + `') >= GREATEST(
				CASE WHEN jsonb_typeof(extra -> '` + codexUsageObservedAtKey + `') = 'number'
				     THEN extra -> '` + codexUsageObservedAtKey + `'
				     ELSE to_jsonb(EXTRACT(EPOCH FROM (` + ollamaCloudUsageParseRFC3339SQL("(extra ->> 'codex_usage_updated_at')") + `)::timestamptz) * 1000000) END,
				CASE WHEN jsonb_typeof(extra -> '` + codexCreditOwnerChangedKey + `') = 'number'
				     THEN extra -> '` + codexCreditOwnerChangedKey + `' END,
				'0'::jsonb)
		ELSE true
	END
), '{}'::jsonb)`

func accountExtraPatchSQL(updates map[string]any) string {
	_, hasPoints := updates[codexCreditsSnapshotKey]
	_, hasCount := updates[codexResetCreditSnapshotKey]
	_, hasWindows := updates[codexUsageObservedAtKey]
	if hasPoints || hasCount || hasWindows {
		return codexQuotaSnapshotPatchSQL
	}
	return "$1::jsonb"
}

// Called while an ordinary edit holds the account row lock. Quota observations
// belong to the upstream owner, including across a token or proxy rotation.
func mergeCodexQuotaSnapshots(ctx context.Context, client *dbent.Client, account *service.Account, extra, current map[string]any, identityUnchanged bool) error {
	keys := [...]string{codexCreditsSnapshotKey, codexResetCreditSnapshotKey, codexCreditOwnerChangedKey}
	for _, key := range keys {
		delete(extra, key)
	}
	isOAuth := account.IsOpenAIOAuth()
	wasOAuth := isOAuth
	if !identityUnchanged && (isOAuth || hasCodexWindowFields(current)) {
		var raw []byte
		err := scanSingleRow(ctx, client, `
			SELECT CASE WHEN platform = 'openai' AND type = 'oauth' THEN credentials END
			FROM accounts WHERE id = $1 AND deleted_at IS NULL`, []any{account.ID}, &raw)
		if err != nil {
			return err
		}
		wasOAuth = len(raw) > 0
		if wasOAuth && isOAuth {
			previous := &service.Account{ID: account.ID, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
			if err := json.Unmarshal(raw, &previous.Credentials); err != nil {
				return err
			}
			identityUnchanged = service.SameOpenAIOAuthOwner(previous, account)
		}
	}
	if !isOAuth && !wasOAuth {
		if changedAt, ok := current[codexCreditOwnerChangedKey]; ok {
			extra[codexCreditOwnerChangedKey] = changedAt
		}
		return nil
	}
	for _, key := range codexWindowKeys {
		delete(extra, key)
		if value, ok := current[key]; identityUnchanged && ok {
			extra[key] = value
		}
	}
	if !identityUnchanged {
		extra[codexCreditOwnerChangedKey] = time.Now().UnixMicro()
		return nil
	}
	for _, key := range keys {
		if value, ok := current[key]; ok {
			extra[key] = value
		}
	}
	return nil
}
