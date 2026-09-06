package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration235UserAffiliateInviteCodes(t *testing.T) {
	content, err := FS.ReadFile("235_user_affiliate_invite_codes.sql")
	require.NoError(t, err)

	sql := string(content)
	require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS user_affiliate_invite_codes")
	require.Contains(t, sql, "used_by BIGINT NULL")
	require.Contains(t, sql, "ON CONFLICT (code) DO NOTHING")
}
