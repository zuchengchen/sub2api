package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const (
	contentModerationArchiveLockFile   = ".sub2api.lock"
	contentModerationRetrySuffix       = ".retry.json"
	contentModerationEmergencySuffix   = ".emergency.json"
	contentModerationDispositionSuffix = ".disposition.json"
	contentModerationLostSummarySuffix = ".lost-summary.json"
	contentModerationDispositionCyber  = "cyber_policy"
	contentModerationDispositionLocal  = "local_auto_ban"
)

var ErrContentModerationArchiveDirectoryLocked = errors.New("content moderation archive directory is already locked")

type ContentModerationArchiveRuntimeOptions struct {
	KeyRingPath      string
	RetryDir         string
	EmergencyDir     string
	ChunkBytes       int
	DiskMinFreeBytes int64
	RetryInitial     time.Duration
	RetryMax         time.Duration
}

type ContentModerationArchiveRuntimeStatus struct {
	Degraded                 bool  `json:"degraded"`
	RetryQueueDepth          int64 `json:"retry_queue_depth"`
	EmergencyQueueDepth      int64 `json:"emergency_queue_depth"`
	ArchiveRetryAttempts     int64 `json:"archive_retry_attempts"`
	ArchiveRetryErrors       int64 `json:"archive_retry_errors"`
	ContentLost              int64 `json:"content_lost"`
	DiskFreeBytes            int64 `json:"disk_free_bytes"`
	DispositionQueueDepth    int64 `json:"disposition_queue_depth"`
	DispositionRetryAttempts int64 `json:"disposition_retry_attempts"`
	DispositionRetryErrors   int64 `json:"disposition_retry_errors"`
	LostSummaryQueueDepth    int64 `json:"lost_summary_queue_depth"`
}

type contentModerationDispositionRetryProcessor func(context.Context, *contentModerationDispositionRetryFile) error

type contentModerationArchiveRuntime struct {
	repo                 ContentModerationArchiveRepository
	keyRing              *ContentModerationArchiveKeyRingFile
	cipher               *ContentModerationArchiveCipher
	options              ContentModerationArchiveRuntimeOptions
	lockFile             *os.File
	stop                 chan struct{}
	done                 chan struct{}
	closeOnce            sync.Once
	dispositionMu        sync.RWMutex
	dispositionProcessor contentModerationDispositionRetryProcessor

	degraded                 atomic.Bool
	retryQueueDepth          atomic.Int64
	emergencyQueueDepth      atomic.Int64
	archiveRetryAttempts     atomic.Int64
	archiveRetryErrors       atomic.Int64
	contentLost              atomic.Int64
	diskFreeBytes            atomic.Int64
	dispositionQueueDepth    atomic.Int64
	dispositionRetryAttempts atomic.Int64
	dispositionRetryErrors   atomic.Int64
	lostSummaryQueueDepth    atomic.Int64
}

type contentModerationArchiveRetryFile struct {
	OperationKey string                            `json:"operation_key"`
	Attempts     int                               `json:"attempts"`
	NextAttempt  time.Time                         `json:"next_attempt"`
	LastError    string                            `json:"last_error"`
	Log          ContentModerationLog              `json:"log"`
	Archive      ContentModerationEncryptedArchive `json:"archive"`
}

type contentModerationEmergencyFile struct {
	OperationKey string               `json:"operation_key"`
	Attempts     int                  `json:"attempts"`
	NextAttempt  time.Time            `json:"next_attempt"`
	LastError    string               `json:"last_error"`
	Log          ContentModerationLog `json:"log"`
	EnvelopeB64  string               `json:"envelope_base64"`
}

type contentModerationLostSummaryRetryFile struct {
	OperationKey string               `json:"operation_key"`
	Attempts     int                  `json:"attempts"`
	NextAttempt  time.Time            `json:"next_attempt"`
	LastError    string               `json:"last_error"`
	Log          ContentModerationLog `json:"log"`
}

