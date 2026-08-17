// unified-risk-migration backs up and atomically retires the historical
// Prompt Audit store after staging it into unified Risk Control logs.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

const commandTimeout = 30 * time.Minute

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	if err := run(ctx, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "unified risk migration failed: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: unified-risk-migration <backup|verify|prepare|finalize|status> [options]")
	os.Exit(2)
}

func run(ctx context.Context, command string, args []string) error {
	cfg, err := config.LoadForBootstrap()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	db, err := openMigrationDB(ctx, cfg.Database, cfg.Timezone)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	switch command {
	case "backup":
		return runBackup(ctx, db, cfg, args)
	case "verify":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		proofPath := flags.String("proof", "", "verified backup proof JSON")
		if err := flags.Parse(args); err != nil {
			return err
		}
		proof, err := repository.NewUnifiedRiskMigrator(db, nil, *proofPath).Verify(ctx)
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, proof)
	case "prepare", "finalize":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		proofPath := flags.String("proof", "", "verified backup proof JSON")
		keyRingPath := flags.String("key-ring", cfg.ContentModerationArchive.KeyRingPath, "external archive key-ring path")
		chunkBytes := flags.Int("chunk-bytes", cfg.ContentModerationArchive.ChunkBytes, "archive encryption chunk size")
		maintenance := flags.Bool("maintenance-confirmed", false, "confirm traffic is in maintenance mode (finalize only)")
		if err := flags.Parse(args); err != nil {
			return err
		}
		preflight := repository.NewUnifiedRiskMigrator(db, nil, *proofPath)
		if _, err := preflight.Verify(ctx); err != nil {
			return err
		}
		if err := repository.ApplyMigrations(ctx, db); err != nil {
			return fmt.Errorf("apply migration support schema: %w", err)
		}
		cipher := service.NewContentModerationArchiveCipher(
			service.NewContentModerationArchiveKeyRingFile(*keyRingPath), *chunkBytes)
		migrator := repository.NewUnifiedRiskMigrator(db, cipher, *proofPath)
		if command == "prepare" {
			if *maintenance {
				return errors.New("--maintenance-confirmed is not valid for online prepare")
			}
			report, err := migrator.Prepare(ctx)
			if err != nil {
				return err
			}
			return writeJSON(os.Stdout, report)
		}
		report, err := migrator.Finalize(ctx, repository.UnifiedRiskFinalizeOptions{MaintenanceMode: *maintenance})
		if err != nil {
			return err
		}
		return writeJSON(os.Stdout, report)
	case "status":
		flags := flag.NewFlagSet(command, flag.ContinueOnError)
		requireFinalized := flags.Bool("require-finalized", false, "fail unless final migration and archive runtime prerequisites are ready")
		keyRingPath := flags.String("key-ring", cfg.ContentModerationArchive.KeyRingPath, "external archive key-ring path")
		retryDir := flags.String("retry-dir", cfg.ContentModerationArchive.RetryDir, "archive retry directory")
		emergencyDir := flags.String("emergency-dir", cfg.ContentModerationArchive.EmergencyDir, "archive emergency directory")
		if err := flags.Parse(args); err != nil {
			return err
		}
		if flags.NArg() != 0 {
			return errors.New("status accepts no positional arguments")
		}
		return writeMigrationStatus(ctx, db, os.Stdout, migrationStatusOptions{
			requireFinalized: *requireFinalized, keyRingPath: *keyRingPath,
			retryDir: *retryDir, emergencyDir: *emergencyDir,
		})
	default:
		return fmt.Errorf("unknown command %q", command)
	}
}

func openMigrationDB(ctx context.Context, database config.DatabaseConfig, timezone string) (*sql.DB, error) {
	db, err := sql.Open("postgres", database.DSNWithTimezone(timezone))
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	return db, nil
}

type backupOptions struct {
	archivePath   string
	proofPath     string
	pgDumpPath    string
	pgRestorePath string
	adminDatabase string
	restorePrefix string
}

