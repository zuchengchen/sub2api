//go:build integration

package repository

import (
	"context"
	"encoding/json"
	"testing"

	dbmigrations "github.com/Wei-Shaw/sub2api/migrations"
	"github.com/stretchr/testify/require"
)

func TestMigration270FlipsOnlyOpenAIOAuthWSOffDefaults(t *testing.T) {
	tx := testTx(t)
	ctx := context.Background()
	migrationSQL, err := dbmigrations.FS.ReadFile("270_openai_oauth_ws_default_on.sql")
	require.NoError(t, err)

	insert := func(name, platform, accountType, extra string, deleted bool) int64 {
		t.Helper()
		var id int64
		query := `INSERT INTO accounts (name, platform, type, extra) VALUES ($1, $2, $3, $4::jsonb) RETURNING id`
		if deleted {
			query = `INSERT INTO accounts (name, platform, type, extra, deleted_at) VALUES ($1, $2, $3, $4::jsonb, NOW()) RETURNING id`
		}
		require.NoError(t, tx.QueryRowContext(ctx, query, name, platform, accountType, extra).Scan(&id))
		return id
	}
	oauthOff := insert("m270-oauth-off", "openai", "oauth", `{"openai_oauth_responses_websockets_v2_mode":"off","openai_oauth_responses_websockets_v2_enabled":false,"other":"keep"}`, false)
	setupOff := insert("m270-setup-off", "openai", "setup-token", `{"openai_oauth_responses_websockets_v2_mode":"off","openai_oauth_responses_websockets_v2_enabled":false}`, false)
	oauthEnabledOnly := insert("m270-oauth-enabled-only", "openai", "oauth", `{"openai_oauth_responses_websockets_v2_enabled":false}`, false)
	oauthPassthrough := insert("m270-oauth-passthrough", "openai", "oauth", `{"openai_oauth_responses_websockets_v2_mode":"passthrough","openai_oauth_responses_websockets_v2_enabled":true}`, false)
	oauthForceHTTP := insert("m270-oauth-force-http", "openai", "oauth", `{"openai_oauth_responses_websockets_v2_mode":"off","openai_oauth_responses_websockets_v2_enabled":false,"openai_ws_force_http":true}`, false)
	oauthUnset := insert("m270-oauth-unset", "openai", "oauth", `{}`, false)
	apiKeyOff := insert("m270-apikey-off", "openai", "apikey", `{"openai_apikey_responses_websockets_v2_mode":"off","openai_oauth_responses_websockets_v2_mode":"off","openai_oauth_responses_websockets_v2_enabled":false}`, false)
	otherPlatform := insert("m270-anthropic", "anthropic", "oauth", `{"openai_oauth_responses_websockets_v2_mode":"off"}`, false)
	deletedOff := insert("m270-deleted", "openai", "oauth", `{"openai_oauth_responses_websockets_v2_mode":"off","openai_oauth_responses_websockets_v2_enabled":false}`, true)

	snapshot := func() map[int64]string {
		t.Helper()
		out := map[int64]string{}
		for _, id := range []int64{oauthOff, setupOff, oauthEnabledOnly, oauthPassthrough, oauthForceHTTP, oauthUnset, apiKeyOff, otherPlatform, deletedOff} {
			var extra string
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT extra::text FROM accounts WHERE id = $1`, id).Scan(&extra))
			out[id] = extra
		}
		return out
	}
	before := snapshot()

	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)
	after := snapshot()

	requireFlipped := func(id int64, set map[string]any) {
		t.Helper()
		var want map[string]any
		require.NoError(t, json.Unmarshal([]byte(before[id]), &want))
		for key, value := range set {
			want[key] = value
		}
		wantJSON, err := json.Marshal(want)
		require.NoError(t, err)
		require.JSONEq(t, string(wantJSON), after[id], "account %d: only WS keys may change", id)
	}
	bothOn := map[string]any{"openai_oauth_responses_websockets_v2_mode": "ctx_pool", "openai_oauth_responses_websockets_v2_enabled": true}
	requireFlipped(oauthOff, bothOn)
	requireFlipped(setupOff, bothOn)
	requireFlipped(oauthEnabledOnly, map[string]any{"openai_oauth_responses_websockets_v2_enabled": true})
	requireFlipped(oauthForceHTTP, bothOn)
	require.Contains(t, after[oauthOff], `"other": "keep"`)
	require.Contains(t, after[oauthForceHTTP], `"openai_ws_force_http": true`)
	require.NotContains(t, after[oauthEnabledOnly], "openai_oauth_responses_websockets_v2_mode", "mode key must not be created")
	for _, id := range []int64{oauthPassthrough, oauthUnset, apiKeyOff, otherPlatform, deletedOff} {
		require.JSONEq(t, before[id], after[id], "account %d must be untouched", id)
	}

	_, err = tx.ExecContext(ctx, string(migrationSQL))
	require.NoError(t, err)
	again := snapshot()
	for id, value := range after {
		require.JSONEq(t, value, again[id], "rerun must be a no-op for account %d", id)
	}
}
