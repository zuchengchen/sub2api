package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration234DefaultsOpenAILongContextBillingEnabled(t *testing.T) {
	content, err := FS.ReadFile("234_enable_openai_long_context_billing.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "openai_long_context_billing_enabled")
	require.Contains(t, sql, "'true'::jsonb")
	require.Contains(t, sql, "parent_account_id IS NULL")
	require.Contains(t, sql, "quota_dimension = 'spark'")
	require.Contains(t, sql, "INSERT INTO scheduler_outbox")
	require.Contains(t, sql, "'account_changed'")
	require.NotContains(t, sql, "THEN 'false'::jsonb")
}