func runBackup(ctx context.Context, sourceDB *sql.DB, cfg *config.Config, args []string) error {
	flags := flag.NewFlagSet("backup", flag.ContinueOnError)
	options := backupOptions{}
	flags.StringVar(&options.archivePath, "archive", "", "output path for custom-format pg_dump (required)")
	flags.StringVar(&options.proofPath, "proof", "", "output path for verified proof JSON (required)")
	flags.StringVar(&options.pgDumpPath, "pg-dump", "pg_dump", "pg_dump executable")
	flags.StringVar(&options.pgRestorePath, "pg-restore", "pg_restore", "pg_restore executable")
	flags.StringVar(&options.adminDatabase, "admin-database", "postgres", "database used to create the isolated restore database")
	flags.StringVar(&options.restorePrefix, "restore-prefix", "sub2api_risk_restore", "isolated restore database name prefix")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(options.archivePath) == "" || strings.TrimSpace(options.proofPath) == "" {
		return errors.New("backup requires --archive and --proof and accepts no positional arguments")
	}
	adminConfig, err := migrationAdminDatabaseConfig(cfg.Database, options.adminDatabase)
	if err != nil {
		return err
	}
	adminDB, err := openMigrationDB(ctx, adminConfig, cfg.Timezone)
	if err != nil {
		return fmt.Errorf("open migration admin database: %w", err)
	}
	defer func() { _ = adminDB.Close() }()
	return createVerifiedBackup(ctx, sourceDB, adminDB, cfg.Database, adminConfig, options)
}

func migrationAdminDatabaseConfig(source config.DatabaseConfig, defaultDatabase string) (config.DatabaseConfig, error) {
	result := source
	result.DBName = strings.TrimSpace(defaultDatabase)
	if result.DBName == "" {
		return result, errors.New("migration admin database is required")
	}
	applyStringEnv := func(key string, target *string) {
		if value, ok := os.LookupEnv(key); ok {
			*target = strings.TrimSpace(value)
		}
	}
	applyStringEnv("SUB2API_MIGRATION_ADMIN_HOST", &result.Host)
	applyStringEnv("SUB2API_MIGRATION_ADMIN_USER", &result.User)
	applyStringEnv("SUB2API_MIGRATION_ADMIN_PASSWORD", &result.Password)
	applyStringEnv("SUB2API_MIGRATION_ADMIN_SSLMODE", &result.SSLMode)
	applyStringEnv("SUB2API_MIGRATION_ADMIN_DATABASE", &result.DBName)
	if value, ok := os.LookupEnv("SUB2API_MIGRATION_ADMIN_PORT"); ok {
		port, err := strconv.Atoi(strings.TrimSpace(value))
		if err != nil || port < 1 || port > 65535 {
			return result, errors.New("SUB2API_MIGRATION_ADMIN_PORT must be a valid TCP port")
		}
		result.Port = port
	}
	return result, nil
}

