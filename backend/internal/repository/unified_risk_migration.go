package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

const (
	UnifiedRiskBackupProofVersion = 1
	unifiedRiskMigrationLockID    = int64(748617622795573102)
	legacyPromptAuditSettingKey   = "prompt_audit_config"
)

var (
	ErrUnifiedRiskBackupProof = errors.New("unified risk migration backup proof is invalid")
	ErrUnifiedRiskValidation  = errors.New("unified risk migration validation failed")
)

// UnifiedRiskBackupProof is produced only after a custom-format pg_dump can
// be listed and restored into a distinct, isolated database. It contains no
// connection strings or Prompt Audit content.
type UnifiedRiskBackupProof struct {
	Version                int              `json:"version"`
	VerifiedAt             time.Time        `json:"verified_at"`
	SourceDatabase         string           `json:"source_database"`
	SourceSystemIdentifier string           `json:"source_system_identifier"`
	RestoreDatabase        string           `json:"restore_database"`
	ArchivePath            string           `json:"archive_path"`
	ArchiveSHA256          string           `json:"archive_sha256"`
	ArchiveBytes           int64            `json:"archive_bytes"`
	RestoreListSHA256      string           `json:"restore_list_sha256"`
	SourceJobCount         int64            `json:"source_job_count"`
	SourceEventCount       int64            `json:"source_event_count"`
	SourceMaxJobID         int64            `json:"source_max_job_id"`
	SourceMaxEventID       int64            `json:"source_max_event_id"`
	SourceStatusCounts     map[string]int64 `json:"source_status_counts"`
	RestoredJobCount       int64            `json:"restored_job_count"`
	RestoredEventCount     int64            `json:"restored_event_count"`
	RestoredStatusCounts   map[string]int64 `json:"restored_status_counts"`
	ListVerified           bool             `json:"list_verified"`
	RestoreVerified        bool             `json:"restore_verified"`
}

type verifiedUnifiedRiskBackup struct {
	Proof         UnifiedRiskBackupProof
	ProofDigest   []byte
	ArchiveDigest []byte
}

type UnifiedRiskPrepareReport struct {
	SourceDatabase   string           `json:"source_database"`
	WatermarkJobID   int64            `json:"watermark_job_id"`
	WatermarkEventID int64            `json:"watermark_event_id"`
	StagedJobCount   int64            `json:"staged_job_count"`
	StagedEventCount int64            `json:"staged_event_count"`
	ArchivedHitCount int64            `json:"archived_hit_count"`
	StatusCounts     map[string]int64 `json:"status_counts"`
	PreparedAt       time.Time        `json:"prepared_at"`
}

type UnifiedRiskFinalizeOptions struct {
	MaintenanceMode  bool
	beforeValidation func(context.Context, *sql.Tx) error
}

type UnifiedRiskFinalizeReport struct {
	RunID            string           `json:"run_id"`
	SourceDatabase   string           `json:"source_database"`
	JobCount         int64            `json:"job_count"`
	EventCount       int64            `json:"event_count"`
	ArchivedHitCount int64            `json:"archived_hit_count"`
	StatusCounts     map[string]int64 `json:"status_counts"`
	FinalizedAt      time.Time        `json:"finalized_at"`
}

type UnifiedRiskMigrator struct {
	db        *sql.DB
	cipher    *service.ContentModerationArchiveCipher
	proofPath string
}

func NewUnifiedRiskMigrator(db *sql.DB, cipher *service.ContentModerationArchiveCipher, proofPath string) *UnifiedRiskMigrator {
	return &UnifiedRiskMigrator{db: db, cipher: cipher, proofPath: strings.TrimSpace(proofPath)}
}

func (m *UnifiedRiskMigrator) Verify(ctx context.Context) (*UnifiedRiskBackupProof, error) {
	verified, err := m.verifyBackup(ctx)
	if err != nil {
		return nil, err
	}
	proof := verified.Proof
	return &proof, nil
}

