//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const sourceBuildReceiptFile = "source-build-receipt.json"

type sourceBuildCommand struct {
	RunnerSHA256 string            `json:"runner_sha256,omitempty"`
	Argv         []string          `json:"argv"`
	Env          map[string]string `json:"env,omitempty"`
	Cwd          string            `json:"cwd"`
	StartedAt    string            `json:"started_at"`
	CompletedAt  string            `json:"completed_at"`
	ExitCode     *int              `json:"exit_code"`
	Log          string            `json:"log"`
	LogSHA256    string            `json:"log_sha256"`
}

type sourceBuildReceipt struct {
	FailedCommands []sourceBuildCommand `json:"failed_commands,omitempty"`
	Schema         int                  `json:"schema"`
	SourceCommit   string               `json:"source_commit"`
	SourceTree     string               `json:"source_tree"`
	SourceRoot     string               `json:"source_root"`
	SourceClean    bool                 `json:"source_clean"`
	Version        string               `json:"version"`
	Toolchain      struct {
		Rustc       string `json:"rustc"`
		Cargo       string `json:"cargo"`
		RustcSHA256 string `json:"rustc_sha256"`
		CargoSHA256 string `json:"cargo_sha256"`
	} `json:"toolchain"`
	SourceFiles  map[string]string    `json:"source_files"`
	ArtifactRoot string               `json:"artifact_root"`
	Artifacts    map[string]string    `json:"artifacts"`
	Commands     []sourceBuildCommand `json:"commands"`
}

func sourceBuildFileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", fmt.Errorf("missing or non-regular evidence file %s", path)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func sourceBuildGit(root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("source checkout git %s: %w", args[0], err)
	}
	return strings.TrimSpace(string(out)), nil
}

