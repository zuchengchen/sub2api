// Command codexprompts pins the official codex-rs model catalog used by the
// OAuth synthesis path. It never fetches the network: point -models at a
// trusted checkout and inspect the printed slug/prompt inventory before
// copying it into internal/pkg/openai.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

type catalog struct {
	Models []struct {
		Slug          string `json:"slug"`
		ModelMessages struct {
			InstructionsTemplate string `json:"instructions_template"`
		} `json:"model_messages"`
	} `json:"models"`
}

func main() {
	modelsPath := flag.String("models", "../codex/codex-rs/models-manager/models.json", "path to codex-rs/models-manager/models.json")
	write := flag.Bool("write", false, "copy the catalog to internal/pkg/openai/codex_catalog_models.json")
	flag.Parse()

	data, err := os.ReadFile(*modelsPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codexprompts:", err)
		os.Exit(1)
	}
	var parsed catalog
	if err := json.Unmarshal(data, &parsed); err != nil {
		fmt.Fprintln(os.Stderr, "codexprompts: parse:", err)
		os.Exit(1)
	}
	if len(parsed.Models) == 0 {
		fmt.Fprintln(os.Stderr, "codexprompts: catalog has no models")
		os.Exit(1)
	}
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	fmt.Printf("models: %d\nsha256: %s\n", len(parsed.Models), hash)

	slugs := make([]string, 0, len(parsed.Models))
	for _, model := range parsed.Models {
		slugs = append(slugs, model.Slug)
	}
	sort.Strings(slugs)
	for _, slug := range slugs {
		for _, model := range parsed.Models {
			if model.Slug == slug {
				fmt.Printf("  %-28s prompt_bytes=%d\n", slug, len(model.ModelMessages.InstructionsTemplate))
				break
			}
		}
	}

	if !*write {
		return
	}
	target := filepath.Join("internal", "pkg", "openai", "codex_catalog_models.json")
	if err := os.WriteFile(target, data, 0o644); err != nil { //nolint:gosec // G703: target 是硬编码的仓库内相对路径；data 只是写入内容，不参与路径
		fmt.Fprintln(os.Stderr, "codexprompts: write:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\nupdate CodexCatalogPinnedHash in internal/pkg/openai/codex_catalog.go to %s\n", target, hash)
}
