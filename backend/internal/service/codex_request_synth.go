package service

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
)

// codexOfficialTools0163JSON is the official tool array captured from
// codex-cli 0.163.0 (backend/tools/codexgolden, http_fallback_tool_loop).
// It is the schema source for synthesized compatible clients.
//
//go:embed codex_official_tools_0_163.json
var codexOfficialTools0163JSON []byte

type codexCapability string

const (
	codexCapabilityNative     codexCapability = "native"
	codexCapabilityCompatible codexCapability = "compatible"
	codexCapabilityGeneric    codexCapability = "generic"
)

// codexSynthOptions carries request-scoped inputs. DeviceProfile is resolved by
// the caller only after isCodexOAuthAccount passed.
type codexSynthOptions struct {
	ResponsesLite bool
	IsCompact     bool
	WireProfile   codexWireProfile
	WireVersion   string
	Device        codexDeviceProfile
	TurnID        string
	TurnStartedMs int64
	Now           time.Time
	Strict        bool
}

type codexSynthResult struct {
	Capability codexCapability
	// Modified reports whether the body was rebuilt (native requests stay
	// untouched).
	Modified bool
	// ToolNameReverse maps the upstream-visible canonical name back to the
	// client's original name (aliased[lower] = original).
	ToolNameReverse map[string]string
	// Truncated reports that optional synthesized context was trimmed to fit
	// the model context window.
	Truncated bool
	// Injected confirms the base prompt was placed as input[0].
	Injected bool
}

const (
	codexSynthReserveRatio     = 0.25
	codexSynthMinReserveTokens = 4096
)

var (
	codexOfficialToolsOnce sync.Once
	codexOfficialTools     []map[string]any
	codexOfficialToolIndex map[string]map[string]any

	codexSynthDescriptorWarned sync.Map
)

func loadCodexOfficialTools() {
	codexOfficialToolsOnce.Do(func() {
		var tools []map[string]any
		if err := json.Unmarshal(codexOfficialTools0163JSON, &tools); err != nil {
			codexOfficialTools = nil
			codexOfficialToolIndex = map[string]map[string]any{}
			return
		}
		index := make(map[string]map[string]any, len(tools))
		for _, tool := range tools {
			if name, _ := tool["name"].(string); name != "" {
				index[name] = tool
			}
		}
		codexOfficialTools = tools
		codexOfficialToolIndex = index
	})
}

// codexOfficialToolAliases maps client-declared names onto the official tool
// name. Only client-executed core tools are covered; hosted tools are never
// renamed.
var codexOfficialToolAliases = map[string]string{
	"exec_command":                "exec_command",
	"shell":                       "exec_command",
	"run_shell":                   "exec_command",
	"bash":                        "exec_command",
	"terminal":                    "exec_command",
	"local_shell":                 "exec_command",
	"apply_patch":                 "apply_patch",
	"edit":                        "apply_patch",
	"str_replace_editor":          "apply_patch",
	"str_replace_based_edit_tool": "apply_patch",
	"view_image":                  "view_image",
	"update_plan":                 "update_plan",
	"todo_write":                  "update_plan",
	"todowrite":                   "update_plan",
	"plan":                        "update_plan",
}

var codexCoreOfficialToolNames = []string{
	"exec_command", "write_stdin", "apply_patch", "view_image", "update_plan",
}

var codexUpdatePlanTool = map[string]any{
	"type":        "function",
	"name":        "update_plan",
	"description": "Updates the task plan. Provide an optional explanation and a list of plan items, each with a step and status. At most one step can be in_progress at a time.",
	"strict":      false,
	"parameters": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"explanation": map[string]any{"type": "string"},
			"plan": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"step":   map[string]any{"type": "string"},
						"status": map[string]any{"type": "string", "enum": []any{"pending", "in_progress", "completed"}},
					},
					"required":             []any{"step", "status"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []any{"plan"},
		"additionalProperties": false,
	},
}

