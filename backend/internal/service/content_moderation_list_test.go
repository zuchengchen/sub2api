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

func TestContentModerationServiceListLogs_OnlyListsBlockedAuditEvents(t *testing.T) {
	repo := &contentModerationListFilterRepo{}
	svc := &ContentModerationService{repo: repo}

	_, _, err := svc.ListLogs(context.Background(), ContentModerationLogFilter{
		Pagination: pagination.PaginationParams{
			Page:      -1,
			PageSize:  1000,
			SortOrder: "",
		},
		Result: "pass",
	})

	require.NoError(t, err)
	require.Equal(t, "blocked", repo.filter.Result)
	require.Equal(t, 1, repo.filter.Pagination.Page)
	require.Equal(t, 100, repo.filter.Pagination.PageSize)
	require.Equal(t, pagination.SortOrderDesc, repo.filter.Pagination.SortOrder)
}
