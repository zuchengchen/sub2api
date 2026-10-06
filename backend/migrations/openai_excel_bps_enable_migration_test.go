package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration264DefaultsOpenAIExcelBPSEnabled(t *testing.T) {
	content, err := FS.ReadFile("264_enable_openai_excel_bps.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "openai_excel_bps")
	require.Contains(t, sql, "'true'::jsonb")
	require.Contains(t, sql, "parent_account_id IS NULL")
	require.Contains(t, sql, "agentidentity")
	require.Contains(t, sql, "personalaccesstoken")
	require.Contains(t, sql, "INSERT INTO scheduler_outbox")
	require.Contains(t, sql, "'account_changed'")
	require.NotContains(t, sql, "THEN 'false'::jsonb")
}

func TestMigration265RemovesOpenAIExcelBPS(t *testing.T) {
	content, err := FS.ReadFile("265_remove_openai_excel_bps.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "DROP TRIGGER IF EXISTS accounts_enforce_openai_excel_bps_extra")
	require.Contains(t, sql, "DROP FUNCTION IF EXISTS public.enforce_openai_excel_bps_extra")
	require.Contains(t, sql, "- 'openai_excel_bps'")
	require.Contains(t, sql, "- 'codex_tibo_verdict:bps'")
	require.Contains(t, sql, "excel_bps_image_relay_enabled")
	require.Contains(t, sql, "INSERT INTO scheduler_outbox")
}
