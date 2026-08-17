package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
)

func TestBuildContentModerationLogWhere_BlockedIncludesAllBlockActions(t *testing.T) {
	where, args := buildContentModerationLogWhere(service.ContentModerationLogFilter{Result: "blocked"})

	require.Empty(t, args)
	sql := strings.Join(where, " AND ")
	require.Contains(t, sql, "l.action IN ('block', 'keyword_block', 'hash_block', 'second_layer_block', 'cache_block', 'cyber_policy')")
	require.NotContains(t, sql, "l.action = 'block'")
}

func TestBuildContentModerationLogWhere_AuditRecordViews(t *testing.T) {
	tests := []struct {
		name       string
		result     string
		contains   string
		notContain string
	}{
		{
			name:     "cyber policy",
			result:   service.ContentModerationLogResultCyberPolicy,
			contains: "l.action = 'cyber_policy'",
		},
		{
			name:       "content blocked",
			result:     service.ContentModerationLogResultContentBlocked,
			contains:   "l.action IN ('block', 'keyword_block', 'hash_block', 'second_layer_block', 'cache_block')",
			notContain: "'cyber_policy'",
		},
		{
			name:     "risky shadow",
			result:   service.ContentModerationLogResultRiskyShadow,
			contains: "l.action IN ('first_layer_shadow', 'second_layer_shadow', 'whitelist_shadow') AND COALESCE(BTRIM(l.highest_category), '') <> ''",
		},
		{
			name:     "review unavailable",
			result:   service.ContentModerationLogResultReviewFailure,
			contains: "l.action = 'review_unavailable'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			where, args := buildContentModerationLogWhere(service.ContentModerationLogFilter{Result: tt.result})

			require.Empty(t, args)
			sql := strings.Join(where, " AND ")
			require.Contains(t, sql, tt.contains)
			if tt.notContain != "" {
				require.NotContains(t, sql, tt.notContain)
			}
		})
	}
}

func TestContentModerationRepositoryCountFlaggedByUserSince_ExcludesReplayBlocks(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := NewContentModerationRepository(db)
	since := time.Now().Add(-time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("AND action NOT IN ('hash_block', 'cache_block')")).
		WithArgs(int64(1001), since, false, "").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	count, err := repo.CountFlaggedByUserSince(context.Background(), 1001, since, false)

	require.NoError(t, err)
	require.Equal(t, 2, count)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestContentModerationDeepSeekConfidencePreservesDatabaseNullInAPI(t *testing.T) {
	tests := []struct {
		name          string
		scanned       sql.NullFloat64
		wantScore     float64
		wantJSONValue *float64
	}{
		{name: "historical null"},
		{name: "reviewed score", scanned: sql.NullFloat64{Float64: 0.91, Valid: true}, wantScore: 0.91, wantJSONValue: float64Ptr(0.91)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			item := service.ContentModerationLog{DeepSeekConfidence: 0.42, DeepSeekConfidenceValue: float64Ptr(0.42)}
			setContentModerationDeepSeekConfidence(&item, tt.scanned)

			require.Equal(t, tt.wantScore, item.DeepSeekConfidence)
			if tt.wantJSONValue == nil {
				require.Nil(t, item.DeepSeekConfidenceValue)
			} else {
				require.NotNil(t, item.DeepSeekConfidenceValue)
				require.InDelta(t, *tt.wantJSONValue, *item.DeepSeekConfidenceValue, 0.00001)
			}

			raw, err := json.Marshal(item)
			require.NoError(t, err)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(raw, &payload))
			jsonValue, present := payload["deepseek_confidence"]
			require.True(t, present)
			if tt.wantJSONValue == nil {
				require.Nil(t, jsonValue)
			} else {
				require.InDelta(t, *tt.wantJSONValue, jsonValue, 0.00001)
			}
		})
	}
}