func (m *UnifiedRiskMigrator) Prepare(ctx context.Context) (*UnifiedRiskPrepareReport, error) {
	verified, err := m.verifyBackup(ctx)
	if err != nil {
		return nil, err
	}
	if m.cipher == nil {
		return nil, errors.New("unified risk migration archive cipher is required")
	}
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return nil, fmt.Errorf("begin unified risk online preparation: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := acquireUnifiedRiskMigrationLock(ctx, tx); err != nil {
		return nil, err
	}
	identity, err := queryUnifiedRiskSourceIdentity(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := requireProofMatchesSource(verified.Proof, identity); err != nil {
		return nil, err
	}
	if err := m.requireSourceNotOlderThanBackup(ctx, tx, verified.Proof); err != nil {
		return nil, err
	}
	report, err := m.stageAll(ctx, tx, identity)
	if err != nil {
		return nil, err
	}
	if err := validateUnifiedRiskStage(ctx, tx, report, m.cipher); err != nil {
		return nil, err
	}
	statusRaw, err := json.Marshal(report.StatusCounts)
	if err != nil {
		return nil, fmt.Errorf("marshal unified risk preparation counts: %w", err)
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO unified_risk_migration_state (
    singleton, status, source_database, source_system_identifier,
    backup_proof_sha256, backup_archive_sha256, watermark_job_id,
    watermark_event_id, staged_job_count, staged_event_count,
    status_counts, prepared_at, updated_at
) VALUES (
    TRUE, 'prepared', $1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, NOW(), NOW()
)
ON CONFLICT (singleton) DO UPDATE SET
    status = 'prepared', source_database = EXCLUDED.source_database,
    source_system_identifier = EXCLUDED.source_system_identifier,
    backup_proof_sha256 = EXCLUDED.backup_proof_sha256,
    backup_archive_sha256 = EXCLUDED.backup_archive_sha256,
    watermark_job_id = EXCLUDED.watermark_job_id,
    watermark_event_id = EXCLUDED.watermark_event_id,
    staged_job_count = EXCLUDED.staged_job_count,
    staged_event_count = EXCLUDED.staged_event_count,
    status_counts = EXCLUDED.status_counts, updated_at = NOW()`,
		identity.Database, identity.SystemIdentifier, verified.ProofDigest, verified.ArchiveDigest,
		report.WatermarkJobID, report.WatermarkEventID, report.StagedJobCount,
		report.StagedEventCount, string(statusRaw))
	if err != nil {
		return nil, fmt.Errorf("save unified risk migration state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit unified risk online preparation: %w", err)
	}
	report.PreparedAt = time.Now().UTC()
	return report, nil
}

func (m *UnifiedRiskMigrator) Finalize(ctx context.Context, options UnifiedRiskFinalizeOptions) (*UnifiedRiskFinalizeReport, error) {
	if !options.MaintenanceMode {
		return nil, errors.New("unified risk finalization requires confirmed maintenance mode")
	}
	verified, err := m.verifyBackup(ctx)
	if err != nil {
		return nil, err
	}
	if m.cipher == nil {
		return nil, errors.New("unified risk migration archive cipher is required")
	}
	tx, err := m.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return nil, fmt.Errorf("begin unified risk finalization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := acquireUnifiedRiskMigrationLock(ctx, tx); err != nil {
		return nil, err
	}
	identity, err := queryUnifiedRiskSourceIdentity(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := requireProofMatchesSource(verified.Proof, identity); err != nil {
		return nil, err
	}
	stateStatus, err := m.requireMatchingState(ctx, tx, verified, identity)
	if err != nil {
		return nil, err
	}
	if stateStatus == "finalized" {
		report, err := loadUnifiedRiskFinalizedReport(ctx, tx, verified, identity)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("finish idempotent unified risk finalization: %w", err)
		}
		return report, nil
	}
	if stateStatus != "prepared" {
		return nil, fmt.Errorf("%w: migration state is %q, want prepared", ErrUnifiedRiskValidation, stateStatus)
	}
	if _, err := tx.ExecContext(ctx, `LOCK TABLE prompt_audit_jobs, prompt_audit_events IN ACCESS EXCLUSIVE MODE`); err != nil {
		return nil, fmt.Errorf("lock legacy Prompt Audit tables: %w", err)
	}
	if err := m.requirePreparedState(ctx, tx, verified, identity); err != nil {
		return nil, err
	}
	report, err := m.stageAll(ctx, tx, identity)
	if err != nil {
		return nil, fmt.Errorf("stage final Prompt Audit delta: %w", err)
	}
	if options.beforeValidation != nil {
		if err := options.beforeValidation(ctx, tx); err != nil {
			return nil, fmt.Errorf("run unified risk validation hook: %w", err)
		}
	}
	if err := validateUnifiedRiskStage(ctx, tx, report, m.cipher); err != nil {
		return nil, err
	}
	if err := mergeUnifiedRiskStage(ctx, tx); err != nil {
		return nil, err
	}
	if err := validateUnifiedRiskMerge(ctx, tx, report); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM settings WHERE key = $1`, legacyPromptAuditSettingKey); err != nil {
		return nil, fmt.Errorf("delete legacy Prompt Audit configuration: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE prompt_audit_events, prompt_audit_jobs`); err != nil {
		return nil, fmt.Errorf("drop legacy Prompt Audit tables: %w", err)
	}
	statusRaw, err := json.Marshal(report.StatusCounts)
	if err != nil {
		return nil, fmt.Errorf("marshal unified risk final counts: %w", err)
	}
	runID := uuid.NewString()
	finalizedAt := time.Now().UTC()
	_, err = tx.ExecContext(ctx, `
INSERT INTO unified_risk_migration_audits (
    run_id, source_database, source_system_identifier, backup_proof_sha256,
    backup_archive_sha256, job_count, event_count, archived_hit_count,
    status_counts, finalized_at
) VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)`,
		runID, identity.Database, identity.SystemIdentifier, verified.ProofDigest,
		verified.ArchiveDigest, report.StagedJobCount, report.StagedEventCount,
		report.ArchivedHitCount, string(statusRaw), finalizedAt)
	if err != nil {
		return nil, fmt.Errorf("record unified risk migration audit: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE unified_risk_migration_state SET
    status = 'finalized', watermark_job_id = $1, watermark_event_id = $2,
    staged_job_count = $3, staged_event_count = $4, status_counts = $5::jsonb,
    updated_at = $6 WHERE singleton = TRUE`, report.WatermarkJobID,
		report.WatermarkEventID, report.StagedJobCount, report.StagedEventCount,
		string(statusRaw), finalizedAt); err != nil {
		return nil, fmt.Errorf("mark unified risk migration finalized: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM unified_risk_migration_stage`); err != nil {
		return nil, fmt.Errorf("clear unified risk migration staging data: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit unified risk finalization: %w", err)
	}
	return &UnifiedRiskFinalizeReport{
		RunID: runID, SourceDatabase: identity.Database, JobCount: report.StagedJobCount,
		EventCount: report.StagedEventCount, ArchivedHitCount: report.ArchivedHitCount,
		StatusCounts: report.StatusCounts, FinalizedAt: finalizedAt,
	}, nil
}

type unifiedRiskSourceIdentity struct {
	Database         string
	SystemIdentifier string
}

type unifiedRiskDBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func acquireUnifiedRiskMigrationLock(ctx context.Context, db unifiedRiskDBTX) error {
	if _, err := db.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, unifiedRiskMigrationLockID); err != nil {
		return fmt.Errorf("acquire unified risk migration lock: %w", err)
	}
	return nil
}

func queryUnifiedRiskSourceIdentity(ctx context.Context, db unifiedRiskDBTX) (unifiedRiskSourceIdentity, error) {
	var identity unifiedRiskSourceIdentity
	err := db.QueryRowContext(ctx, `SELECT current_database(), system_identifier::text FROM pg_control_system()`).Scan(
		&identity.Database, &identity.SystemIdentifier)
	if err != nil {
		return identity, fmt.Errorf("read PostgreSQL source identity: %w", err)
	}
	return identity, nil
}

func (m *UnifiedRiskMigrator) verifyBackup(ctx context.Context) (*verifiedUnifiedRiskBackup, error) {
	if m == nil || m.db == nil {
		return nil, errors.New("unified risk migration database is required")
	}
	proofPath := strings.TrimSpace(m.proofPath)
	if proofPath == "" {
		return nil, fmt.Errorf("%w: proof path is required", ErrUnifiedRiskBackupProof)
	}
	proofRaw, err := os.ReadFile(proofPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read proof: %v", ErrUnifiedRiskBackupProof, err)
	}
	var proof UnifiedRiskBackupProof
	decoder := json.NewDecoder(bytes.NewReader(proofRaw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&proof); err != nil {
		return nil, fmt.Errorf("%w: decode proof: %v", ErrUnifiedRiskBackupProof, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: trailing proof data", ErrUnifiedRiskBackupProof)
	}
	if err := validateUnifiedRiskProofFields(proof); err != nil {
		return nil, err
	}
	archivePath := proof.ArchivePath
	if !filepath.IsAbs(archivePath) {
		archivePath = filepath.Join(filepath.Dir(proofPath), archivePath)
	}
	info, err := os.Stat(archivePath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: backup archive is unavailable or not regular", ErrUnifiedRiskBackupProof)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%w: backup archive permissions must be 0600 or stricter", ErrUnifiedRiskBackupProof)
	}
	archiveFile, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("%w: open backup archive: %v", ErrUnifiedRiskBackupProof, err)
	}
	defer func() { _ = archiveFile.Close() }()
	header := make([]byte, 5)
	if _, err := io.ReadFull(archiveFile, header); err != nil || string(header) != "PGDMP" {
		return nil, fmt.Errorf("%w: backup archive size or custom-format signature mismatch", ErrUnifiedRiskBackupProof)
	}
	if _, err := archiveFile.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("%w: rewind backup archive: %v", ErrUnifiedRiskBackupProof, err)
	}
	hash := sha256.New()
	archiveBytes, err := io.Copy(hash, archiveFile)
	if err != nil || archiveBytes != proof.ArchiveBytes {
		return nil, fmt.Errorf("%w: read backup archive or size mismatch", ErrUnifiedRiskBackupProof)
	}
	archiveDigest := hash.Sum(nil)
	expectedDigest, err := hex.DecodeString(proof.ArchiveSHA256)
	if err != nil || !bytes.Equal(archiveDigest, expectedDigest) {
		return nil, fmt.Errorf("%w: backup archive digest mismatch", ErrUnifiedRiskBackupProof)
	}
	identity, err := queryUnifiedRiskSourceIdentity(ctx, m.db)
	if err != nil {
		return nil, err
	}
	if err := requireProofMatchesSource(proof, identity); err != nil {
		return nil, err
	}
	proofDigest := sha256.Sum256(proofRaw)
	return &verifiedUnifiedRiskBackup{Proof: proof, ProofDigest: proofDigest[:], ArchiveDigest: archiveDigest}, nil
}

func validateUnifiedRiskProofFields(proof UnifiedRiskBackupProof) error {
	if proof.Version != UnifiedRiskBackupProofVersion || proof.VerifiedAt.IsZero() ||
		strings.TrimSpace(proof.SourceDatabase) == "" || strings.TrimSpace(proof.SourceSystemIdentifier) == "" ||
		strings.TrimSpace(proof.RestoreDatabase) == "" || proof.RestoreDatabase == proof.SourceDatabase ||
		!proof.ListVerified || !proof.RestoreVerified {
		return fmt.Errorf("%w: version, source, isolated restore, listing, and restore verification are required", ErrUnifiedRiskBackupProof)
	}
	if proof.SourceJobCount < 0 || proof.SourceEventCount < 0 || proof.SourceMaxJobID < 0 || proof.SourceMaxEventID < 0 ||
		proof.SourceJobCount != proof.RestoredJobCount || proof.SourceEventCount != proof.RestoredEventCount ||
		!equalUnifiedRiskCounts(proof.SourceStatusCounts, proof.RestoredStatusCounts) {
		return fmt.Errorf("%w: restored counts do not match the backup snapshot", ErrUnifiedRiskBackupProof)
	}
	if proof.ArchiveBytes <= 0 || len(proof.ArchiveSHA256) != sha256.Size*2 || len(proof.RestoreListSHA256) != sha256.Size*2 {
		return fmt.Errorf("%w: archive metadata is incomplete", ErrUnifiedRiskBackupProof)
	}
	if _, err := hex.DecodeString(proof.RestoreListSHA256); err != nil {
		return fmt.Errorf("%w: restore-list digest is invalid", ErrUnifiedRiskBackupProof)
	}
	return nil
}

func requireProofMatchesSource(proof UnifiedRiskBackupProof, identity unifiedRiskSourceIdentity) error {
	if proof.SourceDatabase != identity.Database || proof.SourceSystemIdentifier != identity.SystemIdentifier {
		return fmt.Errorf("%w: proof source identity does not match the connected database", ErrUnifiedRiskBackupProof)
	}
	return nil
}

func (m *UnifiedRiskMigrator) requireSourceNotOlderThanBackup(ctx context.Context, db unifiedRiskDBTX, proof UnifiedRiskBackupProof) error {
	var jobs, events, maxJobID, maxEventID int64
	err := db.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM prompt_audit_jobs),
       (SELECT COUNT(*) FROM prompt_audit_events),
       (SELECT COALESCE(MAX(id), 0) FROM prompt_audit_jobs),
       (SELECT COALESCE(MAX(id), 0) FROM prompt_audit_events)`).Scan(&jobs, &events, &maxJobID, &maxEventID)
	if err != nil {
		return fmt.Errorf("read current Prompt Audit backup bounds: %w", err)
	}
	if jobs < proof.SourceJobCount || events < proof.SourceEventCount || maxJobID < proof.SourceMaxJobID || maxEventID < proof.SourceMaxEventID {
		return fmt.Errorf("%w: live Prompt Audit tables are older than the verified backup snapshot", ErrUnifiedRiskBackupProof)
	}
	return nil
}

func (m *UnifiedRiskMigrator) requirePreparedState(ctx context.Context, db unifiedRiskDBTX, verified *verifiedUnifiedRiskBackup, identity unifiedRiskSourceIdentity) error {
	status, err := m.requireMatchingState(ctx, db, verified, identity)
	if err != nil {
		return err
	}
	if status != "prepared" {
		return fmt.Errorf("%w: migration state is %q, want prepared", ErrUnifiedRiskValidation, status)
	}
	return nil
}

func (m *UnifiedRiskMigrator) requireMatchingState(ctx context.Context, db unifiedRiskDBTX, verified *verifiedUnifiedRiskBackup, identity unifiedRiskSourceIdentity) (string, error) {
	var status, database, systemID string
	var proofDigest, archiveDigest []byte
	err := db.QueryRowContext(ctx, `
SELECT status, source_database, source_system_identifier,
       backup_proof_sha256, backup_archive_sha256
FROM unified_risk_migration_state WHERE singleton = TRUE FOR UPDATE`).Scan(
		&status, &database, &systemID, &proofDigest, &archiveDigest)
	if err != nil {
		return "", fmt.Errorf("read unified risk migration state: %w", err)
	}
	if database != identity.Database || systemID != identity.SystemIdentifier ||
		!bytes.Equal(proofDigest, verified.ProofDigest) || !bytes.Equal(archiveDigest, verified.ArchiveDigest) {
		return "", fmt.Errorf("%w: finalization proof does not match the online preparation", ErrUnifiedRiskValidation)
	}
	return status, nil
}

func loadUnifiedRiskFinalizedReport(ctx context.Context, db unifiedRiskDBTX, verified *verifiedUnifiedRiskBackup, identity unifiedRiskSourceIdentity) (*UnifiedRiskFinalizeReport, error) {
	var report UnifiedRiskFinalizeReport
	var statusRaw []byte
	err := db.QueryRowContext(ctx, `
SELECT run_id::text, source_database, job_count, event_count,
       archived_hit_count, status_counts, finalized_at
FROM unified_risk_migration_audits
WHERE source_database = $1 AND source_system_identifier = $2
  AND backup_proof_sha256 = $3 AND backup_archive_sha256 = $4
ORDER BY finalized_at DESC LIMIT 1`, identity.Database, identity.SystemIdentifier,
		verified.ProofDigest, verified.ArchiveDigest).Scan(
		&report.RunID, &report.SourceDatabase, &report.JobCount, &report.EventCount,
		&report.ArchivedHitCount, &statusRaw, &report.FinalizedAt)
	if err != nil {
		return nil, fmt.Errorf("load completed unified risk migration audit: %w", err)
	}
	if err := json.Unmarshal(statusRaw, &report.StatusCounts); err != nil {
		return nil, fmt.Errorf("decode completed unified risk migration status counts: %w", err)
	}
	return &report, nil
}

func equalUnifiedRiskCounts(left, right map[string]int64) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func canonicalUnifiedRiskCounts(counts map[string]int64) map[string]int64 {
	result := make(map[string]int64, len(counts))
	for key, value := range counts {
		result[strings.TrimSpace(key)] = value
	}
	return result
}

func encodeLegacyPrompt(value string) string {
	return base64.StdEncoding.EncodeToString([]byte(value))
}

type unifiedRiskLegacyJob struct {
	ID                  int64
	RequestID           string
	UserID              sql.NullInt64
	Username            string
	UserEmail           string
	APIKeyID            sql.NullInt64
	APIKeyName          string
	GroupID             sql.NullInt64
	GroupName           string
	Provider            string
	Endpoint            string
	Protocol            string
	Model               string
	PromptHash          string
	RedactedPreview     string
	PromptLength        int
	MessageCount        int
	Stage               string
	ExecutionMode       string
	ConfigVersion       int64
	Status              string
	Attempts            int
	MaxAttempts         int
	ClaimVersion        int64
	NextAttemptAt       time.Time
	ProcessingStartedAt sql.NullTime
	ProcessedAt         sql.NullTime
	LastErrorCode       string
	LastErrorMessage    string
	CreatedAt           time.Time
	UpdatedAt           time.Time
	Events              []unifiedRiskLegacyEvent
}

type unifiedRiskLegacyEvent struct {
	ID              int64
	JobID           int64
	RequestID       string
	UserID          sql.NullInt64
	Username        string
	UserEmail       string
	APIKeyID        sql.NullInt64
	APIKeyName      string
	GroupID         sql.NullInt64
	GroupName       string
	Provider        string
	Endpoint        string
	Protocol        string
	Model           string
	PromptHash      string
	RedactedPreview string
	Stage           string
	Decision        string
	RiskLevel       string
	Action          string
	Categories      json.RawMessage
	MatchedScanners json.RawMessage
	ScannerScores   json.RawMessage
	ScannerEvidence json.RawMessage
	ScannerBackend  string
	ScannerVersion  string
	GuardEndpointID string
	PolicyID        string
	PolicyVersion   int
	ConfigVersion   int64
	ChunkTotal      int
	LatencyMS       int
	FullPrompt      string
	CreatedAt       time.Time
}

type unifiedRiskLogPayload struct {
	Log            service.ContentModerationLog `json:"log"`
	LegacyMetadata json.RawMessage              `json:"legacy_metadata"`
}

type unifiedRiskLegacyEventMetadata struct {
	ID              int64           `json:"id"`
	RequestID       string          `json:"request_id"`
	Stage           string          `json:"stage"`
	Decision        string          `json:"decision"`
	RiskLevel       string          `json:"risk_level"`
	Action          string          `json:"action"`
	Categories      json.RawMessage `json:"categories"`
	MatchedScanners json.RawMessage `json:"matched_scanners"`
	ScannerScores   json.RawMessage `json:"scanner_scores"`
	ScannerEvidence json.RawMessage `json:"scanner_evidence"`
	ScannerBackend  string          `json:"scanner_backend"`
	ScannerVersion  string          `json:"scanner_version"`
	GuardEndpointID string          `json:"guard_endpoint_id"`
	PolicyID        string          `json:"policy_id"`
	PolicyVersion   int             `json:"policy_version"`
	ConfigVersion   int64           `json:"config_version"`
	ChunkTotal      int             `json:"chunk_total"`
	LatencyMS       int             `json:"latency_ms"`
	CreatedAt       time.Time       `json:"created_at"`
}

func (m *UnifiedRiskMigrator) stageAll(ctx context.Context, db unifiedRiskDBTX, identity unifiedRiskSourceIdentity) (*UnifiedRiskPrepareReport, error) {
	jobs, err := loadUnifiedRiskLegacyJobs(ctx, db)
	if err != nil {
		return nil, err
	}
	existing, err := loadUnifiedRiskStageFingerprints(ctx, db)
	if err != nil {
		return nil, err
	}
	report := &UnifiedRiskPrepareReport{StatusCounts: make(map[string]int64)}
	for index := range jobs {
		job := &jobs[index]
		report.StagedJobCount++
		report.StagedEventCount += int64(len(job.Events))
		report.StatusCounts[job.Status]++
		if job.ID > report.WatermarkJobID {
			report.WatermarkJobID = job.ID
		}
		for eventIndex := range job.Events {
			if job.Events[eventIndex].ID > report.WatermarkEventID {
				report.WatermarkEventID = job.Events[eventIndex].ID
			}
		}
		fingerprint, err := fingerprintUnifiedRiskSource(job)
		if err != nil {
			return nil, err
		}
		if staged, ok := existing[job.ID]; ok && bytes.Equal(staged.fingerprint, fingerprint) &&
			staged.status == job.Status && staged.eventCount == len(job.Events) &&
			bytes.Equal(staged.payloadDigest, unifiedRiskPayloadDigest(staged.logPayload, staged.archivePayload)) {
			if staged.archived {
				report.ArchivedHitCount++
			}
			continue
		}
		logPayload, archivePayload, archived, err := m.mapUnifiedRiskJob(identity, job)
		if err != nil {
			return nil, fmt.Errorf("map legacy Prompt Audit job %d: %w", job.ID, err)
		}
		if archived {
			report.ArchivedHitCount++
		}
		payloadDigest := unifiedRiskPayloadDigest(logPayload, archivePayload)
		var archiveArg any
		if len(archivePayload) > 0 {
			archiveArg = string(archivePayload)
		}
		_, err = db.ExecContext(ctx, `
INSERT INTO unified_risk_migration_stage (
    source_job_id, source_updated_at, source_status, source_event_count,
    source_fingerprint, payload_sha256, log_payload, archive_payload, staged_at
) VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, NOW())
ON CONFLICT (source_job_id) DO UPDATE SET
    source_updated_at = EXCLUDED.source_updated_at,
    source_status = EXCLUDED.source_status,
    source_event_count = EXCLUDED.source_event_count,
    source_fingerprint = EXCLUDED.source_fingerprint,
    payload_sha256 = EXCLUDED.payload_sha256,
    log_payload = EXCLUDED.log_payload,
    archive_payload = EXCLUDED.archive_payload,
    staged_at = NOW()`, job.ID, job.UpdatedAt, job.Status, len(job.Events),
			fingerprint, payloadDigest, string(logPayload), archiveArg)
		if err != nil {
			return nil, fmt.Errorf("upsert unified risk stage job %d: %w", job.ID, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
DELETE FROM unified_risk_migration_stage s
WHERE NOT EXISTS (SELECT 1 FROM prompt_audit_jobs j WHERE j.id = s.source_job_id)`); err != nil {
		return nil, fmt.Errorf("remove deleted Prompt Audit staging rows: %w", err)
	}
	report.StatusCounts = canonicalUnifiedRiskCounts(report.StatusCounts)
	return report, nil
}