// codexRequestSynthesizer rebuilds a non-native request into the Codex CLI
// request shape. It must only be called for isCodexOAuthAccount accounts.
func synthesizeCodexOAuthRequest(reqBody map[string]any, account *Account, upstreamModel string, opts codexSynthOptions) (codexSynthResult, error) {
	result := codexSynthResult{}
	if reqBody == nil || !isCodexOAuthAccount(account) || opts.IsCompact {
		return result, nil
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Strict && opts.WireVersion != "" {
		mode := codexWireProfileModeAuto
		if opts.WireProfile.Name == codexWireProfileStable.Name {
			mode = codexWireProfileModeStable
		} else if opts.WireProfile.Name == codexWireProfileMain.Name && CompareVersions(opts.WireVersion, "0.165.0") >= 0 {
			mode = codexWireProfileModeAuto
		}
		if _, known := resolveCodexWireProfile(opts.WireVersion, mode); !known {
			return result, fmt.Errorf("codex synthesis: unknown Codex version %q (gateway.codex_wire.strict=true)", opts.WireVersion)
		}
	}
	capability := classifyCodexCapability(reqBody, account)
	result.Capability = capability
	if capability == codexCapabilityNative {
		return result, nil
	}

	descriptor, hasDescriptor := openai.CodexCatalogModelForSlug(upstreamModel)
	if !hasDescriptor {
		key := upstreamModel
		if _, loaded := codexSynthDescriptorWarned.LoadOrStore(key, struct{}{}); !loaded {
			logger.LegacyPrintf("service.openai_codex",
				"[Codex] no catalog descriptor for model %q; using heuristic instructions (strict=%v)",
				upstreamModel, opts.Strict)
		}
		if opts.Strict {
			return result, fmt.Errorf("codex synthesis: no catalog descriptor for model %q", upstreamModel)
		}
	}

	baseInstructions := codexSynthesisBaseInstructions(upstreamModel, descriptor, hasDescriptor, opts.WireProfile)
	if baseInstructions == "" {
		return result, fmt.Errorf("codex synthesis: empty base instructions for model %q", upstreamModel)
	}

	original, _ := codexSynthesisCollectOriginalInput(reqBody)
	prefix, skillsItem := codexSynthesisPrefixItems(descriptor, hasDescriptor, capability, baseInstructions, opts)
	input := make([]any, 0, len(prefix)+len(original))
	input = append(input, prefix...)
	input = append(input, original...)
	budget := codexSynthesisTokenBudget(descriptor, hasDescriptor)
	input, truncated, budgetErr := codexSynthesisTrimToBudget(input, budget, skillsItem)
	if budgetErr != nil {
		return result, budgetErr
	}
	result.Truncated = truncated
	reqBody["input"] = input
	result.Injected = true

	reverse := codexSynthesisPrepareTools(reqBody, descriptor, hasDescriptor, capability, opts.ResponsesLite)
	result.ToolNameReverse = reverse

	codexSynthesisApplyDefaults(reqBody, descriptor, hasDescriptor, opts)
	result.Modified = true
	return result, nil
}

func classifyCodexCapability(reqBody map[string]any, account *Account) codexCapability {
	switch strings.ToLower(strings.TrimSpace(account.GetExtraString(codexClientCapabilityExtraKey))) {
	case string(codexCapabilityNative):
		return codexCapabilityNative
	case string(codexCapabilityCompatible):
		return codexCapabilityCompatible
	}
	if codexInputMapCarriesBaseInstructions(reqBody) {
		return codexCapabilityNative
	}
	declaresOfficialTool := false
	for _, name := range codexSynthesisToolNames(reqBody) {
		if _, ok := codexOfficialToolAliases[strings.ToLower(strings.TrimSpace(name))]; ok {
			declaresOfficialTool = true
			break
		}
	}
	if declaresOfficialTool {
		return codexCapabilityCompatible
	}
	return codexCapabilityGeneric
}

func codexSynthesisBaseInstructions(model string, descriptor openai.CodexCatalogModel, hasDescriptor bool, profile codexWireProfile) string {
	// The 0.164/main profile may publish a newer prompt for the same slug;
	// prefer the pinned catalog there. The stable profile keeps the prompt
	// captured from the 0.163 golden (gpt-5.5) and the bundled 0.163-era
	// prompts for the rest so version switches do not silently change output.
	if profile.Name == codexWireProfileStable.Name {
		if canonical := openai.CanonicalizeOpenAIModelAliasSpelling(model); canonical == "gpt-5.5" {
			if v := openai.CodexStableGPT55Instructions(); v != "" {
				return v
			}
		}
	}
	if hasDescriptor && profile.Name == codexWireProfileMain.Name {
		if v := strings.TrimSpace(descriptor.ModelMessages.InstructionsTemplate); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(openai.CodexBaseInstructionsForModel(model)); v != "" {
		return v
	}
	if hasDescriptor {
		return strings.TrimSpace(descriptor.ModelMessages.InstructionsTemplate)
	}
	return ""
}

// codexSynthesisCollectOriginalInput removes top-level instructions, promotes
// system roles to developer, drops an existing base prompt marker, and returns
// the client's remaining messages in order.
func codexSynthesisCollectOriginalInput(reqBody map[string]any) ([]any, bool) {
	hadInstructions := false
	input, _ := reqBody["input"].([]any)
	if input == nil {
		if text, ok := reqBody["input"].(string); ok {
			if strings.TrimSpace(text) != "" {
				input = []any{map[string]any{"type": "message", "role": "user", "content": text}}
			} else {
				input = []any{}
			}
		}
	}
	out := make([]any, 0, len(input)+1)
	if raw, ok := reqBody["instructions"].(string); ok {
		delete(reqBody, "instructions")
		if trimmed := strings.TrimSpace(raw); trimmed != "" {
			hadInstructions = true
			// Keep the client's own system prompt after the synthesized context
			// (position 5 of the documented order) instead of dropping it.
			out = append(out, map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": []any{map[string]any{"type": "input_text", "text": raw}},
			})
		}
	}
	for _, raw := range input {
		item, ok := decodeCodexSynthItem(raw)
		if !ok {
			out = append(out, raw)
			continue
		}
		if isCodexBaseInstructionsItem(item) {
			continue
		}
		if role, _ := item["role"].(string); role == "system" {
			item["role"] = "developer"
		}
		out = append(out, item)
	}
	return out, hadInstructions
}

func decodeCodexSynthItem(raw any) (map[string]any, bool) {
	switch v := raw.(type) {
	case map[string]any:
		return v, true
	case json.RawMessage:
		var item map[string]any
		if err := json.Unmarshal(v, &item); err != nil {
			return nil, false
		}
		return item, true
	default:
		return nil, false
	}
}

func isCodexBaseInstructionsItem(item map[string]any) bool {
	if item == nil || item["role"] != "developer" {
		return false
	}
	meta, _ := item["internal_chat_message_metadata_passthrough"].(map[string]any)
	kinds, _ := meta["content_item_kinds"].([]any)
	for _, kind := range kinds {
		if kind == "model.base_instructions" {
			return true
		}
	}
	return false
}

func codexSynthesisPrefixItems(
	descriptor openai.CodexCatalogModel,
	hasDescriptor bool,
	capability codexCapability,
	baseInstructions string,
	opts codexSynthOptions,
) ([]any, json.RawMessage) {
	device := opts.Device
	baseItem := codexSynthMessageRaw(
		codexWSInstructionsItemID(baseInstructions),
		"developer",
		[]string{baseInstructions},
		[]string{"model.base_instructions"},
		"",
		0,
	)

	permissionsText := codexSynthesisPermissionsText(device)
	permissionsItem := codexSynthMessageRaw(
		codexSynthItemID("permissions", permissionsText),
		"developer",
		[]string{permissionsText},
		[]string{"permissions.instructions"},
		opts.TurnID,
		codexSynthCreateTime(opts.TurnStartedMs, 1),
	)

	var skillsItem json.RawMessage
	if capability == codexCapabilityCompatible {
		skillsText := codexSynthesisSkillsText()
		skillsItem = codexSynthMessageRaw(
			codexSynthItemID("skills", skillsText),
			"developer",
			[]string{skillsText},
			[]string{"host_skills.instructions", "permissions.instructions"},
			opts.TurnID,
			codexSynthCreateTime(opts.TurnStartedMs, 2),
		)
	}

	environmentText := codexSynthesisEnvironmentText(device, opts.Now)
	environmentItem := codexSynthMessageRaw(
		codexSynthItemID("environment", environmentText),
		"user",
		[]string{environmentText},
		[]string{"environments.environment_context"},
		opts.TurnID,
		codexSynthCreateTime(opts.TurnStartedMs, 3),
	)

	prefix := []any{baseItem, permissionsItem}
	if skillsItem != nil {
		prefix = append(prefix, skillsItem)
	}
	prefix = append(prefix, environmentItem)
	return prefix, skillsItem
}

func removeCodexSynthItem(items []any, target json.RawMessage) []any {
	out := items[:0]
	for _, item := range items {
		raw, ok := item.(json.RawMessage)
		if ok && bytes.Equal(raw, target) {
			continue
		}
		out = append(out, item)
	}
	return out
}

func codexSynthesisEstimatedTokens(input []any) int {
	estimate := 0
	for _, item := range input {
		switch v := item.(type) {
		case json.RawMessage:
			estimate += len(v)
		case map[string]any:
			if raw, err := json.Marshal(v); err == nil {
				estimate += len(raw)
			}
		default:
			if raw, err := json.Marshal(v); err == nil {
				estimate += len(raw)
			}
		}
	}
	return estimate/4 + 1
}

// codexSynthesisTrimToBudget drops optional synthesized context (skills, then
// permissions) before failing. The base prompt and client messages are never
// truncated.
func codexSynthesisTrimToBudget(input []any, budget int, skillsItem json.RawMessage) ([]any, bool, error) {
	if budget <= 0 {
		return input, false, nil
	}
	truncated := false
	for codexSynthesisEstimatedTokens(input) > budget {
		if skillsItem != nil {
			input = removeCodexSynthItem(input, skillsItem)
			skillsItem = nil
			truncated = true
			continue
		}
		removed := false
		for i, item := range input {
			raw, ok := item.(json.RawMessage)
			if ok && bytes.Contains(raw, []byte(`"content_item_kinds":["permissions.instructions"]`)) {
				input = append(input[:i], input[i+1:]...)
				removed = true
				truncated = true
				break
			}
		}
		if !removed {
			return input, true, fmt.Errorf("codex synthesis: request exceeds the model context window")
		}
	}
	return input, truncated, nil
}

func codexSynthesisTokenBudget(descriptor openai.CodexCatalogModel, hasDescriptor bool) int {
	contextWindow := descriptor.ContextWindow
	if !hasDescriptor || contextWindow <= 0 {
		contextWindow = configuredCodexFallbackContext
	}
	reserve := int(float64(contextWindow) * codexSynthReserveRatio)
	if reserve < codexSynthMinReserveTokens {
		reserve = codexSynthMinReserveTokens
	}
	if int64(reserve) >= contextWindow {
		return 0
	}
	return int(contextWindow) - reserve
}

func codexCatalogDefaultReasoningLevel(descriptor openai.CodexCatalogModel) string {
	if descriptor.DefaultReasoningLevel == nil {
		return ""
	}
	return strings.TrimSpace(*descriptor.DefaultReasoningLevel)
}

func codexSynthesisPermissionsText(device codexDeviceProfile) string {
	mode := strings.TrimSpace(device.SandboxMode)
	var body string
	switch mode {
	case "read-only":
		body = "Filesystem sandboxing defines which files can be read or written. `sandbox_mode` is `read-only`: The sandbox only permits reading files. Network access is restricted."
	case "danger-full-access":
		body = "Filesystem sandboxing defines which files can be read or written. `sandbox_mode` is `danger-full-access`: No filesystem sandboxing - all commands are permitted. Network access is enabled."
	default:
		body = "Filesystem sandboxing defines which files can be read or written. `sandbox_mode` is `workspace-write`: The sandbox permits reading files, and editing files in `cwd` and `writable_roots`. Editing files in other directories requires approval. Network access is restricted."
	}
	return "<permissions instructions>\n" + body +
		"\nApproval policy is currently never. Do not provide the `sandbox_permissions` for any reason, commands will be rejected.\n</permissions instructions>"
}

func codexSynthesisSkillsText() string {
	return `<skills_instructions>
## Skills
A skill is a set of local instructions to follow that is stored in a ` + "`SKILL.md`" + ` file. Below is the list of skills that can be used. Each entry includes a name and a description.
### Available skills
- imagegen: Generate or edit raster images when the task benefits from AI-created bitmap visuals such as photos, illustrations, textures, sprites, mockups, or transparent-background cutouts. Use when Codex should create a brand-new image, transform an existing image, or derive visual variants from references, and the output should be a bitmap asset rather than repo-native code or vector.
- openai-docs: Use for Codex models, pricing, scheduled tasks, skills, settings, setup, troubleshooting, customization, automations, and self-knowledge, and for OpenAI APIs and products.
- skill-creator: Create or update a Codex skill with appropriately scoped instructions and any needed supporting resources.
- skill-installer: Install Codex skills into ` + "`$CODEX_HOME/skills`" + ` from a curated list or a GitHub repo path.
### How to use skills
- Discovery: the list above is the skills available in this session.
- Trigger rules: If the user names a skill or the task clearly matches a skill's description, use that skill for this turn. Do not carry skills across turns unless re-mentioned.
- Missing/blocked: If a named skill is not available, say so briefly and continue with the best fallback.
</skills_instructions>`
}

func codexSynthesisEnvironmentText(device codexDeviceProfile, now time.Time) string {
	location := time.UTC
	if name := strings.TrimSpace(device.Timezone); name != "" {
		if loaded, err := time.LoadLocation(name); err == nil {
			location = loaded
		}
	}
	timezone := strings.TrimSpace(device.Timezone)
	if timezone == "" {
		timezone = codexDefaultTimezone
	}
	access := "write"
	if strings.TrimSpace(device.SandboxMode) == "read-only" {
		access = "read"
	}
	cwd := strings.TrimSpace(device.WorkspaceCWD)
	if cwd == "" {
		cwd = codexDefaultWorkspaceCWD
	}
	return fmt.Sprintf(`<environment_context>
  <cwd>%s</cwd>
  <shell>bash</shell>
  <current_date>%s</current_date>
  <timezone>%s</timezone>
  <filesystem><workspace_roots><root>%s</root></workspace_roots><permission_profile type="managed"><file_system type="restricted"><entry access="%s"><special>:root</special></entry></file_system></permission_profile></filesystem>
</environment_context>`, cwd, now.In(location).Format("2006-01-02"), timezone, cwd, access)
}

func codexSynthCreateTime(turnStartedMs int64, ordinal int) float64 {
	if turnStartedMs <= 0 {
		return 0
	}
	return float64(turnStartedMs)/1000 + float64(ordinal)*0.000001
}

func codexSynthItemID(kind, text string) string {
	return codexWSInstructionsItemID(kind + "\x00" + text)
}

// codexSynthMessageRaw builds a developer/user message in the official serde
// order. RawMessage keeps this order through json.Marshal of the body.
func codexSynthMessageRaw(id, role string, texts, kinds []string, turnID string, createTime float64) json.RawMessage {
	var buf bytes.Buffer
	buf.WriteString(`{"type":"message","id":`)
	writeCodexJSONString(&buf, id)
	buf.WriteString(`,"role":`)
	writeCodexJSONString(&buf, role)
	buf.WriteString(`,"content":[`)
	for i, text := range texts {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(`{"type":"input_text","text":`)
		writeCodexJSONString(&buf, text)
		buf.WriteByte('}')
	}
	buf.WriteString(`],"internal_chat_message_metadata_passthrough":{`)
	first := true
	if turnID != "" {
		buf.WriteString(`"turn_id":`)
		writeCodexJSONString(&buf, turnID)
		first = false
	}
	if createTime > 0 {
		if !first {
			buf.WriteByte(',')
		}
		buf.WriteString(`"create_time":`)
		buf.WriteString(strconv.FormatFloat(createTime, 'f', -1, 64))
		first = false
	}
	if len(kinds) > 0 {
		if !first {
			buf.WriteByte(',')
		}
		buf.WriteString(`"content_item_kinds":[`)
		for i, kind := range kinds {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeCodexJSONString(&buf, kind)
		}
		buf.WriteByte(']')
	}
	buf.WriteString(`}}`)
	return json.RawMessage(escapeCodexNonASCII(buf.Bytes()))
}

func codexSynthesisToolNames(reqBody map[string]any) []string {
	names := make([]string, 0, 8)
	var collect func(raw any)
	collect = func(raw any) {
		tools, _ := raw.([]any)
		for _, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			if name, _ := tool["name"].(string); strings.TrimSpace(name) != "" {
				names = append(names, name)
			}
			if nested, ok := tool["tools"].([]any); ok {
				collect(nested)
			}
		}
	}
	collect(reqBody["tools"])
	if input, ok := reqBody["input"].([]any); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok || item["type"] != openAIAdditionalToolsItemType {
				continue
			}
			collect(item["tools"])
		}
	}
	return names
}