type contentModerationDispositionRetryFile struct {
	OperationKey            string    `json:"operation_key"`
	Kind                    string    `json:"kind,omitempty"`
	Attempts                int       `json:"attempts"`
	NextAttempt             time.Time `json:"next_attempt"`
	LastError               string    `json:"last_error"`
	UserID                  int64     `json:"user_id"`
	UserEmail               string    `json:"user_email,omitempty"`
	APIKeyID                int64     `json:"api_key_id"`
	UserRole                string    `json:"user_role"`
	LogID                   int64     `json:"log_id"`
	ArchiveID               string    `json:"archive_id"`
	CreatedAt               time.Time `json:"created_at"`
	DispositionTarget       string    `json:"disposition_target"`
	DispositionStatus       string    `json:"disposition_status,omitempty"`
	DispositionTransitioned bool      `json:"disposition_transitioned"`
	AutoBanned              bool      `json:"auto_banned"`
	ViolationCount          int       `json:"violation_count,omitempty"`
	DispositionComplete     bool      `json:"disposition_complete"`
	EmailEnabled            bool      `json:"email_enabled"`
	EmailRequired           bool      `json:"email_required"`
	EmailCompletionRequired bool      `json:"email_completion_required"`
	EmailSent               bool      `json:"email_sent"`
	BanThreshold            int       `json:"ban_threshold,omitempty"`
	ViolationWindowHours    int       `json:"violation_window_hours,omitempty"`
	ExcludeCyberPolicy      bool      `json:"exclude_cyber_policy,omitempty"`
}

func newContentModerationArchiveRuntime(repo ContentModerationArchiveRepository, options ContentModerationArchiveRuntimeOptions) (*contentModerationArchiveRuntime, error) {
	if repo == nil {
		return nil, nil
	}
	options.RetryDir = strings.TrimSpace(options.RetryDir)
	options.EmergencyDir = strings.TrimSpace(options.EmergencyDir)
	if options.RetryDir == "" || options.EmergencyDir == "" {
		return nil, errors.New("content moderation retry and emergency directories are required")
	}
	if options.RetryInitial <= 0 {
		options.RetryInitial = time.Second
	}
	if options.RetryMax <= 0 {
		options.RetryMax = 5 * time.Minute
	}
	if options.RetryMax < options.RetryInitial {
		options.RetryMax = options.RetryInitial
	}
	for _, dir := range []string{options.RetryDir, options.EmergencyDir} {
		if err := ensurePrivateModerationDirectory(dir); err != nil {
			return nil, err
		}
	}
	lockPath := filepath.Join(options.RetryDir, contentModerationArchiveLockFile)
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open content moderation archive lock: %w", err)
	}
	if err := lockFile.Chmod(0o600); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("restrict content moderation archive lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		return nil, fmt.Errorf("%w: %s", ErrContentModerationArchiveDirectoryLocked, options.RetryDir)
	}
	runtime := &contentModerationArchiveRuntime{
		repo: repo, options: options, lockFile: lockFile,
		keyRing: NewContentModerationArchiveKeyRingFile(options.KeyRingPath),
		stop:    make(chan struct{}), done: make(chan struct{}),
	}
	runtime.cipher = NewContentModerationArchiveCipher(runtime.keyRing, options.ChunkBytes)
	runtime.refreshDepths()
	if _, _, err := runtime.keyRing.Current(); err != nil {
		runtime.degraded.Store(true)
	}
	go runtime.loop()
	return runtime, nil
}

func ensurePrivateModerationDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create content moderation archive directory: %w", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("restrict content moderation archive directory: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return errors.New("content moderation archive directory is not private")
	}
	return nil
}

func (r *contentModerationArchiveRuntime) Close() {
	if r == nil {
		return
	}
	r.closeOnce.Do(func() {
		close(r.stop)
		<-r.done
		if r.lockFile != nil {
			_ = syscall.Flock(int(r.lockFile.Fd()), syscall.LOCK_UN)
			_ = r.lockFile.Close()
		}
	})
}