func createVerifiedBackup(ctx context.Context, sourceDB, adminDB *sql.DB, sourceConfig, adminConfig config.DatabaseConfig, options backupOptions) error {
	archivePath, err := filepath.Abs(options.archivePath)
	if err != nil {
		return fmt.Errorf("resolve backup archive path: %w", err)
	}
	proofPath, err := filepath.Abs(options.proofPath)
	if err != nil {
		return fmt.Errorf("resolve backup proof path: %w", err)
	}
	if archivePath == proofPath {
		return errors.New("backup archive and proof paths must differ")
	}
	for _, path := range []string{archivePath, proofPath} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite existing output %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect output %s: %w", path, err)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return fmt.Errorf("create output directory for %s: %w", path, err)
		}
	}
	temporaryArchive := archivePath + ".tmp-" + uuid.NewString()
	temporaryProof := proofPath + ".tmp-" + uuid.NewString()
	defer func() {
		_ = os.Remove(temporaryArchive)
		_ = os.Remove(temporaryProof)
	}()
	archiveFile, err := os.OpenFile(temporaryArchive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create temporary backup archive: %w", err)
	}
	if err := archiveFile.Close(); err != nil {
		return fmt.Errorf("close temporary backup archive: %w", err)
	}

	tx, err := sourceDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin consistent backup snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	snapshotID, sourceSnapshot, err := readBackupSourceSnapshot(ctx, tx)
	if err != nil {
		return err
	}
	dumpArgs := []string{
		"--format=custom", "--no-owner", "--no-privileges",
		"--snapshot=" + snapshotID, "--table=public.prompt_audit_jobs",
		"--table=public.prompt_audit_events", "--file=" + temporaryArchive,
	}
	if _, err := runPostgresTool(ctx, options.pgDumpPath, dumpArgs, sourceConfig); err != nil {
		return fmt.Errorf("create Prompt Audit pg_dump: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("release consistent backup snapshot: %w", err)
	}
	if err := os.Chmod(temporaryArchive, 0o600); err != nil {
		return fmt.Errorf("restrict backup archive permissions: %w", err)
	}
	listOutput, err := runPostgresTool(ctx, options.pgRestorePath, []string{"--list", temporaryArchive}, sourceConfig)
	if err != nil {
		return fmt.Errorf("list Prompt Audit pg_dump: %w", err)
	}
	if err := validateRestoreList(listOutput); err != nil {
		return err
	}
	listDigest := sha256.Sum256(listOutput)
	isolatedList, err := isolatedRestoreList(listOutput)
	if err != nil {
		return err
	}
	isolatedListPath := temporaryArchive + ".restore-list"
	if err := os.WriteFile(isolatedListPath, isolatedList, 0o600); err != nil {
		return fmt.Errorf("write isolated restore list: %w", err)
	}
	defer func() { _ = os.Remove(isolatedListPath) }()

	restoreDatabase := safeRestoreDatabaseName(options.restorePrefix)
	if _, err := adminDB.ExecContext(ctx, `CREATE DATABASE `+pq.QuoteIdentifier(restoreDatabase)); err != nil {
		return fmt.Errorf("create isolated restore database: %w", err)
	}
	restoreCreated := true
	defer func() {
		if restoreCreated {
			_ = dropRestoreDatabase(context.Background(), adminDB, restoreDatabase)
		}
	}()
	restoreConfig := adminConfig
	restoreConfig.DBName = restoreDatabase
	restoreDB, err := openMigrationDB(ctx, restoreConfig, "UTC")
	if err != nil {
		return fmt.Errorf("connect isolated restore database: %w", err)
	}
	if err := repository.ApplyMigrations(ctx, restoreDB); err != nil {
		_ = restoreDB.Close()
		return fmt.Errorf("prepare isolated restore dependencies: %w", err)
	}
	if err := restoreDB.Close(); err != nil {
		return fmt.Errorf("close isolated restore database before pg_restore: %w", err)
	}
	restoreArgs := []string{
		"--clean", "--if-exists", "--exit-on-error", "--no-owner", "--no-privileges",
		"--use-list=" + isolatedListPath, "--dbname=" + restoreDatabase, temporaryArchive,
	}
	if _, err := runPostgresTool(ctx, options.pgRestorePath, restoreArgs, restoreConfig); err != nil {
		return fmt.Errorf("restore Prompt Audit pg_dump in isolation: %w", err)
	}
	restoreDB, err = openMigrationDB(ctx, restoreConfig, "UTC")
	if err != nil {
		return fmt.Errorf("reconnect isolated restore database: %w", err)
	}
	restoredSnapshot, err := readRestoredBackupSnapshot(ctx, restoreDB)
	closeErr := restoreDB.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close verified restore database: %w", closeErr)
	}
	if sourceSnapshot.JobCount != restoredSnapshot.JobCount ||
		sourceSnapshot.EventCount != restoredSnapshot.EventCount ||
		!equalStatusCounts(sourceSnapshot.StatusCounts, restoredSnapshot.StatusCounts) {
		return errors.New("isolated Prompt Audit restore counts differ from the exported snapshot")
	}
	if err := dropRestoreDatabase(ctx, adminDB, restoreDatabase); err != nil {
		return err
	}
	restoreCreated = false

	archiveDigest, archiveBytes, err := digestRegularFile(temporaryArchive)
	if err != nil {
		return err
	}
	proof := repository.UnifiedRiskBackupProof{
		Version: repository.UnifiedRiskBackupProofVersion, VerifiedAt: time.Now().UTC(),
		SourceDatabase:         sourceSnapshot.Database,
		SourceSystemIdentifier: sourceSnapshot.SystemIdentifier,
		RestoreDatabase:        restoreDatabase, ArchivePath: archivePath,
		ArchiveSHA256: hex.EncodeToString(archiveDigest), ArchiveBytes: archiveBytes,
		RestoreListSHA256: hex.EncodeToString(listDigest[:]),
		SourceJobCount:    sourceSnapshot.JobCount, SourceEventCount: sourceSnapshot.EventCount,
		SourceMaxJobID: sourceSnapshot.MaxJobID, SourceMaxEventID: sourceSnapshot.MaxEventID,
		SourceStatusCounts: sourceSnapshot.StatusCounts,
		RestoredJobCount:   restoredSnapshot.JobCount, RestoredEventCount: restoredSnapshot.EventCount,
		RestoredStatusCounts: restoredSnapshot.StatusCounts,
		ListVerified:         true, RestoreVerified: true,
	}
	if err := writeJSONFile(temporaryProof, proof); err != nil {
		return err
	}
	if err := os.Rename(temporaryArchive, archivePath); err != nil {
		return fmt.Errorf("publish verified backup archive: %w", err)
	}
	if err := os.Rename(temporaryProof, proofPath); err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("publish verified backup proof: %w", err)
	}
	return writeJSON(os.Stdout, proof)
}