// codexSynthesisPrepareTools aliases client tool names and injects the core
// official tool set for compatible clients. Generic clients keep their tools
// and only receive the hosted web_search declaration.
func codexSynthesisPrepareTools(
	reqBody map[string]any,
	descriptor openai.CodexCatalogModel,
	hasDescriptor bool,
	capability codexCapability,
	responsesLite bool,
) map[string]string {
	loadCodexOfficialTools()
	reverse := map[string]string{}
	if capability == codexCapabilityGeneric {
		if codexSynthesisToolChoiceNone(reqBody) {
			return reverse
		}
		if !codexSynthesisHasToolType(reqBody, "web_search") && codexDescriptorSupportsWebSearch(descriptor, hasDescriptor) {
			codexSynthesisAppendTools(reqBody, []any{map[string]any{"type": "web_search"}}, responsesLite)
		}
		return reverse
	}

	// Compatible: rename client names to the official name (keeping the
	// client's schema) and then fill missing core tools.
	declared := map[string]struct{}{}
	var aliasTools func(raw any)
	aliasTools = func(raw any) {
		tools, _ := raw.([]any)
		for _, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if !ok {
				continue
			}
			name, _ := tool["name"].(string)
			key := strings.ToLower(strings.TrimSpace(name))
			if canonical, ok := codexOfficialToolAliases[key]; ok {
				if name != canonical {
					reverse[canonical] = name
					tool["name"] = canonical
				}
				declared[canonical] = struct{}{}
			}
			if nested, ok := tool["tools"].([]any); ok {
				aliasTools(nested)
			}
		}
	}
	aliasTools(reqBody["tools"])
	if input, ok := reqBody["input"].([]any); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if item["type"] == openAIAdditionalToolsItemType {
				aliasTools(item["tools"])
			}
			// History may reference the client name; rewrite it so the upstream
			// sees one consistent tool vocabulary.
			if isCodexToolCallItemType(firstNonEmptyString(item["type"])) {
				if name, _ := item["name"].(string); name != "" {
					if canonical, ok := codexOfficialToolAliases[strings.ToLower(strings.TrimSpace(name))]; ok && canonical != name {
						item["name"] = canonical
					}
				}
			}
		}
	}
	if choice, ok := reqBody["tool_choice"].(map[string]any); ok {
		if name, _ := choice["name"].(string); name != "" {
			if canonical, ok := codexOfficialToolAliases[strings.ToLower(strings.TrimSpace(name))]; ok {
				choice["name"] = canonical
			}
		}
	}
	if choice, ok := reqBody["tool_choice"].(string); ok {
		if canonical, exists := codexOfficialToolAliases[strings.ToLower(strings.TrimSpace(choice))]; exists {
			reqBody["tool_choice"] = canonical
		}
	}

	wanted := codexSynthesisWantedOfficialTools(descriptor, hasDescriptor)
	missing := make([]any, 0, len(wanted))
	for _, name := range wanted {
		if _, ok := declared[name]; ok {
			continue
		}
		if tool, ok := codexOfficialToolIndex[name]; ok {
			missing = append(missing, deepCopyCodexMap(tool))
			continue
		}
		if name == "update_plan" {
			missing = append(missing, deepCopyCodexMap(codexUpdatePlanTool))
		}
	}
	if len(missing) > 0 {
		codexSynthesisAppendTools(reqBody, missing, responsesLite)
	}
	return reverse
}