func (r *contentModerationArchiveRuntime) Status() ContentModerationArchiveRuntimeStatus {
	if r == nil {
		return ContentModerationArchiveRuntimeStatus{Degraded: true}
	}
	return ContentModerationArchiveRuntimeStatus{
		Degraded: r.degraded.Load(), RetryQueueDepth: r.retryQueueDepth.Load(),
		EmergencyQueueDepth: r.emergencyQueueDepth.Load(), ArchiveRetryAttempts: r.archiveRetryAttempts.Load(),
		ArchiveRetryErrors: r.archiveRetryErrors.Load(), ContentLost: r.contentLost.Load(), DiskFreeBytes: r.diskFreeBytes.Load(),
		DispositionQueueDepth: r.dispositionQueueDepth.Load(), DispositionRetryAttempts: r.dispositionRetryAttempts.Load(),
		DispositionRetryErrors: r.dispositionRetryErrors.Load(),
		LostSummaryQueueDepth:  r.lostSummaryQueueDepth.Load(),
	}
}

func (r *contentModerationArchiveRuntime) SetDispositionProcessor(processor contentModerationDispositionRetryProcessor) {
	if r == nil {
		return
	}
	r.dispositionMu.Lock()
	r.dispositionProcessor = processor
	r.dispositionMu.Unlock()
}

func (r *contentModerationArchiveRuntime) QueueDispositionRetry(input CyberPolicyRecordInput, log *ContentModerationLog, cause error) error {
	return r.queueDispositionRetry(contentModerationDispositionRetryFile{
		Kind: contentModerationDispositionCyber, UserID: input.UserID,
		UserEmail: input.UserEmail, APIKeyID: input.APIKeyID, UserRole: input.UserRole,
		EmailEnabled: true,
	}, log, cause)
}

func (r *contentModerationArchiveRuntime) QueueLocalDispositionRetry(input ContentModerationCheckInput, cfg *ContentModerationConfig, log *ContentModerationLog, dispositionComplete, emailEnabled, emailRequired, emailCompletionRequired, emailSent bool, cause error) error {
	entry := contentModerationDispositionRetryFile{
		Kind: contentModerationDispositionLocal, UserID: input.UserID,
		UserEmail: input.UserEmail, APIKeyID: input.APIKeyID, UserRole: input.UserRole,
		DispositionComplete: dispositionComplete, EmailEnabled: emailEnabled,
		EmailRequired: emailRequired, EmailCompletionRequired: emailCompletionRequired,
		EmailSent: emailSent,
	}
	if cfg != nil {
		entry.BanThreshold = cfg.BanThreshold
		entry.ViolationWindowHours = cfg.ViolationWindowHours
		entry.ExcludeCyberPolicy = cfg.CyberPolicyExcludeFromBanCount
	}
	return r.queueDispositionRetry(entry, log, cause)
}

func (r *contentModerationArchiveRuntime) QueueCyberDispositionRetry(input CyberPolicyRecordInput, log *ContentModerationLog, dispositionComplete, emailEnabled, emailRequired, emailCompletionRequired, emailSent bool, cause error) error {
	return r.queueDispositionRetry(contentModerationDispositionRetryFile{
		Kind: contentModerationDispositionCyber, UserID: input.UserID,
		UserEmail: input.UserEmail, APIKeyID: input.APIKeyID, UserRole: input.UserRole,
		DispositionComplete: dispositionComplete, EmailEnabled: emailEnabled,
		EmailRequired: emailRequired, EmailCompletionRequired: emailCompletionRequired,
		EmailSent: emailSent,
	}, log, cause)
}

