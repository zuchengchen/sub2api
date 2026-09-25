package repository

import (
	"os"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
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

func TestIntelligentAccountWhereSchedulableMeansDispatchable(t *testing.T) {
	t.Parallel()
	w := intelligentAccountWhere(service.IntelligentTestFilter{AccountStatus: "schedulable"})
	sqlText := w.sql()
	require.Contains(t, sqlText, "a.status = 'active'")
	require.Contains(t, sqlText, "a.schedulable = TRUE")
	require.Contains(t, sqlText, "a.rate_limit_reset_at IS NULL OR a.rate_limit_reset_at <= NOW()")
	require.Contains(t, sqlText, "a.overload_until IS NULL OR a.overload_until <= NOW()")
	require.Contains(t, sqlText, "a.temp_unschedulable_until IS NULL OR a.temp_unschedulable_until <= NOW()")
	require.Contains(t, sqlText, "a.auto_pause_on_expired = FALSE")
	require.Empty(t, w.args)

	exact := intelligentAccountWhere(service.IntelligentTestFilter{AccountStatus: "disabled"})
	require.Contains(t, exact.sql(), "a.status=$1")
	require.Equal(t, []any{"disabled"}, exact.args)
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
	require.Contains(t, body, "a.type = ANY($3)")
	require.NotContains(t, body, "https://ai8.my/v1")
	require.NotContains(t, body, "AS preferred")
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