func codexSynthesisWantedOfficialTools(descriptor openai.CodexCatalogModel, hasDescriptor bool) []string {
	wanted := make([]string, 0, len(codexCoreOfficialToolNames))
	if !hasDescriptor || descriptor.ShellType != "" && descriptor.ShellType != "disabled" {
		wanted = append(wanted, "exec_command", "write_stdin")
	}
	if !hasDescriptor || descriptor.ApplyPatchToolType != nil {
		wanted = append(wanted, "apply_patch")
	}
	wanted = append(wanted, "view_image", "update_plan")
	return wanted
}

func codexDescriptorSupportsWebSearch(descriptor openai.CodexCatalogModel, hasDescriptor bool) bool {
	if !hasDescriptor {
		return true
	}
	return strings.TrimSpace(descriptor.WebSearchToolType) != "" && descriptor.WebSearchToolType != "disabled"
}

func codexSynthesisToolChoiceNone(reqBody map[string]any) bool {
	switch choice := reqBody["tool_choice"].(type) {
	case string:
		return strings.EqualFold(strings.TrimSpace(choice), "none")
	case map[string]any:
		return strings.EqualFold(strings.TrimSpace(firstNonEmptyString(choice["type"])), "none")
	default:
		return false
	}
}

func codexSynthesisHasToolType(reqBody map[string]any, toolType string) bool {
	has := false
	check := func(raw any) {
		tools, _ := raw.([]any)
		for _, rawTool := range tools {
			tool, ok := rawTool.(map[string]any)
			if ok && tool["type"] == toolType {
				has = true
				return
			}
		}
	}
	check(reqBody["tools"])
	if input, ok := reqBody["input"].([]any); ok {
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if ok && item["type"] == openAIAdditionalToolsItemType {
				check(item["tools"])
			}
		}
	}
	return has
}