func (r *contentModerationArchiveRuntime) queueDispositionRetry(entry contentModerationDispositionRetryFile, log *ContentModerationLog, cause error) error {
	if r == nil || log == nil {
		return errors.New("content moderation disposition retry runtime unavailable")
	}
	if cause == nil {
		cause = errors.New("content moderation disposition retry requested")
	}
	operationID := strings.TrimSpace(log.ArchiveID)
	if operationID == "" {
		operationID = uuid.NewString()
	}
	entry.OperationKey = "disposition:" + operationID
	entry.NextAttempt = time.Now().Add(r.options.RetryInitial)
	entry.LastError = boundedModerationArchiveError(errors.New(redactContentModerationSecrets(cause.Error())))
	entry.LogID = log.ID
	entry.ArchiveID = log.ArchiveID
	entry.CreatedAt = log.CreatedAt
	entry.DispositionTarget = log.DispositionTarget
	entry.DispositionStatus = log.DispositionStatus
	entry.DispositionTransitioned = log.DispositionTransitioned
	entry.AutoBanned = log.AutoBanned
	entry.ViolationCount = log.ViolationCount
	path := filepath.Join(r.options.RetryDir, operationID+contentModerationDispositionSuffix)
	if err := writePrivateModerationJSON(path, entry); err != nil {
		return fmt.Errorf("persist content moderation disposition retry: %w", err)
	}
	r.refreshDepths()
	return nil
}

func (r *contentModerationArchiveRuntime) RemoveLocalCopies(archiveID string) error {
	if r == nil {
		return nil
	}
	parsed, err := uuid.Parse(strings.TrimSpace(archiveID))
	if err != nil {
		return errors.New("invalid content moderation archive ID")
	}
	name := parsed.String()
	paths := []string{
		filepath.Join(r.options.RetryDir, name+contentModerationRetrySuffix),
		filepath.Join(r.options.RetryDir, name+contentModerationDispositionSuffix),
		filepath.Join(r.options.RetryDir, name+contentModerationLostSummarySuffix),
		filepath.Join(r.options.EmergencyDir, name+contentModerationEmergencySuffix),
	}
	var removeErrs []error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			removeErrs = append(removeErrs, err)
		}
	}
	r.refreshDepths()
	return errors.Join(removeErrs...)
}

func (r *contentModerationArchiveRuntime) Store(ctx context.Context, log *ContentModerationLog, envelope []byte) error {
	return r.storeWithArchiveID(ctx, log, uuid.NewString(), envelope)
}

func (r *contentModerationArchiveRuntime) storeWithArchiveID(ctx context.Context, log *ContentModerationLog, archiveID string, envelope []byte) error {
	if r == nil || log == nil {
		return errors.New("content moderation archive runtime unavailable")
	}
	archiveID = strings.TrimSpace(archiveID)
	if archiveID == "" {
		return errors.New("content moderation archive ID is required")
	}
	archive, err := r.cipher.Encrypt(archiveID, envelope)
	if err != nil {
		r.degraded.Store(true)
		return r.writeEmergency(log, archiveID, envelope, err)
	}
	r.degraded.Store(false)
	applyContentModerationArchiveMetadata(log, archive)
	if err := r.repo.CreateLogWithArchive(ctx, log, archive); err != nil {
		return r.writeEncryptedRetry(log, archive, err)
	}
	return nil
}

func (r *contentModerationArchiveRuntime) writeEncryptedRetry(log *ContentModerationLog, archive *ContentModerationEncryptedArchive, cause error) error {
	if !r.hasDiskHeadroom(r.options.RetryDir) {
		r.contentLost.Add(1)
		log.ArchiveStatus = ContentModerationArchiveStatusLost
		log.ArchiveContentLost = true
		return r.persistContentLostSummary(log, cause)
	}
	entry := contentModerationArchiveRetryFile{
		OperationKey: "archive:" + archive.ArchiveID, NextAttempt: time.Now().Add(r.options.RetryInitial),
		LastError: boundedModerationArchiveError(cause), Log: *cloneContentModerationLog(log), Archive: *archive,
	}
	path := filepath.Join(r.options.RetryDir, archive.ArchiveID+contentModerationRetrySuffix)
	if err := writePrivateModerationJSON(path, entry); err != nil {
		return fmt.Errorf("persist encrypted content moderation archive retry: %w", err)
	}
	r.refreshDepths()
	return fmt.Errorf("content moderation archive queued for retry: %w", cause)
}

