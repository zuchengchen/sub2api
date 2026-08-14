//go:build unit

package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type updateServiceCacheStub struct {
	data string
}

func (s *updateServiceCacheStub) GetUpdateInfo(context.Context) (string, error) {
	if s.data == "" {
		return "", errors.New("cache miss")
	}
	return s.data, nil
}

func (s *updateServiceCacheStub) SetUpdateInfo(_ context.Context, data string, _ time.Duration) error {
	s.data = data
	return nil
}

type updateServiceGitHubClientStub struct {
	release     *GitHubRelease
	recentCalls int
}

func (s *updateServiceGitHubClientStub) FetchLatestRelease(context.Context, string) (*GitHubRelease, error) {
	return s.release, nil
}

func (s *updateServiceGitHubClientStub) FetchRecentReleases(context.Context, string, int) ([]*GitHubRelease, error) {
	s.recentCalls++
	return nil, errors.New("rollback history must not use GitHub")
}

func (s *updateServiceGitHubClientStub) DownloadFile(context.Context, string, string, int64) error {
	panic("DownloadFile should not be called when no update is available")
}

func (s *updateServiceGitHubClientStub) FetchChecksumFile(context.Context, string) ([]byte, error) {
	panic("FetchChecksumFile should not be called when no update is available")
}

func TestUpdateServicePerformUpdateNoUpdateReturnsSentinel(t *testing.T) {
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		&updateServiceGitHubClientStub{
			release: &GitHubRelease{
				TagName: "v0.1.132",
				Name:    "v0.1.132",
			},
		},
		"0.1.132",
		"test-commit",
		"release",
	)

	err := svc.PerformUpdate(context.Background())

	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoUpdateAvailable))
	require.ErrorIs(t, err, ErrNoUpdateAvailable)
}

func TestUpdateServiceListRollbackVersionsUsesVerifiedLocalHistory(t *testing.T) {
	svc, exePath, github := newLocalRollbackTestService(t, "current-binary")
	baseTime := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

	writeRollbackTestEntry(t, exePath, "entry-current", "0.1.176.1", "current-commit", "current-binary", baseTime.Add(4*time.Hour))
	writeRollbackTestEntry(t, exePath, "entry-newest", "0.1.176", "commit-newest", "newest-binary", baseTime.Add(3*time.Hour))
	writeRollbackTestEntry(t, exePath, "entry-second", "0.1.175.2", "commit-second", "second-binary", baseTime.Add(2*time.Hour))
	writeRollbackTestEntry(t, exePath, "entry-third", "0.1.174", "commit-third", "third-binary", baseTime.Add(time.Hour))
	corrupt := writeRollbackTestEntry(t, exePath, "entry-corrupt", "0.1.999", "commit-corrupt", "corrupt-binary", baseTime.Add(5*time.Hour))
	require.NoError(t, os.WriteFile(corrupt.binaryPath, []byte("tampered"), 0755))

	versions, err := svc.ListRollbackVersions(context.Background())

	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.Equal(t, "entry-newest", versions[0].ID)
	require.Equal(t, "0.1.176", versions[0].Version)
	require.Equal(t, "commit-newest", versions[0].Commit)
	require.Equal(t, "entry-second", versions[1].ID)
	require.Equal(t, 0, github.recentCalls)
}

func TestUpdateServiceRollbackRejectsUnknownCorruptAndCurrentEntries(t *testing.T) {
	svc, exePath, _ := newLocalRollbackTestService(t, "current-binary")
	baseTime := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	writeRollbackTestEntry(t, exePath, "entry-current", "0.1.176.1", "current-commit", "current-binary", baseTime)
	corrupt := writeRollbackTestEntry(t, exePath, "entry-corrupt", "0.1.175", "old-commit", "old-binary", baseTime.Add(-time.Hour))
	require.NoError(t, os.WriteFile(corrupt.binaryPath, []byte("tampered"), 0755))

	for _, target := range []string{"", "missing", "entry-current", "entry-corrupt", "9.9.9"} {
		err := svc.RollbackToVersion(context.Background(), target)
		require.ErrorIs(t, err, ErrRollbackVersionNotAllowed, "target %q should be rejected", target)
	}
}

