//go:build integration

package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/require"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

func TestUnifiedRiskMigrationAcceptance(t *testing.T) {
	ctx := context.Background()
	require.NotNil(t, integrationPostgres)
	require.NotEmpty(t, integrationDSN)

	rdb := testRedis(t)
	require.NoError(t, rdb.Set(ctx, "unified-risk-migration:gate", "ready", 0).Err())
	gate, err := rdb.Get(ctx, "unified-risk-migration:gate").Result()
	require.NoError(t, err)
	require.Equal(t, "ready", gate)

	adminDB := openUnifiedRiskIntegrationDatabase(t, "postgres")
	defer func() { _ = adminDB.Close() }()

	badDB, badName := newUnifiedRiskIntegrationDatabase(t, adminDB)
	seedUnifiedRiskLegacyRows(t, badDB, false)
	cipher := unifiedRiskIntegrationCipher(t)
	missingProof := filepath.Join(t.TempDir(), "missing-proof.json")
	badMigrator := NewUnifiedRiskMigrator(badDB, cipher, missingProof)
	_, err = badMigrator.Prepare(ctx)
	require.ErrorIs(t, err, ErrUnifiedRiskBackupProof)
	assertUnifiedRiskNoMigrationMutation(t, badDB)
	dropUnifiedRiskIntegrationDatabase(t, adminDB, badDB, badName)

	sourceDB, sourceName := newUnifiedRiskIntegrationDatabase(t, adminDB)
	seeded := seedUnifiedRiskLegacyRows(t, sourceDB, true)
	backup := createUnifiedRiskIntegrationBackup(t, adminDB, sourceDB, sourceName)
	migrator := NewUnifiedRiskMigrator(sourceDB, cipher, backup.proofPath)

	proof, err := migrator.Verify(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), proof.SourceJobCount)
	require.Equal(t, int64(4), proof.SourceEventCount)
	require.True(t, proof.ListVerified)
	require.True(t, proof.RestoreVerified)

	prepared, err := migrator.Prepare(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(3), prepared.StagedJobCount)
	require.Equal(t, int64(4), prepared.StagedEventCount)
	require.Equal(t, int64(2), prepared.ArchivedHitCount)
	require.Equal(t, map[string]int64{"done": 2, "failed": 1}, prepared.StatusCounts)
	assertUnifiedRiskStageHasNoPlaintext(t, sourceDB, seeded.allPrompts)

	preparedAgain, err := migrator.Prepare(ctx)
	require.NoError(t, err)
	require.Equal(t, prepared.StagedJobCount, preparedAgain.StagedJobCount)
	require.Equal(t, prepared.StagedEventCount, preparedAgain.StagedEventCount)
	assertUnifiedRiskStageHasNoPlaintext(t, sourceDB, seeded.allPrompts)

	deltaJobID, deltaEventID, deltaPrompt := insertUnifiedRiskDelta(t, sourceDB)
	preparedDelta, err := migrator.Prepare(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(4), preparedDelta.StagedJobCount)
	require.Equal(t, int64(5), preparedDelta.StagedEventCount)
	require.Equal(t, int64(3), preparedDelta.ArchivedHitCount)
	require.GreaterOrEqual(t, preparedDelta.WatermarkJobID, deltaJobID)
	require.GreaterOrEqual(t, preparedDelta.WatermarkEventID, deltaEventID)
	assertUnifiedRiskStageHasNoPlaintext(t, sourceDB, append(seeded.allPrompts, deltaPrompt))

	rollbackOptions := UnifiedRiskFinalizeOptions{
		MaintenanceMode: true,
		beforeValidation: func(ctx context.Context, tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `
UPDATE unified_risk_migration_stage
SET source_event_count = source_event_count + 1
WHERE source_job_id = (SELECT MIN(source_job_id) FROM unified_risk_migration_stage)`)
			return err
		},
	}
	_, err = migrator.Finalize(ctx, rollbackOptions)
	require.ErrorIs(t, err, ErrUnifiedRiskValidation)
	assertUnifiedRiskLegacyObjectsPresent(t, sourceDB, true, true)
	assertUnifiedRiskNoMergedLogs(t, sourceDB)

	final, err := migrator.Finalize(ctx, UnifiedRiskFinalizeOptions{MaintenanceMode: true})
	require.NoError(t, err)
	require.Equal(t, int64(4), final.JobCount)
	require.Equal(t, int64(5), final.EventCount)
	require.Equal(t, int64(3), final.ArchivedHitCount)
	require.Equal(t, map[string]int64{"done": 3, "failed": 1}, final.StatusCounts)
	assertUnifiedRiskLegacyObjectsPresent(t, sourceDB, false, false)
	assertUnifiedRiskFinalRows(t, sourceDB, cipher, seeded, deltaPrompt)

	finalAgain, err := migrator.Finalize(ctx, UnifiedRiskFinalizeOptions{MaintenanceMode: true})
	require.NoError(t, err)
	require.Equal(t, final.RunID, finalAgain.RunID)

	restoreUnifiedRiskIntegrationBackup(t, sourceName, backup.containerPath)
	assertUnifiedRiskLegacyObjectsPresent(t, sourceDB, true, false)
	var restoredJobs, restoredEvents int64
	require.NoError(t, sourceDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_audit_jobs`).Scan(&restoredJobs))
	require.NoError(t, sourceDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_audit_events`).Scan(&restoredEvents))
	require.Equal(t, int64(3), restoredJobs)
	require.Equal(t, int64(4), restoredEvents)
	assertUnifiedRiskRestoredSchemaCompatible(t, sourceDB)
	assertUnifiedRiskFinalRows(t, sourceDB, cipher, seeded, deltaPrompt)
	dropUnifiedRiskIntegrationDatabase(t, adminDB, sourceDB, sourceName)
}