func (r *contentModerationArchiveRuntime) writeEmergency(log *ContentModerationLog, archiveID string, envelope []byte, cause error) error {
	log.ArchiveID = archiveID
	if !r.hasDiskHeadroom(r.options.EmergencyDir) {
		r.contentLost.Add(1)
		log.ArchiveStatus = ContentModerationArchiveStatusLost
		log.ArchiveContentLost = true
		return r.persistContentLostSummary(log, cause)
	}
	log.ArchiveStatus = ContentModerationArchiveStatusEmergency
	entry := contentModerationEmergencyFile{
		OperationKey: "emergency:" + archiveID, NextAttempt: time.Now().Add(r.options.RetryInitial),
		LastError: boundedModerationArchiveError(cause), Log: *cloneContentModerationLog(log),
		EnvelopeB64: base64.StdEncoding.EncodeToString(envelope),
	}
	path := filepath.Join(r.options.EmergencyDir, archiveID+contentModerationEmergencySuffix)
	if err := writePrivateModerationJSON(path, entry); err != nil {
		return fmt.Errorf("persist content moderation emergency archive: %w", err)
	}
	r.refreshDepths()
	return fmt.Errorf("content moderation key unavailable; emergency archive queued: %w", cause)
}

func writePrivateModerationJSON(path string, value any) error {
	return writePrivateModerationJSONAtomic(path, value)
}

func writePrivateModerationJSONAtomic(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".moderation-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	writeErr := error(nil)
	if _, err := file.Write(raw); err != nil {
		writeErr = err
	} else if err := file.Sync(); err != nil {
		writeErr = err
	}
	if err := file.Close(); writeErr == nil {
		writeErr = err
	}
	if writeErr != nil {
		_ = os.Remove(tmp)
		return writeErr
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (r *contentModerationArchiveRuntime) hasDiskHeadroom(path string) bool {
	if r.options.DiskMinFreeBytes <= 0 {
		return true
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		slog.Error("content_moderation.archive_disk_stat_failed", "error", err)
		return false
	}
	free := int64(stat.Bavail) * int64(stat.Bsize)
	r.diskFreeBytes.Store(free)
	return free >= r.options.DiskMinFreeBytes
}

func (r *contentModerationArchiveRuntime) loop() {
	defer close(r.done)
	ticker := time.NewTicker(r.options.RetryInitial)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			r.processOnce(ctx)
			cancel()
		}
	}
}

func (r *contentModerationArchiveRuntime) processOnce(ctx context.Context) {
	if _, _, err := r.keyRing.Current(); err != nil {
		r.degraded.Store(true)
	} else {
		r.degraded.Store(false)
		r.processEmergency(ctx)
	}
	r.processEncryptedRetries(ctx)
	r.processLostSummaryRetries(ctx)
	r.processDispositionRetries(ctx)
	r.refreshDepths()
}

func (r *contentModerationArchiveRuntime) processEmergency(ctx context.Context) {
	paths, _ := filepath.Glob(filepath.Join(r.options.EmergencyDir, "*"+contentModerationEmergencySuffix))
	sort.Strings(paths)
	for _, path := range paths {
		var entry contentModerationEmergencyFile
		if err := readPrivateModerationJSON(path, &entry); err != nil || time.Now().Before(entry.NextAttempt) {
			continue
		}
		plaintext, err := base64.StdEncoding.DecodeString(entry.EnvelopeB64)
		if err == nil {
			archiveID := strings.TrimSuffix(filepath.Base(path), contentModerationEmergencySuffix)
			var archive *ContentModerationEncryptedArchive
			archive, err = r.cipher.Encrypt(archiveID, plaintext)
			if err == nil {
				entry.Log.ArchiveStatus = ContentModerationArchiveStatusAvailable
				err = r.repo.CreateLogWithArchive(ctx, &entry.Log, archive)
			}
		}
		r.archiveRetryAttempts.Add(1)
		if err == nil {
			_ = os.Remove(path)
			continue
		}
		r.archiveRetryErrors.Add(1)
		entry.Attempts++
		entry.LastError = boundedModerationArchiveError(err)
		entry.NextAttempt = time.Now().Add(r.backoff(entry.Attempts))
		_ = replacePrivateModerationJSON(path, entry)
	}
}

