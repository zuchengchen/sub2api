package service

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ContentModerationModelProfileQwen         = "qwen_guard"
	ContentModerationModelProfileYuFengXGuard = "yufeng_xguard"
	ContentModerationFirstLayerStageEnforce   = "enforce"
	ContentModerationFirstLayerStageShadow    = "shadow"
	ContentModerationSecondLayerStageEnforce  = "enforce"
	ContentModerationSecondLayerStageShadow   = "shadow"

	DefaultContentModerationFragmentBlockTTLSeconds = 10 * 60 * 60
	DefaultContentModerationFragmentAllowTTLSeconds = 10 * 60 * 60
	MinContentModerationFragmentBlockTTLSeconds     = 300
	MaxContentModerationFragmentBlockTTLSeconds     = 86400
	MaxContentModerationFragmentAllowTTLSeconds     = 86400
	ContentModerationFragmentTTLPolicyVersion       = "ttl-v2"
	ContentModerationContextPolicyVersion           = "context-v3"
	ContentModerationEvidencePolicyVersion          = "keyword-windows-v5"
	ContentModerationKeywordPolicyVersion           = "keyword-v4"
	ContentModerationYuFengPromptVersion            = "yufeng-xguard-v3"
	contentModerationLegacyFragmentTTLPolicyVersion = "ttl-v1"
	contentModerationLegacyContextPolicyVersion     = "context-v1"
	contentModerationPreviousContextPolicyVersion   = "context-v2"
	contentModerationOlderKeywordPolicyVersion      = "keyword-v2"
	contentModerationPreviousKeywordPolicyVersion   = "keyword-v3"
	contentModerationLegacyEvidencePolicyVersion    = "evidence-v1"
	contentModerationOlderEvidencePolicyVersion     = "keyword-windows-v2"
	contentModerationEarlierEvidencePolicyVersion   = "keyword-windows-v3"
	contentModerationPreviousEvidencePolicyVersion  = "keyword-windows-v4"
	contentModerationYuFengLegacyPromptVersion      = "yufeng-xguard-v1"
	contentModerationYuFengPreviousPromptVersion    = "yufeng-xguard-v2"
	ContentModerationContextUser                    = "user"
	ContentModerationContextAssistant               = "assistant_untrusted"
	ContentModerationContextTool                    = "tool"
	ContentModerationContextServiceLog              = "service_log"
	ContentModerationContextCode                    = "code"
	ContentModerationContextConfig                  = "config"
	ContentModerationContextUnknown                 = "unknown"
)

func normalizeContentModerationModelProfile(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "qwen", "qwen_guard", "qwen-guard", "qwen3guard":
		return ContentModerationModelProfileQwen
	case "yufeng", "yufeng_xguard", "yufeng-xguard", "xguard":
		return ContentModerationModelProfileYuFengXGuard
	default:
		return ContentModerationModelProfileQwen
	}
}

func normalizeContentModerationSecondLayerStage(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), ContentModerationSecondLayerStageShadow) {
		return ContentModerationSecondLayerStageShadow
	}
	return ContentModerationSecondLayerStageEnforce
}

func normalizeContentModerationFirstLayerStage(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), ContentModerationFirstLayerStageShadow) {
		return ContentModerationFirstLayerStageShadow
	}
	return ContentModerationFirstLayerStageEnforce
}

// classifyContentModerationContext is intentionally deterministic and uses
// metadata before heuristics. Unknown content stays on the model/candidate
// path instead of being treated as user text or silently allowed.
func classifyContentModerationContext(fragment ContentModerationFragment) string {
	role := strings.ToLower(strings.TrimSpace(fragment.Role))
	kind := strings.ToLower(strings.TrimSpace(fragment.Kind))
	path := strings.ToLower(strings.TrimSpace(fragment.Path))
	if strings.Contains(kind, "config") || hasAnyContextToken(path, "config", "yaml", "yml", "toml", "json", "env", "systemd", "unit") {
		return ContentModerationContextConfig
	}
	if strings.Contains(kind, "code") || hasAnyContextToken(path, "code", "script", "diff", "sql", "source", "stack") {
		return ContentModerationContextCode
	}
	if hasAnyContextToken(path, "log", "systemctl", "journal", "diagnostic", "status", "stderr", "stdout") || hasServiceLogShape(fragment.Text) {
		return ContentModerationContextServiceLog
	}
	if role == "tool" || strings.Contains(kind, "tool") || strings.Contains(path, "tool") || strings.Contains(path, "function") || strings.Contains(path, "plugin") {
		return ContentModerationContextTool
	}
	if role == "user" || role == "developer" || role == "system" {
		return ContentModerationContextUser
	}
	if role == "assistant" {
		return ContentModerationContextAssistant
	}
	return ContentModerationContextUnknown
}

func hasAnyContextToken(value string, tokens ...string) bool {
	for _, token := range tokens {
		if strings.Contains(value, token) {
			return true
		}
	}
	return false
}

func hasServiceLogShape(text string) bool {
	text = strings.ToLower(text)
	return strings.Contains(text, "systemctl status") || strings.Contains(text, "journalctl") ||
		strings.Contains(text, "active: ") || strings.Contains(text, "loaded: ") ||
		(strings.Contains(text, "● ") && strings.Contains(text, "service"))
}

type moderationEvidenceSegment struct {
	Text             string `json:"text"`
	Origin           string `json:"origin"`
	LineStart        int    `json:"line_start,omitempty"`
	LineEnd          int    `json:"line_end,omitempty"`
	Role             string `json:"role"`
	Kind             string `json:"kind"`
	ContextClass     string `json:"context_class"`
	ExtractorVersion string `json:"extractor_version"`
	Truncated        bool   `json:"truncated"`
}