type unifiedRiskSeededData struct {
	jobIDs          []int64
	allPrompts      []string
	archivedPrompts []string
}

type unifiedRiskIntegrationBackup struct {
	proofPath     string
	containerPath string
}

func openUnifiedRiskIntegrationDatabase(t *testing.T, database string) *sql.DB {
	t.Helper()
	dsn := replaceUnifiedRiskDatabase(integrationDSN, database)
	db, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	require.NoError(t, db.PingContext(context.Background()))
	return db
}

func replaceUnifiedRiskDatabase(dsn, database string) string {
	queryIndex := strings.IndexByte(dsn, '?')
	query := ""
	base := dsn
	if queryIndex >= 0 {
		base = dsn[:queryIndex]
		query = dsn[queryIndex:]
	}
	lastSlash := strings.LastIndexByte(base, '/')
	if lastSlash < 0 {
		panic("integration DSN has no database path")
	}
	return base[:lastSlash+1] + database + query
}

func newUnifiedRiskIntegrationDatabase(t *testing.T, adminDB *sql.DB) (*sql.DB, string) {
	t.Helper()
	name := "risk_migration_" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
	_, err := adminDB.ExecContext(context.Background(), `CREATE DATABASE `+pq.QuoteIdentifier(name))
	require.NoError(t, err)
	db := openUnifiedRiskIntegrationDatabase(t, name)
	require.NoError(t, ApplyMigrations(context.Background(), db))
	return db, name
}

func dropUnifiedRiskIntegrationDatabase(t *testing.T, adminDB, database *sql.DB, name string) {
	t.Helper()
	require.NoError(t, database.Close())
	_, err := adminDB.ExecContext(context.Background(), `DROP DATABASE `+pq.QuoteIdentifier(name)+` WITH (FORCE)`)
	require.NoError(t, err)
}

