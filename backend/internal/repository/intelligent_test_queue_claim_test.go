package repository

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeleteStalePelicanTestsSQLKeepsTwentyFourHours(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("intelligent_test_public.go")
	require.NoError(t, err)
	body := string(src)
	require.Contains(t, body, "INTERVAL '24 hours'")
	require.Contains(t, body, "DELETE FROM account_tests")
	require.NotContains(t, body, "INTERVAL '6 hours'")
	require.NotContains(t, body, "INTERVAL '3 hours'")
}

func TestListGPTProOpenAIAccountIDsSQLRequiresLiveAstraTicket(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("intelligent_test_public.go")
	require.NoError(t, err)
	body := string(src)
	require.Contains(t, body, "codex_turn_ticket:gpt-6-astra")
	require.Contains(t, body, "LIKE 'gAAAAA%'")
	require.Contains(t, body, "expires_at")
}

func TestIntelligentClaimPickSQLStartsImmediately(t *testing.T) {
	t.Parallel()
	require.NotContains(t, intelligentClaimPickSQL, "<4")
	require.NotContains(t, intelligentClaimPickSQL, "status='running'")
	require.False(t, strings.Contains(strings.ToLower(intelligentClaimPickSQL), "not exists"))
	require.Contains(t, intelligentClaimPickSQL, "status='queued'")
}