func (r *contentModerationArchiveRuntime) processEncryptedRetries(ctx context.Context) {
	paths, _ := filepath.Glob(filepath.Join(r.options.RetryDir, "*"+contentModerationRetrySuffix))
	sort.Strings(paths)
	for _, path := range paths {
		var entry contentModerationArchiveRetryFile
		if err := readPrivateModerationJSON(path, &entry); err != nil || time.Now().Before(entry.NextAttempt) {
			continue
		}
		r.archiveRetryAttempts.Add(1)
		err := r.repo.CreateLogWithArchive(ctx, &entry.Log, &entry.Archive)
		if err == nil {
			_ = os.Remove(path)
			continue
		}
		r.archiveRetryErrors.Add(1)
		entry.Attempts++
		entry.LastError = boundedModerationArchiveError(err)
		entry.NextAttempt = time.Now().Add(r.backoff(entry.Attempts))
		_ = replacePrivateModerationJSON(path, entry)
	}
}

func (r *contentModerationArchiveRuntime) persistContentLostSummary(log *ContentModerationLog, cause error) error {
	if r == nil || log == nil {
		return errors.New("content moderation content-lost summary unavailable")
	}
	log.ArchiveStatus = ContentModerationArchiveStatusLost
	log.ArchiveContentLost = true
	if err := r.repo.CreateContentLostLog(context.Background(), log); err == nil {
		return fmt.Errorf("content moderation archive content lost at disk threshold: %w", cause)
	} else {
		entry := contentModerationLostSummaryRetryFile{
			OperationKey: "content-lost:" + log.ArchiveID,
			NextAttempt:  time.Now().Add(r.options.RetryInitial),
			LastError:    boundedModerationArchiveError(err), Log: *cloneContentModerationLog(log),
		}
		path := filepath.Join(r.options.RetryDir, log.ArchiveID+contentModerationLostSummarySuffix)
		if writeErr := writePrivateModerationJSON(path, entry); writeErr != nil {
			return fmt.Errorf("content moderation archive content and summary lost at disk threshold: %v: %w", cause, writeErr)
		}
		r.refreshDepths()
		return fmt.Errorf("content moderation archive content lost; summary queued: %w", cause)
	}
}

func (r *contentModerationArchiveRuntime) processLostSummaryRetries(ctx context.Context) {
	paths, _ := filepath.Glob(filepath.Join(r.options.RetryDir, "*"+contentModerationLostSummarySuffix))
	sort.Strings(paths)
	for _, path := range paths {
		var entry contentModerationLostSummaryRetryFile
		if err := readPrivateModerationJSON(path, &entry); err != nil || time.Now().Before(entry.NextAttempt) {
			continue
		}
		r.archiveRetryAttempts.Add(1)
		err := r.repo.CreateContentLostLog(ctx, &entry.Log)
		if err == nil {
			_ = os.Remove(path)
			continue
		}
		r.archiveRetryErrors.Add(1)
		entry.Attempts++
		entry.LastError = boundedModerationArchiveError(err)
		entry.NextAttempt = time.Now().Add(r.backoff(entry.Attempts))
		_ = replacePrivateModerationJSON(path, entry)
	}
}