func TestUpdateServiceRollbackRotatesVerifiedLocalHistory(t *testing.T) {
	svc, exePath, github := newLocalRollbackTestService(t, "current-binary")
	baseTime := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return baseTime }
	selected := writeRollbackTestEntry(t, exePath, "entry-selected", "0.1.176", "selected-commit", "selected-binary", baseTime.Add(-2*time.Hour))
	writeRollbackTestEntry(t, exePath, "entry-older", "0.1.175", "older-commit", "older-binary", baseTime.Add(-3*time.Hour))

	err := svc.RollbackToVersion(context.Background(), selected.ID)

	require.NoError(t, err)
	installedBinary, err := os.ReadFile(exePath)
	require.NoError(t, err)
	require.Equal(t, "selected-binary", string(installedBinary))

	currentMetadata, err := readCurrentInstallationMetadata(exePath)
	require.NoError(t, err)
	require.Equal(t, "0.1.176", currentMetadata.Version)
	require.Equal(t, "selected-commit", currentMetadata.Commit)
	require.Equal(t, baseTime.Format(time.RFC3339Nano), currentMetadata.InstalledAt)
	require.Equal(t, testSHA256("selected-binary"), currentMetadata.SHA256)

	versions, err := svc.ListRollbackVersions(context.Background())
	require.NoError(t, err)
	require.Len(t, versions, 2)
	require.Equal(t, "0.1.176.1", versions[0].Version)
	require.Equal(t, "current-commit", versions[0].Commit)
	require.Equal(t, "0.1.175", versions[1].Version)
	require.Equal(t, 0, github.recentCalls)

	_, err = os.Stat(selected.directoryPath)
	require.True(t, os.IsNotExist(err))
}

func newLocalRollbackTestService(t *testing.T, currentBinary string) (*UpdateService, string, *updateServiceGitHubClientStub) {
	t.Helper()
	directory := t.TempDir()
	exePath := filepath.Join(directory, "sub2api")
	require.NoError(t, os.WriteFile(exePath, []byte(currentBinary), 0755))
	installedAt := time.Date(2026, 8, 14, 10, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(exePath, installedAt, installedAt))

	github := &updateServiceGitHubClientStub{}
	svc := NewUpdateService(
		&updateServiceCacheStub{},
		github,
		"0.1.176.1",
		"current-commit",
		"release",
	)
	svc.executablePath = func() (string, error) { return exePath, nil }
	svc.now = func() time.Time { return time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC) }
	require.NoError(t, writeCurrentInstallationMetadata(exePath, localInstallationMetadata{
		Version:     "0.1.176.1",
		Commit:      "current-commit",
		InstalledAt: installedAt.Format(time.RFC3339Nano),
		SHA256:      testSHA256(currentBinary),
	}))
	return svc, exePath, github
}

func writeRollbackTestEntry(t *testing.T, exePath, id, version, commit, binary string, installedAt time.Time) localHistoryEntry {
	t.Helper()
	directoryPath := filepath.Join(versionHistoryEntriesRoot(exePath), id)
	require.NoError(t, os.MkdirAll(directoryPath, 0750))
	binaryPath := filepath.Join(directoryPath, versionHistoryBinaryName)
	require.NoError(t, os.WriteFile(binaryPath, []byte(binary), 0755))
	archivedAt := installedAt.Add(30 * time.Minute)
	metadata := RollbackVersion{
		ID:          id,
		Version:     version,
		Commit:      commit,
		InstalledAt: installedAt.UTC().Format(time.RFC3339Nano),
		ArchivedAt:  archivedAt.UTC().Format(time.RFC3339Nano),
		SHA256:      testSHA256(binary),
	}
	require.NoError(t, writeJSONFile(filepath.Join(directoryPath, versionHistoryMetadata), metadata, 0600))
	return localHistoryEntry{
		RollbackVersion: metadata,
		directoryPath:   directoryPath,
		binaryPath:      binaryPath,
		installedAt:     installedAt,
		archivedAt:      archivedAt,
	}
}

func testSHA256(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
