package service

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type moderationArchiveRuntimeTestRepo struct {
	mu       sync.Mutex
	fail     bool
	calls    int
	logs     []ContentModerationLog
	archives []ContentModerationEncryptedArchive
}

type moderationDispositionRetryTestRepo struct {
	contentModerationTestRepo
	mu                 sync.Mutex
	failDisposition    bool
	userActive         bool
	disableCalls       int
	archiveLog         *ContentModerationLog
	archive            *ContentModerationEncryptedArchive
	dispositionUpdates int
}

func (r *moderationDispositionRetryTestRepo) CreateLogWithArchive(_ context.Context, log *ContentModerationLog, archive *ContentModerationEncryptedArchive) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.archiveLog != nil && r.archiveLog.ArchiveID == archive.ArchiveID {
		log.ID = r.archiveLog.ID
		return nil
	}
	clone := cloneContentModerationLog(log)
	clone.ID = 101
	log.ID = clone.ID
	r.archiveLog = clone
	r.archive = cloneModerationEncryptedArchive(archive)
	return nil
}

func (r *moderationDispositionRetryTestRepo) CreateContentLostLog(_ context.Context, log *ContentModerationLog) error {
	return r.CreateLogWithArchive(context.Background(), log, &ContentModerationEncryptedArchive{
		ArchiveID: log.ArchiveID, Chunks: []ContentModerationArchiveChunk{{}},
	})
}

func (r *moderationDispositionRetryTestRepo) GetArchive(context.Context, int64) (*ContentModerationLog, *ContentModerationEncryptedArchive, error) {
	return nil, nil, sql.ErrNoRows
}

func (r *moderationDispositionRetryTestRepo) DeleteArchive(context.Context, ContentModerationArchiveAccess) (bool, error) {
	return false, nil
}

func (r *moderationDispositionRetryTestRepo) RecordArchiveAccess(context.Context, ContentModerationArchiveAccess) error {
	return nil
}

func (r *moderationDispositionRetryTestRepo) ReferencedArchiveKeyIDs(context.Context) ([]string, error) {
	return nil, nil
}

func (r *moderationDispositionRetryTestRepo) DisableUserIfActive(context.Context, int64) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disableCalls++
	if r.failDisposition {
		return false, errors.New("temporary disposition database failure")
	}
	if !r.userActive {
		return false, nil
	}
	r.userActive = false
	return true, nil
}

func (*moderationDispositionRetryTestRepo) DisableAPIKeyIfActive(context.Context, int64) (string, bool, error) {
	return "", false, nil
}

func (r *moderationDispositionRetryTestRepo) UpdateLogDispositionByArchiveID(_ context.Context, archiveID, status, target string, transitioned, autoBanned bool, violationCount int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.archiveLog == nil || r.archiveLog.ArchiveID != archiveID {
		return sql.ErrNoRows
	}
	r.archiveLog.DispositionStatus = status
	r.archiveLog.DispositionTarget = target
	r.archiveLog.DispositionTransitioned = transitioned
	r.archiveLog.AutoBanned = autoBanned
	r.archiveLog.ViolationCount = violationCount
	r.dispositionUpdates++
	return nil
}

func (r *moderationDispositionRetryTestRepo) setDispositionFailure(fail bool) {
	r.mu.Lock()
	r.failDisposition = fail
	r.mu.Unlock()
}

func (r *moderationDispositionRetryTestRepo) dispositionSnapshot() (int, bool, int, *ContentModerationLog) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.disableCalls, r.userActive, r.dispositionUpdates, cloneContentModerationLog(r.archiveLog)
}

func (r *moderationArchiveRuntimeTestRepo) CreateLogWithArchive(_ context.Context, log *ContentModerationLog, archive *ContentModerationEncryptedArchive) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fail {
		return errors.New("temporary database failure")
	}
	r.logs = append(r.logs, *cloneContentModerationLog(log))
	r.archives = append(r.archives, *cloneModerationEncryptedArchive(archive))
	return nil
}

func (r *moderationArchiveRuntimeTestRepo) CreateContentLostLog(_ context.Context, log *ContentModerationLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.fail {
		return errors.New("temporary database failure")
	}
	r.logs = append(r.logs, *cloneContentModerationLog(log))
	return nil
}

