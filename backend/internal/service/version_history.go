package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	maxRollbackVersions      = 2
	versionHistoryDirName    = ".version-history"
	versionHistoryEntriesDir = "entries"
	versionHistoryBinaryName = "sub2api"
	versionHistoryMetadata   = "metadata.json"
	currentVersionMetadata   = "current.json"
	maxVersionMetadataSize   = 64 * 1024
)

type localInstallationMetadata struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	InstalledAt string `json:"installed_at"`
	SHA256      string `json:"sha256"`
}

type localHistoryEntry struct {
	RollbackVersion
	directoryPath string
	binaryPath    string
	installedAt   time.Time
	archivedAt    time.Time
}

func resolvedExecutablePath() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("failed to get executable path: %w", err)
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve executable symlinks: %w", err)
	}
	return exePath, nil
}

func (s *UpdateService) nowUTC() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func versionHistoryRoot(exePath string) string {
	return filepath.Join(filepath.Dir(exePath), versionHistoryDirName)
}

func versionHistoryEntriesRoot(exePath string) string {
	return filepath.Join(versionHistoryRoot(exePath), versionHistoryEntriesDir)
}

// ListRollbackVersions returns only checksum-verified binaries that were archived
// from this installation. GitHub release availability is intentionally irrelevant.
func (s *UpdateService) ListRollbackVersions(ctx context.Context) ([]RollbackVersion, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	exePath, err := s.executablePath()
	if err != nil {
		return nil, err
	}
	currentHash, err := hashRegularFile(exePath)
	if err != nil {
		return nil, fmt.Errorf("failed to verify current executable: %w", err)
	}
	entries, err := listVerifiedLocalHistory(exePath)
	if err != nil {
		return nil, err
	}

	versions := make([]RollbackVersion, 0, maxRollbackVersions)
	for _, entry := range entries {
		if entry.SHA256 == currentHash {
			continue
		}
		versions = append(versions, entry.RollbackVersion)
		if len(versions) == maxRollbackVersions {
			break
		}
	}
	return versions, nil
}

// Rollback restores the newest verified local rollback binary.
func (s *UpdateService) Rollback() error {
	versions, err := s.ListRollbackVersions(context.Background())
	if err != nil {
		return err
	}
	if len(versions) == 0 {
		return fmt.Errorf("no verified local rollback version found")
	}
	return s.RollbackToVersion(context.Background(), versions[0].ID)
}