func loadUnifiedRiskLegacyJobs(ctx context.Context, db unifiedRiskDBTX) ([]unifiedRiskLegacyJob, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, request_id, user_id, username_snapshot, user_email_snapshot,
       api_key_id, api_key_name_snapshot, group_id, group_name, provider,
       endpoint, protocol, model, prompt_hash, redacted_preview,
       prompt_length, message_count, stage, execution_mode, config_version,
       status, attempts, max_attempts, claim_version, next_attempt_at,
       processing_started_at, processed_at, last_error_code,
       last_error_message, created_at, updated_at
FROM prompt_audit_jobs ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list legacy Prompt Audit jobs: %w", err)
	}
	jobs := make([]unifiedRiskLegacyJob, 0)
	for rows.Next() {
		var job unifiedRiskLegacyJob
		if err := rows.Scan(
			&job.ID, &job.RequestID, &job.UserID, &job.Username, &job.UserEmail,
			&job.APIKeyID, &job.APIKeyName, &job.GroupID, &job.GroupName, &job.Provider,
			&job.Endpoint, &job.Protocol, &job.Model, &job.PromptHash, &job.RedactedPreview,
			&job.PromptLength, &job.MessageCount, &job.Stage, &job.ExecutionMode, &job.ConfigVersion,
			&job.Status, &job.Attempts, &job.MaxAttempts, &job.ClaimVersion, &job.NextAttemptAt,
			&job.ProcessingStartedAt, &job.ProcessedAt, &job.LastErrorCode,
			&job.LastErrorMessage, &job.CreatedAt, &job.UpdatedAt,
		); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan legacy Prompt Audit job: %w", err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate legacy Prompt Audit jobs: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close legacy Prompt Audit jobs: %w", err)
	}
	jobIndexes := make(map[int64]int, len(jobs))
	for index := range jobs {
		jobIndexes[jobs[index].ID] = index
	}
	events, err := loadAllUnifiedRiskLegacyEvents(ctx, db)
	if err != nil {
		return nil, err
	}
	for index := range events {
		jobIndex, ok := jobIndexes[events[index].JobID]
		if !ok {
			return nil, fmt.Errorf("legacy Prompt Audit event %d refers to missing job %d", events[index].ID, events[index].JobID)
		}
		jobs[jobIndex].Events = append(jobs[jobIndex].Events, events[index])
	}
	return jobs, nil
}