type backupSnapshot struct {
	Database         string
	SystemIdentifier string
	JobCount         int64
	EventCount       int64
	MaxJobID         int64
	MaxEventID       int64
	StatusCounts     map[string]int64
}

func readBackupSourceSnapshot(ctx context.Context, tx *sql.Tx) (string, backupSnapshot, error) {
	var snapshotID string
	var result backupSnapshot
	err := tx.QueryRowContext(ctx, `
SELECT pg_export_snapshot(), current_database(), system_identifier::text,
       (SELECT COUNT(*) FROM prompt_audit_jobs),
       (SELECT COUNT(*) FROM prompt_audit_events),
       (SELECT COALESCE(MAX(id), 0) FROM prompt_audit_jobs),
       (SELECT COALESCE(MAX(id), 0) FROM prompt_audit_events)
FROM pg_control_system()`).Scan(&snapshotID, &result.Database, &result.SystemIdentifier,
		&result.JobCount, &result.EventCount, &result.MaxJobID, &result.MaxEventID)
	if err != nil {
		return "", result, fmt.Errorf("read consistent Prompt Audit backup snapshot: %w", err)
	}
	result.StatusCounts, err = readStatusCounts(ctx, tx)
	if err != nil {
		return "", result, err
	}
	return snapshotID, result, nil
}

type statusCountQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func readStatusCounts(ctx context.Context, db statusCountQueryer) (map[string]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT status, COUNT(*) FROM prompt_audit_jobs GROUP BY status ORDER BY status`)
	if err != nil {
		return nil, fmt.Errorf("read Prompt Audit status counts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	result := make(map[string]int64)
	for rows.Next() {
		var status string
		var count int64
		if err := rows.Scan(&status, &count); err != nil {
			return nil, fmt.Errorf("scan Prompt Audit status counts: %w", err)
		}
		result[status] = count
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Prompt Audit status counts: %w", err)
	}
	return result, nil
}

func readRestoredBackupSnapshot(ctx context.Context, db *sql.DB) (backupSnapshot, error) {
	var result backupSnapshot
	err := db.QueryRowContext(ctx, `
SELECT COUNT(*), (SELECT COUNT(*) FROM prompt_audit_events),
       COALESCE(MAX(id), 0), (SELECT COALESCE(MAX(id), 0) FROM prompt_audit_events)
FROM prompt_audit_jobs`).Scan(&result.JobCount, &result.EventCount, &result.MaxJobID, &result.MaxEventID)
	if err != nil {
		return result, fmt.Errorf("read isolated Prompt Audit restore counts: %w", err)
	}
	result.StatusCounts, err = readStatusCounts(ctx, db)
	if err != nil {
		return result, err
	}
	var tables, indexes int64
	err = db.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM pg_tables WHERE schemaname = 'public' AND tablename IN ('prompt_audit_jobs', 'prompt_audit_events')),
       (SELECT COUNT(*) FROM pg_indexes WHERE schemaname = 'public' AND indexname LIKE 'idx_prompt_audit_%')`).Scan(&tables, &indexes)
	if err != nil {
		return result, fmt.Errorf("inspect isolated Prompt Audit restore objects: %w", err)
	}
	if tables != 2 || indexes < 10 {
		return result, fmt.Errorf("isolated Prompt Audit restore is incomplete: tables=%d indexes=%d", tables, indexes)
	}
	return result, nil
}

func validateRestoreList(list []byte) error {
	text := string(list)
	required := []string{
		"TABLE public prompt_audit_jobs", "TABLE DATA public prompt_audit_jobs",
		"TABLE public prompt_audit_events", "TABLE DATA public prompt_audit_events",
		"INDEX public idx_prompt_audit_jobs_schedule", "INDEX public idx_prompt_audit_events_job",
		"FK CONSTRAINT public prompt_audit_events prompt_audit_events_api_key_id_fkey",
		"FK CONSTRAINT public prompt_audit_events prompt_audit_events_group_id_fkey",
		"FK CONSTRAINT public prompt_audit_events prompt_audit_events_job_id_fkey",
		"FK CONSTRAINT public prompt_audit_events prompt_audit_events_user_id_fkey",
		"FK CONSTRAINT public prompt_audit_jobs prompt_audit_jobs_api_key_id_fkey",
		"FK CONSTRAINT public prompt_audit_jobs prompt_audit_jobs_group_id_fkey",
		"FK CONSTRAINT public prompt_audit_jobs prompt_audit_jobs_user_id_fkey",
	}
	for _, marker := range required {
		if !strings.Contains(text, marker) {
			return fmt.Errorf("pg_restore listing is missing required object %q", marker)
		}
	}
	return nil
}

func isolatedRestoreList(list []byte) ([]byte, error) {
	const internalForeignKey = "FK CONSTRAINT public prompt_audit_events prompt_audit_events_job_id_fkey"
	const promptAuditForeignKey = "FK CONSTRAINT public prompt_audit_"

	var result bytes.Buffer
	removed := 0
	internalFound := false
	for _, line := range bytes.SplitAfter(list, []byte("\n")) {
		text := string(line)
		if strings.Contains(text, internalForeignKey) {
			internalFound = true
		}
		if strings.Contains(text, promptAuditForeignKey) && !strings.Contains(text, internalForeignKey) {
			removed++
			continue
		}
		_, _ = result.Write(line)
	}
	if !internalFound || removed != 6 {
		return nil, fmt.Errorf("unexpected Prompt Audit foreign-key layout: internal_found=%t external_count=%d", internalFound, removed)
	}
	return result.Bytes(), nil
}