// RollbackToVersion restores a verified local history entry. The preferred target
// is its opaque ID; a version string remains accepted for compatibility and picks
// the newest matching local entry.
func (s *UpdateService) RollbackToVersion(ctx context.Context, target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return ErrRollbackVersionNotAllowed
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	exePath, err := s.executablePath()
	if err != nil {
		return err
	}
	currentHash, err := hashRegularFile(exePath)
	if err != nil {
		return fmt.Errorf("failed to verify current executable: %w", err)
	}
	entries, err := listVerifiedLocalHistory(exePath)
	if err != nil {
		return err
	}

	var selected *localHistoryEntry
	for i := range entries {
		if entries[i].ID == target && entries[i].SHA256 != currentHash {
			selected = &entries[i]
			break
		}
	}
	if selected == nil {
		legacyVersion := strings.TrimPrefix(target, "v")
		for i := range entries {
			if entries[i].Version == legacyVersion && entries[i].SHA256 != currentHash {
				selected = &entries[i]
				break
			}
		}
	}
	if selected == nil {
		return ErrRollbackVersionNotAllowed
	}

	tempDir, err := os.MkdirTemp(filepath.Dir(exePath), ".sub2api-rollback-*")
	if err != nil {
		return fmt.Errorf("failed to create rollback staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	stagedPath := filepath.Join(tempDir, versionHistoryBinaryName)
	stagedHash, err := copyRegularFile(ctx, selected.binaryPath, stagedPath, 0755)
	if err != nil {
		return fmt.Errorf("failed to stage rollback binary: %w", err)
	}
	if stagedHash != selected.SHA256 {
		return fmt.Errorf("rollback binary checksum changed during staging")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	archivedID, err := s.archiveCurrentExecutable(ctx, exePath, false)
	if err != nil {
		return fmt.Errorf("failed to archive current version: %w", err)
	}
	if err := ctx.Err(); err != nil {
		_ = s.removeHistoryEntry(exePath, archivedID)
		return err
	}

	metadata := localInstallationMetadata{
		Version:     selected.Version,
		Commit:      selected.Commit,
		InstalledAt: s.nowUTC().Format(time.RFC3339Nano),
		SHA256:      selected.SHA256,
	}
	if err := s.replaceExecutable(exePath, stagedPath, metadata); err != nil {
		_ = s.removeHistoryEntry(exePath, archivedID)
		return err
	}

	if err := s.removeHistoryEntry(exePath, selected.ID); err != nil {
		slog.Warn("local rollback history entry cleanup failed", "id", selected.ID, "error", err)
	}
	if err := s.pruneLocalHistory(exePath); err != nil {
		slog.Warn("local rollback history pruning failed", "error", err)
	}
	return nil
}

func (s *UpdateService) archiveCurrentExecutable(ctx context.Context, exePath string, prune bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	currentHash, err := hashRegularFile(exePath)
	if err != nil {
		return "", err
	}
	fileInfo, err := os.Lstat(exePath)
	if err != nil {
		return "", err
	}

	metadata := s.currentInstallationMetadata(exePath, currentHash, fileInfo.ModTime())
	entriesRoot := versionHistoryEntriesRoot(exePath)
	if err := os.MkdirAll(entriesRoot, 0750); err != nil {
		return "", fmt.Errorf("failed to create local version history: %w", err)
	}

	entries, err := listVerifiedLocalHistory(exePath)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.SHA256 == currentHash {
			if err := s.removeHistoryEntry(exePath, entry.ID); err != nil {
				return "", err
			}
		}
	}

	archivedAt := s.nowUTC()
	id, err := uniqueHistoryID(entriesRoot, archivedAt, currentHash)
	if err != nil {
		return "", err
	}
	pendingDir, err := os.MkdirTemp(entriesRoot, ".pending-*")
	if err != nil {
		return "", err
	}
	keepPending := false
	defer func() {
		if !keepPending {
			_ = os.RemoveAll(pendingDir)
		}
	}()

	binaryPath := filepath.Join(pendingDir, versionHistoryBinaryName)
	copiedHash, err := copyRegularFile(ctx, exePath, binaryPath, 0755)
	if err != nil {
		return "", err
	}
	if copiedHash != currentHash {
		return "", fmt.Errorf("current executable changed while it was being archived")
	}

	historyMetadata := RollbackVersion{
		ID:          id,
		Version:     metadata.Version,
		Commit:      metadata.Commit,
		InstalledAt: metadata.InstalledAt,
		ArchivedAt:  archivedAt.Format(time.RFC3339Nano),
		SHA256:      currentHash,
	}
	if err := writeJSONFile(filepath.Join(pendingDir, versionHistoryMetadata), historyMetadata, 0600); err != nil {
		return "", err
	}
	finalDir := filepath.Join(entriesRoot, id)
	if err := os.Rename(pendingDir, finalDir); err != nil {
		return "", fmt.Errorf("failed to publish local version history entry: %w", err)
	}
	keepPending = true

	if prune {
		if err := s.pruneLocalHistory(exePath); err != nil {
			return id, err
		}
	}
	return id, nil
}

func (s *UpdateService) currentInstallationMetadata(exePath, currentHash string, fallbackTime time.Time) localInstallationMetadata {
	metadata, err := readCurrentInstallationMetadata(exePath)
	if err == nil && metadata.SHA256 == currentHash {
		if strings.TrimSpace(metadata.Version) == "" {
			metadata.Version = s.currentVersion
		}
		if (metadata.Commit == "" || metadata.Commit == "unknown") && metadata.Version == s.currentVersion && strings.TrimSpace(s.currentCommit) != "" {
			metadata.Commit = s.currentCommit
		}
		return metadata
	}

	commit := strings.TrimSpace(s.currentCommit)
	if commit == "" {
		commit = "unknown"
	}
	return localInstallationMetadata{
		Version:     strings.TrimPrefix(strings.TrimSpace(s.currentVersion), "v"),
		Commit:      commit,
		InstalledAt: fallbackTime.UTC().Format(time.RFC3339Nano),
		SHA256:      currentHash,
	}
}

func readCurrentInstallationMetadata(exePath string) (localInstallationMetadata, error) {
	var metadata localInstallationMetadata
	err := readJSONFile(filepath.Join(versionHistoryRoot(exePath), currentVersionMetadata), &metadata)
	if err != nil {
		return metadata, err
	}
	if strings.TrimSpace(metadata.Version) == "" || !validSHA256(metadata.SHA256) {
		return metadata, fmt.Errorf("invalid current installation metadata")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.InstalledAt); err != nil {
		return metadata, fmt.Errorf("invalid current installation time: %w", err)
	}
	metadata.SHA256 = strings.ToLower(metadata.SHA256)
	return metadata, nil
}

func (s *UpdateService) replaceExecutable(exePath, stagedPath string, metadata localInstallationMetadata) error {
	stagedHash, err := hashRegularFile(stagedPath)
	if err != nil {
		return fmt.Errorf("failed to verify staged executable: %w", err)
	}
	if stagedHash != metadata.SHA256 {
		return fmt.Errorf("staged executable checksum mismatch")
	}
	if err := os.Chmod(stagedPath, 0755); err != nil {
		return fmt.Errorf("failed to set executable permissions: %w", err)
	}

	backupPath := exePath + ".replacement-backup"
	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove stale replacement backup: %w", err)
	}
	if err := os.Rename(exePath, backupPath); err != nil {
		return fmt.Errorf("failed to move current executable aside: %w", err)
	}
	if err := os.Rename(stagedPath, exePath); err != nil {
		if restoreErr := os.Rename(backupPath, exePath); restoreErr != nil {
			return fmt.Errorf("replacement failed: %w; restoring current executable also failed: %v", err, restoreErr)
		}
		return fmt.Errorf("replacement failed and current executable was restored: %w", err)
	}

	if err := writeCurrentInstallationMetadata(exePath, metadata); err != nil {
		removeErr := os.Remove(exePath)
		restoreErr := os.Rename(backupPath, exePath)
		if removeErr != nil || restoreErr != nil {
			return fmt.Errorf("failed to record replacement metadata: %w; restore errors: remove=%v rename=%v", err, removeErr, restoreErr)
		}
		return fmt.Errorf("failed to record replacement metadata; current executable was restored: %w", err)
	}

	if err := os.Remove(backupPath); err != nil && !os.IsNotExist(err) {
		slog.Warn("replacement backup cleanup failed", "path", backupPath, "error", err)
	}
	// Remove the single-file backup used by older updater versions. The verified
	// history entry created above is now the rollback source of truth.
	if err := os.Remove(exePath + ".backup"); err != nil && !os.IsNotExist(err) {
		slog.Warn("legacy updater backup cleanup failed", "path", exePath+".backup", "error", err)
	}
	return nil
}

func writeCurrentInstallationMetadata(exePath string, metadata localInstallationMetadata) error {
	root := versionHistoryRoot(exePath)
	if err := os.MkdirAll(root, 0750); err != nil {
		return err
	}
	if strings.TrimSpace(metadata.Version) == "" || !validSHA256(metadata.SHA256) {
		return fmt.Errorf("invalid current installation metadata")
	}
	if _, err := time.Parse(time.RFC3339Nano, metadata.InstalledAt); err != nil {
		return fmt.Errorf("invalid current installation time: %w", err)
	}

	tempFile, err := os.CreateTemp(root, ".current-*.json")
	if err != nil {
		return err
	}
	tempPath := tempFile.Name()
	defer func() { _ = os.Remove(tempPath) }()
	if err := tempFile.Chmod(0600); err != nil {
		_ = tempFile.Close()
		return err
	}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		_ = tempFile.Close()
		return err
	}
	data = append(data, '\n')
	if _, err := tempFile.Write(data); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, filepath.Join(root, currentVersionMetadata))
}

