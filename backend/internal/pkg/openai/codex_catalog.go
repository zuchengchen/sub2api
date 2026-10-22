package openai

import (
	_ "embed"
	"encoding/json"
	"strings"
	"sync"
)

// codexCatalogModelsJSON is the official codex-rs models-manager/models.json
// pinned at HEAD d138398cef (rust-v0.164.0-alpha.9). It is the single source of
// truth for Codex model descriptors and prompts on the OAuth synthesis path;
// the hash is asserted by TestCodexCatalogPinnedHash.
//
//go:embed codex_catalog_models.json
var codexCatalogModelsJSON []byte

// CodexCatalogModel is the subset of ModelInfo used by request synthesis and
// manifest generation. Unknown fields stay in the raw catalog.
type CodexCatalogModel struct {
	Slug                              string   `json:"slug"`
	DisplayName                       string   `json:"display_name"`
	Description                       string   `json:"description"`
	ShellType                         string   `json:"shell_type"`
	ApplyPatchToolType                *string  `json:"apply_patch_tool_type"`
	WebSearchToolType                 string   `json:"web_search_tool_type"`
	ToolMode                          any      `json:"tool_mode"`
	UseResponsesLite                  bool     `json:"use_responses_lite"`
	ContextWindow                     int64    `json:"context_window"`
	MaxContextWindow                  int64    `json:"max_context_window"`
	ExperimentalSupportedTools        []string `json:"experimental_supported_tools"`
	IncludeSkillsUsageInstructions    bool     `json:"include_skills_usage_instructions"`
	IncludePluginUsageInstructions    bool     `json:"include_plugin_usage_instructions"`
	IncludeAppsUsageInstructions      bool     `json:"include_apps_usage_instructions"`
	SupportsParallelToolCalls         bool     `json:"supports_parallel_tool_calls"`
	SupportsReasoningSummaryParameter bool     `json:"supports_reasoning_summary_parameter"`
	DefaultReasoningSummary           string   `json:"default_reasoning_summary"`
	DefaultReasoningLevel             *string  `json:"default_reasoning_level"`
	SupportVerbosity                  bool     `json:"support_verbosity"`
	DefaultVerbosity                  *string  `json:"default_verbosity"`
	NodeREPLAutoReviewRequired        bool     `json:"node_repl_auto_review_required"`
	NodeREPLDisabled                  bool     `json:"node_repl_disabled"`
	ModelMessages                     struct {
		InstructionsTemplate string `json:"instructions_template"`
	} `json:"model_messages"`
}

type codexCatalogIndex struct {
	bySlug map[string]CodexCatalogModel
}

var (
	codexCatalogOnce  sync.Once
	codexCatalogValue *codexCatalogIndex
)

func loadCodexCatalog() *codexCatalogIndex {
	codexCatalogOnce.Do(func() {
		var root struct {
			Models []CodexCatalogModel `json:"models"`
		}
		if err := json.Unmarshal(codexCatalogModelsJSON, &root); err != nil {
			codexCatalogValue = &codexCatalogIndex{bySlug: map[string]CodexCatalogModel{}}
			return
		}
		index := &codexCatalogIndex{bySlug: make(map[string]CodexCatalogModel, len(root.Models))}
		for _, model := range root.Models {
			slug := CanonicalizeOpenAIModelAliasSpelling(model.Slug)
			if slug == "" {
				continue
			}
			index.bySlug[slug] = model
		}
		codexCatalogValue = index
	})
	return codexCatalogValue
}

var codexCatalogEffortSuffixes = []string{
	"-openai-compact", "-none", "-minimal", "-low", "-medium", "-high", "-xhigh", "-max", "-ultra",
}

// CodexCatalogModelForSlug resolves effort/compact spellings back to the
// published catalog slug (gpt-5.6-sol-high -> gpt-5.6-sol).
func CodexCatalogModelForSlug(model string) (CodexCatalogModel, bool) {
	canonical := CanonicalizeOpenAIModelAliasSpelling(model)
	if canonical == "" {
		return CodexCatalogModel{}, false
	}
	index := loadCodexCatalog()
	if entry, ok := index.bySlug[canonical]; ok {
		return entry, true
	}
	candidate := canonical
	for {
		trimmed := false
		for _, suffix := range codexCatalogEffortSuffixes {
			if next, ok := strings.CutSuffix(candidate, suffix); ok && next != "" {
				candidate = next
				trimmed = true
				break
			}
		}
		if !trimmed {
			break
		}
		if entry, ok := index.bySlug[candidate]; ok {
			return entry, true
		}
	}
	return CodexCatalogModel{}, false
}

// CodexCatalogInstructionsForSlug returns the official base instructions for a
// published catalog model. Empty when the model is not in the pinned catalog.
func CodexCatalogInstructionsForSlug(model string) string {
	entry, ok := CodexCatalogModelForSlug(model)
	if !ok {
		return ""
	}
	return strings.TrimSpace(entry.ModelMessages.InstructionsTemplate)
}

// CodexCatalogPinnedHash is the sha256 of the embedded catalog. The generator
// (backend/tools/codexprompts) prints this value after refreshing the file.
const CodexCatalogPinnedHash = "943ca7fe1d19ed019054158303f0dbbd80b3bef43b3aa425a71aa7cb3fb52c2b"