func runPostgresTool(ctx context.Context, executable string, args []string, database config.DatabaseConfig) ([]byte, error) {
	path, err := exec.LookPath(executable)
	if err != nil {
		return nil, fmt.Errorf("locate %s: %w", executable, err)
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Env = postgresToolEnvironment(database)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		message := output.Bytes()
		if len(message) > 4096 {
			message = message[:4096]
		}
		return nil, fmt.Errorf("%s exited unsuccessfully: %w: %s", filepath.Base(path), err, strings.TrimSpace(string(message)))
	}
	return output.Bytes(), nil
}

func postgresToolEnvironment(database config.DatabaseConfig) []string {
	filtered := make([]string, 0, len(os.Environ())+6)
	for _, item := range os.Environ() {
		key := item
		if index := strings.IndexByte(item, '='); index >= 0 {
			key = item[:index]
		}
		switch key {
		case "PGHOST", "PGPORT", "PGUSER", "PGPASSWORD", "PGDATABASE", "PGSSLMODE":
			continue
		}
		filtered = append(filtered, item)
	}
	return append(filtered,
		"PGHOST="+database.Host,
		"PGPORT="+strconv.Itoa(database.Port),
		"PGUSER="+database.User,
		"PGPASSWORD="+database.Password,
		"PGDATABASE="+database.DBName,
		"PGSSLMODE="+database.SSLMode,
	)
}

func safeRestoreDatabaseName(prefix string) string {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	var normalized strings.Builder
	for _, r := range prefix {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			_, _ = normalized.WriteRune(r)
		}
	}
	if normalized.Len() == 0 {
		_, _ = normalized.WriteString("sub2api_risk_restore")
	}
	name := normalized.String()
	if len(name) > 40 {
		name = name[:40]
	}
	return name + "_" + strings.ReplaceAll(uuid.NewString()[:12], "-", "")
}

func dropRestoreDatabase(ctx context.Context, adminDB *sql.DB, database string) error {
	dropCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := adminDB.ExecContext(dropCtx, `DROP DATABASE IF EXISTS `+pq.QuoteIdentifier(database)+` WITH (FORCE)`); err != nil {
		return fmt.Errorf("drop isolated restore database %s: %w", database, err)
	}
	return nil
}

func digestRegularFile(path string) ([]byte, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("open verified backup archive: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, 0, errors.New("verified backup archive is not a regular file")
	}
	hash := sha256.New()
	bytesWritten, err := io.Copy(hash, file)
	if err != nil {
		return nil, 0, fmt.Errorf("hash verified backup archive: %w", err)
	}
	return hash.Sum(nil), bytesWritten, nil
}

