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

func TestListPelicanCandidatesSQLRequiresSchedulableAndPrefers292(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("intelligent_test_public.go")
	require.NoError(t, err)
	body := string(src)
	require.Contains(t, body, "func (r *intelligentTestRepository) ListPelicanCandidates")
	require.Contains(t, body, "rate_limit_reset_at")
	require.Contains(t, body, "temp_unschedulable_until")
	require.Contains(t, body, "codex_7d_used_percent")
	require.Contains(t, body, "codex_turn_ticket:gpt-6-astra")
	require.Contains(t, body, "AS has_ticket")
	require.Contains(t, body, "COALESCE((a.extra->$5->>'length')::int, 0) = 292")
}

func TestListPelicanSlotAttemptsSQLIncludesRetries(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("intelligent_test_public.go")
	require.NoError(t, err)
	body := string(src)
	require.Contains(t, body, "ListPelicanSlotAttempts")
	require.Contains(t, body, "request_key LIKE $1 || '-r%'")
	require.Contains(t, body, "t.result ILIKE '%<svg%'")
}

func TestAdminIntelligentTestSQLExcludesPelicanSchedule(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("intelligent_test_repo.go")
	require.NoError(t, err)
	body := string(src)
	require.Contains(t, body, "pelican-schedule")
	require.Contains(t, body, "intelligentAdminManualSQL")
	require.Contains(t, body, "pelican-slot-%")
	require.GreaterOrEqual(t, strings.Count(body, "intelligentAdminManualSQL"), 6)
}

func TestIntelligentClaimPickSQLStartsImmediately(t *testing.T) {
	t.Parallel()
	require.NotContains(t, intelligentClaimPickSQL, "<4")
	require.NotContains(t, intelligentClaimPickSQL, "status='running'")
	require.False(t, strings.Contains(strings.ToLower(intelligentClaimPickSQL), "not exists"))
	require.Contains(t, intelligentClaimPickSQL, "status='queued'")
}