func loadAllUnifiedRiskLegacyEvents(ctx context.Context, db unifiedRiskDBTX) ([]unifiedRiskLegacyEvent, error) {
	rows, err := db.QueryContext(ctx, `
SELECT id, job_id, request_id, user_id, username_snapshot,
       user_email_snapshot, api_key_id, api_key_name_snapshot, group_id,
       group_name, provider, endpoint, protocol, model, prompt_hash,
       redacted_preview, stage, decision, risk_level, action, categories,
       matched_scanners, scanner_scores, scanner_evidence, scanner_backend,
       scanner_version, guard_endpoint_id, policy_id, policy_version,
       config_version, chunk_total, latency_ms, full_prompt, created_at
FROM prompt_audit_events ORDER BY job_id, id`)
	if err != nil {
		return nil, fmt.Errorf("list legacy Prompt Audit events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	events := make([]unifiedRiskLegacyEvent, 0)
	for rows.Next() {
		var event unifiedRiskLegacyEvent
		if err := rows.Scan(
			&event.ID, &event.JobID, &event.RequestID, &event.UserID, &event.Username,
			&event.UserEmail, &event.APIKeyID, &event.APIKeyName, &event.GroupID,
			&event.GroupName, &event.Provider, &event.Endpoint, &event.Protocol, &event.Model,
			&event.PromptHash, &event.RedactedPreview, &event.Stage, &event.Decision,
			&event.RiskLevel, &event.Action, &event.Categories, &event.MatchedScanners,
			&event.ScannerScores, &event.ScannerEvidence, &event.ScannerBackend,
			&event.ScannerVersion, &event.GuardEndpointID, &event.PolicyID,
			&event.PolicyVersion, &event.ConfigVersion, &event.ChunkTotal,
			&event.LatencyMS, &event.FullPrompt, &event.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan legacy Prompt Audit event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate legacy Prompt Audit events: %w", err)
	}
	return events, nil
}

type unifiedRiskStageFingerprint struct {
	fingerprint    []byte
	archived       bool
	status         string
	eventCount     int
	payloadDigest  []byte
	logPayload     []byte
	archivePayload []byte
}

func loadUnifiedRiskStageFingerprints(ctx context.Context, db unifiedRiskDBTX) (map[int64]unifiedRiskStageFingerprint, error) {
	rows, err := db.QueryContext(ctx, `
SELECT source_job_id, source_fingerprint, archive_payload IS NOT NULL,
       source_status, source_event_count, payload_sha256,
       log_payload, archive_payload
FROM unified_risk_migration_stage`)
	if err != nil {
		return nil, fmt.Errorf("list unified risk staging fingerprints: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[int64]unifiedRiskStageFingerprint)
	for rows.Next() {
		var id int64
		var fingerprint []byte
		var archived bool
		var status string
		var eventCount int
		var payloadDigest, logPayload []byte
		var archivePayload sql.NullString
		if err := rows.Scan(&id, &fingerprint, &archived, &status, &eventCount,
			&payloadDigest, &logPayload, &archivePayload); err != nil {
			return nil, fmt.Errorf("scan unified risk staging fingerprint: %w", err)
		}
		archiveRaw := []byte(nil)
		if archivePayload.Valid {
			archiveRaw = []byte(archivePayload.String)
		}
		result[id] = unifiedRiskStageFingerprint{
			fingerprint: append([]byte(nil), fingerprint...), archived: archived,
			status: status, eventCount: eventCount,
			payloadDigest: append([]byte(nil), payloadDigest...),
			logPayload:    append([]byte(nil), logPayload...), archivePayload: archiveRaw,
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate unified risk staging fingerprints: %w", err)
	}
	return result, nil
}

func fingerprintUnifiedRiskSource(job *unifiedRiskLegacyJob) ([]byte, error) {
	raw, err := json.Marshal(job)
	if err != nil {
		return nil, fmt.Errorf("marshal legacy Prompt Audit source fingerprint: %w", err)
	}
	digest := sha256.Sum256(raw)
	return digest[:], nil
}

func unifiedRiskPayloadDigest(logPayload, archivePayload []byte) []byte {
	hash := sha256.New()
	_, _ = hash.Write(canonicalUnifiedRiskJSON(logPayload))
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(canonicalUnifiedRiskJSON(archivePayload))
	return hash.Sum(nil)
}

func canonicalUnifiedRiskJSON(raw []byte) []byte {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return append([]byte(nil), raw...)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return append([]byte(nil), raw...)
	}
	return canonical
}

func (m *UnifiedRiskMigrator) mapUnifiedRiskJob(identity unifiedRiskSourceIdentity, job *unifiedRiskLegacyJob) ([]byte, []byte, bool, error) {
	if job == nil {
		return nil, nil, false, errors.New("nil legacy Prompt Audit job")
	}
	representative := chooseUnifiedRiskLegacyEvent(job.Events)
	flagged := false
	for index := range job.Events {
		if unifiedRiskLegacyEventIsHit(job.Events[index]) {
			flagged = true
			break
		}
	}
	userID := nullableUnifiedRiskInt64(job.UserID)
	apiKeyID := nullableUnifiedRiskInt64(job.APIKeyID)
	groupID := nullableUnifiedRiskInt64(job.GroupID)
	action := service.ContentModerationActionAllow
	highestCategory := ""
	categoryScores := make(map[string]float64)
	var upstreamLatency *int
	if representative != nil {
		action = strings.ToLower(strings.TrimSpace(representative.Action))
		if action == "" {
			action = service.ContentModerationActionAllow
		}
		highestCategory = unifiedRiskHighestCategory(representative)
		latency := representative.LatencyMS
		upstreamLatency = &latency
	}
	highestScore := 0.0
	for index := range job.Events {
		for category, score := range unifiedRiskScannerScores(job.Events[index].ScannerScores) {
			if score > categoryScores[category] {
				categoryScores[category] = score
			}
			if score > highestScore {
				highestScore = score
			}
		}
	}
	eventMetadata := make([]unifiedRiskLegacyEventMetadata, 0, len(job.Events))
	for index := range job.Events {
		event := job.Events[index]
		eventMetadata = append(eventMetadata, unifiedRiskLegacyEventMetadata{
			ID: event.ID, RequestID: event.RequestID, Stage: event.Stage,
			Decision: event.Decision, RiskLevel: event.RiskLevel, Action: event.Action,
			Categories:      cloneUnifiedRiskJSON(event.Categories, `[]`),
			MatchedScanners: cloneUnifiedRiskJSON(event.MatchedScanners, `[]`),
			ScannerScores:   cloneUnifiedRiskJSON(event.ScannerScores, `{}`),
			ScannerEvidence: cloneUnifiedRiskJSON(event.ScannerEvidence, `{}`),
			ScannerBackend:  event.ScannerBackend, ScannerVersion: event.ScannerVersion,
			GuardEndpointID: event.GuardEndpointID, PolicyID: event.PolicyID,
			PolicyVersion: event.PolicyVersion, ConfigVersion: event.ConfigVersion,
			ChunkTotal: event.ChunkTotal, LatencyMS: event.LatencyMS, CreatedAt: event.CreatedAt,
		})
	}
	metadata, err := json.Marshal(map[string]any{
		"username_snapshot":     job.Username,
		"prompt_length":         job.PromptLength,
		"message_count":         job.MessageCount,
		"stage":                 job.Stage,
		"execution_mode":        job.ExecutionMode,
		"config_version":        job.ConfigVersion,
		"attempts":              job.Attempts,
		"max_attempts":          job.MaxAttempts,
		"claim_version":         job.ClaimVersion,
		"next_attempt_at":       job.NextAttemptAt,
		"processing_started_at": nullableUnifiedRiskTime(job.ProcessingStartedAt),
		"processed_at":          nullableUnifiedRiskTime(job.ProcessedAt),
		"last_error_code":       job.LastErrorCode,
		"last_error_message":    job.LastErrorMessage,
		"updated_at":            job.UpdatedAt,
		"events":                eventMetadata,
	})
	if err != nil {
		return nil, nil, false, fmt.Errorf("marshal legacy Prompt Audit job metadata: %w", err)
	}
	log := service.ContentModerationLog{
		RequestID: job.RequestID, UserID: userID, UserEmail: job.UserEmail,
		APIKeyID: apiKeyID, APIKeyName: job.APIKeyName, GroupID: groupID,
		GroupName: job.GroupName, Endpoint: job.Endpoint, Provider: job.Provider,
		Model: job.Model, Mode: "legacy_prompt_audit", Action: action,
		Flagged: flagged, HighestCategory: highestCategory, HighestScore: highestScore,
		CategoryScores: categoryScores, ThresholdSnapshot: map[string]float64{},
		InputExcerpt: job.RedactedPreview, UpstreamLatencyMS: upstreamLatency,
		Error: job.LastErrorCode, Protocol: job.Protocol, Transport: "legacy",
		RequestStage: job.Stage, InputHash: job.PromptHash,
		ArchiveStatus:     service.ContentModerationArchiveStatusNone,
		DispositionStatus: "none", LegacySourceJobID: &job.ID,
		LegacyStatus: job.Status, LegacyEventCount: len(job.Events),
		LegacyMetadata: metadata, CreatedAt: job.CreatedAt,
	}
	var archivePayload []byte
	if flagged {
		archiveID := unifiedRiskLegacyArchiveID(identity, job.ID)
		envelope := buildUnifiedRiskLegacyEnvelope(archiveID, job, action)
		plaintext, err := json.Marshal(envelope)
		if err != nil {
			return nil, nil, false, fmt.Errorf("marshal legacy Prompt Audit archive: %w", err)
		}
		archive, err := m.cipher.Encrypt(archiveID, plaintext)
		if err != nil {
			return nil, nil, false, fmt.Errorf("encrypt legacy Prompt Audit archive: %w", err)
		}
		applyArchiveMetadata(&log, archive)
		log.ArchiveIncomplete = true
		archivePayload, err = json.Marshal(archive)
		if err != nil {
			return nil, nil, false, fmt.Errorf("marshal encrypted legacy Prompt Audit archive: %w", err)
		}
	}
	logPayload, err := json.Marshal(unifiedRiskLogPayload{Log: log, LegacyMetadata: metadata})
	if err != nil {
		return nil, nil, false, fmt.Errorf("marshal unified risk log payload: %w", err)
	}
	return logPayload, archivePayload, flagged, nil
}

func chooseUnifiedRiskLegacyEvent(events []unifiedRiskLegacyEvent) *unifiedRiskLegacyEvent {
	var selected *unifiedRiskLegacyEvent
	selectedScore := -1
	for index := range events {
		score := unifiedRiskLegacyEventSeverity(events[index])
		if selected == nil || score > selectedScore || (score == selectedScore && events[index].ID > selected.ID) {
			selected = &events[index]
			selectedScore = score
		}
	}
	return selected
}

func unifiedRiskLegacyEventSeverity(event unifiedRiskLegacyEvent) int {
	switch strings.ToLower(strings.TrimSpace(event.Decision)) {
	case "critical":
		return 3
	case "flag":
		return 2
	case "pass":
		return 1
	default:
		return 0
	}
}

func unifiedRiskLegacyEventIsHit(event unifiedRiskLegacyEvent) bool {
	return unifiedRiskLegacyEventSeverity(event) >= 2 ||
		strings.EqualFold(event.Action, "Warn") || strings.EqualFold(event.Action, "Block")
}

func unifiedRiskHighestCategory(event *unifiedRiskLegacyEvent) string {
	if event == nil {
		return ""
	}
	var categories []string
	if err := json.Unmarshal(event.Categories, &categories); err == nil && len(categories) > 0 {
		return categories[0]
	}
	return strings.TrimSpace(event.RiskLevel)
}

func unifiedRiskScannerScores(raw json.RawMessage) map[string]float64 {
	result := make(map[string]float64)
	_ = json.Unmarshal(raw, &result)
	return result
}

func unifiedRiskLegacyArchiveID(identity unifiedRiskSourceIdentity, jobID int64) string {
	name := fmt.Sprintf("sub2api/unified-risk/%s/%s/prompt-audit-job/%d", identity.SystemIdentifier, identity.Database, jobID)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func buildUnifiedRiskLegacyEnvelope(archiveID string, job *unifiedRiskLegacyJob, action string) service.ContentModerationArchiveEnvelope {
	events := make([]service.ContentModerationLegacyPromptAuditEvent, 0, len(job.Events))
	body := ""
	for index := range job.Events {
		event := job.Events[index]
		fullPrompt := encodeLegacyPrompt(event.FullPrompt)
		if unifiedRiskLegacyEventIsHit(event) {
			if body == "" {
				body = fullPrompt
			}
		}
		events = append(events, service.ContentModerationLegacyPromptAuditEvent{
			SourceEventID: event.ID, RequestID: event.RequestID,
			UserID: nullableUnifiedRiskInt64(event.UserID), Username: event.Username,
			UserEmail: event.UserEmail, APIKeyID: nullableUnifiedRiskInt64(event.APIKeyID),
			APIKeyName: event.APIKeyName, GroupID: nullableUnifiedRiskInt64(event.GroupID),
			GroupName: event.GroupName, Provider: event.Provider, Endpoint: event.Endpoint,
			Protocol: event.Protocol, Model: event.Model, PromptHash: event.PromptHash,
			RedactedPreview: event.RedactedPreview, Stage: event.Stage,
			Decision: event.Decision, RiskLevel: event.RiskLevel, Action: event.Action,
			Categories:      cloneUnifiedRiskJSON(event.Categories, `[]`),
			MatchedScanners: cloneUnifiedRiskJSON(event.MatchedScanners, `[]`),
			ScannerScores:   cloneUnifiedRiskJSON(event.ScannerScores, `{}`),
			ScannerEvidence: cloneUnifiedRiskJSON(event.ScannerEvidence, `{}`),
			ScannerBackend:  event.ScannerBackend, ScannerVersion: event.ScannerVersion,
			GuardEndpointID: event.GuardEndpointID, PolicyID: event.PolicyID,
			PolicyVersion: event.PolicyVersion, ConfigVersion: event.ConfigVersion,
			ChunkTotal: event.ChunkTotal, LatencyMS: event.LatencyMS,
			FullPromptBase64: fullPrompt, CreatedAt: event.CreatedAt,
		})
	}
	return service.ContentModerationArchiveEnvelope{
		ArchiveID: archiveID, Version: service.ContentModerationArchiveVersion,
		CapturedAt: job.CreatedAt,
		Request: service.ContentModerationArchiveRequest{
			BodyBase64: body, Transport: "legacy", Stage: "legacy_prompt_only",
		},
		InputHash: job.PromptHash, Action: action, Incomplete: true,
		LegacyPromptAudit: &service.ContentModerationLegacyPromptAuditArchive{
			SourceJobID: job.ID, Status: job.Status, Events: events,
		},
	}
}

func nullableUnifiedRiskInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	result := value.Int64
	return &result
}

func nullableUnifiedRiskTime(value sql.NullTime) any {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func cloneUnifiedRiskJSON(raw json.RawMessage, fallback string) json.RawMessage {
	if !json.Valid(raw) {
		return json.RawMessage(fallback)
	}
	return append(json.RawMessage(nil), raw...)
}

func validateUnifiedRiskStage(ctx context.Context, db unifiedRiskDBTX, report *UnifiedRiskPrepareReport, cipher *service.ContentModerationArchiveCipher) error {
	if report == nil {
		return fmt.Errorf("%w: nil preparation report", ErrUnifiedRiskValidation)
	}
	var sourceJobs, sourceEvents, stageJobs, stageEvents, stagedArchives int64
	err := db.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM prompt_audit_jobs),
       (SELECT COUNT(*) FROM prompt_audit_events),
       (SELECT COUNT(*) FROM unified_risk_migration_stage),
       (SELECT COALESCE(SUM(source_event_count), 0) FROM unified_risk_migration_stage),
       (SELECT COUNT(*) FROM unified_risk_migration_stage WHERE archive_payload IS NOT NULL)`).Scan(
		&sourceJobs, &sourceEvents, &stageJobs, &stageEvents, &stagedArchives)
	if err != nil {
		return fmt.Errorf("%w: count source and staging rows: %v", ErrUnifiedRiskValidation, err)
	}
	if sourceJobs != report.StagedJobCount || stageJobs != sourceJobs ||
		sourceEvents != report.StagedEventCount || stageEvents != sourceEvents ||
		stagedArchives != report.ArchivedHitCount {
		return fmt.Errorf("%w: source/stage totals differ (jobs %d/%d, events %d/%d, archives %d/%d)",
			ErrUnifiedRiskValidation, sourceJobs, stageJobs, sourceEvents, stageEvents,
			stagedArchives, report.ArchivedHitCount)
	}
	var missingMappings int64
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
    SELECT j.id FROM prompt_audit_jobs j
    FULL OUTER JOIN unified_risk_migration_stage s ON s.source_job_id = j.id
    WHERE j.id IS NULL OR s.source_job_id IS NULL
) mismatches`).Scan(&missingMappings); err != nil {
		return fmt.Errorf("%w: compare source job mappings: %v", ErrUnifiedRiskValidation, err)
	}
	if missingMappings != 0 {
		return fmt.Errorf("%w: %d source jobs do not have exactly one stage row", ErrUnifiedRiskValidation, missingMappings)
	}
	statusCounts, err := queryUnifiedRiskCounts(ctx, db, `SELECT status, COUNT(*) FROM prompt_audit_jobs GROUP BY status`)
	if err != nil {
		return err
	}
	stageStatusCounts, err := queryUnifiedRiskCounts(ctx, db, `SELECT source_status, COUNT(*) FROM unified_risk_migration_stage GROUP BY source_status`)
	if err != nil {
		return err
	}
	if !equalUnifiedRiskCounts(statusCounts, report.StatusCounts) || !equalUnifiedRiskCounts(statusCounts, stageStatusCounts) {
		return fmt.Errorf("%w: Prompt Audit status counts differ from staging", ErrUnifiedRiskValidation)
	}
	var sourceHits int64
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(DISTINCT job_id) FROM prompt_audit_events
WHERE decision IN ('flag', 'critical') OR action IN ('Warn', 'Block')`).Scan(&sourceHits); err != nil {
		return fmt.Errorf("%w: count source Prompt Audit hits: %v", ErrUnifiedRiskValidation, err)
	}
	if sourceHits != stagedArchives {
		return fmt.Errorf("%w: encrypted legacy hit count %d differs from source %d", ErrUnifiedRiskValidation, stagedArchives, sourceHits)
	}
	rows, err := db.QueryContext(ctx, `
SELECT source_job_id, source_status, source_event_count, payload_sha256,
       log_payload, archive_payload
FROM unified_risk_migration_stage ORDER BY source_job_id`)
	if err != nil {
		return fmt.Errorf("%w: list staging payloads: %v", ErrUnifiedRiskValidation, err)
	}
	type validationPayload struct {
		sourceJobID      int64
		sourceStatus     string
		sourceEventCount int
		payloadDigest    []byte
		logRaw           []byte
		archiveRaw       sql.NullString
	}
	payloads := make([]validationPayload, 0, report.StagedJobCount)
	for rows.Next() {
		var item validationPayload
		if err := rows.Scan(&item.sourceJobID, &item.sourceStatus, &item.sourceEventCount,
			&item.payloadDigest, &item.logRaw, &item.archiveRaw); err != nil {
			_ = rows.Close()
			return fmt.Errorf("%w: scan staging payload: %v", ErrUnifiedRiskValidation, err)
		}
		payloads = append(payloads, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("%w: iterate staging payloads: %v", ErrUnifiedRiskValidation, err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("%w: close staging payloads: %v", ErrUnifiedRiskValidation, err)
	}
	for _, item := range payloads {
		archiveBytes := []byte(nil)
		if item.archiveRaw.Valid {
			archiveBytes = []byte(item.archiveRaw.String)
		}
		if !bytes.Equal(item.payloadDigest, unifiedRiskPayloadDigest(item.logRaw, archiveBytes)) {
			return fmt.Errorf("%w: stage payload digest mismatch for job %d", ErrUnifiedRiskValidation, item.sourceJobID)
		}
		var payload unifiedRiskLogPayload
		if err := json.Unmarshal(item.logRaw, &payload); err != nil {
			return fmt.Errorf("%w: decode staged log for job %d: %v", ErrUnifiedRiskValidation, item.sourceJobID, err)
		}
		if payload.Log.LegacySourceJobID == nil || *payload.Log.LegacySourceJobID != item.sourceJobID ||
			payload.Log.LegacyStatus != item.sourceStatus || payload.Log.LegacyEventCount != item.sourceEventCount ||
			payload.Log.Mode != "legacy_prompt_audit" || !json.Valid(payload.LegacyMetadata) {
			return fmt.Errorf("%w: staged log identity mismatch for job %d", ErrUnifiedRiskValidation, item.sourceJobID)
		}
		payload.Log.LegacyMetadata = append(json.RawMessage(nil), payload.LegacyMetadata...)
		if item.archiveRaw.Valid {
			if !payload.Log.Flagged || !payload.Log.ArchiveIncomplete || payload.Log.ArchiveStatus != service.ContentModerationArchiveStatusAvailable {
				return fmt.Errorf("%w: staged hit metadata is not permanent/incomplete for job %d", ErrUnifiedRiskValidation, item.sourceJobID)
			}
			var archive service.ContentModerationEncryptedArchive
			if err := json.Unmarshal(archiveBytes, &archive); err != nil {
				return fmt.Errorf("%w: decode staged archive for job %d: %v", ErrUnifiedRiskValidation, item.sourceJobID, err)
			}
			if payload.Log.ArchiveID != archive.ArchiveID || payload.Log.ArchiveVersion != archive.Version ||
				payload.Log.ArchiveKeyID != archive.KeyID || payload.Log.ArchiveBytes != archive.PlaintextSize {
				return fmt.Errorf("%w: staged archive metadata differs for job %d", ErrUnifiedRiskValidation, item.sourceJobID)
			}
			if err := validateUnifiedRiskLegacyArchive(cipher, &archive, item.sourceJobID, item.sourceEventCount); err != nil {
				return err
			}
			if err := validateUnifiedRiskLegacyArchiveAgainstSource(ctx, db, cipher, &archive, item.sourceJobID); err != nil {
				return err
			}
		} else if payload.Log.Flagged || payload.Log.ArchiveStatus != service.ContentModerationArchiveStatusNone || payload.Log.ArchiveID != "" {
			return fmt.Errorf("%w: non-hit job %d unexpectedly has permanent archive metadata", ErrUnifiedRiskValidation, item.sourceJobID)
		}
	}
	return nil
}

func validateUnifiedRiskLegacyArchive(cipher *service.ContentModerationArchiveCipher, archive *service.ContentModerationEncryptedArchive, sourceJobID int64, sourceEventCount int) error {
	if cipher == nil || archive == nil || len(archive.Chunks) == 0 {
		return fmt.Errorf("%w: encrypted archive is missing for job %d", ErrUnifiedRiskValidation, sourceJobID)
	}
	plaintext, err := cipher.Decrypt(archive)
	if err != nil {
		return fmt.Errorf("%w: decrypt archive for job %d: %v", ErrUnifiedRiskValidation, sourceJobID, err)
	}
	var envelope service.ContentModerationArchiveEnvelope
	if err := json.Unmarshal(plaintext, &envelope); err != nil {
		return fmt.Errorf("%w: decode archive envelope for job %d: %v", ErrUnifiedRiskValidation, sourceJobID, err)
	}
	if envelope.ArchiveID != archive.ArchiveID || !envelope.Incomplete ||
		envelope.Request.Transport != "legacy" || envelope.Request.Stage != "legacy_prompt_only" ||
		envelope.LegacyPromptAudit == nil || envelope.LegacyPromptAudit.SourceJobID != sourceJobID ||
		len(envelope.LegacyPromptAudit.Events) != sourceEventCount {
		return fmt.Errorf("%w: legacy_prompt_only/incomplete envelope mismatch for job %d", ErrUnifiedRiskValidation, sourceJobID)
	}
	for _, event := range envelope.LegacyPromptAudit.Events {
		if event.FullPromptBase64 == "" {
			continue
		}
		if _, err := base64.StdEncoding.DecodeString(event.FullPromptBase64); err != nil {
			return fmt.Errorf("%w: invalid encrypted prompt encoding for event %d", ErrUnifiedRiskValidation, event.SourceEventID)
		}
	}
	return nil
}

func validateUnifiedRiskLegacyArchiveAgainstSource(ctx context.Context, db unifiedRiskDBTX, cipher *service.ContentModerationArchiveCipher, archive *service.ContentModerationEncryptedArchive, sourceJobID int64) error {
	plaintext, err := cipher.Decrypt(archive)
	if err != nil {
		return fmt.Errorf("%w: decrypt archive for source comparison job %d: %v", ErrUnifiedRiskValidation, sourceJobID, err)
	}
	var envelope service.ContentModerationArchiveEnvelope
	if err := json.Unmarshal(plaintext, &envelope); err != nil || envelope.LegacyPromptAudit == nil {
		return fmt.Errorf("%w: decode archive for source comparison job %d", ErrUnifiedRiskValidation, sourceJobID)
	}
	rows, err := db.QueryContext(ctx, `
SELECT id, full_prompt FROM prompt_audit_events WHERE job_id = $1 ORDER BY id`, sourceJobID)
	if err != nil {
		return fmt.Errorf("%w: list source prompts for job %d: %v", ErrUnifiedRiskValidation, sourceJobID, err)
	}
	defer func() { _ = rows.Close() }()
	index := 0
	for rows.Next() {
		var eventID int64
		var prompt string
		if err := rows.Scan(&eventID, &prompt); err != nil {
			return fmt.Errorf("%w: scan source prompt for job %d: %v", ErrUnifiedRiskValidation, sourceJobID, err)
		}
		if index >= len(envelope.LegacyPromptAudit.Events) {
			return fmt.Errorf("%w: encrypted events are missing for job %d", ErrUnifiedRiskValidation, sourceJobID)
		}
		archivedEvent := envelope.LegacyPromptAudit.Events[index]
		decoded, err := base64.StdEncoding.DecodeString(archivedEvent.FullPromptBase64)
		if err != nil || archivedEvent.SourceEventID != eventID || !bytes.Equal(decoded, []byte(prompt)) {
			return fmt.Errorf("%w: encrypted prompt differs for event %d", ErrUnifiedRiskValidation, eventID)
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("%w: iterate source prompts for job %d: %v", ErrUnifiedRiskValidation, sourceJobID, err)
	}
	if index != len(envelope.LegacyPromptAudit.Events) {
		return fmt.Errorf("%w: encrypted event count differs for job %d", ErrUnifiedRiskValidation, sourceJobID)
	}
	return nil
}

func queryUnifiedRiskCounts(ctx context.Context, db unifiedRiskDBTX, query string) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("%w: query status counts: %v", ErrUnifiedRiskValidation, err)
	}
	defer func() { _ = rows.Close() }()
	counts := make(map[string]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("%w: scan status counts: %v", ErrUnifiedRiskValidation, err)
		}
		counts[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%w: iterate status counts: %v", ErrUnifiedRiskValidation, err)
	}
	return canonicalUnifiedRiskCounts(counts), nil
}

func mergeUnifiedRiskStage(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT source_job_id, log_payload, archive_payload
FROM unified_risk_migration_stage ORDER BY source_job_id`)
	if err != nil {
		return fmt.Errorf("list unified risk stage for merge: %w", err)
	}
	type rowPayload struct {
		jobID      int64
		logRaw     []byte
		archiveRaw sql.NullString
	}
	payloads := make([]rowPayload, 0)
	for rows.Next() {
		var item rowPayload
		if err := rows.Scan(&item.jobID, &item.logRaw, &item.archiveRaw); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan unified risk stage for merge: %w", err)
		}
		payloads = append(payloads, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("iterate unified risk stage for merge: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close unified risk stage for merge: %w", err)
	}
	for _, item := range payloads {
		var payload unifiedRiskLogPayload
		if err := json.Unmarshal(item.logRaw, &payload); err != nil {
			return fmt.Errorf("decode unified risk staged log %d: %w", item.jobID, err)
		}
		payload.Log.LegacyMetadata = append(json.RawMessage(nil), payload.LegacyMetadata...)
		var archive *service.ContentModerationEncryptedArchive
		if item.archiveRaw.Valid {
			archive = &service.ContentModerationEncryptedArchive{}
			if err := json.Unmarshal([]byte(item.archiveRaw.String), archive); err != nil {
				return fmt.Errorf("decode staged archive for job %d: %w", item.jobID, err)
			}
			applyArchiveMetadata(&payload.Log, archive)
			payload.Log.ArchiveIncomplete = true
		}
		var existingID int64
		var existingStatus, existingArchiveID string
		var existingEventCount int
		err := tx.QueryRowContext(ctx, `
SELECT id, legacy_status, legacy_event_count, COALESCE(archive_id::text, '')
FROM content_moderation_logs WHERE legacy_source_job_id = $1`, item.jobID).Scan(
			&existingID, &existingStatus, &existingEventCount, &existingArchiveID)
		if err == nil {
			if existingStatus != payload.Log.LegacyStatus || existingEventCount != payload.Log.LegacyEventCount ||
				existingArchiveID != payload.Log.ArchiveID {
				return fmt.Errorf("%w: existing unified log conflicts with source job %d", ErrUnifiedRiskValidation, item.jobID)
			}
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check existing unified log for job %d: %w", item.jobID, err)
		}
		if err := insertContentModerationLog(ctx, tx, &payload.Log); err != nil {
			return fmt.Errorf("merge legacy Prompt Audit job %d: %w", item.jobID, err)
		}
		if archive == nil {
			continue
		}
		for _, chunk := range archive.Chunks {
			if _, err := tx.ExecContext(ctx, `
INSERT INTO content_moderation_log_chunks (
    log_id, archive_id, chunk_index, chunk_total, nonce, ciphertext, plaintext_bytes
) VALUES ($1, $2::uuid, $3, $4, $5, $6, $7)`, payload.Log.ID,
				archive.ArchiveID, chunk.Index, chunk.Total, chunk.Nonce,
				chunk.Ciphertext, chunk.PlaintextBytes); err != nil {
				return fmt.Errorf("merge archive chunk %d for job %d: %w", chunk.Index, item.jobID, err)
			}
		}
	}
	return nil
}

func validateUnifiedRiskMerge(ctx context.Context, db unifiedRiskDBTX, report *UnifiedRiskPrepareReport) error {
	var jobs, events, archives, incompleteArchives, chunklessArchives int64
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*), COALESCE(SUM(legacy_event_count), 0),
       COUNT(*) FILTER (WHERE archive_id IS NOT NULL),
       COUNT(*) FILTER (WHERE archive_id IS NOT NULL AND archive_incomplete),
       COUNT(*) FILTER (
           WHERE archive_id IS NOT NULL AND NOT EXISTS (
               SELECT 1 FROM content_moderation_log_chunks c WHERE c.log_id = l.id
           )
       )
FROM content_moderation_logs l WHERE legacy_source_job_id IS NOT NULL`).Scan(
		&jobs, &events, &archives, &incompleteArchives, &chunklessArchives)
	if err != nil {
		return fmt.Errorf("%w: validate merged unified logs: %v", ErrUnifiedRiskValidation, err)
	}
	if jobs != report.StagedJobCount || events != report.StagedEventCount ||
		archives != report.ArchivedHitCount || incompleteArchives != archives || chunklessArchives != 0 {
		return fmt.Errorf("%w: merged log totals, permanent archives, or chunks differ", ErrUnifiedRiskValidation)
	}
	mergedStatusCounts, err := queryUnifiedRiskCounts(ctx, db, `
SELECT legacy_status, COUNT(*) FROM content_moderation_logs
WHERE legacy_source_job_id IS NOT NULL GROUP BY legacy_status`)
	if err != nil {
		return err
	}
	if !equalUnifiedRiskCounts(mergedStatusCounts, report.StatusCounts) {
		return fmt.Errorf("%w: merged legacy status counts differ", ErrUnifiedRiskValidation)
	}
	var duplicates int64
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM (
    SELECT legacy_source_job_id FROM content_moderation_logs
    WHERE legacy_source_job_id IS NOT NULL
    GROUP BY legacy_source_job_id HAVING COUNT(*) <> 1
) duplicate_sources`).Scan(&duplicates); err != nil {
		return fmt.Errorf("%w: validate unique merged source jobs: %v", ErrUnifiedRiskValidation, err)
	}
	if duplicates != 0 {
		return fmt.Errorf("%w: duplicate unified logs exist for legacy jobs", ErrUnifiedRiskValidation)
	}
	return nil
}
