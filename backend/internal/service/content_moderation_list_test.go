package service

import (
	"context"
	"net/http"
	"testing"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type contentModerationListFilterRepo struct {
	contentModerationTestRepo
	filter ContentModerationLogFilter
	items  []ContentModerationLog
}

func (r *contentModerationListFilterRepo) ListLogs(_ context.Context, filter ContentModerationLogFilter) ([]ContentModerationLog, *pagination.PaginationResult, error) {
	r.filter = filter
	return r.items, &pagination.PaginationResult{Total: int64(len(r.items)), Page: 1, PageSize: filter.Pagination.PageSize}, nil
}

func TestContentModerationServiceGetLog_ReturnsAnyActionByID(t *testing.T) {
	repo := &contentModerationListFilterRepo{
		items: []ContentModerationLog{{ID: 41, Action: ContentModerationActionAllow}},
	}
	svc := &ContentModerationService{repo: repo}

	item, err := svc.GetLog(context.Background(), 41)

	require.NoError(t, err)
	require.Equal(t, int64(41), item.ID)
	require.NotNil(t, repo.filter.LogID)
	require.Equal(t, int64(41), *repo.filter.LogID)
	require.Empty(t, repo.filter.Result)
	require.Equal(t, 1, repo.filter.Pagination.PageSize)
}

func TestContentModerationServiceGetLog_NotFound(t *testing.T) {
	svc := &ContentModerationService{repo: &contentModerationListFilterRepo{}}

	item, err := svc.GetLog(context.Background(), 404)

	require.Nil(t, item)
	require.Error(t, err)
	require.Equal(t, http.StatusNotFound, infraerrors.Code(err))
	require.Equal(t, "CONTENT_MODERATION_LOG_NOT_FOUND", infraerrors.Reason(err))
}

func TestContentModerationServiceListLogs_AllowsOnlyAuditRecordViews(t *testing.T) {
	tests := []struct {
		name     string
		result   string
		expected string
	}{
		{name: "defaults to violation blocked view", result: "pass", expected: ContentModerationLogResultViolationBlocked},
		{name: "cyber policy", result: ContentModerationLogResultCyberPolicy, expected: ContentModerationLogResultCyberPolicy},
		{name: "violation blocked", result: ContentModerationLogResultViolationBlocked, expected: ContentModerationLogResultViolationBlocked},
		{name: "legacy content blocked", result: ContentModerationLogResultContentBlocked, expected: ContentModerationLogResultViolationBlocked},
		{name: "legacy blocked", result: ContentModerationLogResultBlocked, expected: ContentModerationLogResultViolationBlocked},
		{name: "restricted", result: ContentModerationLogResultRestricted, expected: ContentModerationLogResultRestricted},
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