func writeJSONFile(path string, value any) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create backup proof: %w", err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	encodeErr := encoder.Encode(value)
	syncErr := file.Sync()
	closeErr := file.Close()
	if encodeErr != nil {
		return fmt.Errorf("encode backup proof: %w", encodeErr)
	}
	if syncErr != nil {
		return fmt.Errorf("sync backup proof: %w", syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close backup proof: %w", closeErr)
	}
	return nil
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func equalStatusCounts(left, right map[string]int64) bool {
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

type migrationStatusOptions struct {
	requireFinalized bool
	keyRingPath      string
	retryDir         string
	emergencyDir     string
}

func writeMigrationStatus(ctx context.Context, db *sql.DB, writer io.Writer, options migrationStatusOptions) error {
	var stateTable, jobsTable, eventsTable bool
	err := db.QueryRowContext(ctx, `
SELECT to_regclass('public.unified_risk_migration_state') IS NOT NULL,
       to_regclass('public.prompt_audit_jobs') IS NOT NULL,
       to_regclass('public.prompt_audit_events') IS NOT NULL`).Scan(&stateTable, &jobsTable, &eventsTable)
	if err != nil {
		return fmt.Errorf("read migration object status: %w", err)
	}
	result := map[string]any{
		"state_table": stateTable, "prompt_audit_jobs": jobsTable,
		"prompt_audit_events": eventsTable,
	}
	status := ""
	if stateTable {
		var jobCount, eventCount int64
		var updatedAt time.Time
		err := db.QueryRowContext(ctx, `
SELECT status, staged_job_count, staged_event_count, updated_at
FROM unified_risk_migration_state WHERE singleton = TRUE`).Scan(&status, &jobCount, &eventCount, &updatedAt)
		if err == nil {
			result["status"] = status
			result["staged_job_count"] = jobCount
			result["staged_event_count"] = eventCount
			result["updated_at"] = updatedAt
		} else if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("read migration state: %w", err)
		}
	}
	if options.requireFinalized {
		if status != "finalized" || jobsTable || eventsTable {
			return errors.New("unified risk cutover is not finalized or legacy tables still exist")
		}
		if err := verifyMigrationArchiveKeyReferences(ctx, db, options.keyRingPath); err != nil {
			return err
		}
		for _, dir := range []string{options.retryDir, options.emergencyDir} {
			if err := verifyMigrationRuntimeDirectory(dir); err != nil {
				return err
			}
		}
		if err := verifyMigrationRuntimeLock(options.retryDir); err != nil {
			return err
		}
		result["archive_key_references"] = "ready"
		result["runtime_directories"] = "ready"
		result["runtime_queue_lock"] = "held"
		result["ready"] = true
	}
	return writeJSON(writer, result)
}

func verifyMigrationArchiveKeyReferences(ctx context.Context, db *sql.DB, path string) error {
	ring := service.NewContentModerationArchiveKeyRingFile(path)
	available, err := ring.KeyIDs()
	if err != nil {
		return fmt.Errorf("archive key ring is not ready: %w", err)
	}
	availableSet := make(map[string]struct{}, len(available))
	for _, keyID := range available {
		availableSet[keyID] = struct{}{}
	}
	archiveRepo, ok := repository.NewContentModerationRepository(db).(service.ContentModerationArchiveRepository)
	if !ok {
		return errors.New("content moderation archive repository is unavailable")
	}
	referenced, err := archiveRepo.ReferencedArchiveKeyIDs(ctx)
	if err != nil {
		return fmt.Errorf("read referenced content moderation key IDs: %w", err)
	}
	for _, keyID := range referenced {
		if _, ok := availableSet[keyID]; !ok {
			return fmt.Errorf("content moderation key ring is missing referenced key ID %q", keyID)
		}
	}
	return nil
}

func verifyMigrationRuntimeDirectory(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("archive runtime directory path is empty")
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("archive runtime directory %q must exist with mode 0700", path)
	}
	probe, err := os.CreateTemp(path, ".cutover-readiness-*")
	if err != nil {
		return fmt.Errorf("archive runtime directory %q is not writable: %w", path, err)
	}
	probePath := probe.Name()
	closeErr := probe.Close()
	removeErr := os.Remove(probePath)
	if closeErr != nil || removeErr != nil {
		return fmt.Errorf("archive runtime directory %q readiness probe cleanup failed", path)
	}
	return nil
}

func verifyMigrationRuntimeLock(retryDir string) error {
	lockPath := filepath.Join(strings.TrimSpace(retryDir), ".sub2api.lock")
	info, err := os.Stat(lockPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("archive runtime lock %q must be a regular file with mode 0600", lockPath)
	}
	lockFile, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open archive runtime lock %q: %w", lockPath, err)
	}
	defer func() { _ = lockFile.Close() }()
	err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("probe archive runtime lock %q: %w", lockPath, err)
	}
	_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	return fmt.Errorf("archive runtime lock %q is not held by the service", lockPath)
}

func init() {
	// Backup files may contain raw historical prompts. Restrict any files a
	// PostgreSQL client creates even if the invoking shell has a permissive umask.
	syscall.Umask(0o077)
}