type moderationEvidence struct {
	Text      string
	Mode      string
	Truncated bool
	Segments  []moderationEvidenceSegment
}

var (
	evidenceSecretPattern        = regexp.MustCompile(`(?i)(authorization|bearer|token|api[_-]?key|cookie|private[_-]?key|password|secret)(\s*[:=]\s*)([^\s,;]+)`)
	evidenceAuthorizationPattern = regexp.MustCompile(`(?im)(authorization|cookie)(\s*:\s*)[^\r\n]+`)
	evidenceURLPattern           = regexp.MustCompile(`https?://[^\s<>"']+`)
	evidenceDangerPattern        = regexp.MustCompile(`(?i)(ignore\s+previous|send\s+(?:the\s+)?token|curl\s+[^\n]*\|\s*(?:sh|bash)|wget\s+[^\n]*&&\s*(?:sh|bash)|sudo\s+|chmod\s+\+s|rm\s+-rf|execute\s+command|exfiltrat)`)
)

// buildModerationEvidence selects high-value lines first, then fills a stable
// bounded context window. Candidate-gated request handling uses keyword
// windows instead; this selector remains for the legacy direct scanner API.
func buildModerationEvidence(fragment ContentModerationFragment, limit int) moderationEvidence {
	if limit <= 0 {
		limit = defaultContentModerationSecondLayerInputLimit
	}
	text := strings.TrimSpace(fragment.Text)
	text = evidenceAuthorizationPattern.ReplaceAllString(text, "$1$2[REDACTED]")
	text = evidenceSecretPattern.ReplaceAllString(text, "$1$2[REDACTED]")
	text = redactContentModerationSecrets(text)
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	selected := make([]moderationEvidenceSegment, 0, len(lines))
	seen := make(map[string]struct{})
	nonEmptyLines := 0
	priorityLines := make([]int, 0)
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nonEmptyLines++
		if evidenceDangerPattern.MatchString(line) || evidenceURLPattern.MatchString(line) || strings.Contains(line, "=") || strings.Contains(line, ":") {
			priorityLines = append(priorityLines, i)
		}
	}
	addLine := func(index int) {
		if index < 0 || index >= len(lines) {
			return
		}
		line := strings.TrimSpace(lines[index])
		if line == "" {
			return
		}
		if _, exists := seen[line]; exists {
			return
		}
		seen[line] = struct{}{}
		selected = append(selected, moderationEvidenceSegment{
			Text: line, Origin: fragment.Path, LineStart: index + 1, LineEnd: index + 1,
			Role: fragment.Role, Kind: fragment.Kind, ContextClass: fragment.ContextClass,
			ExtractorVersion: ContentModerationEvidencePolicyVersion,
		})
	}
	for _, index := range priorityLines {
		addLine(index - 1)
		addLine(index)
		addLine(index + 1)
	}
	if len(selected) == 0 {
		for i := range lines {
			addLine(i)
			if len(selected) >= 8 {
				break
			}
		}
	}
	// Preserve source order after selecting high-value lines.
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].LineStart < selected[j].LineStart })
	parts := make([]string, 0, len(selected))
	included := make([]moderationEvidenceSegment, 0, len(selected))
	used := 0
	// Selection and de-duplication are intentional reductions too. Mark them so
	// callers run the bounded first/last fallback instead of treating a partial
	// evidence view as a complete safe decision.
	truncated := len(selected) < nonEmptyLines
	for _, segment := range selected {
		separatorRunes := 0
		if used > 0 {
			separatorRunes = 1
		}
		remaining := limit - used - separatorRunes
		if remaining <= 0 {
			truncated = true
			break
		}
		line := segment.Text
		if len([]rune(line)) > remaining {
			line = trimRunes(line, remaining)
			segment.Truncated = true
			truncated = true
		}
		segment.Text = line
		if separatorRunes > 0 {
			line = "\n" + line
		}
		parts = append(parts, line)
		included = append(included, segment)
		used += len([]rune(line))
		if used >= limit {
			truncated = truncated || used < len([]rune(text))
			break
		}
	}
	if len(parts) == 0 && text != "" {
		part := trimRunes(text, limit)
		parts = []string{part}
		truncated = len([]rune(text)) > limit
		included = []moderationEvidenceSegment{{
			Text: part, Origin: fragment.Path, LineStart: 1, LineEnd: strings.Count(part, "\n") + 1,
			Role: fragment.Role, Kind: fragment.Kind, ContextClass: fragment.ContextClass,
			ExtractorVersion: ContentModerationEvidencePolicyVersion, Truncated: truncated,
		}}
	}
	mode := "selected"
	if len(selected) == 0 {
		mode = "fallback"
	}
	return moderationEvidence{Text: strings.Join(parts, ""), Mode: mode, Truncated: truncated, Segments: included}
}

func contentModerationPolicyDigest(cfg *ContentModerationConfig) string {
	if cfg == nil {
		return ""
	}
	value := strings.Join([]string{
		cfg.FragmentTTLPolicyVersion, cfg.KeywordPolicyVersion, cfg.ContextPolicyVersion,
		cfg.EvidencePolicyVersion, normalizeContentModerationFirstLayerStage(cfg.FirstLayerStage),
		normalizeContentModerationSecondLayerStage(cfg.SecondLayerStage),
	}, "\x00")
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:8])
}

func moderationFragmentTTL(cfg *ContentModerationConfig, result string) time.Duration {
	if cfg == nil {
		return 0
	}
	seconds := cfg.FragmentAllowTTLSeconds
	if result == ContentModerationFragmentBlock {
		seconds = cfg.FragmentBlockTTLSeconds
	}
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}