func seedUnifiedRiskLegacyRows(t *testing.T, db *sql.DB, includeEvents bool) unifiedRiskSeededData {
	t.Helper()
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `
INSERT INTO settings (key, value) VALUES ('prompt_audit_config', '{"enabled":true}')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	require.NoError(t, err)

	statuses := []string{"done", "failed", "done"}
	data := unifiedRiskSeededData{}
	for index, status := range statuses {
		errorCode := ""
		errorMessage := ""
		if status == "failed" {
			errorCode = "guard_timeout"
			errorMessage = "historical timeout"
		}
		var jobID int64
		err := db.QueryRowContext(ctx, `
INSERT INTO prompt_audit_jobs (
    request_id, username_snapshot, user_email_snapshot, api_key_name_snapshot,
    group_name, provider, endpoint, protocol, model, prompt_hash,
    redacted_preview, prompt_length, message_count, stage, execution_mode,
    config_version, status, attempts, max_attempts, last_error_code,
    last_error_message, processed_at, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, 'openai', '/v1/chat/completions', 'openai',
          'gpt-5.6', $6, $7, $8, 1, 'http', 'async_audit', 7, $9,
          $10, 3, $11, $12, NOW(),
          NOW() - ($13::int * interval '1 minute'), NOW()) RETURNING id`,
			fmt.Sprintf("legacy-request-%d", index+1), strings.Repeat("u", 255),
			strings.Repeat("e", 300), strings.Repeat("k", 180), "GPT legacy",
			fmt.Sprintf("hash-%d", index+1), fmt.Sprintf("redacted-%d", index+1),
			20+index, status, index, errorCode, errorMessage, index+1).Scan(&jobID)
		require.NoError(t, err)
		data.jobIDs = append(data.jobIDs, jobID)
	}
	if !includeEvents {
		return data
	}

	events := []struct {
		jobIndex int
		decision string
		risk     string
		action   string
		prompt   string
		hit      bool
	}{
		{0, "critical", "critical", "Block", "LEGACY_SECRET_CRITICAL_001", true},
		{0, "flag", "high", "Warn", "LEGACY_SECRET_SECOND_EVENT_002", true},
		{1, "critical", "critical", "Block", "LEGACY_FAILED_JOB_PROMPT_003", true},
		{2, "pass", "low", "Allow", "LEGACY_PASS_PROMPT_004", false},
	}
	for index, event := range events {
		_, err := db.ExecContext(ctx, `
INSERT INTO prompt_audit_events (
    job_id, request_id, username_snapshot, user_email_snapshot,
    api_key_name_snapshot, group_name, provider, endpoint, protocol, model,
    prompt_hash, redacted_preview, stage, decision, risk_level, action,
    categories, matched_scanners, scanner_scores, scanner_evidence,
    scanner_backend, scanner_version, guard_endpoint_id, policy_id,
    policy_version, config_version, chunk_total, latency_ms, full_prompt,
    created_at
) VALUES ($1, $2, 'legacy-user', 'legacy@example.invalid', 'legacy-key',
          'GPT legacy', 'openai', '/v1/chat/completions', 'openai', 'gpt-5.6',
          $3, $4, 'http', $5, $6, $7, $8::jsonb, $9::jsonb, $10::jsonb,
          $11::jsonb, 'qwen3guard-openai', 'legacy-v1', 'ep-1', 'policy-1',
          1, 7, 1, 15, $12, NOW() + ($13::int * interval '1 second'))`,
			data.jobIDs[event.jobIndex], fmt.Sprintf("legacy-event-%d", index+1),
			fmt.Sprintf("event-hash-%d", index+1), fmt.Sprintf("event-redacted-%d", index+1),
			event.decision, event.risk, event.action, `["violence"]`, `["scanner-a"]`,
			`{"violence":0.9}`, `{"violence":"redacted evidence"}`, event.prompt, index+1)
		require.NoError(t, err)
		data.allPrompts = append(data.allPrompts, event.prompt)
		if event.hit {
			data.archivedPrompts = append(data.archivedPrompts, event.prompt)
		}
	}
	return data
}

func insertUnifiedRiskDelta(t *testing.T, db *sql.DB) (int64, int64, string) {
	t.Helper()
	ctx := context.Background()
	prompt := "LEGACY_DELTA_PROMPT_005"
	var jobID, eventID int64
	require.NoError(t, db.QueryRowContext(ctx, `
INSERT INTO prompt_audit_jobs (
    request_id, group_name, provider, endpoint, protocol, model, prompt_hash,
    redacted_preview, prompt_length, message_count, status, execution_mode,
    processed_at, created_at, updated_at
) VALUES ('legacy-delta', 'GPT delta', 'openai', '/v1/responses', 'responses',
          'gpt-5.6', 'delta-hash', 'delta-redacted', 22, 1, 'done',
          'blocking', NOW(), NOW(), NOW()) RETURNING id`).Scan(&jobID))
	require.NoError(t, db.QueryRowContext(ctx, `
INSERT INTO prompt_audit_events (
    job_id, request_id, group_name, provider, endpoint, protocol, model,
    prompt_hash, redacted_preview, decision, risk_level, action, categories,
    matched_scanners, scanner_scores, scanner_evidence, full_prompt
) VALUES ($1, 'legacy-delta', 'GPT delta', 'openai', '/v1/responses',
          'responses', 'gpt-5.6', 'delta-hash', 'delta-redacted', 'critical',
          'critical', 'Block', '["cyber"]'::jsonb, '["scanner-b"]'::jsonb,
          '{"cyber":1}'::jsonb, '{}'::jsonb, $2) RETURNING id`, jobID, prompt).Scan(&eventID))
	return jobID, eventID, prompt
}

func unifiedRiskIntegrationCipher(t *testing.T) *service.ContentModerationArchiveCipher {
	t.Helper()
	key := []byte("0123456789abcdef0123456789abcdef")
	ring := service.ContentModerationArchiveKeyRing{
		CurrentKeyID: "integration-k1",
		Keys:         map[string]string{"integration-k1": base64.StdEncoding.EncodeToString(key)},
	}
	raw, err := json.Marshal(ring)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "key-ring.json")
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return service.NewContentModerationArchiveCipher(
		service.NewContentModerationArchiveKeyRingFile(path), 31)
}

func createUnifiedRiskIntegrationBackup(t *testing.T, adminDB, sourceDB *sql.DB, sourceName string) unifiedRiskIntegrationBackup {
	t.Helper()
	ctx := context.Background()
	containerPath := "/tmp/" + sourceName + ".dump"
	exitCode, output, err := integrationPostgres.Exec(ctx, []string{
		"pg_dump", "--format=custom", "--no-owner", "--no-privileges",
		"--host=127.0.0.1", "--username=postgres",
		"--table=public.prompt_audit_jobs", "--table=public.prompt_audit_events",
		"--dbname=" + sourceName, "--file=" + containerPath,
	}, tcexec.Multiplexed())
	require.NoError(t, err)
	toolOutput, readErr := io.ReadAll(output)
	require.NoError(t, readErr)
	require.Equalf(t, 0, exitCode, "pg_dump output: %s", toolOutput)
	t.Cleanup(func() {
		_, _, _ = integrationPostgres.Exec(
			context.Background(), []string{"rm", "-f", containerPath}, tcexec.Multiplexed())
	})

	archiveReader, err := integrationPostgres.CopyFileFromContainer(ctx, containerPath)
	require.NoError(t, err)
	archivePath := filepath.Join(t.TempDir(), "prompt-audit.dump")
	archiveFile, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, copyErr := io.Copy(archiveFile, archiveReader)
	require.NoError(t, copyErr)
	require.NoError(t, archiveReader.Close())
	require.NoError(t, archiveFile.Sync())
	require.NoError(t, archiveFile.Close())

	listCode, listReader, err := integrationPostgres.Exec(
		ctx, []string{"pg_restore", "--list", containerPath}, tcexec.Multiplexed())
	require.NoError(t, err)
	list, err := io.ReadAll(listReader)
	require.NoError(t, err)
	require.Equal(t, 0, listCode)
	require.Contains(t, string(list), "TABLE public prompt_audit_jobs")
	require.Contains(t, string(list), "TABLE public prompt_audit_events")
	require.Contains(t, string(list), "INDEX public idx_prompt_audit_jobs_schedule")
	require.Contains(t, string(list), "INDEX public idx_prompt_audit_events_job")

	restoreDB, restoreName := newUnifiedRiskIntegrationDatabase(t, adminDB)
	require.NoError(t, restoreDB.Close())
	restoreCode, restoreReader, err := integrationPostgres.Exec(ctx, []string{
		"pg_restore", "--clean", "--if-exists", "--exit-on-error", "--no-owner", "--no-privileges",
		"--host=127.0.0.1", "--username=postgres",
		"--dbname=" + restoreName, containerPath,
	}, tcexec.Multiplexed())
	require.NoError(t, err)
	restoreOutput, err := io.ReadAll(restoreReader)
	require.NoError(t, err)
	require.Equalf(t, 0, restoreCode, "pg_restore output: %s", restoreOutput)
	restoreDB = openUnifiedRiskIntegrationDatabase(t, restoreName)
	var restoredJobs, restoredEvents int64
	require.NoError(t, restoreDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_audit_jobs`).Scan(&restoredJobs))
	require.NoError(t, restoreDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_audit_events`).Scan(&restoredEvents))
	require.Equal(t, int64(3), restoredJobs)
	require.Equal(t, int64(4), restoredEvents)
	dropUnifiedRiskIntegrationDatabase(t, adminDB, restoreDB, restoreName)

	archiveRaw, err := os.ReadFile(archivePath)
	require.NoError(t, err)
	require.True(t, bytes.HasPrefix(archiveRaw, []byte("PGDMP")))
	archiveDigest := sha256.Sum256(archiveRaw)
	listDigest := sha256.Sum256(list)
	var database, systemIdentifier string
	var jobCount, eventCount, maxJobID, maxEventID int64
	require.NoError(t, sourceDB.QueryRowContext(ctx, `
SELECT current_database(), system_identifier::text,
       (SELECT COUNT(*) FROM prompt_audit_jobs),
       (SELECT COUNT(*) FROM prompt_audit_events),
       (SELECT COALESCE(MAX(id), 0) FROM prompt_audit_jobs),
       (SELECT COALESCE(MAX(id), 0) FROM prompt_audit_events)
FROM pg_control_system()`).Scan(
		&database, &systemIdentifier, &jobCount, &eventCount, &maxJobID, &maxEventID))
	statusCounts := map[string]int64{"done": 2, "failed": 1}
	proof := UnifiedRiskBackupProof{
		Version: UnifiedRiskBackupProofVersion, VerifiedAt: time.Now().UTC(),
		SourceDatabase: database, SourceSystemIdentifier: systemIdentifier,
		RestoreDatabase: restoreName, ArchivePath: archivePath,
		ArchiveSHA256: hex.EncodeToString(archiveDigest[:]), ArchiveBytes: int64(len(archiveRaw)),
		RestoreListSHA256: hex.EncodeToString(listDigest[:]),
		SourceJobCount:    jobCount, SourceEventCount: eventCount,
		SourceMaxJobID: maxJobID, SourceMaxEventID: maxEventID,
		SourceStatusCounts: statusCounts, RestoredJobCount: restoredJobs,
		RestoredEventCount: restoredEvents, RestoredStatusCounts: statusCounts,
		ListVerified: true, RestoreVerified: true,
	}
	proofRaw, err := json.Marshal(proof)
	require.NoError(t, err)
	proofPath := filepath.Join(filepath.Dir(archivePath), "backup-proof.json")
	require.NoError(t, os.WriteFile(proofPath, proofRaw, 0o600))
	return unifiedRiskIntegrationBackup{proofPath: proofPath, containerPath: containerPath}
}

func restoreUnifiedRiskIntegrationBackup(t *testing.T, database, containerPath string) {
	t.Helper()
	exitCode, output, err := integrationPostgres.Exec(context.Background(), []string{
		"pg_restore", "--clean", "--if-exists", "--exit-on-error", "--no-owner", "--no-privileges",
		"--host=127.0.0.1", "--username=postgres",
		"--dbname=" + database, containerPath,
	}, tcexec.Multiplexed())
	require.NoError(t, err)
	toolOutput, err := io.ReadAll(output)
	require.NoError(t, err)
	require.Equalf(t, 0, exitCode, "rollback pg_restore output: %s", toolOutput)
}

func assertUnifiedRiskNoMigrationMutation(t *testing.T, db *sql.DB) {
	t.Helper()
	var stageRows, logs int64
	require.NoError(t, db.QueryRowContext(
		context.Background(), `SELECT COUNT(*) FROM unified_risk_migration_stage`).Scan(&stageRows))
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM content_moderation_logs WHERE legacy_source_job_id IS NOT NULL`).Scan(&logs))
	require.Zero(t, stageRows)
	require.Zero(t, logs)
	assertUnifiedRiskLegacyObjectsPresent(t, db, true, true)
}