func listVerifiedLocalHistory(exePath string) ([]localHistoryEntry, error) {
	entriesRoot := versionHistoryEntriesRoot(exePath)
	dirEntries, err := os.ReadDir(entriesRoot)
	if os.IsNotExist(err) {
		return []localHistoryEntry{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to read local version history: %w", err)
	}

	entries := make([]localHistoryEntry, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		id := dirEntry.Name()
		if !dirEntry.IsDir() || !safeHistoryID(id) {
			continue
		}
		entry, err := readLocalHistoryEntry(filepath.Join(entriesRoot, id), id)
		if err != nil {
			continue
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].installedAt.Equal(entries[j].installedAt) {
			return entries[i].installedAt.After(entries[j].installedAt)
		}
		if !entries[i].archivedAt.Equal(entries[j].archivedAt) {
			return entries[i].archivedAt.After(entries[j].archivedAt)
		}
		return entries[i].ID > entries[j].ID
	})
	return entries, nil
}

func readLocalHistoryEntry(directoryPath, expectedID string) (localHistoryEntry, error) {
	var metadata RollbackVersion
	if err := readJSONFile(filepath.Join(directoryPath, versionHistoryMetadata), &metadata); err != nil {
		return localHistoryEntry{}, err
	}
	if metadata.ID != expectedID || !safeHistoryID(metadata.ID) || strings.TrimSpace(metadata.Version) == "" || !validSHA256(metadata.SHA256) {
		return localHistoryEntry{}, fmt.Errorf("invalid local version history metadata")
	}
	installedAt, err := time.Parse(time.RFC3339Nano, metadata.InstalledAt)
	if err != nil {
		return localHistoryEntry{}, err
	}
	archivedAt, err := time.Parse(time.RFC3339Nano, metadata.ArchivedAt)
	if err != nil {
		return localHistoryEntry{}, err
	}
	binaryPath := filepath.Join(directoryPath, versionHistoryBinaryName)
	actualHash, err := hashRegularFile(binaryPath)
	if err != nil {
		return localHistoryEntry{}, err
	}
	metadata.SHA256 = strings.ToLower(metadata.SHA256)
	if actualHash != metadata.SHA256 {
		return localHistoryEntry{}, fmt.Errorf("local version history checksum mismatch")
	}
	return localHistoryEntry{
		RollbackVersion: metadata,
		directoryPath:   directoryPath,
		binaryPath:      binaryPath,
		installedAt:     installedAt,
		archivedAt:      archivedAt,
	}, nil
}

