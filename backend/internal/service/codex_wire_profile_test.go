package service

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestVerifiedCodexWireProfilesRequireCapturedExactRelease(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.164.0"} {
		profile, verified := verifiedCodexWireProfile(version)
		require.True(t, verified)
		resolved, known := resolveCodexWireProfile(version, "auto")
		require.True(t, known)
		require.Equal(t, resolved, profile)
		data, err := os.ReadFile(filepath.Join("../pkg/codexwire/testdata/golden", version, "meta.json"))
		require.NoError(t, err)
		var meta struct {
			Version   string   `json:"codex_version"`
			Scenarios []string `json:"scenarios"`
			Leaks     []string `json:"outbound_leaks"`
			Strace    bool     `json:"strace_guard"`
		}
		require.NoError(t, json.Unmarshal(data, &meta))
		require.Equal(t, "codex-cli "+version, meta.Version)
		require.GreaterOrEqual(t, len(meta.Scenarios), 3)
		require.True(t, meta.Strace)
		require.Empty(t, meta.Leaks)
	}
	for _, version := range []string{"", "0.163", "0.163.1", "0.164.1", "0.164.0-alpha.1", "0.165.0", "0.999.0", "invalid"} {
		_, verified := verifiedCodexWireProfile(version)
		require.False(t, verified, version)
	}
}

func TestOpenAICodexVersionSyncRejectsUnverifiedAndRepairsOldSync(t *testing.T) {
	t.Run("unverified latest and list never update UA", func(t *testing.T) {
		repo := newCodexVersionSyncSettingRepoStub(map[string]string{SettingKeyOpenAICodexClientVersionSynced: "0.163.0"})
		github := &codexVersionSyncGitHubStub{
			latest:   &GitHubRelease{TagName: "rust-v0.999.0"},
			releases: []*GitHubRelease{{TagName: "rust-v0.164.1"}, {TagName: "rust-v0.999.0"}},
		}
		newCodexVersionSyncService(repo, github).runOnce()
		require.Empty(t, repo.syncedWrites())
	})
	t.Run("recent unsafe persisted sync is repaired", func(t *testing.T) {
		repo := newCodexVersionSyncSettingRepoStub(map[string]string{SettingKeyOpenAICodexClientVersionSynced: "0.999.0"})
		repo.updatedAt = time.Now()
		github := &codexVersionSyncGitHubStub{latest: &GitHubRelease{TagName: "rust-v0.164.0"}}
		newCodexVersionSyncService(repo, github).runInitial()
		require.Equal(t, []string{"0.164.0"}, repo.syncedWrites())
	})
	t.Run("unsafe stored sync never reaches canonical UA before sync runs", func(t *testing.T) {
		svc := NewSettingService(&codexVersionSettingRepoStub{values: map[string]string{SettingKeyOpenAICodexClientVersionSynced: "0.999.0"}}, nil)
		require.Equal(t, codexCLIVersion, svc.GetOpenAICodexClientVersion(context.Background()))
		require.Equal(t, buildCodexCLIUserAgent(codexCLIVersion), svc.GetOpenAICodexCanonicalUserAgent(context.Background()))
	})
}