func assertUnifiedRiskStageHasNoPlaintext(t *testing.T, db *sql.DB, prompts []string) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), `
SELECT log_payload::text, COALESCE(archive_payload::text, '')
FROM unified_risk_migration_stage`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var logPayload, archivePayload string
		require.NoError(t, rows.Scan(&logPayload, &archivePayload))
		for _, prompt := range prompts {
			require.NotContains(t, logPayload, prompt)
			require.NotContains(t, archivePayload, prompt)
		}
	}
	require.NoError(t, rows.Err())
}

func assertUnifiedRiskLegacyObjectsPresent(t *testing.T, db *sql.DB, expectedTables, expectedConfig bool) {
	t.Helper()
	var jobs, events bool
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT to_regclass('public.prompt_audit_jobs') IS NOT NULL,
       to_regclass('public.prompt_audit_events') IS NOT NULL`).Scan(&jobs, &events))
	require.Equal(t, expectedTables, jobs)
	require.Equal(t, expectedTables, events)
	var configCount int64
	require.NoError(t, db.QueryRowContext(
		context.Background(), `SELECT COUNT(*) FROM settings WHERE key = 'prompt_audit_config'`).Scan(&configCount))
	if expectedConfig {
		require.Equal(t, int64(1), configCount)
	} else {
		require.Zero(t, configCount)
	}
	if expectedTables {
		return
	}
	var indexes int64
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM pg_indexes WHERE indexname LIKE 'idx_prompt_audit_%'`).Scan(&indexes))
	require.Zero(t, indexes)
}

