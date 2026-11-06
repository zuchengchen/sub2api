//go:build linux

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func syntheticSourceBuildReceipt(t *testing.T) sourceBuildReceipt {
	t.Helper()
	root := t.TempDir()
	source, artifacts := filepath.Join(root, "source"), filepath.Join(root, "package")
	require.NoError(t, os.MkdirAll(source, 0700))
	require.NoError(t, os.MkdirAll(artifacts, 0700))
	r := sourceBuildReceipt{Schema: 1, SourceRoot: source, SourceClean: true, Version: "codex-cli 0.0.0", ArtifactRoot: artifacts, SourceFiles: map[string]string{}, Artifacts: map[string]string{}}
	for _, path := range []string{"codex-rs/Cargo.toml", "codex-rs/Cargo.lock", "codex-rs/rust-toolchain.toml", "codex-rs/models-manager/models.json"} {
		full := filepath.Join(source, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0700))
		require.NoError(t, os.WriteFile(full, []byte("synthetic verifier fixture "+path), 0600))
		hash, err := sourceBuildFileHash(full)
		require.NoError(t, err)
		r.SourceFiles[path] = hash
	}
	// Scratch git objects calibrate provenance checks; this is not the project
	// checkout and these synthetic bytes are never acceptance evidence.
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", source, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Verifier Fixture", "-c", "user.email=fixture@example.invalid"}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", out)
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet")
	git("add", ".")
	git("commit", "--quiet", "--no-gpg-sign", "-m", "Synthetic receipt calibration")
	r.SourceCommit, r.SourceTree = git("rev-parse", "HEAD"), git("rev-parse", "HEAD^{tree}")
	for _, path := range []string{"bin/codex", "bin/codex-code-mode-host", "bin/codex-exec-server", "codex-resources/bwrap", "codex-path/rg", "codex-package.json"} {
		full := filepath.Join(artifacts, path)
		require.NoError(t, os.MkdirAll(filepath.Dir(full), 0700))
		require.NoError(t, os.WriteFile(full, []byte("synthetic artifact "+path), 0700))
		hash, err := sourceBuildFileHash(full)
		require.NoError(t, err)
		r.Artifacts[path] = hash
	}
	r.Toolchain.Rustc, r.Toolchain.Cargo = "rustc 1.95.0\nhost: fixture", "cargo 1.95.0\nhost: fixture"
	r.Toolchain.RustcSHA256, r.Toolchain.CargoSHA256 = strings.Repeat("1", 64), strings.Repeat("2", 64)
	log := filepath.Join(root, "build.log")
	require.NoError(t, os.WriteFile(log, []byte("synthetic successful build output\n"), 0600))
	logHash, err := sourceBuildFileHash(log)
	require.NoError(t, err)
	exit := 0
	now := time.Now().UTC()
	r.Commands = []sourceBuildCommand{{Argv: []string{"cargo", "build", "--locked", "--release", "-p", "codex-cli", "-p", "codex-code-mode-host", "-p", "codex-exec-server-cli", "-p", "codex-bwrap"}, Cwd: filepath.Join(source, "codex-rs"), StartedAt: now.Add(-time.Second).Format(time.RFC3339Nano), CompletedAt: now.Format(time.RFC3339Nano), ExitCode: &exit, Log: log, LogSHA256: logHash}}
	return r
}

func TestSourceBuildReceiptRequiresActualCompleteEvidence(t *testing.T) {
	for _, fault := range []string{"none", "taskset", "failed build", "missing exit", "not built", "unlocked", "wrong source", "dirty source", "changed companion", "missing companion", "changed catalog", "changed log", "truncated json", "wrong version", "wrong binary"} {
		t.Run(fault, func(t *testing.T) {
			r := syntheticSourceBuildReceipt(t)
			version, binary := r.Version, r.Artifacts["bin/codex"]
			switch fault {
			case "taskset":
				r.Commands[0].Argv = append([]string{"taskset", "-c", "0,1"}, r.Commands[0].Argv...)
			case "failed build":
				v := 1
				r.Commands[0].ExitCode = &v
			case "missing exit":
				r.Commands[0].ExitCode = nil
			case "not built":
				r.Commands = nil
			case "unlocked":
				r.Commands[0].Argv[2] = "--offline"
			case "wrong source":
				r.SourceCommit = strings.Repeat("f", 40)
			case "dirty source":
				require.NoError(t, os.WriteFile(filepath.Join(r.SourceRoot, "new-file"), []byte("dirty"), 0600))
			case "changed companion":
				require.NoError(t, os.WriteFile(filepath.Join(r.ArtifactRoot, "bin/codex-code-mode-host"), []byte("changed"), 0700))
			case "missing companion":
				require.NoError(t, os.Remove(filepath.Join(r.ArtifactRoot, "bin/codex-exec-server")))
			case "changed catalog":
				r.SourceFiles["codex-rs/models-manager/models.json"] = strings.Repeat("f", 64)
			case "changed log":
				require.NoError(t, os.WriteFile(r.Commands[0].Log, []byte("truncated"), 0600))
			case "wrong version":
				version = "codex-cli 0.164.0"
			case "wrong binary":
				binary = strings.Repeat("e", 64)
			}
			raw, err := json.Marshal(r)
			require.NoError(t, err)
			if fault == "truncated json" {
				raw = raw[:len(raw)/2]
			}
			err = validateSourceBuildReceipt(raw, version, r.SourceCommit, binary, r.SourceFiles["codex-rs/models-manager/models.json"])
			if fault == "none" || fault == "taskset" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestSourceBuildReceiptMetadataAndExitTwo(t *testing.T) {
	r := syntheticSourceBuildReceipt(t)
	base, candidate := t.TempDir(), t.TempDir()
	for _, dir := range []string{base, candidate} {
		comparatorFixture(t, dir)
		meta, err := readCaptureMetadata(dir)
		require.NoError(t, err)
		meta.Version, meta.Commit, meta.BinaryHash, meta.ModelsHash = r.Version, r.SourceCommit, r.Artifacts["bin/codex"], r.SourceFiles["codex-rs/models-manager/models.json"]
		data := mustJSON(r)
		require.NoError(t, os.WriteFile(filepath.Join(dir, sourceBuildReceiptFile), data, 0600))
		meta.SourceBuildReceiptHash, err = sourceBuildFileHash(filepath.Join(dir, sourceBuildReceiptFile))
		require.NoError(t, err)
		writeJSON(filepath.Join(dir, "meta.json"), meta)
	}
	require.Equal(t, 0, compareMain([]string{"-golden", base, "-candidate", candidate}))
	require.NoError(t, os.WriteFile(filepath.Join(candidate, sourceBuildReceiptFile), []byte(`{"schema":1}`), 0600))
	require.Equal(t, 2, compareMain([]string{"-golden", base, "-candidate", candidate}), "unreadable or unbound source provenance is invalid evidence")
	t.Setenv("CODEXGOLDEN_SOURCE_BUILD_RECEIPT", "")
	require.Error(t, bindSourceBuildReceipt(map[string]any{"codex_version": r.Version}, t.TempDir(), filepath.Join(r.ArtifactRoot, "bin/codex")))
}
