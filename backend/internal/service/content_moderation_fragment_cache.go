package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode"

	contentmoderationassets "github.com/Wei-Shaw/sub2api/resources/content-moderation"
)

const (
	defaultContentModerationSecondLayerModel      = "sileader/qwen3guard:0.6b"
	defaultContentModerationSecondLayerTimeoutMS  = 3000
	defaultContentModerationSecondLayerInputLimit = 4000
	minContentModerationSecondLayerTimeoutMS      = 100
	minContentModerationSecondLayerInputLimit     = 128
	maxContentModerationSecondLayerInputLimit     = 100000
)

var contentModerationScannerIDs = []string{
	"violent",
	"non_violent_illegal_acts",
	"sexual_content_or_sexual_acts",
	"pii",
	"suicide_and_self_harm",
	"unethical_acts",
	"politically_sensitive_topics",
	"copyright_violation",
	"jailbreak",
}

func normalizeContentModerationCacheVersion(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return defaultContentModerationCacheVersion
	}
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			_, _ = builder.WriteRune(r)
		}
		if builder.Len() >= 64 {
			break
		}
	}
	if builder.Len() == 0 {
		return defaultContentModerationCacheVersion
	}
	return builder.String()
}

func normalizeContentModerationEndpoints(endpoints []ContentModerationEndpoint) []ContentModerationEndpoint {
	out := make([]ContentModerationEndpoint, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		endpoint.ID = strings.TrimSpace(endpoint.ID)
		endpoint.Name = strings.TrimSpace(endpoint.Name)
		endpoint.BaseURL = strings.TrimRight(strings.TrimSpace(endpoint.BaseURL), "/")
		endpoint.Model = strings.TrimSpace(endpoint.Model)
		endpoint.Profile = normalizeContentModerationModelProfile(endpoint.Profile)
		endpoint.ModelRevision = strings.TrimSpace(endpoint.ModelRevision)
		endpoint.PromptVersion = strings.TrimSpace(endpoint.PromptVersion)
		endpoint.StopTokens = normalizeContentModerationStopTokens(endpoint.StopTokens)
		endpoint.Token = strings.TrimSpace(endpoint.Token)
		if endpoint.ID == "" || endpoint.BaseURL == "" {
			continue
		}
		if _, exists := seen[endpoint.ID]; exists {
			continue
		}
		seen[endpoint.ID] = struct{}{}
		if endpoint.Name == "" {
			endpoint.Name = endpoint.ID
		}
		if endpoint.Model == "" {
			endpoint.Model = defaultContentModerationSecondLayerModel
		}
		if endpoint.Profile == ContentModerationModelProfileYuFengXGuard &&
			(endpoint.PromptVersion == "" || endpoint.PromptVersion == contentModerationYuFengLegacyPromptVersion ||
				endpoint.PromptVersion == contentModerationYuFengPreviousPromptVersion) {
			endpoint.PromptVersion = ContentModerationYuFengPromptVersion
		}
		if endpoint.TimeoutMS <= 0 {
			endpoint.TimeoutMS = defaultContentModerationSecondLayerTimeoutMS
		}
		if endpoint.TimeoutMS > maxContentModerationTimeoutMS {
			endpoint.TimeoutMS = maxContentModerationTimeoutMS
		}
		if endpoint.InputLimit <= 0 {
			endpoint.InputLimit = defaultContentModerationSecondLayerInputLimit
		}
		if endpoint.InputLimit > maxContentModerationSecondLayerInputLimit {
			endpoint.InputLimit = maxContentModerationSecondLayerInputLimit
		}
		out = append(out, endpoint)
	}
	return out
}

