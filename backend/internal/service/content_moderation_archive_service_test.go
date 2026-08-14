package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/stretchr/testify/require"
)

type moderationArchiveServiceTestRepo struct {
	mu       sync.Mutex
	log      *ContentModerationLog
	archive  *ContentModerationEncryptedArchive
	audits   []ContentModerationArchiveAccess
	auditErr error
	deleted  bool
}

func (*moderationArchiveServiceTestRepo) CreateLog(context.Context, *ContentModerationLog) error {
	return nil
}
func (*moderationArchiveServiceTestRepo) ListLogs(context.Context, ContentModerationLogFilter) ([]ContentModerationLog, *pagination.PaginationResult, error) {
	return nil, &pagination.PaginationResult{}, nil
}
func (*moderationArchiveServiceTestRepo) CountFlaggedByUserSince(context.Context, int64, time.Time, bool) (int, error) {
	return 0, nil
}
func (*moderationArchiveServiceTestRepo) CleanupExpiredLogs(context.Context, time.Time, time.Time) (*ContentModerationCleanupResult, error) {
	return &ContentModerationCleanupResult{}, nil
}
func (*moderationArchiveServiceTestRepo) CreateLogWithArchive(context.Context, *ContentModerationLog, *ContentModerationEncryptedArchive) error {
	return nil
}
func (*moderationArchiveServiceTestRepo) CreateContentLostLog(context.Context, *ContentModerationLog) error {
	return nil
}
func (r *moderationArchiveServiceTestRepo) GetArchive(_ context.Context, logID int64) (*ContentModerationLog, *ContentModerationEncryptedArchive, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.log == nil || r.log.ID != logID || r.deleted {
		return nil, nil, sql.ErrNoRows
	}
	return cloneContentModerationLog(r.log), cloneModerationEncryptedArchive(r.archive), nil
}
func (r *moderationArchiveServiceTestRepo) DeleteArchive(_ context.Context, access ContentModerationArchiveAccess) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deleted {
		return false, nil
	}
	r.deleted = true
	r.audits = append(r.audits, access)
	return true, nil
}
func (r *moderationArchiveServiceTestRepo) RecordArchiveAccess(_ context.Context, access ContentModerationArchiveAccess) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.auditErr != nil {
		return r.auditErr
	}
	r.audits = append(r.audits, access)
	return nil
}
func (*moderationArchiveServiceTestRepo) ReferencedArchiveKeyIDs(context.Context) ([]string, error) {
	return nil, nil
}

func (r *moderationArchiveServiceTestRepo) snapshotAudits() []ContentModerationArchiveAccess {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ContentModerationArchiveAccess, len(r.audits))
	copy(out, r.audits)
	return out
}

