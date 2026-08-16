package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type contentModerationRepository struct {
	db *sql.DB
}

type contentModerationQueryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func NewContentModerationRepository(db *sql.DB) service.ContentModerationRepository {
	return &contentModerationRepository{db: db}
}

func (r *contentModerationRepository) CreateLog(ctx context.Context, log *service.ContentModerationLog) error {
	return insertContentModerationLog(ctx, r.db, log)
}

func (r *contentModerationRepository) CreateContentLostLog(ctx context.Context, log *service.ContentModerationLog) error {
	if log == nil || strings.TrimSpace(log.ArchiveID) == "" {
		return errors.New("content moderation content-lost log requires an archive ID")
	}
	log.ArchiveStatus = service.ContentModerationArchiveStatusLost
	log.ArchiveContentLost = true
	var existingID int64
	var existingCreatedAt time.Time
	err := r.db.QueryRowContext(ctx, `SELECT id, created_at FROM content_moderation_logs WHERE archive_id = $1::uuid`, log.ArchiveID).Scan(&existingID, &existingCreatedAt)
	if err == nil {
		log.ID = existingID
		log.CreatedAt = existingCreatedAt
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check existing content-lost summary: %w", err)
	}
	return insertContentModerationLog(ctx, r.db, log)
}

func insertContentModerationLog(ctx context.Context, db contentModerationQueryRower, log *service.ContentModerationLog) error {
	if log == nil {
		return nil
	}
	categoryScores, err := json.Marshal(log.CategoryScores)
	if err != nil {
		return fmt.Errorf("marshal moderation category scores: %w", err)
	}
	thresholdSnapshot, err := json.Marshal(log.ThresholdSnapshot)
	if err != nil {
		return fmt.Errorf("marshal moderation thresholds: %w", err)
	}
	legacyMetadata := log.LegacyMetadata
	if len(legacyMetadata) == 0 {
		legacyMetadata = json.RawMessage(`{}`)
	}
	if !json.Valid(legacyMetadata) {
		return errors.New("content moderation legacy metadata is invalid JSON")
	}
	var userID any
	if log.UserID != nil {
		userID = *log.UserID
	}
	var apiKeyID any
	if log.APIKeyID != nil {
		apiKeyID = *log.APIKeyID
	}
	var groupID any
	if log.GroupID != nil {
		groupID = *log.GroupID
	}
	var latency any
	if log.UpstreamLatencyMS != nil {
		latency = *log.UpstreamLatencyMS
	}
	archiveStatus := strings.TrimSpace(log.ArchiveStatus)
	if archiveStatus == "" {
		archiveStatus = service.ContentModerationArchiveStatusNone
	}
	transport := strings.TrimSpace(log.Transport)
	if transport == "" {
		transport = "http"
	}
	requestStage := strings.TrimSpace(log.RequestStage)
	if requestStage == "" {
		requestStage = "http"
	}
	err = db.QueryRowContext(ctx, `
INSERT INTO content_moderation_logs (
    request_id, user_id, user_email, api_key_id, api_key_name, group_id, group_name,
    endpoint, provider, model, mode, action, flagged, highest_category, highest_score,
    category_scores, threshold_snapshot, input_excerpt, upstream_latency_ms, error,
    violation_count, auto_banned, email_sent, queue_delay_ms, matched_keyword,
    protocol, transport, request_stage, request_target, input_hash,
    cache_hit, decision_source, source_log_id, replay_of_input_hash,
    fragment_role, fragment_kind, context_class, fragment_path,
    cache_namespace, policy_version, model_profile, prompt_version,
    evidence_policy_version, keyword_tier, keyword_rule_id, evidence_mode,
    evidence_truncated, parser_status,
    archive_id, archive_version, archive_key_id, archive_plaintext_sha256,
    archive_plaintext_bytes, archive_status, archive_incomplete, archive_content_lost,
    archive_deleted_at, disposition_status, disposition_target,
    disposition_transitioned, legacy_source_job_id, legacy_status,
    legacy_event_count, legacy_metadata, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7,
    $8, $9, $10, $11, $12, $13, $14, $15,
    $16::jsonb, $17::jsonb, $18, $19, $20,
    $21, $22, $23, $24, $25,
    $26, $27, $28, $29, $30,
    $31, $32, $33, $34,
    $35, $36, $37, $38,
    $39, $40, $41, $42,
    $43, $44, $45, $46,
    $47, $48,
    NULLIF($49, '')::uuid, NULLIF($50, 0), $51, $52,
    $53, $54, $55, $56,
    $57, $58, $59,
    $60, $61, $62,
    $63, $64::jsonb, COALESCE($65, NOW())
) RETURNING id, created_at`,
		log.RequestID, userID, log.UserEmail, apiKeyID, log.APIKeyName, groupID, log.GroupName,
		log.Endpoint, log.Provider, log.Model, log.Mode, log.Action, log.Flagged, log.HighestCategory, log.HighestScore,
		string(categoryScores), string(thresholdSnapshot), log.InputExcerpt, latency, log.Error,
		log.ViolationCount, log.AutoBanned, log.EmailSent, nullableIntPtr(log.QueueDelayMS), log.MatchedKeyword,
		log.Protocol, transport, requestStage, log.RequestTarget, log.InputHash,
		log.CacheHit, log.DecisionSource, nullableInt64Ptr(log.SourceLogID), log.ReplayOfInputHash,
		log.FragmentRole, log.FragmentKind, log.ContextClass, log.FragmentPath,
		log.CacheNamespace, log.PolicyVersion, log.ModelProfile, log.PromptVersion,
		log.EvidencePolicyVersion, log.KeywordTier, log.KeywordRuleID, log.EvidenceMode,
		log.EvidenceTruncated, log.ParserStatus,
		log.ArchiveID, log.ArchiveVersion, log.ArchiveKeyID, nullableBytes(log.ArchiveSHA256),
		log.ArchiveBytes, archiveStatus, log.ArchiveIncomplete, log.ArchiveContentLost,
		log.ArchiveDeletedAt, log.DispositionStatus, log.DispositionTarget,
		log.DispositionTransitioned, nullableInt64Ptr(log.LegacySourceJobID), log.LegacyStatus,
		log.LegacyEventCount, string(legacyMetadata), nullableTime(log.CreatedAt),
	).Scan(&log.ID, &log.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert content moderation log: %w", err)
	}
	return nil
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func nullableInt64Ptr(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}

func (r *contentModerationRepository) ListLogs(ctx context.Context, filter service.ContentModerationLogFilter) ([]service.ContentModerationLog, *pagination.PaginationResult, error) {
	where, args := buildContentModerationLogWhere(filter)
	whereSQL := "WHERE " + strings.Join(where, " AND ")

	var total int64
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM content_moderation_logs l "+whereSQL, args...).Scan(&total); err != nil {
		return nil, nil, fmt.Errorf("count content moderation logs: %w", err)
	}

	params := filter.Pagination
	if params.Page <= 0 {
		params.Page = 1
	}
	if params.PageSize <= 0 {
		params.PageSize = 20
	}
	if params.PageSize > 100 {
		params.PageSize = 100
	}
	queryArgs := append([]any{}, args...)
	queryArgs = append(queryArgs, params.Limit(), params.Offset())
	rows, err := r.db.QueryContext(ctx, `
SELECT
    l.id, l.request_id, l.user_id, l.user_email, l.api_key_id, l.api_key_name, l.group_id, l.group_name,
    l.endpoint, l.provider, l.model, l.mode, l.action, l.flagged, l.highest_category, l.highest_score,
    l.category_scores, l.threshold_snapshot, l.input_excerpt, l.upstream_latency_ms, l.error,
    l.violation_count, l.auto_banned, l.email_sent, l.email_delivery_status,
    l.email_delivery_claimed_at, COALESCE(u.status, ''), l.queue_delay_ms, l.matched_keyword,
    l.protocol, l.transport, l.request_stage, l.request_target, l.input_hash,
    l.cache_hit, l.decision_source, l.source_log_id, l.replay_of_input_hash,
    l.fragment_role, l.fragment_kind, l.context_class, l.fragment_path,
    l.cache_namespace, l.policy_version, l.model_profile, l.prompt_version,
    l.evidence_policy_version, l.keyword_tier, l.keyword_rule_id, l.evidence_mode,
    l.evidence_truncated, l.parser_status,
    COALESCE(l.archive_id::text, ''), COALESCE(l.archive_version, 0), l.archive_key_id,
    l.archive_plaintext_bytes, l.archive_status, l.archive_incomplete, l.archive_content_lost,
    l.archive_deleted_at, l.disposition_status, l.disposition_target, l.disposition_transitioned,
    l.legacy_source_job_id, l.legacy_status, l.legacy_event_count,
    l.legacy_metadata, l.created_at
FROM content_moderation_logs l
LEFT JOIN users u ON u.id = l.user_id `+whereSQL+`
ORDER BY l.created_at DESC, l.id DESC
LIMIT $`+fmt.Sprint(len(queryArgs)-1)+` OFFSET $`+fmt.Sprint(len(queryArgs)),
		queryArgs...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("list content moderation logs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	items := make([]service.ContentModerationLog, 0)
	for rows.Next() {
		var item service.ContentModerationLog
		var userID, apiKeyID, groupID, latency, queueDelay, sourceLogID, legacySourceJobID sql.NullInt64
		var archiveDeletedAt, emailDeliveryClaimedAt sql.NullTime
		var scoresRaw, thresholdsRaw []byte
		var legacyMetadataRaw []byte
		if err := rows.Scan(
			&item.ID,
			&item.RequestID,
			&userID,
			&item.UserEmail,
			&apiKeyID,
			&item.APIKeyName,
			&groupID,
			&item.GroupName,
			&item.Endpoint,
			&item.Provider,
			&item.Model,
			&item.Mode,
			&item.Action,
			&item.Flagged,
			&item.HighestCategory,
			&item.HighestScore,
			&scoresRaw,
			&thresholdsRaw,
			&item.InputExcerpt,
			&latency,
			&item.Error,
			&item.ViolationCount,
			&item.AutoBanned,
			&item.EmailSent,
			&item.EmailDeliveryStatus,
			&emailDeliveryClaimedAt,
			&item.UserStatus,
			&queueDelay,
			&item.MatchedKeyword,
			&item.Protocol,
			&item.Transport,
			&item.RequestStage,
			&item.RequestTarget,
			&item.InputHash,
			&item.CacheHit,
			&item.DecisionSource,
			&sourceLogID,
			&item.ReplayOfInputHash,
			&item.FragmentRole,
			&item.FragmentKind,
			&item.ContextClass,
			&item.FragmentPath,
			&item.CacheNamespace,
			&item.PolicyVersion,
			&item.ModelProfile,
			&item.PromptVersion,
			&item.EvidencePolicyVersion,
			&item.KeywordTier,
			&item.KeywordRuleID,
			&item.EvidenceMode,
			&item.EvidenceTruncated,
			&item.ParserStatus,
			&item.ArchiveID,
			&item.ArchiveVersion,
			&item.ArchiveKeyID,
			&item.ArchiveBytes,
			&item.ArchiveStatus,
			&item.ArchiveIncomplete,
			&item.ArchiveContentLost,
			&archiveDeletedAt,
			&item.DispositionStatus,
			&item.DispositionTarget,
			&item.DispositionTransitioned,
			&legacySourceJobID,
			&item.LegacyStatus,
			&item.LegacyEventCount,
			&legacyMetadataRaw,
			&item.CreatedAt,
		); err != nil {
			return nil, nil, fmt.Errorf("scan content moderation log: %w", err)
		}
		if userID.Valid {
			v := userID.Int64
			item.UserID = &v
		}
		if apiKeyID.Valid {
			v := apiKeyID.Int64
			item.APIKeyID = &v
		}
		if groupID.Valid {
			v := groupID.Int64
			item.GroupID = &v
		}
		if latency.Valid {
			v := int(latency.Int64)
			item.UpstreamLatencyMS = &v
		}
		if queueDelay.Valid {
			v := int(queueDelay.Int64)
			item.QueueDelayMS = &v
		}
		if sourceLogID.Valid {
			value := sourceLogID.Int64
			item.SourceLogID = &value
		}
		if archiveDeletedAt.Valid {
			value := archiveDeletedAt.Time
			item.ArchiveDeletedAt = &value
		}
		if emailDeliveryClaimedAt.Valid {
			value := emailDeliveryClaimedAt.Time
			item.EmailDeliveryClaimedAt = &value
		}
		if legacySourceJobID.Valid {
			value := legacySourceJobID.Int64
			item.LegacySourceJobID = &value
		}
		item.CategoryScores = map[string]float64{}
		_ = json.Unmarshal(scoresRaw, &item.CategoryScores)
		item.ThresholdSnapshot = map[string]float64{}
		_ = json.Unmarshal(thresholdsRaw, &item.ThresholdSnapshot)
		item.LegacyMetadata = append(json.RawMessage(nil), legacyMetadataRaw...)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate content moderation logs: %w", err)
	}
	return items, paginationResultFromTotal(total, params), nil
}

func (r *contentModerationRepository) CountFlaggedByUserSince(ctx context.Context, userID int64, since time.Time, excludeCyberPolicy bool) (int, error) {
	return r.CountFlaggedByUserSinceExcludingArchive(ctx, userID, since, excludeCyberPolicy, "")
}

func (r *contentModerationRepository) CountFlaggedByUserSinceExcludingArchive(ctx context.Context, userID int64, since time.Time, excludeCyberPolicy bool, archiveID string) (int, error) {
	if userID <= 0 {
		return 0, nil
	}
	// SQL action literals must stay aligned with the service constants. Replay
	// rows prove no new violation and therefore do not contribute to auto-ban.
	var count int
	err := r.db.QueryRowContext(ctx, `
WITH last_auto_ban AS (
    SELECT MAX(created_at) AS at
    FROM content_moderation_logs
    WHERE user_id = $1 AND auto_banned = TRUE
)
SELECT COUNT(*)
FROM content_moderation_logs
WHERE user_id = $1
  AND flagged = TRUE
  AND action NOT IN ('hash_block', 'cache_block')
  AND cache_hit = FALSE
  AND decision_source <> 'cache_replay'
  AND ($3::bool IS FALSE OR action <> 'cyber_policy')
  AND ($4::text = '' OR archive_id IS NULL OR archive_id <> NULLIF($4::text, '')::uuid)
  AND created_at >= $2
  AND created_at > COALESCE((SELECT at FROM last_auto_ban), '-infinity'::timestamptz)
`, userID, since, excludeCyberPolicy, strings.TrimSpace(archiveID)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count user content moderation flagged logs: %w", err)
	}
	return count, nil
}

func (r *contentModerationRepository) UpdateLogDispositionByArchiveID(ctx context.Context, archiveID, status, target string, transitioned, autoBanned bool, violationCount int) error {
	archiveID = strings.TrimSpace(archiveID)
	if archiveID == "" {
		return errors.New("content moderation archive ID is required")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE content_moderation_logs
SET disposition_status = $2, disposition_target = $3,
    disposition_transitioned = $4, auto_banned = $5,
    violation_count = GREATEST(violation_count, $6)
WHERE archive_id = $1::uuid`, archiveID, status, target, transitioned, autoBanned, violationCount)
	if err != nil {
		return fmt.Errorf("update content moderation disposition: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read content moderation disposition update count: %w", err)
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *contentModerationRepository) ClaimLogEmailDelivery(ctx context.Context, logID int64) (service.ContentModerationEmailDeliveryClaim, error) {
	if logID <= 0 {
		return service.ContentModerationEmailDeliveryClaim{}, errors.New("content moderation log ID is required")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE content_moderation_logs
SET email_delivery_status = 'claimed', email_delivery_claimed_at = NOW()
WHERE id = $1 AND email_delivery_claimed_at IS NULL`, logID)
	if err != nil {
		return service.ContentModerationEmailDeliveryClaim{}, fmt.Errorf("claim content moderation email delivery: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return service.ContentModerationEmailDeliveryClaim{}, fmt.Errorf("read content moderation email claim count: %w", err)
	}
	if changed > 0 {
		return service.ContentModerationEmailDeliveryClaim{Exists: true, Claimed: true, Status: "claimed"}, nil
	}
	return r.readLogEmailDeliveryClaim(ctx, `SELECT email_delivery_status FROM content_moderation_logs WHERE id = $1`, logID)
}

func (r *contentModerationRepository) ClaimLogEmailDeliveryByArchiveID(ctx context.Context, archiveID string) (service.ContentModerationEmailDeliveryClaim, error) {
	archiveID = strings.TrimSpace(archiveID)
	if archiveID == "" {
		return service.ContentModerationEmailDeliveryClaim{}, errors.New("content moderation archive ID is required")
	}
	result, err := r.db.ExecContext(ctx, `
UPDATE content_moderation_logs
SET email_delivery_status = 'claimed', email_delivery_claimed_at = NOW()
WHERE archive_id = $1::uuid AND email_delivery_claimed_at IS NULL`, archiveID)
	if err != nil {
		return service.ContentModerationEmailDeliveryClaim{}, fmt.Errorf("claim content moderation archive email delivery: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return service.ContentModerationEmailDeliveryClaim{}, fmt.Errorf("read content moderation archive email claim count: %w", err)
	}
	if changed > 0 {
		return service.ContentModerationEmailDeliveryClaim{Exists: true, Claimed: true, Status: "claimed"}, nil
	}
	return r.readLogEmailDeliveryClaim(ctx, `SELECT email_delivery_status FROM content_moderation_logs WHERE archive_id = $1::uuid`, archiveID)
}

func (r *contentModerationRepository) readLogEmailDeliveryClaim(ctx context.Context, query string, arg any) (service.ContentModerationEmailDeliveryClaim, error) {
	var status string
	err := r.db.QueryRowContext(ctx, query, arg).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return service.ContentModerationEmailDeliveryClaim{}, nil
	}
	if err != nil {
		return service.ContentModerationEmailDeliveryClaim{}, fmt.Errorf("read content moderation email delivery claim: %w", err)
	}
	return service.ContentModerationEmailDeliveryClaim{Exists: true, Status: status}, nil
}

func (r *contentModerationRepository) CompleteLogEmailDelivery(ctx context.Context, logID int64, sent bool) error {
	if logID <= 0 {
		return errors.New("content moderation log ID is required")
	}
	return r.completeLogEmailDelivery(ctx, `
UPDATE content_moderation_logs
SET email_delivery_status = $2, email_sent = $3
WHERE id = $1 AND email_delivery_claimed_at IS NOT NULL`, logID, sent)
}

func (r *contentModerationRepository) CompleteLogEmailDeliveryByArchiveID(ctx context.Context, archiveID string, sent bool) error {
	archiveID = strings.TrimSpace(archiveID)
	if archiveID == "" {
		return errors.New("content moderation archive ID is required")
	}
	return r.completeLogEmailDelivery(ctx, `
UPDATE content_moderation_logs
SET email_delivery_status = $2, email_sent = $3
WHERE archive_id = $1::uuid AND email_delivery_claimed_at IS NOT NULL`, archiveID, sent)
}

func (r *contentModerationRepository) completeLogEmailDelivery(ctx context.Context, query string, arg any, sent bool) error {
	status := "failed"
	if sent {
		status = "sent"
	}
	result, err := r.db.ExecContext(ctx, query, arg, status, sent)
	if err != nil {
		return fmt.Errorf("complete content moderation email delivery: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read content moderation email completion count: %w", err)
	}
	if changed == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *contentModerationRepository) CleanupExpiredLogs(ctx context.Context, hitBefore time.Time, nonHitBefore time.Time) (*service.ContentModerationCleanupResult, error) {
	result := &service.ContentModerationCleanupResult{FinishedAt: time.Now()}
	if r == nil || r.db == nil {
		return result, nil
	}
	hitExec, err := r.db.ExecContext(ctx, `
DELETE FROM content_moderation_logs
WHERE flagged = TRUE
  AND created_at < $1
  AND archive_status = 'none'
  AND archive_incomplete = FALSE
  AND action NOT IN ('block', 'keyword_block', 'second_layer_block', 'cache_block', 'cyber_policy')
`, hitBefore)
	if err != nil {
		return nil, fmt.Errorf("delete expired hit content moderation logs: %w", err)
	}
	result.DeletedHit, _ = hitExec.RowsAffected()

	nonHitExec, err := r.db.ExecContext(ctx, `
DELETE FROM content_moderation_logs
WHERE flagged = FALSE AND created_at < $1
`, nonHitBefore)
	if err != nil {
		return nil, fmt.Errorf("delete expired non-hit content moderation logs: %w", err)
	}
	result.DeletedNonHit, _ = nonHitExec.RowsAffected()

	result.FinishedAt = time.Now()
	return result, nil
}

func nullableIntPtr(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func (r *contentModerationRepository) CreateLogWithArchive(ctx context.Context, log *service.ContentModerationLog, archive *service.ContentModerationEncryptedArchive) error {
	if log == nil || archive == nil || len(archive.Chunks) == 0 {
		return fmt.Errorf("content moderation encrypted archive is required")
	}
	applyArchiveMetadata(log, archive)
	var existingID int64
	var existingCreatedAt time.Time
	err := r.db.QueryRowContext(ctx, `
SELECT id, created_at FROM content_moderation_logs WHERE archive_id = $1::uuid`, archive.ArchiveID).Scan(&existingID, &existingCreatedAt)
	if err == nil {
		log.ID = existingID
		log.CreatedAt = existingCreatedAt
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("check existing content moderation archive: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin content moderation archive transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertContentModerationLog(ctx, tx, log); err != nil {
		return err
	}
	for _, chunk := range archive.Chunks {
		if _, err := tx.ExecContext(ctx, `
INSERT INTO content_moderation_log_chunks (
    log_id, archive_id, chunk_index, chunk_total, nonce, ciphertext, plaintext_bytes
) VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)`,
			log.ID, archive.ArchiveID, chunk.Index, chunk.Total, chunk.Nonce, chunk.Ciphertext, chunk.PlaintextBytes,
		); err != nil {
			return fmt.Errorf("insert content moderation archive chunk %d: %w", chunk.Index, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit content moderation archive transaction: %w", err)
	}
	return nil
}

func applyArchiveMetadata(log *service.ContentModerationLog, archive *service.ContentModerationEncryptedArchive) {
	log.ArchiveID = archive.ArchiveID
	log.ArchiveVersion = archive.Version
	log.ArchiveKeyID = archive.KeyID
	log.ArchiveSHA256 = append([]byte(nil), archive.PlaintextHash...)
	log.ArchiveBytes = archive.PlaintextSize
	log.ArchiveStatus = service.ContentModerationArchiveStatusAvailable
}

func (r *contentModerationRepository) GetArchive(ctx context.Context, logID int64) (*service.ContentModerationLog, *service.ContentModerationEncryptedArchive, error) {
	log := &service.ContentModerationLog{ID: logID}
	var archiveID sql.NullString
	var version sql.NullInt64
	var deletedAt sql.NullTime
	err := r.db.QueryRowContext(ctx, `
SELECT request_id, action, input_hash, COALESCE(archive_id::text, ''),
       COALESCE(archive_version, 0), archive_key_id, archive_plaintext_sha256,
       archive_plaintext_bytes, archive_status, archive_incomplete,
       archive_content_lost, archive_deleted_at, disposition_status,
       disposition_target, created_at
FROM content_moderation_logs WHERE id = $1`, logID).Scan(
		&log.RequestID, &log.Action, &log.InputHash, &archiveID, &version,
		&log.ArchiveKeyID, &log.ArchiveSHA256, &log.ArchiveBytes,
		&log.ArchiveStatus, &log.ArchiveIncomplete, &log.ArchiveContentLost,
		&deletedAt, &log.DispositionStatus, &log.DispositionTarget, &log.CreatedAt,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("get content moderation archive metadata: %w", err)
	}
	if !archiveID.Valid || archiveID.String == "" || !version.Valid || version.Int64 <= 0 || deletedAt.Valid {
		return log, nil, sql.ErrNoRows
	}
	archive := &service.ContentModerationEncryptedArchive{
		ArchiveID: archiveID.String, Version: int(version.Int64), KeyID: log.ArchiveKeyID,
		PlaintextHash: append([]byte(nil), log.ArchiveSHA256...), PlaintextSize: log.ArchiveBytes,
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT chunk_index, chunk_total, nonce, ciphertext, plaintext_bytes
FROM content_moderation_log_chunks WHERE log_id = $1 ORDER BY chunk_index`, logID)
	if err != nil {
		return nil, nil, fmt.Errorf("list content moderation archive chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var chunk service.ContentModerationArchiveChunk
		if err := rows.Scan(&chunk.Index, &chunk.Total, &chunk.Nonce, &chunk.Ciphertext, &chunk.PlaintextBytes); err != nil {
			return nil, nil, fmt.Errorf("scan content moderation archive chunk: %w", err)
		}
		archive.Chunks = append(archive.Chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate content moderation archive chunks: %w", err)
	}
	return log, archive, nil
}

func (r *contentModerationRepository) RecordArchiveAccess(ctx context.Context, access service.ContentModerationArchiveAccess) error {
	_, err := r.db.ExecContext(ctx, `
INSERT INTO content_moderation_archive_access_audits (
    log_id, archive_id, actor_user_id, action, request_id, result, bytes_served, detail
) SELECT id, archive_id, NULLIF($2, 0), $3, $4, $5, $6, $7
  FROM content_moderation_logs WHERE id = $1`,
		access.LogID, access.ActorUserID, access.Action, access.RequestID,
		access.Result, access.BytesServed, access.Detail,
	)
	if err != nil {
		return fmt.Errorf("record content moderation archive access: %w", err)
	}
	return nil
}

func (r *contentModerationRepository) DeleteArchive(ctx context.Context, access service.ContentModerationArchiveAccess) (bool, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin content moderation archive delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `
UPDATE content_moderation_logs
SET archive_status = 'deleted', archive_deleted_at = NOW()
WHERE id = $1 AND archive_id IS NOT NULL AND archive_deleted_at IS NULL`, access.LogID)
	if err != nil {
		return false, fmt.Errorf("mark content moderation archive deleted: %w", err)
	}
	changed, _ := result.RowsAffected()
	if _, err := tx.ExecContext(ctx, `DELETE FROM content_moderation_log_chunks WHERE log_id = $1`, access.LogID); err != nil {
		return false, fmt.Errorf("delete content moderation archive chunks: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO content_moderation_archive_access_audits (
    log_id, archive_id, actor_user_id, action, request_id, result, bytes_served, detail
) SELECT id, archive_id, NULLIF($2, 0), 'delete', $3, $4, 0, $5
  FROM content_moderation_logs WHERE id = $1`,
		access.LogID, access.ActorUserID, access.RequestID, access.Result, access.Detail,
	); err != nil {
		return false, fmt.Errorf("record content moderation archive deletion: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit content moderation archive delete: %w", err)
	}
	return changed > 0, nil
}

func (r *contentModerationRepository) ReferencedArchiveKeyIDs(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `
SELECT DISTINCT archive_key_id FROM content_moderation_logs
WHERE archive_id IS NOT NULL AND archive_deleted_at IS NULL AND archive_key_id <> ''
ORDER BY archive_key_id`)
	if err != nil {
		return nil, fmt.Errorf("list referenced content moderation archive keys: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *contentModerationRepository) DisableUserIfActive(ctx context.Context, userID int64) (bool, error) {
	result, err := r.db.ExecContext(ctx, `
UPDATE users SET status = 'disabled', updated_at = NOW()
WHERE id = $1 AND status = 'active' AND deleted_at IS NULL`, userID)
	if err != nil {
		return false, fmt.Errorf("disable cyber policy user: %w", err)
	}
	changed, err := result.RowsAffected()
	return changed > 0, err
}

func (r *contentModerationRepository) DisableAPIKeyIfActive(ctx context.Context, apiKeyID int64) (string, bool, error) {
	var credential string
	err := r.db.QueryRowContext(ctx, `
UPDATE api_keys SET status = 'disabled', updated_at = NOW()
WHERE id = $1 AND status = 'active' AND deleted_at IS NULL
RETURNING key`, apiKeyID).Scan(&credential)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("disable cyber policy API key: %w", err)
	}
	return credential, true, nil
}

func buildContentModerationLogWhere(filter service.ContentModerationLogFilter) ([]string, []any) {
	where := []string{"l.id IS NOT NULL"}
	args := make([]any, 0)
	add := func(expr string, value any) {
		args = append(args, value)
		where = append(where, fmt.Sprintf(expr, len(args)))
	}
	switch strings.ToLower(strings.TrimSpace(filter.Result)) {
	case "hit", "flagged":
		where = append(where, "l.flagged = TRUE")
	case "blocked", "block":
		where = append(where, "l.action IN ('block', 'keyword_block', 'hash_block', 'second_layer_block', 'cache_block', 'cyber_policy')")
	case service.ContentModerationLogResultCyberPolicy:
		where = append(where, "l.action = 'cyber_policy'")
	case service.ContentModerationLogResultContentBlocked:
		where = append(where, "l.action IN ('block', 'keyword_block', 'hash_block', 'second_layer_block', 'cache_block')")
	case service.ContentModerationLogResultRiskyShadow:
		where = append(where, "l.action IN ('second_layer_shadow', 'whitelist_shadow') AND COALESCE(BTRIM(l.highest_category), '') <> ''")
	case "pass", "allow":
		where = append(where, "l.flagged = FALSE AND l.error = ''")
	case "error":
		where = append(where, "l.error <> ''")
	}
	if filter.LogID != nil {
		add("l.id = $%d", *filter.LogID)
	}
	if filter.GroupID != nil {
		add("l.group_id = $%d", *filter.GroupID)
	}
	if endpoint := strings.TrimSpace(filter.Endpoint); endpoint != "" {
		add("l.endpoint = $%d", endpoint)
	}
	if contextClass := strings.TrimSpace(filter.ContextClass); contextClass != "" {
		add("l.context_class = $%d", contextClass)
	}
	if profile := strings.TrimSpace(filter.ModelProfile); profile != "" {
		add("l.model_profile = $%d", profile)
	}
	if source := strings.TrimSpace(filter.DecisionSource); source != "" {
		add("l.decision_source = $%d", source)
	}
	if search := strings.TrimSpace(filter.Search); search != "" {
		like := "%" + search + "%"
		args = append(args, like, like, like, like, like)
		idx := len(args) - 4
		where = append(where, fmt.Sprintf("(l.request_id ILIKE $%d OR l.user_email ILIKE $%d OR l.api_key_name ILIKE $%d OR l.model ILIKE $%d OR l.input_excerpt ILIKE $%d)", idx, idx+1, idx+2, idx+3, idx+4))
	}
	if filter.From != nil && !filter.From.IsZero() {
		add("l.created_at >= $%d", *filter.From)
	}
	if filter.To != nil && !filter.To.IsZero() {
		add("l.created_at <= $%d", *filter.To)
	}
	return where, args
}