// This is provenance validation, not permission to relabel a source build as an
// official stable release. Every claimed input and output remains inspectable.
func validateSourceBuildReceipt(raw []byte, version, commit, binaryHash, modelsHash string) error {
	var r sourceBuildReceipt
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("decode source build receipt: %w", err)
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return fmt.Errorf("trailing source build receipt data")
	}
	hex40, hex64 := regexp.MustCompile(`^[0-9a-f]{40}$`), regexp.MustCompile(`^[0-9a-f]{64}$`)
	if r.Schema != 1 || r.Version != version || r.SourceCommit != commit || !r.SourceClean || !hex40.MatchString(r.SourceCommit) || !hex40.MatchString(r.SourceTree) || !filepath.IsAbs(r.SourceRoot) || !filepath.IsAbs(r.ArtifactRoot) {
		return fmt.Errorf("invalid source build identity")
	}
	if !strings.HasPrefix(r.Toolchain.Rustc, "rustc ") || !strings.Contains(r.Toolchain.Rustc, "host:") || !strings.HasPrefix(r.Toolchain.Cargo, "cargo ") || !strings.Contains(r.Toolchain.Cargo, "host:") || !hex64.MatchString(r.Toolchain.RustcSHA256) || !hex64.MatchString(r.Toolchain.CargoSHA256) {
		return fmt.Errorf("incomplete source build toolchain evidence")
	}
	for _, check := range []struct {
		args []string
		want string
	}{
		{[]string{"rev-parse", "HEAD"}, r.SourceCommit},
		{[]string{"rev-parse", "HEAD^{tree}"}, r.SourceTree},
		{[]string{"status", "--porcelain", "--untracked-files=normal"}, ""},
	} {
		got, err := sourceBuildGit(r.SourceRoot, check.args...)
		if err != nil || got != check.want {
			return fmt.Errorf("source build checkout changed: %s", strings.Join(check.args, " "))
		}
	}
	for _, name := range []string{"codex-rs/Cargo.lock", "codex-rs/Cargo.toml", "codex-rs/rust-toolchain.toml", "codex-rs/models-manager/models.json"} {
		if !hex64.MatchString(r.SourceFiles[name]) {
			return fmt.Errorf("missing source input %s", name)
		}
	}
	for _, name := range []string{"bin/codex", "bin/codex-code-mode-host", "bin/codex-exec-server", "codex-resources/bwrap", "codex-path/rg", "codex-package.json"} {
		if !hex64.MatchString(r.Artifacts[name]) {
			return fmt.Errorf("missing source artifact %s", name)
		}
		info, err := os.Stat(filepath.Join(r.ArtifactRoot, name))
		if err != nil || (name != "codex-package.json" && info.Mode().Perm()&0111 == 0) {
			return fmt.Errorf("artifact is not executable: %s", name)
		}
	}
	for _, group := range []struct {
		root   string
		hashes map[string]string
	}{{r.SourceRoot, r.SourceFiles}, {r.ArtifactRoot, r.Artifacts}} {
		for name, hash := range group.hashes {
			if !filepath.IsLocal(name) || !hex64.MatchString(hash) {
				return fmt.Errorf("invalid source evidence path or hash")
			}
			actual, err := sourceBuildFileHash(filepath.Join(group.root, name))
			if err != nil || actual != hash {
				return fmt.Errorf("source build evidence changed: %s", name)
			}
		}
	}
	if r.Artifacts["bin/codex"] != binaryHash || r.SourceFiles["codex-rs/models-manager/models.json"] != modelsHash {
		return fmt.Errorf("source build binary or catalog differs from recording")
	}
	for _, cmd := range r.FailedCommands {
		start, first := time.Parse(time.RFC3339Nano, cmd.StartedAt)
		end, last := time.Parse(time.RFC3339Nano, cmd.CompletedAt)
		if cmd.ExitCode == nil || *cmd.ExitCode == 0 || first != nil || last != nil || end.Before(start) || len(cmd.Argv) == 0 || !filepath.IsAbs(cmd.Cwd) || !filepath.IsAbs(cmd.Log) || !hex64.MatchString(cmd.LogSHA256) {
			return fmt.Errorf("invalid failed build command receipt")
		}
		actual, err := sourceBuildFileHash(cmd.Log)
		if err != nil || actual != cmd.LogSHA256 || (cmd.RunnerSHA256 != "" && !hex64.MatchString(cmd.RunnerSHA256)) {
			return fmt.Errorf("failed build log missing or changed")
		}
	}
	built := map[string]bool{}
	for _, cmd := range r.Commands {
		start, errStart := time.Parse(time.RFC3339Nano, cmd.StartedAt)
		end, errEnd := time.Parse(time.RFC3339Nano, cmd.CompletedAt)
		if cmd.ExitCode == nil || *cmd.ExitCode != 0 || errStart != nil || errEnd != nil || end.Before(start) || !filepath.IsAbs(cmd.Log) || !hex64.MatchString(cmd.LogSHA256) {
			return fmt.Errorf("missing, failed or invalid build command receipt")
		}
		actual, err := sourceBuildFileHash(cmd.Log)
		if err != nil || actual != cmd.LogSHA256 || (cmd.RunnerSHA256 != "" && !hex64.MatchString(cmd.RunnerSHA256)) {
			return fmt.Errorf("build command log unavailable or changed")
		}
		if path := cmd.Env["RUSTC"]; path != "" {
			hash, err := sourceBuildFileHash(path)
			if err != nil || hash != r.Toolchain.RustcSHA256 {
				return fmt.Errorf("build compiler differs from recorded toolchain")
			}
		}
		if path, hash := cmd.Env["RUSTY_V8_ARCHIVE"], cmd.Env["RUSTY_V8_ARCHIVE_SHA256"]; path != "" || hash != "" {
			actual, err := sourceBuildFileHash(path)
			if err != nil || !hex64.MatchString(hash) || actual != hash {
				return fmt.Errorf("build V8 archive evidence unavailable or changed")
			}
		}
		argv := cmd.Argv
		if len(argv) >= 4 && filepath.Base(argv[0]) == "taskset" && argv[1] == "-c" && regexp.MustCompile(`^[0-9,-]+$`).MatchString(argv[2]) {
			argv = argv[3:]
		}
		if len(argv) < 2 || filepath.Base(argv[0]) != "cargo" || argv[1] != "build" || filepath.Clean(cmd.Cwd) != filepath.Join(r.SourceRoot, "codex-rs") {
			continue
		}
		flags := map[string]bool{}
		for _, arg := range argv {
			flags[arg] = true
		}
		if !flags["--locked"] || !flags["--release"] {
			return fmt.Errorf("build must use locked release inputs")
		}
		for i, arg := range argv {
			if (arg == "-p" || arg == "--package") && i+1 < len(argv) {
				built[argv[i+1]] = true
			}
			if strings.HasPrefix(arg, "--package=") {
				built[strings.TrimPrefix(arg, "--package=")] = true
			}
		}
	}
	for _, name := range []string{"codex-cli", "codex-code-mode-host", "codex-exec-server-cli", "codex-bwrap"} {
		if !built[name] {
			return fmt.Errorf("required source build was not executed: %s", name)
		}
	}
	return nil
}

func bindSourceBuildReceipt(meta map[string]any, dir, bin string) error {
	version, _ := meta["codex_version"].(string)
	path := os.Getenv("CODEXGOLDEN_SOURCE_BUILD_RECEIPT")
	if path == "" {
		if version == "codex-cli 0.0.0" || strings.Contains(strings.TrimPrefix(version, "codex-cli "), "-") {
			return fmt.Errorf("source CLI requires CODEXGOLDEN_SOURCE_BUILD_RECEIPT")
		}
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	commit, _ := meta["source_commit"].(string)
	binary, _ := meta["binary_sha256"].(string)
	models, _ := meta["models_sha256"].(string)
	if err := validateSourceBuildReceipt(raw, version, commit, binary, models); err != nil {
		return err
	}
	var receipt sourceBuildReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return err
	}
	wanted, err := filepath.EvalSymlinks(filepath.Join(receipt.ArtifactRoot, "bin/codex"))
	if err != nil || actual != wanted {
		return fmt.Errorf("CLI must use the verified companion artifact layout")
	}
	if err := os.WriteFile(filepath.Join(dir, sourceBuildReceiptFile), raw, 0600); err != nil {
		return err
	}
	meta["source_build_receipt_sha256"] = fmt.Sprintf("%x", sha256.Sum256(raw))
	return nil
}