func (*moderationArchiveRuntimeTestRepo) GetArchive(context.Context, int64) (*ContentModerationLog, *ContentModerationEncryptedArchive, error) {
	return nil, nil, sql.ErrNoRows
}

func (*moderationArchiveRuntimeTestRepo) DeleteArchive(context.Context, ContentModerationArchiveAccess) (bool, error) {
	return false, nil
}

func (*moderationArchiveRuntimeTestRepo) RecordArchiveAccess(context.Context, ContentModerationArchiveAccess) error {
	return nil
}

func (*moderationArchiveRuntimeTestRepo) ReferencedArchiveKeyIDs(context.Context) ([]string, error) {
	return nil, nil
}

func (r *moderationArchiveRuntimeTestRepo) setFail(value bool) {
	r.mu.Lock()
	r.fail = value
	r.mu.Unlock()
}

func (r *moderationArchiveRuntimeTestRepo) snapshot() (int, []ContentModerationEncryptedArchive) {
	r.mu.Lock()
	defer r.mu.Unlock()
	archives := make([]ContentModerationEncryptedArchive, len(r.archives))
	copy(archives, r.archives)
	return r.calls, archives
}

func (r *moderationArchiveRuntimeTestRepo) snapshotLogs() []ContentModerationLog {
	r.mu.Lock()
	defer r.mu.Unlock()
	logs := make([]ContentModerationLog, len(r.logs))
	copy(logs, r.logs)
	return logs
}

func moderationArchiveRuntimeOptions(root, keyRing string) ContentModerationArchiveRuntimeOptions {
	return ContentModerationArchiveRuntimeOptions{
		KeyRingPath: keyRing, RetryDir: filepath.Join(root, "retry"), EmergencyDir: filepath.Join(root, "emergency"),
		ChunkBytes: 8, RetryInitial: time.Hour, RetryMax: 2 * time.Hour,
	}
}

func TestContentModerationArchiveRuntimeExclusiveLockAndPrivatePermissions(t *testing.T) {
	root := t.TempDir()
	keyRing := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	repo := &moderationArchiveRuntimeTestRepo{fail: true}
	options := moderationArchiveRuntimeOptions(root, keyRing)
	runtime, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	defer runtime.Close()

	_, err = newContentModerationArchiveRuntime(repo, options)
	require.ErrorIs(t, err, ErrContentModerationArchiveDirectoryLocked)

	log := &ContentModerationLog{Action: ContentModerationActionKeywordBlock}
	err = runtime.Store(context.Background(), log, []byte(`{"secret":"raw"}`))
	require.Error(t, err)
	require.Equal(t, ContentModerationArchiveStatusAvailable, log.ArchiveStatus)

	for _, dir := range []string{options.RetryDir, options.EmergencyDir} {
		info, statErr := os.Stat(dir)
		require.NoError(t, statErr)
		require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	}
	files, err := filepath.Glob(filepath.Join(options.RetryDir, "*"+contentModerationRetrySuffix))
	require.NoError(t, err)
	require.Len(t, files, 1)
	info, err := os.Stat(files[0])
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestContentModerationArchiveRuntimeRestrictsExistingLockFile(t *testing.T) {
	root := t.TempDir()
	retryDir := filepath.Join(root, "retry")
	require.NoError(t, os.MkdirAll(retryDir, 0o700))
	lockPath := filepath.Join(retryDir, contentModerationArchiveLockFile)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o666))
	require.NoError(t, os.Chmod(lockPath, 0o666))

	repo := &moderationArchiveRuntimeTestRepo{}
	options := moderationArchiveRuntimeOptions(root, filepath.Join(root, "missing-keyring.json"))
	runtime, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	t.Cleanup(runtime.Close)

	info, err := os.Stat(lockPath)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestContentModerationArchiveRuntimeImportsEmergencyAfterKeyRecovery(t *testing.T) {
	root := t.TempDir()
	keyRing := filepath.Join(root, "missing-keyring.json")
	repo := &moderationArchiveRuntimeTestRepo{}
	runtime, err := newContentModerationArchiveRuntime(repo, moderationArchiveRuntimeOptions(root, keyRing))
	require.NoError(t, err)
	defer runtime.Close()

	log := &ContentModerationLog{Action: ContentModerationActionCyberPolicy}
	err = runtime.Store(context.Background(), log, []byte("exact raw envelope"))
	require.Error(t, err)
	require.Equal(t, ContentModerationArchiveStatusEmergency, log.ArchiveStatus)
	require.True(t, runtime.Status().Degraded)

	files, err := filepath.Glob(filepath.Join(runtime.options.EmergencyDir, "*"+contentModerationEmergencySuffix))
	require.NoError(t, err)
	require.Len(t, files, 1)
	makeModerationRetryDue(t, files[0])
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	runtime.processOnce(context.Background())

	_, err = os.Stat(files[0])
	require.ErrorIs(t, err, os.ErrNotExist)
	_, archives := repo.snapshot()
	require.Len(t, archives, 1)
	plaintext, err := runtime.cipher.Decrypt(&archives[0])
	require.NoError(t, err)
	require.Equal(t, []byte("exact raw envelope"), plaintext)
	require.False(t, runtime.Status().Degraded)
}