func newModerationArchiveServiceFixture(t *testing.T, plaintext []byte) (*ContentModerationService, *moderationArchiveServiceTestRepo, *contentModerationArchiveRuntime) {
	t.Helper()
	root := t.TempDir()
	keyRing := filepath.Join(root, "keyring.json")
	writeModerationArchiveTestKeyRing(t, keyRing, "k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	repo := &moderationArchiveServiceTestRepo{}
	options := moderationArchiveRuntimeOptions(root, keyRing)
	runtime, err := newContentModerationArchiveRuntime(repo, options)
	require.NoError(t, err)
	t.Cleanup(runtime.Close)
	archive, err := runtime.cipher.Encrypt("4fb15011-24a0-4fa8-a563-fbe205c27411", plaintext)
	require.NoError(t, err)
	repo.log = &ContentModerationLog{
		ID: 77, ArchiveID: archive.ArchiveID, ArchiveVersion: archive.Version,
		ArchiveKeyID: archive.KeyID, ArchiveSHA256: archive.PlaintextHash,
		ArchiveBytes: archive.PlaintextSize, ArchiveStatus: ContentModerationArchiveStatusAvailable,
	}
	repo.archive = archive
	svc := NewContentModerationService(nil, repo, nil, nil, nil, nil, nil, nil)
	svc.archiveRuntime = runtime
	return svc, repo, runtime
}

func TestContentModerationArchivePreviewCapsAtOneMiBAndAuditsReturnedBytes(t *testing.T) {
	body := make([]byte, ContentModerationArchivePreviewMaxBytes+321)
	for i := range body {
		body[i] = byte('a' + (i % 26))
	}
	plaintext, err := json.Marshal(ContentModerationArchiveEnvelope{
		Request: ContentModerationArchiveRequest{BodyBase64: base64.StdEncoding.EncodeToString(body)},
	})
	require.NoError(t, err)
	svc, repo, _ := newModerationArchiveServiceFixture(t, plaintext)

	preview, err := svc.PreviewArchive(context.Background(), 77, 9, "req-preview")
	require.NoError(t, err)
	require.True(t, preview.Truncated)
	require.Equal(t, int64(len(body)), preview.TotalBytes)
	require.Equal(t, int64(ContentModerationArchivePreviewMaxBytes), preview.ReturnedBytes)
	require.Equal(t, string(body[:ContentModerationArchivePreviewMaxBytes]), preview.Content)
	audits := repo.snapshotAudits()
	require.Len(t, audits, 1)
	require.Equal(t, "preview", audits[0].Action)
	require.Equal(t, int64(ContentModerationArchivePreviewMaxBytes), audits[0].BytesServed)
	require.Equal(t, int64(9), audits[0].ActorUserID)
}

func TestContentModerationArchivePreviewReturnsDecodedUTF8Body(t *testing.T) {
	body := []byte(`{"input":"系统管理员查看请求原文"}`)
	plaintext, err := json.Marshal(ContentModerationArchiveEnvelope{
		Request: ContentModerationArchiveRequest{BodyBase64: base64.StdEncoding.EncodeToString(body)},
	})
	require.NoError(t, err)
	svc, repo, _ := newModerationArchiveServiceFixture(t, plaintext)

	preview, err := svc.PreviewArchive(context.Background(), 77, 9, "req-preview-utf8")
	require.NoError(t, err)
	require.Equal(t, string(body), preview.Content)
	require.Equal(t, int64(len(body)), preview.ReturnedBytes)
	require.Equal(t, int64(len(body)), preview.TotalBytes)
	require.False(t, preview.Truncated)
	require.Equal(t, int64(len(body)), repo.snapshotAudits()[0].BytesServed)
}

func TestContentModerationArchivePreviewRejectsInvalidBodyEncodingAndAuditsFailure(t *testing.T) {
	plaintext, err := json.Marshal(ContentModerationArchiveEnvelope{
		Request: ContentModerationArchiveRequest{BodyBase64: "not-base64"},
	})
	require.NoError(t, err)
	svc, repo, _ := newModerationArchiveServiceFixture(t, plaintext)

	preview, err := svc.PreviewArchive(context.Background(), 77, 9, "req-preview-invalid")
	require.ErrorIs(t, err, ErrModerationArchiveIntegrity)
	require.Nil(t, preview)
	audits := repo.snapshotAudits()
	require.Len(t, audits, 1)
	require.Equal(t, "preview", audits[0].Action)
	require.Equal(t, "failed", audits[0].Result)
	require.Zero(t, audits[0].BytesServed)
}

func TestContentModerationArchiveDownloadRequiresSuccessfulAudit(t *testing.T) {
	svc, repo, _ := newModerationArchiveServiceFixture(t, []byte("full envelope"))
	repo.auditErr = errors.New("audit database unavailable")

	raw, err := svc.DownloadArchive(context.Background(), 77, 9, "req-download")
	require.Error(t, err)
	require.Nil(t, raw, "plaintext must not be returned when the sensitive-read audit cannot be committed")
}

func TestContentModerationArchiveDeleteRemovesAllLocalCopies(t *testing.T) {
	svc, repo, runtime := newModerationArchiveServiceFixture(t, []byte("full envelope"))
	archiveID := repo.log.ArchiveID
	paths := []string{
		filepath.Join(runtime.options.RetryDir, archiveID+contentModerationRetrySuffix),
		filepath.Join(runtime.options.RetryDir, archiveID+contentModerationDispositionSuffix),
		filepath.Join(runtime.options.EmergencyDir, archiveID+contentModerationEmergencySuffix),
	}
	for _, path := range paths {
		require.NoError(t, os.WriteFile(path, []byte(`{}`), 0o600))
	}

	deleted, err := svc.DeleteArchive(context.Background(), 77, 9, "req-delete")
	require.NoError(t, err)
	require.True(t, deleted)
	for _, path := range paths {
		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	audits := repo.snapshotAudits()
	require.Len(t, audits, 1)
	require.Equal(t, "delete", audits[0].Action)
}
