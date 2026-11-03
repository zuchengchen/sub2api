//go:build integration

package repository

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestGetErrorLogByID_APIKeyPrefixAndUpstreamStatus(t *testing.T) {
	ctx := context.Background()
	_, _ = integrationDB.ExecContext(ctx, "TRUNCATE ops_error_logs RESTART IDENTITY CASCADE")
	repo := NewOpsRepository(integrationDB).(*opsRepository)

	var plainID int64
	err := integrationDB.QueryRowContext(ctx, `
		INSERT INTO ops_error_logs (
			error_phase, error_type, severity, status_code, created_at
		) VALUES (
			'upstream', 'upstream_error', 'error', 500, NOW()
		) RETURNING id`,
	).Scan(&plainID)
	require.NoError(t, err)

	plain, err := repo.GetErrorLogByID(ctx, plainID)
	require.NoError(t, err)
	require.Empty(t, plain.APIKeyPrefix)

	validID, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
		ErrorPhase:   "request",
		ErrorType:    "api_error",
		Severity:     "error",
		StatusCode:   402,
		CreatedAt:    time.Now(),
		APIKeyPrefix: "sk-valid",
	})
	require.NoError(t, err)

	valid, err := repo.GetErrorLogByID(ctx, validID)
	require.NoError(t, err)
	require.Equal(t, "sk-valid", valid.APIKeyPrefix)

	zero := 0
	credentialFailureID, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
		ErrorPhase:         "account_auth",
		ErrorType:          "upstream_error",
		Severity:           "error",
		StatusCode:         503,
		UpstreamStatusCode: &zero,
		CreatedAt:          time.Now(),
	})
	require.NoError(t, err)

	credentialFailure, err := repo.GetErrorLogByID(ctx, credentialFailureID)
	require.NoError(t, err)
	require.NotNil(t, credentialFailure.UpstreamStatusCode)
	require.Zero(t, *credentialFailure.UpstreamStatusCode)
	require.Equal(t, 503, credentialFailure.StatusCode, "zero upstream status means no HTTP response, not a displayed zero status")

	// Positive upstream HTTP codes remain the displayed/filter/sort value.
	upstreamStatus := 429
	providerFailureID, err := repo.InsertErrorLog(ctx, &service.OpsInsertErrorLogInput{
		ErrorPhase:         "upstream",
		ErrorType:          "upstream_error",
		Severity:           "error",
		StatusCode:         502,
		UpstreamStatusCode: &upstreamStatus,
		CreatedAt:          time.Now(),
	})
	require.NoError(t, err)

	for _, direction := range []string{"asc", "desc"} {
		list, err := repo.ListErrorLogs(ctx, &service.OpsErrorLogFilter{View: "all", SortBy: "status_code", SortOrder: direction})
		require.NoError(t, err)
		require.Equal(t, 4, list.Total)
		var gotIDs []int64
		var gotStatus []int
		for _, row := range list.Errors {
			gotIDs = append(gotIDs, row.ID)
			gotStatus = append(gotStatus, row.StatusCode)
		}
		if direction == "asc" {
			require.Equal(t, []int64{validID, providerFailureID, plainID, credentialFailureID}, gotIDs)
			require.Equal(t, []int{402, 429, 500, 503}, gotStatus)
		} else {
			require.Equal(t, []int64{credentialFailureID, plainID, providerFailureID, validID}, gotIDs)
			require.Equal(t, []int{503, 500, 429, 402}, gotStatus)
		}
	}

	filtered, err := repo.ListErrorLogs(ctx, &service.OpsErrorLogFilter{View: "all", StatusCodes: []int{503}})
	require.NoError(t, err)
	require.Equal(t, 1, filtered.Total)
	require.Len(t, filtered.Errors, 1)
	require.Equal(t, credentialFailureID, filtered.Errors[0].ID)
	require.Equal(t, 503, filtered.Errors[0].StatusCode)

	providerFiltered, err := repo.ListErrorLogs(ctx, &service.OpsErrorLogFilter{View: "all", StatusCodes: []int{429}})
	require.NoError(t, err)
	require.Len(t, providerFiltered.Errors, 1)
	require.Equal(t, providerFailureID, providerFiltered.Errors[0].ID)

	otherStatus, err := repo.ListErrorLogs(ctx, &service.OpsErrorLogFilter{View: "all", StatusCodesOther: true})
	require.NoError(t, err)
	require.Equal(t, 1, otherStatus.Total, "only the uncommon 402 belongs in the Other status bucket")
	require.Len(t, otherStatus.Errors, 1)
	require.Equal(t, validID, otherStatus.Errors[0].ID)

	// The display fallback never rewrites the explicit upstream sentinel.
	credentialFailure, err = repo.GetErrorLogByID(ctx, credentialFailureID)
	require.NoError(t, err)
	require.NotNil(t, credentialFailure.UpstreamStatusCode)
	require.Zero(t, *credentialFailure.UpstreamStatusCode)
	require.Equal(t, 503, credentialFailure.StatusCode)
}