func TestContentModerationArchiveRuntimeRetriesSameArchiveId(t *testing.T) {
	root := t.TempDir()
	keyRing := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	repo := &moderationArchiveRuntimeTestRepo{fail: true}
	runtime, err := newContentModerationArchiveRuntime(repo, moderationArchiveRuntimeOptions(root, keyRing))
	require.NoError(t, err)
	defer runtime.Close()

	log := &ContentModerationLog{Action: ContentModerationActionSecondLayerBlock}
	err = runtime.Store(context.Background(), log, []byte("raw"))
	require.Error(t, err)
	archiveID := log.ArchiveID
	files, err := filepath.Glob(filepath.Join(runtime.options.RetryDir, "*"+contentModerationRetrySuffix))
	require.NoError(t, err)
	require.Len(t, files, 1)
	makeModerationRetryDue(t, files[0])
	repo.setFail(false)
	runtime.processOnce(context.Background())

	_, err = os.Stat(files[0])
	require.ErrorIs(t, err, os.ErrNotExist)
	calls, archives := repo.snapshot()
	require.Equal(t, 2, calls)
	require.Len(t, archives, 1)
	require.Equal(t, archiveID, archives[0].ArchiveID)
}

func TestContentModerationDispositionRetrySurvivesRuntimeRestart(t *testing.T) {
	root := t.TempDir()
	keyRing := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	repo := &moderationArchiveRuntimeTestRepo{}
	options := moderationArchiveRuntimeOptions(root, keyRing)
	first, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	err = first.QueueDispositionRetry(CyberPolicyRecordInput{UserID: 7, APIKeyID: 9, UserRole: RoleAdmin}, &ContentModerationLog{ArchiveID: "c73f726e-4c80-45ad-9124-77d021fdb79a"}, errors.New("password=do-not-persist"))
	require.NoError(t, err)
	first.Close()

	second, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	defer second.Close()
	files, err := filepath.Glob(filepath.Join(options.RetryDir, "*"+contentModerationDispositionSuffix))
	require.NoError(t, err)
	require.Len(t, files, 1)
	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NotContains(t, string(raw), "do-not-persist")
	makeModerationRetryDue(t, files[0])

	processed := 0
	second.SetDispositionProcessor(func(_ context.Context, entry *contentModerationDispositionRetryFile) error {
		processed++
		require.Equal(t, int64(7), entry.UserID)
		require.Equal(t, int64(9), entry.APIKeyID)
		return nil
	})
	second.processOnce(context.Background())
	require.Equal(t, 1, processed)
	_, err = os.Stat(files[0])
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestLocalContentModerationDispositionFailureRetriesAcrossRestartOnce(t *testing.T) {
	root := t.TempDir()
	keyRing := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	options := moderationArchiveRuntimeOptions(root, keyRing)
	repo := &moderationDispositionRetryTestRepo{failDisposition: true, userActive: true}
	first, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	svc.archiveRuntime = first
	first.SetDispositionProcessor(svc.retryCyberPolicyDisposition)
	cfg := defaultContentModerationConfig()
	cfg.AutoBanEnabled = true
	cfg.BanThreshold = 1
	cfg.EmailOnHit = false
	input := ContentModerationCheckInput{
		UserID: 77, UserRole: RoleUser, UserEmail: "user@example.com",
		RawRequest: ContentModerationRawRequest{Method: "POST", Target: "/v1/responses", Body: []byte(`{"input":"blocked"}`)},
	}
	log := &ContentModerationLog{UserID: &input.UserID, UserEmail: input.UserEmail, Flagged: true, Action: ContentModerationActionKeywordBlock}
	svc.persistContentModerationLogWithInput(context.Background(), cfg, log, strings.Repeat("b", 64), false, true, &input)
	require.Equal(t, "retry_required", log.DispositionStatus)
	files, err := filepath.Glob(filepath.Join(options.RetryDir, "*"+contentModerationDispositionSuffix))
	require.NoError(t, err)
	require.Len(t, files, 1)
	first.Close()

	repo.setDispositionFailure(false)
	second, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	defer second.Close()
	svc.archiveRuntime = second
	second.SetDispositionProcessor(svc.retryCyberPolicyDisposition)
	makeModerationRetryDue(t, files[0])
	second.processOnce(context.Background())
	_, err = os.Stat(files[0])
	require.ErrorIs(t, err, os.ErrNotExist)
	calls, active, updates, stored := repo.dispositionSnapshot()
	require.Equal(t, 2, calls)
	require.False(t, active)
	require.Equal(t, 1, updates)
	require.Equal(t, "disabled", stored.DispositionStatus)
	require.True(t, stored.DispositionTransitioned)
	require.True(t, stored.AutoBanned)

	second.processOnce(context.Background())
	callsAfter, _, updatesAfter, _ := repo.dispositionSnapshot()
	require.Equal(t, calls, callsAfter)
	require.Equal(t, updates, updatesAfter)
}

func TestContentModerationArchiveRuntimeDiskThresholdMarksContentLostWithoutDeletingQueue(t *testing.T) {
	root := t.TempDir()
	keyRing := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	repo := &moderationArchiveRuntimeTestRepo{fail: true}
	options := moderationArchiveRuntimeOptions(root, keyRing)
	options.DiskMinFreeBytes = math.MaxInt64
	runtime, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	defer runtime.Close()

	oldPath := filepath.Join(options.RetryDir, "existing"+contentModerationRetrySuffix)
	require.NoError(t, os.WriteFile(oldPath, []byte(`{"existing":true}`), 0o600))
	log := &ContentModerationLog{Action: ContentModerationActionKeywordBlock}
	err = runtime.Store(context.Background(), log, []byte("new raw"))
	require.Error(t, err)
	require.Equal(t, ContentModerationArchiveStatusLost, log.ArchiveStatus)
	require.True(t, log.ArchiveContentLost)
	_, err = os.Stat(oldPath)
	require.NoError(t, err, "disk guard must never delete an existing retry")
	require.Equal(t, int64(1), runtime.Status().ContentLost)
}

func TestContentModerationArchiveRuntimeContentLostSummarySurvivesRestart(t *testing.T) {
	root := t.TempDir()
	keyRing := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	repo := &moderationArchiveRuntimeTestRepo{fail: true}
	options := moderationArchiveRuntimeOptions(root, keyRing)
	options.DiskMinFreeBytes = math.MaxInt64

	first, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	log := &ContentModerationLog{Action: ContentModerationActionKeywordBlock, InputExcerpt: "redacted summary"}
	err = first.Store(context.Background(), log, []byte("raw body that must not enter the summary retry"))
	require.Error(t, err)
	require.Equal(t, ContentModerationArchiveStatusLost, log.ArchiveStatus)
	files, err := filepath.Glob(filepath.Join(options.RetryDir, "*"+contentModerationLostSummarySuffix))
	require.NoError(t, err)
	require.Len(t, files, 1)
	raw, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.NotContains(t, string(raw), "raw body that must not enter the summary retry")
	first.Close()

	repo.setFail(false)
	second, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	defer second.Close()
	makeModerationRetryDue(t, files[0])
	second.processOnce(context.Background())
	_, err = os.Stat(files[0])
	require.ErrorIs(t, err, os.ErrNotExist)
	logs := repo.snapshotLogs()
	require.Len(t, logs, 1)
	require.Equal(t, ContentModerationArchiveStatusLost, logs[0].ArchiveStatus)
	require.True(t, logs[0].ArchiveContentLost)
}

func makeModerationRetryDue(t *testing.T, path string) {
	t.Helper()
	var raw map[string]any
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, &raw))
	raw["next_attempt"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	require.NoError(t, replacePrivateModerationJSON(path, raw))
}
