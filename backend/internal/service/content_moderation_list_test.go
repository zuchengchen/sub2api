package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type contentModerationListFilterRepo struct {
	contentModerationTestRepo
	filter ContentModerationLogFilter
}

func (r *contentModerationListFilterRepo) ListLogs(_ context.Context, filter ContentModerationLogFilter) ([]ContentModerationLog, *pagination.PaginationResult, error) {
	r.filter = filter
	return nil, &pagination.PaginationResult{}, nil
}

func TestContentModerationServiceListLogs_AllowsOnlyAuditRecordViews(t *testing.T) {
	tests := []struct {
		name     string
		result   string
		expected string
	}{
		{name: "defaults to combined blocked view", result: "pass", expected: ContentModerationLogResultBlocked},
		{name: "cyber policy", result: ContentModerationLogResultCyberPolicy, expected: ContentModerationLogResultCyberPolicy},
		{name: "content blocked", result: ContentModerationLogResultContentBlocked, expected: ContentModerationLogResultContentBlocked},
		{name: "normalizes risky shadow", result: " RISKY_SHADOW ", expected: ContentModerationLogResultRiskyShadow},
		{name: "review unavailable", result: ContentModerationLogResultReviewFailure, expected: ContentModerationLogResultReviewFailure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &contentModerationListFilterRepo{}
			svc := &ContentModerationService{repo: repo}

			_, _, err := svc.ListLogs(context.Background(), ContentModerationLogFilter{
				Pagination: pagination.PaginationParams{
					Page:      -1,
					PageSize:  1000,
					SortOrder: "",
				},
				Result: tt.result,
			})

			require.NoError(t, err)
			require.Equal(t, tt.expected, repo.filter.Result)
			require.Equal(t, 1, repo.filter.Pagination.Page)
			require.Equal(t, 100, repo.filter.Pagination.PageSize)
			require.Equal(t, pagination.SortOrderDesc, repo.filter.Pagination.SortOrder)
		})
	}
}