func (r *contentModerationArchiveRuntime) hasPendingArchive(archiveID string) bool {
	if r == nil {
		return false
	}
	archiveID = strings.TrimSpace(archiveID)
	if archiveID == "" {
		return false
	}
	for _, path := range []string{
		filepath.Join(r.options.RetryDir, archiveID+contentModerationRetrySuffix),
		filepath.Join(r.options.EmergencyDir, archiveID+contentModerationEmergencySuffix),
		filepath.Join(r.options.RetryDir, archiveID+contentModerationLostSummarySuffix),
	} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func (r *contentModerationArchiveRuntime) processDispositionRetries(ctx context.Context) {
	r.dispositionMu.RLock()
	processor := r.dispositionProcessor
	r.dispositionMu.RUnlock()
	if processor == nil {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(r.options.RetryDir, "*"+contentModerationDispositionSuffix))
	sort.Strings(paths)
	for _, path := range paths {
		var entry contentModerationDispositionRetryFile
		if err := readPrivateModerationJSON(path, &entry); err != nil || time.Now().Before(entry.NextAttempt) {
			continue
		}
		r.dispositionRetryAttempts.Add(1)
		err := processor(ctx, &entry)
		if err == nil {
			_ = os.Remove(path)
			continue
		}
		r.dispositionRetryErrors.Add(1)
		entry.Attempts++
		entry.LastError = boundedModerationArchiveError(err)
		entry.NextAttempt = time.Now().Add(r.backoff(entry.Attempts))
		_ = replacePrivateModerationJSON(path, entry)
	}
}

func readPrivateModerationJSON(path string, target any) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("content moderation retry file is not private")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("content moderation retry file has trailing data")
	}
	return nil
}

func replacePrivateModerationJSON(path string, value any) error {
	return writePrivateModerationJSONAtomic(path, value)
}

func (r *contentModerationArchiveRuntime) backoff(attempt int) time.Duration {
	backoff := r.options.RetryInitial
	for i := 0; i < attempt && backoff < r.options.RetryMax/2; i++ {
		backoff *= 2
	}
	if backoff > r.options.RetryMax {
		return r.options.RetryMax
	}
	return backoff
}

func (r *contentModerationArchiveRuntime) refreshDepths() {
	retry, _ := filepath.Glob(filepath.Join(r.options.RetryDir, "*"+contentModerationRetrySuffix))
	emergency, _ := filepath.Glob(filepath.Join(r.options.EmergencyDir, "*"+contentModerationEmergencySuffix))
	dispositions, _ := filepath.Glob(filepath.Join(r.options.RetryDir, "*"+contentModerationDispositionSuffix))
	lostSummaries, _ := filepath.Glob(filepath.Join(r.options.RetryDir, "*"+contentModerationLostSummarySuffix))
	r.retryQueueDepth.Store(int64(len(retry)))
	r.emergencyQueueDepth.Store(int64(len(emergency)))
	r.dispositionQueueDepth.Store(int64(len(dispositions)))
	r.lostSummaryQueueDepth.Store(int64(len(lostSummaries)))
}

func boundedModerationArchiveError(err error) string {
	if err == nil {
		return ""
	}
	value := err.Error()
	if len(value) > 1024 {
		value = value[:1024]
	}
	return value
}

func cloneContentModerationLog(log *ContentModerationLog) *ContentModerationLog {
	if log == nil {
		return &ContentModerationLog{}
	}
	clone := *log
	clone.UserID = cloneInt64Ptr(log.UserID)
	clone.APIKeyID = cloneInt64Ptr(log.APIKeyID)
	clone.GroupID = cloneInt64Ptr(log.GroupID)
	clone.LegacySourceJobID = cloneInt64Ptr(log.LegacySourceJobID)
	clone.CategoryScores = cloneFloatMap(log.CategoryScores)
	clone.ThresholdSnapshot = cloneFloatMap(log.ThresholdSnapshot)
	clone.ArchiveSHA256 = append([]byte(nil), log.ArchiveSHA256...)
	return &clone
}

func applyContentModerationArchiveMetadata(log *ContentModerationLog, archive *ContentModerationEncryptedArchive) {
	if log == nil || archive == nil {
		return
	}
	log.ArchiveID = archive.ArchiveID
	log.ArchiveVersion = archive.Version
	log.ArchiveKeyID = archive.KeyID
	log.ArchiveSHA256 = append([]byte(nil), archive.PlaintextHash...)
	log.ArchiveBytes = archive.PlaintextSize
	log.ArchiveStatus = ContentModerationArchiveStatusAvailable
}