func assertUnifiedRiskRestoredSchemaCompatible(t *testing.T, db *sql.DB) {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var jobID, eventID int64
	require.NoError(t, tx.QueryRowContext(context.Background(), `
INSERT INTO prompt_audit_jobs (request_id, status, execution_mode)
VALUES ('rollback-compatibility', 'done', 'async_audit') RETURNING id`).Scan(&jobID))
	require.NoError(t, tx.QueryRowContext(context.Background(), `
INSERT INTO prompt_audit_events (job_id, request_id, decision, risk_level, action, full_prompt)
VALUES ($1, 'rollback-compatibility', 'pass', 'low', 'Allow', 'compatibility prompt')
RETURNING id`, jobID).Scan(&eventID))
	var prompt string
	require.NoError(t, tx.QueryRowContext(context.Background(), `
SELECT full_prompt FROM prompt_audit_events WHERE id = $1`, eventID).Scan(&prompt))
	require.Equal(t, "compatibility prompt", prompt)
}

func assertUnifiedRiskNoMergedLogs(t *testing.T, db *sql.DB) {
	t.Helper()
	var count int64
	require.NoError(t, db.QueryRowContext(context.Background(), `
SELECT COUNT(*) FROM content_moderation_logs WHERE legacy_source_job_id IS NOT NULL`).Scan(&count))
	require.Zero(t, count)
}

