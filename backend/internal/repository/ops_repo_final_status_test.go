//go:build unit

package repository

import (
	"database/sql/driver"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestOpsErrorFinalStatusScopeIsExplicit(t *testing.T) {
	legacy := &service.OpsErrorLogFilter{View: "all", StatusCodes: []int{499}, SortBy: "status_code", SortOrder: "asc"}
	where, _ := buildOpsErrorLogsWhere(legacy)
	require.Contains(t, where, "COALESCE(NULLIF(e.upstream_status_code, 0), e.status_code, 0) = ANY(")
	require.Contains(t, opsErrorLogsOrderBy(legacy), "NULLIF(e.upstream_status_code, 0)")
	request := *legacy
	request.UseFinalRequestStatus = true
	where, _ = buildOpsErrorLogsWhere(&request)
	require.Contains(t, where, "COALESCE(e.status_code, 0) = ANY(")
	require.NotContains(t, where, "e.upstream_status_code")
	require.Contains(t, where, "COALESCE(e.status_code, 0) >= 400", "recovered attempts must remain excluded")
	require.Equal(t, "COALESCE(e.status_code, 0) ASC, e.id ASC", opsErrorLogsOrderBy(&request))
}

func TestOpsErrorFinalStatusOtherMatchesRequestOptions(t *testing.T) {
	for _, scope := range []struct {
		name    string
		request bool
		array   string
	}{
		{"request", true, "{400,401,403,404,408,413,429,499,500,502,503,504,529}"},
		{"legacy", false, "{400,401,403,404,409,422,429,500,502,503,504,529}"},
	} {
		t.Run(scope.name, func(t *testing.T) {
			where, args := buildOpsErrorLogsWhere(&service.OpsErrorLogFilter{
				View: "all", StatusCodesOther: true, UseFinalRequestStatus: scope.request,
			})
			require.Len(t, args, 1)
			value, err := args[0].(driver.Valuer).Value()
			require.NoError(t, err)
			require.Equal(t, scope.array, value)
			require.Contains(t, where, "NOT (")
		})
	}
}