func (s *UpdateService) pruneLocalHistory(exePath string) error {
	entries, err := listVerifiedLocalHistory(exePath)
	if err != nil {
		return err
	}
	currentHash, err := hashRegularFile(exePath)
	if err != nil {
		return err
	}
	kept := 0
	for _, entry := range entries {
		if entry.SHA256 == currentHash || kept >= maxRollbackVersions {
			if err := s.removeHistoryEntry(exePath, entry.ID); err != nil {
				return err
			}
			continue
		}
		kept++
	}
	return nil
}

func (s *UpdateService) removeHistoryEntry(exePath, id string) error {
	if !safeHistoryID(id) {
		return fmt.Errorf("invalid local version history ID")
	}
	return os.RemoveAll(filepath.Join(versionHistoryEntriesRoot(exePath), id))
}

func uniqueHistoryID(entriesRoot string, archivedAt time.Time, sha string) (string, error) {
	base := archivedAt.UTC().Format("20060102T150405.000000000Z") + "-" + sha[:12]
	for sequence := 1; ; sequence++ {
		candidate := base
		if sequence > 1 {
			candidate = fmt.Sprintf("%s-%d", base, sequence)
		}
		if _, err := os.Lstat(filepath.Join(entriesRoot, candidate)); os.IsNotExist(err) {
			return candidate, nil
		} else if err != nil {
			return "", fmt.Errorf("failed to inspect local version history ID: %w", err)
		}
	}
}

func safeHistoryID(id string) bool {
	if id == "" || len(id) > 128 || id == "." || id == ".." {
		return false
	}
	for _, char := range id {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validSHA256(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func hashRegularFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("not a regular file: %s", path)
	}
	if info.Size() <= 0 || info.Size() > maxDownloadSize {
		return "", fmt.Errorf("invalid executable size: %d", info.Size())
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func copyRegularFile(ctx context.Context, sourcePath, destinationPath string, mode os.FileMode) (hashValue string, err error) {
	info, err := os.Lstat(sourcePath)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("not a regular file: %s", sourcePath)
	}
	if info.Size() <= 0 || info.Size() > maxDownloadSize {
		return "", fmt.Errorf("invalid executable size: %d", info.Size())
	}

	source, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer func() { _ = source.Close() }()
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return "", err
	}
	defer func() {
		if closeErr := destination.Close(); err == nil && closeErr != nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(destinationPath)
		}
	}()

	hash := sha256.New()
	buffer := make([]byte, 1024*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		readCount, readErr := source.Read(buffer)
		if readCount > 0 {
			chunk := buffer[:readCount]
			if _, err := destination.Write(chunk); err != nil {
				return "", err
			}
			if _, err := hash.Write(chunk); err != nil {
				return "", err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if err := destination.Sync(); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readJSONFile(path string, destination any) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maxVersionMetadataSize+1))
	if err != nil {
		return err
	}
	if len(data) > maxVersionMetadataSize {
		return fmt.Errorf("version metadata is too large")
	}
	return json.Unmarshal(data, destination)
}

func writeJSONFile(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