func normalizeContentModerationStopTokens(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len([]rune(value)) > 32 {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

// mergeContentModerationEndpointTokens treats an empty token as "keep the
// existing token" for a matching endpoint. Disabling an endpoint never copies
// a token into a response; explicit token removal is performed by deleting and
// recreating the endpoint while disabled.
func mergeContentModerationEndpointTokens(existing, updates []ContentModerationEndpoint) []ContentModerationEndpoint {
	tokens := make(map[string]string, len(existing))
	for _, endpoint := range existing {
		if id := strings.TrimSpace(endpoint.ID); id != "" {
			tokens[id] = endpoint.Token
		}
	}
	merged := append([]ContentModerationEndpoint(nil), updates...)
	for i := range merged {
		if strings.TrimSpace(merged[i].Token) == "" {
			merged[i].Token = tokens[strings.TrimSpace(merged[i].ID)]
		}
	}
	return merged
}

func normalizeContentModerationScannerIDs(values []string) []string {
	allowed := make(map[string]struct{}, len(contentModerationScannerIDs))
	for _, value := range contentModerationScannerIDs {
		allowed[value] = struct{}{}
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if _, ok := allowed[value]; !ok {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func (cfg *ContentModerationConfig) fragmentCacheNamespace() string {
	return cfg.fragmentCacheNamespaceWithPolicyRevisions(
		contentModerationKeywordContextPolicyRevision,
		contentModerationHardKeywordMatcherPolicyVersion,
	)
}

func (cfg *ContentModerationConfig) fragmentCacheNamespaceWithKeywordContextRevision(keywordContextRevision string) string {
	return cfg.fragmentCacheNamespaceWithPolicyRevisions(
		keywordContextRevision,
		contentModerationHardKeywordMatcherPolicyVersion,
	)
}

func (cfg *ContentModerationConfig) fragmentCacheNamespaceWithPolicyRevisions(keywordContextRevision, keywordMatcherRevision string) string {
	if cfg == nil {
		return ""
	}
	policy := struct {
		Version           string                      `json:"version"`
		Keywords          []string                    `json:"keywords"`
		HardPatterns      []string                    `json:"hard_patterns"`
		CandidateKeywords []string                    `json:"candidate_keywords"`
		KeywordAllowlist  []string                    `json:"keyword_allowlist"`
		KeywordContextRev string                      `json:"keyword_context_revision"`
		KeywordMatcherRev string                      `json:"keyword_matcher_revision"`
		Candidate         string                      `json:"candidate"`
		CandidateOn       bool                        `json:"candidate_on"`
		CandidateRev      string                      `json:"candidate_revision"`
		Prefilter         string                      `json:"second_layer_prefilter"`
		FirstLayerStage   string                      `json:"first_layer_stage"`
		SecondLayerOn     bool                        `json:"second_layer_on"`
		SecondLayerStage  string                      `json:"second_layer_stage"`
		Endpoints         []ContentModerationEndpoint `json:"endpoints"`
		Scanners          []string                    `json:"scanners"`
		BlockTTL          int                         `json:"block_ttl"`
		AllowTTL          int                         `json:"allow_ttl"`
		TTLPolicy         string                      `json:"ttl_policy"`
		KeywordPolicy     string                      `json:"keyword_policy"`
		ContextPolicy     string                      `json:"context_policy"`
		EvidencePolicy    string                      `json:"evidence_policy"`
		YuFengPolicy      string                      `json:"yufeng_policy,omitempty"`
		PolicyDigest      string                      `json:"policy_digest"`
	}{
		Version:           normalizeContentModerationCacheVersion(cfg.CacheVersion),
		Keywords:          normalizeBlockedKeywords(cfg.BlockedKeywords),
		HardPatterns:      normalizeBlockedKeywords(cfg.HardBlockPatterns),
		CandidateKeywords: normalizeBlockedKeywords(cfg.CandidateKeywords),
		KeywordAllowlist:  normalizeBlockedKeywords(cfg.KeywordAllowlist),
		KeywordContextRev: strings.TrimSpace(keywordContextRevision),
		KeywordMatcherRev: strings.TrimSpace(keywordMatcherRevision),
		Candidate:         strings.TrimSpace(cfg.CandidateAsset),
		CandidateOn:       cfg.CandidateEnabled,
		CandidateRev:      contentModerationCandidateRevision(cfg),
		Prefilter:         contentModerationSecondLayerPrefilterCacheRevision(cfg),
		FirstLayerStage:   normalizeContentModerationFirstLayerStage(cfg.FirstLayerStage),
		SecondLayerOn:     cfg.SecondLayerEnabled,
		SecondLayerStage:  normalizeContentModerationSecondLayerStage(cfg.SecondLayerStage),
		Endpoints:         normalizeContentModerationEndpoints(cfg.SecondLayerEndpoints),
		Scanners:          normalizeContentModerationScannerIDs(cfg.SecondLayerScanners),
		BlockTTL:          cfg.FragmentBlockTTLSeconds,
		AllowTTL:          cfg.FragmentAllowTTLSeconds,
		TTLPolicy:         strings.TrimSpace(cfg.FragmentTTLPolicyVersion),
		KeywordPolicy:     strings.TrimSpace(cfg.KeywordPolicyVersion),
		ContextPolicy:     strings.TrimSpace(cfg.ContextPolicyVersion),
		EvidencePolicy:    strings.TrimSpace(cfg.EvidencePolicyVersion),
		YuFengPolicy:      contentModerationYuFengPolicyCacheRevision(cfg),
		PolicyDigest:      contentModerationPolicyDigest(cfg),
	}
	raw, _ := json.Marshal(policy)
	digest := sha256.Sum256(raw)
	return policy.Version + ":" + hex.EncodeToString(digest[:16])
}

func contentModerationYuFengPolicyCacheRevision(cfg *ContentModerationConfig) string {
	if cfg == nil || !cfg.SecondLayerEnabled {
		return ""
	}
	for _, endpoint := range cfg.SecondLayerEndpoints {
		if normalizeContentModerationModelProfile(endpoint.Profile) == ContentModerationModelProfileYuFengXGuard {
			return ContentModerationYuFengPromptVersion
		}
	}
	return ""
}

func contentModerationCandidateRevision(cfg *ContentModerationConfig) string {
	if cfg == nil || !cfg.CandidateEnabled {
		return ""
	}
	asset, err := contentmoderationassets.Load(cfg.CandidateAsset)
	if err != nil {
		return "invalid:" + strings.TrimSpace(cfg.CandidateAsset)
	}
	return asset.Manifest.SourceCommit + ":" +
		asset.Manifest.Layer1.EmbeddedSHA256 + ":" +
		asset.Manifest.Layer2.EmbeddedSHA256 + ":" +
		asset.Manifest.Layer1Demotions.EmbeddedSHA256 + ":" +
		asset.Manifest.Layer1Suppressions.EmbeddedSHA256
}

func contentModerationSecondLayerPrefilterCacheRevision(cfg *ContentModerationConfig) string {
	if cfg == nil || !cfg.CandidateEnabled {
		return ""
	}
	return contentModerationSecondLayerPrefilterPolicyVersion
}

func contentModerationEndpointViews(endpoints []ContentModerationEndpoint) []ContentModerationEndpointView {
	endpoints = normalizeContentModerationEndpoints(endpoints)
	out := make([]ContentModerationEndpointView, 0, len(endpoints))
	for _, endpoint := range endpoints {
		out = append(out, ContentModerationEndpointView{
			ID:              endpoint.ID,
			Name:            endpoint.Name,
			BaseURL:         endpoint.BaseURL,
			Model:           endpoint.Model,
			Profile:         endpoint.Profile,
			ModelRevision:   endpoint.ModelRevision,
			PromptVersion:   endpoint.PromptVersion,
			StopTokens:      append([]string(nil), endpoint.StopTokens...),
			Enabled:         endpoint.Enabled,
			TimeoutMS:       endpoint.TimeoutMS,
			InputLimit:      endpoint.InputLimit,
			TokenConfigured: endpoint.Token != "",
			TokenMasked:     maskSecretTail(endpoint.Token),
		})
	}
	return out
}
