package repository

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestUserPelicanTestsDistinguishesIncompleteAndRestrictedPreviews(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now()
	rows := sqlmock.NewRows([]string{"id", "status", "result", "model", "created_at", "finished_at", "duration_ms", "group_name", "reasoning_effort", "prompt"})
	rows.AddRow(4, "failed", `<html><body><svg><path d="`, "gpt-6-astra", now, now, 90053, "GPT-PRO", "low", "draw")
	rows.AddRow(3, "completed", `<html><body><svg></svg>`, "gpt-6-astra", now, now, 120000, "GPT-PRO", "low", "draw")
	rows.AddRow(2, "completed", `<html><body><svg></svg><script>draw()</script></body></html>`, "gpt-6-astra", now, now, 120000, "GPT-PRO", "low", "draw")
	rows.AddRow(1, "completed", `<html><body><svg></svg></body></html>`, "gpt-6-astra", now, now, 120000, "GPT-PRO", "low", "draw")
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).WithArgs(service.VipDiscountedGroupName).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(4))
	mock.ExpectQuery(`SELECT t.id,t.status,t.result`).WithArgs(service.VipDiscountedGroupName, 96, 0).WillReturnRows(rows)

	page, err := (&intelligentTestRepository{db: db}).UserPelicanTests(context.Background(), service.IntelligentTestFilter{Page: 1, PageSize: 96})
	require.NoError(t, err)
	require.EqualValues(t, 4, page.Total)
	require.Len(t, page.Items, 4, "keep interrupted attempts visible in history")
	for _, item := range page.Items[:2] {
		require.Empty(t, item.HTML)
		require.Equal(t, "incomplete", item.PreviewIssue)
	}
	require.Equal(t, "scripts_removed", page.Items[2].PreviewIssue)
	require.Contains(t, page.Items[2].HTML, "<svg>")
	require.NotContains(t, page.Items[2].HTML, "<script>")
	require.Empty(t, page.Items[3].PreviewIssue)
	require.Contains(t, page.Items[3].HTML, "</html>")
	require.NoError(t, mock.ExpectationsWereMet())
}
