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

const DeepSeekV4FlashAuditV1 = "deepseek-v4-flash-audit-v1"

//go:embed deepseek-v4-flash-audit-v1/*
var files embed.FS

type FileManifest struct {
	File           string `json:"file"`
	Entries        int    `json:"entries"`
	SourceSHA256   string `json:"source_sha256"`
	EmbeddedSHA256 string `json:"embedded_sha256"`
}

type TextFileManifest struct {
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
}

type Manifest struct {
	ID                  string           `json:"id"`
	EnabledByDefault    bool             `json:"enabled_by_default"`
	SourceCommit        string           `json:"source_commit"`
	PolicyVersion       string           `json:"policy_version"`
	DefaultModel        string           `json:"default_model"`
	ThinkingType        string           `json:"thinking_type"`
	ResponseFormat      string           `json:"response_format"`
	ConfidenceThreshold float64          `json:"confidence_threshold"`
	ReasonMaxRunes      int              `json:"reason_max_runes"`
	RiskCategories      []string         `json:"risk_categories"`
	SystemPrompt        TextFileManifest `json:"system_prompt"`
	Layer1              FileManifest     `json:"layer1"`
	Layer2              FileManifest     `json:"layer2"`
	Layer1Demotions     FileManifest     `json:"layer1_demotions"`
	Layer1Suppressions  FileManifest     `json:"layer1_suppressions"`
}

type Asset struct {
	Manifest           Manifest
	SystemPrompt       string
	Layer1             []string
	Layer2             []string
	Layer1Demotions    []string
	Layer1Suppressions []string
}

var (
	deepSeekOnce  sync.Once
	deepSeekAsset Asset
	deepSeekErr   error
)

func Load(id string) (Asset, error) {
	switch strings.TrimSpace(id) {
	case DeepSeekV4FlashAuditV1:
		deepSeekOnce.Do(func() {
			deepSeekAsset, deepSeekErr = loadAsset(DeepSeekV4FlashAuditV1)
		})
		if deepSeekErr != nil {
			return Asset{}, deepSeekErr
		}
		return cloneAsset(deepSeekAsset), nil
	default:
		return Asset{}, fmt.Errorf("unknown content moderation candidate asset %q", id)
	}
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
	if manifest.ID != id {
		return Asset{}, errors.New("candidate manifest identity is invalid")
	}
	if err := validateManifest(manifest); err != nil {
		return Asset{}, err
	}
	var systemPrompt string
	if manifest.SystemPrompt.File != "" {
		systemPrompt, err = loadText(id, manifest.SystemPrompt)
		if err != nil {
			return Asset{}, fmt.Errorf("load system prompt: %w", err)
		}
	}
	layer1, err := loadTerms(id, manifest.Layer1)
	if err != nil {
		return Asset{}, fmt.Errorf("load candidate layer 1: %w", err)
	}
	layer2, err := loadTerms(id, manifest.Layer2)
	if err != nil {
		return Asset{}, fmt.Errorf("load candidate layer 2: %w", err)
	}
	var demotions []string
	if manifest.Layer1Demotions.File != "" {
		demotions, err = loadTerms(id, manifest.Layer1Demotions)
		if err != nil {
			return Asset{}, fmt.Errorf("load candidate layer 1 demotions: %w", err)
		}
	}
	var suppressions []string
	if manifest.Layer1Suppressions.File != "" {
		suppressions, err = loadTerms(id, manifest.Layer1Suppressions)
		if err != nil {
			return Asset{}, fmt.Errorf("load candidate layer 1 suppressions: %w", err)
		}
	}
	layer1, layer2, err = applyLayer1Overrides(layer1, layer2, demotions, suppressions)
	if err != nil {
		return Asset{}, err
	}
	return Asset{
		Manifest: manifest, SystemPrompt: systemPrompt, Layer1: layer1, Layer2: layer2,
		Layer1Demotions: demotions, Layer1Suppressions: suppressions,
	}, nil
}

func validateManifest(manifest Manifest) error {
	switch manifest.ID {
	case DeepSeekV4FlashAuditV1:
		if !manifest.EnabledByDefault || manifest.PolicyVersion != DeepSeekV4FlashAuditV1 {
			return errors.New("DeepSeek policy identity or default state is invalid")
		}
		if manifest.DefaultModel != "deepseek-v4-flash" || manifest.ThinkingType != "disabled" || manifest.ResponseFormat != "json_object" {
			return errors.New("DeepSeek runtime contract is invalid")
		}
		if manifest.ConfidenceThreshold != 0.8 || manifest.ReasonMaxRunes != 20 {
			return errors.New("DeepSeek decision contract is invalid")
		}
		if manifest.SystemPrompt.File == "" || manifest.SystemPrompt.SHA256 == "" {
			return errors.New("DeepSeek system prompt manifest is missing")
		}
		expectedCategories := []string{
			"cyber_abuse", "cracking", "security_bypass", "account_abuse",
			"sexual_deepfake", "doxxing", "violent_threat", "self_harm",
			"weapons", "sexual_content",
		}
		if len(manifest.RiskCategories) != len(expectedCategories) {
			return errors.New("DeepSeek risk category contract is invalid")
		}
		for i := range expectedCategories {
			if manifest.RiskCategories[i] != expectedCategories[i] {
				return errors.New("DeepSeek risk category contract is invalid")
			}
		}
		return nil
	default:
		return errors.New("unsupported candidate manifest")
	}
}

func loadText(id string, manifest TextFileManifest) (string, error) {
	if path.Base(manifest.File) != manifest.File || strings.TrimSpace(manifest.SHA256) == "" {
		return "", errors.New("invalid text file manifest")
	}
	raw, err := files.ReadFile(path.Join(id, manifest.File))
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(raw)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), manifest.SHA256) {
		return "", errors.New("text asset checksum mismatch")
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "", errors.New("text asset is empty")
	}
	return string(raw), nil
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
	asset.Manifest.RiskCategories = append([]string(nil), asset.Manifest.RiskCategories...)
	return asset
}
