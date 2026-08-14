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
	"unicode"
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
	Layer1Demotions    FileManifest        `json:"layer1_demotions"`
	Layer1Suppressions FileManifest        `json:"layer1_suppressions"`
	CandidateEndpoints []CandidateEndpoint `json:"candidate_endpoints"`
}

type Asset struct {
	Manifest           Manifest
	Layer1             []string
	Layer2             []string
	Layer1Demotions    []string
	Layer1Suppressions []string
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
	demotions, err := loadTerms(id, manifest.Layer1Demotions)
	if err != nil {
		return Asset{}, fmt.Errorf("load candidate layer 1 demotions: %w", err)
	}
	suppressions, err := loadTerms(id, manifest.Layer1Suppressions)
	if err != nil {
		return Asset{}, fmt.Errorf("load candidate layer 1 suppressions: %w", err)
	}
	layer1, layer2, err = applyLayer1Overrides(layer1, layer2, demotions, suppressions)
	if err != nil {
		return Asset{}, err
	}
	for _, endpoint := range manifest.CandidateEndpoints {
		if strings.TrimSpace(endpoint.ID) == "" || strings.TrimSpace(endpoint.Model) == "" || endpoint.Enabled || strings.TrimSpace(endpoint.Token) != "" {
			return Asset{}, errors.New("candidate endpoint must be named, tokenless, and disabled")
		}
	}
	return Asset{
		Manifest: manifest, Layer1: layer1, Layer2: layer2,
		Layer1Demotions: demotions, Layer1Suppressions: suppressions,
	}, nil
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

func applyLayer1Overrides(layer1, layer2, demotions, suppressions []string) ([]string, []string, error) {
	layer1Terms := make(map[string]struct{}, len(layer1))
	for _, term := range layer1 {
		layer1Terms[normalizedCandidateTerm(term)] = struct{}{}
	}

	excluded := make(map[string]string, len(demotions)+len(suppressions))
	register := func(terms []string, disposition string) error {
		for _, term := range terms {
			key := normalizedCandidateTerm(term)
			if _, exists := layer1Terms[key]; !exists {
				return fmt.Errorf("candidate layer 1 %s term %q is not in the source list", disposition, term)
			}
			if previous, exists := excluded[key]; exists {
				return fmt.Errorf("candidate layer 1 term %q has both %s and %s dispositions", term, previous, disposition)
			}
			excluded[key] = disposition
		}
		return nil
	}
	if err := register(demotions, "demotion"); err != nil {
		return nil, nil, err
	}
	if err := register(suppressions, "suppression"); err != nil {
		return nil, nil, err
	}

	effectiveLayer1 := make([]string, 0, len(layer1)-len(excluded))
	for _, term := range layer1 {
		if _, overridden := excluded[normalizedCandidateTerm(term)]; !overridden {
			effectiveLayer1 = append(effectiveLayer1, term)
		}
	}

	effectiveLayer2 := make([]string, 0, len(layer2)+len(demotions))
	seenLayer2 := make(map[string]struct{}, len(layer2)+len(demotions))
	for _, term := range append(append([]string(nil), layer2...), demotions...) {
		key := candidatePrefilterKey(term)
		if key == "" {
			continue
		}
		if _, exists := seenLayer2[key]; exists {
			continue
		}
		seenLayer2[key] = struct{}{}
		effectiveLayer2 = append(effectiveLayer2, term)
	}
	return effectiveLayer1, effectiveLayer2, nil
}

func normalizedCandidateTerm(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func candidatePrefilterKey(value string) string {
	var builder strings.Builder
	lastSpace := false
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			_, _ = builder.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			_ = builder.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(builder.String())
}

func cloneAsset(asset Asset) Asset {
	asset.Layer1 = append([]string(nil), asset.Layer1...)
	asset.Layer2 = append([]string(nil), asset.Layer2...)
	asset.Layer1Demotions = append([]string(nil), asset.Layer1Demotions...)
	asset.Layer1Suppressions = append([]string(nil), asset.Layer1Suppressions...)
	asset.Manifest.CandidateEndpoints = append([]CandidateEndpoint(nil), asset.Manifest.CandidateEndpoints...)
	return asset
}
