package repository

import (
	"context"
	"encoding/json"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

const (
	codexCreditsSnapshotKey     = "codex_credits_snapshot"
	codexResetCreditSnapshotKey = "codex_reset_credit_snapshot"
	codexCreditOwnerChangedKey  = "codex_reset_credit_owner_changed_at_us"
)

// A refresh may complete after another refresh, a redemption, or an account
// reauthorization. Apply the comparison to each field on the locked UPDATE row
// so unrelated fields and the other snapshot can still advance independently.
const codexCreditSnapshotPatchSQL = `COALESCE((
	SELECT jsonb_object_agg(patch.key, patch.value)
	FROM jsonb_each($1::jsonb) AS patch
	WHERE patch.key NOT IN ('` + codexCreditsSnapshotKey + `', '` + codexResetCreditSnapshotKey + `')
		OR patch.value = 'null'::jsonb
		OR (platform = 'openai' AND type = 'oauth'
			AND NOT EXISTS (
				SELECT 1
				FROM (VALUES (extra -> patch.key -> 'query_started_at_us'),
				             (extra -> '` + codexCreditOwnerChangedKey + `')) AS version(started_at)
				WHERE jsonb_typeof(version.started_at) = 'number'
					AND (jsonb_typeof(patch.value -> 'query_started_at_us') IS DISTINCT FROM 'number'
						OR version.started_at > (patch.value -> 'query_started_at_us'))
			))
), '{}'::jsonb)`

func accountExtraPatchSQL(updates map[string]any) string {
	_, hasPoints := updates[codexCreditsSnapshotKey]
	_, hasCount := updates[codexResetCreditSnapshotKey]
	if hasPoints || hasCount {
		return codexCreditSnapshotPatchSQL
	}
	return "$1::jsonb"
}

// Called while an ordinary edit holds the account row lock. Quota observations
// belong to the upstream owner, including across a token or proxy rotation.
func mergeCodexCreditSnapshots(ctx context.Context, client *dbent.Client, account *service.Account, extra, current map[string]any, identityUnchanged bool) error {
	keys := [...]string{codexCreditsSnapshotKey, codexResetCreditSnapshotKey, codexCreditOwnerChangedKey}
	for _, key := range keys {
		delete(extra, key)
	}
	if !account.IsOpenAIOAuth() {
		return nil
	}
	if !identityUnchanged {
		var raw []byte
		err := scanSingleRow(ctx, client, `
			SELECT CASE WHEN platform = 'openai' AND type = 'oauth' THEN credentials END
			FROM accounts WHERE id = $1 AND deleted_at IS NULL`, []any{account.ID}, &raw)
		if err != nil {
			return err
		}
		previous := &service.Account{ID: account.ID, Platform: service.PlatformOpenAI, Type: service.AccountTypeOAuth}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &previous.Credentials); err != nil {
				return err
			}
		}
		if len(raw) == 0 || !service.SameOpenAIOAuthOwner(previous, account) {
			extra[codexCreditOwnerChangedKey] = time.Now().UnixMicro()
			return nil
		}
	}
	for _, key := range keys {
		if value, ok := current[key]; ok {
			extra[key] = value
		}
	}
	return nil
}