func assertUnifiedRiskFinalRows(t *testing.T, db *sql.DB, cipher *service.ContentModerationArchiveCipher, seeded unifiedRiskSeededData, deltaPrompt string) {
	t.Helper()
	ctx := context.Background()
	var jobCount, eventCount, flaggedCount, archivedCount, nonHitArchived int64
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(legacy_event_count), 0),
       COUNT(*) FILTER (WHERE flagged), COUNT(*) FILTER (WHERE archive_id IS NOT NULL),
       COUNT(*) FILTER (WHERE NOT flagged AND archive_id IS NOT NULL)
FROM content_moderation_logs WHERE legacy_source_job_id IS NOT NULL`).Scan(
		&jobCount, &eventCount, &flaggedCount, &archivedCount, &nonHitArchived))
	require.Equal(t, int64(4), jobCount)
	require.Equal(t, int64(5), eventCount)
	require.Equal(t, int64(3), flaggedCount)
	require.Equal(t, flaggedCount, archivedCount)
	require.Zero(t, nonHitArchived)

	var done, failed int64
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM content_moderation_logs
WHERE legacy_source_job_id IS NOT NULL AND legacy_status = 'done'`).Scan(&done))
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM content_moderation_logs
WHERE legacy_source_job_id IS NOT NULL AND legacy_status = 'failed'`).Scan(&failed))
	require.Equal(t, int64(3), done)
	require.Equal(t, int64(1), failed)

	var duplicateCount int64
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
    SELECT legacy_source_job_id FROM content_moderation_logs
    WHERE legacy_source_job_id IS NOT NULL
    GROUP BY legacy_source_job_id HAVING COUNT(*) <> 1
) duplicates`).Scan(&duplicateCount))
	require.Zero(t, duplicateCount)

	rows, err := db.QueryContext(ctx, `
SELECT id FROM content_moderation_logs
WHERE legacy_source_job_id IS NOT NULL AND archive_id IS NOT NULL
ORDER BY legacy_source_job_id`)
	require.NoError(t, err)
	logIDs := make([]int64, 0)
	for rows.Next() {
		var logID int64
		require.NoError(t, rows.Scan(&logID))
		logIDs = append(logIDs, logID)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	repo := NewContentModerationRepository(db).(*contentModerationRepository)
	decryptedPrompts := make(map[string]bool)
	for _, logID := range logIDs {
		log, archive, err := repo.GetArchive(ctx, logID)
		require.NoError(t, err)
		require.True(t, log.ArchiveIncomplete)
		plaintext, err := cipher.Decrypt(archive)
		require.NoError(t, err)
		var envelope service.ContentModerationArchiveEnvelope
		require.NoError(t, json.Unmarshal(plaintext, &envelope))
		require.True(t, envelope.Incomplete)
		require.Equal(t, "legacy_prompt_only", envelope.Request.Stage)
		require.NotNil(t, envelope.LegacyPromptAudit)
		for _, event := range envelope.LegacyPromptAudit.Events {
			decoded, err := base64.StdEncoding.DecodeString(event.FullPromptBase64)
			require.NoError(t, err)
			decryptedPrompts[string(decoded)] = true
		}
	}
	for _, prompt := range append(seeded.archivedPrompts, deltaPrompt) {
		require.Truef(t, decryptedPrompts[prompt], "missing decrypted legacy prompt %q", prompt)
	}

	var ordinaryPayload string
	require.NoError(t, db.QueryRowContext(ctx, `
SELECT row_to_json(l)::text FROM content_moderation_logs l
WHERE legacy_source_job_id IS NOT NULL AND flagged = FALSE LIMIT 1`).Scan(&ordinaryPayload))
	for _, prompt := range append(seeded.allPrompts, deltaPrompt) {
		require.NotContains(t, ordinaryPayload, prompt)
	}
}
