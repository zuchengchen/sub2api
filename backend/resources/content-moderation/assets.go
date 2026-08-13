package contentmoderationassets

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
)

const LegacyPromptAuditV1 = "legacy-prompt-audit-v1"

//go:embed legacy-prompt-audit-v1/*.json
var files embed.FS

type FileManifest struct {
	File           string `json:"file"`
	Entries        int    `json:"entries"`
	SourceSHA256   string `json:"source_sha256"`
	EmbeddedSHA256 string `json:"embedded_sha256"`
}

type CandidateEndpoint struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	Token      string `json:"token"`
	Enabled    bool   `json:"enabled"`
	TimeoutMS  int    `json:"timeout_ms"`
	InputLimit int    `json:"input_limit"`
}

type Manifest struct {
	ID                 string              `json:"id"`
	EnabledByDefault   bool                `json:"enabled_by_default"`
	SourceCommit       string              `json:"source_commit"`
	Layer1             FileManifest        `json:"layer1"`
	Layer2             FileManifest        `json:"layer2"`
	CandidateEndpoints []CandidateEndpoint `json:"candidate_endpoints"`
}

type Asset struct {
	Manifest Manifest
	Layer1   []string
	Layer2   []string
}

var (
	legacyOnce  sync.Once
	legacyAsset Asset
	legacyErr   error
)

func Load(id string) (Asset, error) {
	if strings.TrimSpace(id) != LegacyPromptAuditV1 {
		return Asset{}, fmt.Errorf("unknown content moderation candidate asset %q", id)
	}
	legacyOnce.Do(func() {
		legacyAsset, legacyErr = loadAsset(LegacyPromptAuditV1)
	})
	if legacyErr != nil {
		return Asset{}, legacyErr
	}
	return cloneAsset(legacyAsset), nil
}

func loadAsset(id string) (Asset, error) {
	manifestRaw, err := files.ReadFile(path.Join(id, "manifest.json"))
	if err != nil {
		return Asset{}, fmt.Errorf("read candidate manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return Asset{}, fmt.Errorf("decode candidate manifest: %w", err)
	}
	if manifest.ID != id || manifest.EnabledByDefault {
		return Asset{}, errors.New("candidate manifest identity or default state is invalid")
	}
	layer1, err := loadTerms(id, manifest.Layer1)
	if err != nil {
		return Asset{}, fmt.Errorf("load candidate layer 1: %w", err)
	}
	layer2, err := loadTerms(id, manifest.Layer2)
	if err != nil {
		return Asset{}, fmt.Errorf("load candidate layer 2: %w", err)
	}
	for _, endpoint := range manifest.CandidateEndpoints {
		if strings.TrimSpace(endpoint.ID) == "" || strings.TrimSpace(endpoint.Model) == "" || endpoint.Enabled || strings.TrimSpace(endpoint.Token) != "" {
			return Asset{}, errors.New("candidate endpoint must be named, tokenless, and disabled")
		}
	}
	return Asset{Manifest: manifest, Layer1: layer1, Layer2: layer2}, nil
}

func loadTerms(id string, manifest FileManifest) ([]string, error) {
	if path.Base(manifest.File) != manifest.File || manifest.Entries <= 0 {
		return nil, errors.New("invalid candidate file manifest")
	}
	raw, err := files.ReadFile(path.Join(id, manifest.File))
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), manifest.EmbeddedSHA256) {
		return nil, errors.New("candidate asset checksum mismatch")
	}
	var terms []string
	if err := json.Unmarshal(raw, &terms); err != nil {
		return nil, err
	}
	if len(terms) != manifest.Entries {
		return nil, fmt.Errorf("candidate entry count is %d, want %d", len(terms), manifest.Entries)
	}
	seen := make(map[string]struct{}, len(terms))
	for _, term := range terms {
		key := strings.ToLower(strings.TrimSpace(term))
		if key == "" {
			return nil, errors.New("candidate contains an empty term")
		}
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("candidate contains duplicate term %q", term)
		}
		seen[key] = struct{}{}
	}
	return terms, nil
}

func cloneAsset(asset Asset) Asset {
	asset.Layer1 = append([]string(nil), asset.Layer1...)
	asset.Layer2 = append([]string(nil), asset.Layer2...)
	asset.Manifest.CandidateEndpoints = append([]CandidateEndpoint(nil), asset.Manifest.CandidateEndpoints...)
	return asset
}