func codexSynthesisAppendTools(reqBody map[string]any, tools []any, responsesLite bool) {
	if len(tools) == 0 {
		return
	}
	if responsesLite {
		input, _ := reqBody["input"].([]any)
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if ok && item["type"] == openAIAdditionalToolsItemType {
				existing, _ := item["tools"].([]any)
				item["tools"] = append(existing, tools...)
				return
			}
		}
		additional := map[string]any{
			"type":  openAIAdditionalToolsItemType,
			"role":  openAIAdditionalToolsDefaultRole,
			"tools": tools,
		}
		reqBody["input"] = append(input, additional)
		return
	}
	existing, _ := reqBody["tools"].([]any)
	reqBody["tools"] = append(existing, tools...)
}

func codexSynthesisApplyDefaults(reqBody map[string]any, descriptor openai.CodexCatalogModel, hasDescriptor bool, opts codexSynthOptions) {
	if _, ok := reqBody["tool_choice"]; !ok {
		reqBody["tool_choice"] = "auto"
	}
	if _, ok := reqBody["parallel_tool_calls"]; !ok {
		reqBody["parallel_tool_calls"] = openAIRequestBodyHasToolsMap(reqBody)
	}
	if _, ok := reqBody["reasoning"]; !ok {
		reqBody["reasoning"] = map[string]any{}
	}
	if reasoning, ok := reqBody["reasoning"].(map[string]any); ok {
		if _, exists := reasoning["effort"]; !exists && hasDescriptor && strings.TrimSpace(codexCatalogDefaultReasoningLevel(descriptor)) != "" {
			reasoning["effort"] = codexCatalogDefaultReasoningLevel(descriptor)
		}
	}
	if _, ok := reqBody["text"]; !ok && hasDescriptor && descriptor.SupportVerbosity && descriptor.DefaultVerbosity != nil {
		if verbosity := strings.TrimSpace(*descriptor.DefaultVerbosity); verbosity != "" {
			reqBody["text"] = map[string]any{"verbosity": verbosity}
		}
	}
	// A 0.164 profile may be configured to omit session-derived cache keys.
	if opts.WireProfile.PromptCacheKeyPolicy == codexPromptCacheKeyOmit {
		delete(reqBody, "prompt_cache_key")
	}
}

func deepCopyCodexMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return in
	}
	return out
}