func TestContentModerationDeepSeekConfidenceInsertValuePreservesAbsence(t *testing.T) {
	require.Nil(t, contentModerationDeepSeekConfidenceValue(&service.ContentModerationLog{}))
	require.Nil(t, contentModerationDeepSeekConfidenceValue(&service.ContentModerationLog{ReviewOutcome: "unavailable"}))
	require.Equal(t, 0.0, contentModerationDeepSeekConfidenceValue(&service.ContentModerationLog{ReviewOutcome: "safe"}))
	require.Equal(t, 0.0, contentModerationDeepSeekConfidenceValue(&service.ContentModerationLog{DeepSeekCategory: "safe"}))
	require.Equal(t, 0.91, contentModerationDeepSeekConfidenceValue(&service.ContentModerationLog{DeepSeekConfidence: 0.91}))
}

func float64Ptr(value float64) *float64 {
	return &value
}

func TestContentModerationRepositoryCountFlaggedByUserSince_ExcludesCyberPolicyWhenRequested(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	repo := NewContentModerationRepository(db)
	since := time.Now().Add(-time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("AND ($3::bool IS FALSE OR action <> 'cyber_policy')")).
		WithArgs(int64(1001), since, true, "").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	count, err := repo.CountFlaggedByUserSince(context.Background(), 1001, since, true)

	require.NoError(t, err)
	require.Equal(t, 3, count)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestContentModerationRepositoryClaimLogEmailDeliveryWinsOnlyOnce(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo, ok := NewContentModerationRepository(db).(service.ContentModerationEmailDeliveryRepository)
	require.True(t, ok)

	mock.ExpectExec(regexp.QuoteMeta("WHERE id = $1 AND email_delivery_claimed_at IS NULL")).
		WithArgs(int64(77)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	claim, err := repo.ClaimLogEmailDelivery(context.Background(), 77)
	require.NoError(t, err)
	require.Equal(t, service.ContentModerationEmailDeliveryClaim{Exists: true, Claimed: true, Status: "claimed"}, claim)

	mock.ExpectExec(regexp.QuoteMeta("WHERE id = $1 AND email_delivery_claimed_at IS NULL")).
		WithArgs(int64(77)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT email_delivery_status FROM content_moderation_logs WHERE id = $1")).
		WithArgs(int64(77)).
		WillReturnRows(sqlmock.NewRows([]string{"email_delivery_status"}).AddRow("claimed"))
	claim, err = repo.ClaimLogEmailDelivery(context.Background(), 77)
	require.NoError(t, err)
	require.Equal(t, service.ContentModerationEmailDeliveryClaim{Exists: true, Status: "claimed"}, claim)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestContentModerationRepositoryEmailCompletionRequiresClaim(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo, ok := NewContentModerationRepository(db).(service.ContentModerationEmailDeliveryRepository)
	require.True(t, ok)

	mock.ExpectExec(regexp.QuoteMeta("WHERE archive_id = $1::uuid AND email_delivery_claimed_at IS NOT NULL")).
		WithArgs("b411db6f-39c3-4ff7-acfb-ecb860d6a68b", "sent", true).
		WillReturnResult(sqlmock.NewResult(0, 1))
	require.NoError(t, repo.CompleteLogEmailDeliveryByArchiveID(context.Background(), "b411db6f-39c3-4ff7-acfb-ecb860d6a68b", true))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestContentModerationRepositoryReferencedArchiveKeyIDsIncludesDeepSeekCredentialEnvelopes(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo, ok := NewContentModerationRepository(db).(service.ContentModerationArchiveRepository)
	require.True(t, ok)

	mock.ExpectQuery(`(?s)WITH referenced_keys AS .*jsonb_path_query.*api_key_envelope.key_id.*SELECT DISTINCT key_id`).
		WithArgs(service.SettingKeyContentModerationConfig).
		WillReturnRows(sqlmock.NewRows([]string{"key_id"}).
			AddRow("archive-key-2026-01").
			AddRow("credential-key-2026-02"))

	keyIDs, err := repo.ReferencedArchiveKeyIDs(context.Background())

	require.NoError(t, err)
	require.Equal(t, []string{"archive-key-2026-01", "credential-key-2026-02"}, keyIDs)
	require.NoError(t, mock.ExpectationsWereMet())
}
