//go:build unit

package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAILegacyProtectionMigrationIsExtraMergeOnly(t *testing.T) {
	body, err := FS.ReadFile("253_openai_legacy_protection_backfill.sql")
	require.NoError(t, err)
	sql := string(body)
	require.Contains(t, sql, "platform = 'openai'")
	require.Contains(t, sql, "parent_account_id IS NULL")
	require.Contains(t, sql, "anti_degradation")
	require.Contains(t, sql, "'legacy'")
	require.NotContains(t, strings.ToLower(sql), "auto_pause_5h")
	require.NotContains(t, strings.ToLower(sql), "auto_pause_7d")
	require.Contains(t, sql, "COALESCE(extra, '{}'::jsonb) ||")
}

func TestSecurityPolicyMigrationDefaultOff(t *testing.T) {
	body, err := FS.ReadFile("254_group_security_policy.sql")
	require.NoError(t, err)
	sql := string(body)
	require.Contains(t, sql, "security_policy_enabled BOOLEAN NOT NULL DEFAULT FALSE")
	require.Contains(t, sql, "security_policy_keywords")
	require.Contains(t, sql, "overturned BOOLEAN NOT NULL DEFAULT FALSE")
	require.NotContains(t, sql, "super_admin")
}
