package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/tidwall/gjson"
)

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
	// Environment contains only explicitly configured downstream facts, never
	// the defaults used to construct the outbound device identity.
	Environment   codexDeviceProfile
	TurnID        string
	TurnStartedMs int64
	Now           time.Time
	Strict        bool
	// OfficialClient is a prompt-preservation hint, not tool capability
	// evidence. Custom Codex providers can omit base-instruction metadata.
	OfficialClient bool
}

// ErrCodexSynthContextWindowExceeded is returned when a synthesized request is
// certainly larger than the model's hard context limit. Callers surface it as
// the standard context_length_exceeded error so clients can compact.
var ErrCodexSynthContextWindowExceeded = errors.New("codex synthesis: request exceeds the model context window")

// codexSynthContextLengthMessage is the client-facing text for
// ErrCodexSynthContextWindowExceeded, aligned with OpenAI's
// context_length_exceeded wording so client compaction heuristics match.
const codexSynthContextLengthMessage = "Your input exceeds the context window of this model. Please adjust your input and try again."

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

// The client compacts against the advertised window (see /models), so the
// gateway only trims optional synthesized context past that window and only
// rejects past the hard maximum. No extra reserve: the estimate already
// over-counts JSON framing and escapes.
type codexSynthContextLimits struct {
	Advertised int
	Hard       int
}

var codexSynthDescriptorWarned sync.Map

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
	capability := classifyCodexCapability(reqBody, account, opts.OfficialClient)
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

	opts.Environment = codexSynthesisConfiguredEnvironment(account, reqBody)
	original, _ := codexSynthesisCollectOriginalInput(reqBody)
	prefix, skillsItem := codexSynthesisPrefixItems(descriptor, hasDescriptor, capability, baseInstructions, opts)
	input := make([]any, 0, len(prefix)+len(original))
	input = append(input, prefix...)
	input = append(input, original...)
	limits := codexSynthesisContextLimitsFor(account, upstreamModel, descriptor, hasDescriptor)
	input, truncated, budgetErr := codexSynthesisTrimToBudget(input, limits, skillsItem)
	if budgetErr != nil {
		logger.LegacyPrintf("service.openai_codex",
			"[Codex] synthesis rejected: model=%s account=%d estimated=%d advertised=%d hard=%d",
			upstreamModel, accountIDForLog(account), codexSynthesisEstimatedTokens(input), limits.Advertised, limits.Hard)
		return result, budgetErr
	}
	if truncated {
		logger.LegacyPrintf("service.openai_codex",
			"[Codex] synthesis trimmed optional context: model=%s account=%d advertised=%d hard=%d",
			upstreamModel, accountIDForLog(account), limits.Advertised, limits.Hard)
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

func classifyCodexCapability(reqBody map[string]any, account *Account, officialClient bool) codexCapability {
	// An existing base prompt always wins over account defaults. This only
	// controls prompt synthesis; it never grants an undeclared tool executor.
	if codexInputMapCarriesBaseInstructions(reqBody) {
		return codexCapabilityNative
	}
	if officialClient {
		if instructions, _ := reqBody["instructions"].(string); strings.TrimSpace(instructions) != "" {
			return codexCapabilityNative
		}
		if input, _ := reqBody["input"].([]any); len(input) > 0 {
			if first, ok := decodeCodexSynthItem(input[0]); ok && first["role"] == "developer" && first["type"] != openAIAdditionalToolsItemType {
				return codexCapabilityNative
			}
		}
	}
	if account != nil {
		switch strings.ToLower(strings.TrimSpace(account.GetExtraString(codexClientCapabilityExtraKey))) {
		case string(codexCapabilityNative):
			return codexCapabilityNative
		case string(codexCapabilityCompatible):
			return codexCapabilityCompatible
		}
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
			// Keep the client's own system prompt after the synthesized context.
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
	baseItem := codexSynthMessageRaw(
		codexWSInstructionsItemID(baseInstructions),
		"developer",
		[]string{baseInstructions},
		[]string{"model.base_instructions"},
		"",
		0,
	)

	prefix := []any{baseItem}
	if capability != codexCapabilityNative {
		// Model base prompts can mention official CLI tools even when the
		// downstream has no such executor. Actual declarations are authoritative.
		text := "Use only the tools declared for this request, with their exact names and schemas. References to other tools in the base instructions do not make those tools available. Do not infer local tools, skills, shell access, filesystem paths, network access, or approval permissions unless the client declares them or explicit environment configuration supplies them."
		prefix = append(prefix, codexSynthMessageRaw(
			codexSynthItemID("capabilities", text), "developer", []string{text}, nil, opts.TurnID, 0,
		))
	}
	if text := codexSynthesisPermissionsText(opts.Environment); text != "" {
		prefix = append(prefix, codexSynthMessageRaw(
			codexSynthItemID("permissions", text),
			"developer",
			[]string{text},
			[]string{"permissions.instructions"},
			opts.TurnID,
			codexSynthCreateTime(opts.TurnStartedMs, 1),
		))
	}
	if text := codexSynthesisEnvironmentText(opts.Environment, opts.Now); text != "" {
		prefix = append(prefix, codexSynthMessageRaw(
			codexSynthItemID("environment", text),
			"user",
			[]string{text},
			[]string{"environments.environment_context"},
			opts.TurnID,
			codexSynthCreateTime(opts.TurnStartedMs, 3),
		))
	}
	return prefix, nil
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

// codexSynthesisEstimatedTokens approximates input tokens with the ~4 bytes per
// token heuristic. Inline images are charged by pixel area instead of their
// base64 length: a ~600 KB base64 screenshot would otherwise count as ~150k
// tokens while the upstream bills it at roughly a thousand. Opaque
// encrypted_content blobs (reasoning/compaction replay) are excluded: their
// base64 size has no stable relation to prompt tokens.
func codexSynthesisEstimatedTokens(input []any) int {
	estimate := 0
	for _, item := range input {
		var raw []byte
		switch v := item.(type) {
		case json.RawMessage:
			raw = v
		default:
			if encoded, err := json.Marshal(v); err == nil {
				raw = encoded
			}
		}
		estimate += estimateJSONTokensWithImages(raw)
		estimate -= codexSynthesisEncryptedContentBytes(raw) / estimateBytesPerToken
	}
	if estimate < 0 {
		estimate = 0
	}
	return estimate + 1
}

func codexSynthesisEncryptedContentBytes(raw []byte) int {
	if len(raw) == 0 || !bytes.Contains(raw, []byte(`"encrypted_content"`)) {
		return 0
	}
	total := 0
	var walk func(node gjson.Result)
	walk = func(node gjson.Result) {
		if !node.IsObject() && !node.IsArray() {
			return
		}
		node.ForEach(func(key, child gjson.Result) bool {
			if child.Type == gjson.String && key.String() == "encrypted_content" {
				total += len(child.Raw)
				return true
			}
			walk(child)
			return true
		})
	}
	walk(gjson.ParseBytes(raw))
	return total
}

// codexSynthesisTrimToBudget drops optional synthesized context (skills, then
// permissions) once the estimate passes the advertised window, then still
// forwards the request so the upstream decides with its real tokenizer. It
// rejects only when the estimate passes the hard maximum. The base prompt and
// client messages are never truncated.
func codexSynthesisTrimToBudget(input []any, limits codexSynthContextLimits, skillsItem json.RawMessage) ([]any, bool, error) {
	if limits.Advertised <= 0 && limits.Hard <= 0 {
		return input, false, nil
	}
	soft := limits.Advertised
	if soft <= 0 || (limits.Hard > 0 && soft > limits.Hard) {
		soft = limits.Hard
	}
	truncated := false
	for codexSynthesisEstimatedTokens(input) > soft {
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
			break
		}
	}
	if limits.Hard > 0 && codexSynthesisEstimatedTokens(input) > limits.Hard {
		return input, true, ErrCodexSynthContextWindowExceeded
	}
	return input, truncated, nil
}

var codexSynthConfiguredLimitsCache sync.Map // model -> codexSynthContextLimits

// codexSynthesisContextLimitsFor resolves the same window the gateway
// advertises on /models: synced account metadata, then the configured Codex
// descriptor, then the pinned catalog, then the generic fallback.
func codexSynthesisContextLimitsFor(account *Account, upstreamModel string, descriptor openai.CodexCatalogModel, hasDescriptor bool) codexSynthContextLimits {
	limits := codexSynthConfiguredContextLimits(upstreamModel)
	if hasDescriptor {
		if limits.Advertised <= 0 && descriptor.ContextWindow > 0 {
			limits.Advertised = int(descriptor.ContextWindow)
		}
		if int(descriptor.MaxContextWindow) > limits.Hard {
			limits.Hard = int(descriptor.MaxContextWindow)
		}
	}
	if account != nil {
		if metadata, ok := account.GetUpstreamModelMetadata(upstreamModel); ok {
			if metadata.ContextWindow > 0 {
				limits.Advertised = int(metadata.ContextWindow)
			}
			if metadata.MaxContextWindow > 0 {
				limits.Hard = int(metadata.MaxContextWindow)
			}
		}
	}
	if limits.Advertised <= 0 {
		limits.Advertised = configuredCodexFallbackContext
	}
	if limits.Hard < limits.Advertised {
		limits.Hard = limits.Advertised
	}
	return limits
}

func codexSynthConfiguredContextLimits(model string) codexSynthContextLimits {
	model = strings.TrimSpace(model)
	if model == "" {
		return codexSynthContextLimits{}
	}
	if cached, ok := codexSynthConfiguredLimitsCache.Load(model); ok {
		if limits, ok := cached.(codexSynthContextLimits); ok {
			return limits
		}
	}
	configured := newConfiguredCodexModelDescriptor(model)
	limits := codexSynthContextLimits{
		Advertised: int(configured.ContextWindow),
		Hard:       int(configured.MaxContextWindow),
	}
	codexSynthConfiguredLimitsCache.Store(model, limits)
	return limits
}

func accountIDForLog(account *Account) int64 {
	if account == nil {
		return 0
	}
	return account.ID
}

func codexCatalogDefaultReasoningLevel(descriptor openai.CodexCatalogModel) string {
	if descriptor.DefaultReasoningLevel == nil {
		return ""
	}
	return strings.TrimSpace(*descriptor.DefaultReasoningLevel)
}

// codexSynthesisConfiguredEnvironment reads explicit downstream facts. Device
// identity defaults describe the outbound connection, not a client's executor.
func codexSynthesisConfiguredEnvironment(account *Account, reqBody map[string]any) codexDeviceProfile {
	if !isCodexOAuthAccount(account) {
		return codexDeviceProfile{}
	}
	// Keep client-provided context authoritative, including unmarked context
	// from custom Codex providers. Do not put a conflicting account default
	// ahead of it, or interpret tool results as environment declarations.
	texts := []string{firstNonEmptyString(reqBody["instructions"])}
	if text, ok := reqBody["input"].(string); ok {
		texts = append(texts, text)
	}
	input, _ := reqBody["input"].([]any)
	for _, raw := range input {
		item, ok := decodeCodexSynthItem(raw)
		if !ok || (item["role"] != "developer" && item["role"] != "system" && item["role"] != "user") {
			continue
		}
		if kind, _ := item["type"].(string); kind != "" && kind != "message" {
			continue
		}
		meta, _ := item["internal_chat_message_metadata_passthrough"].(map[string]any)
		kinds, _ := meta["content_item_kinds"].([]any)
		for _, kind := range kinds {
			if kind == "permissions.instructions" || kind == "environments.environment_context" {
				return codexDeviceProfile{}
			}
		}
		texts = append(texts, extractTextFromContent(item["content"]))
	}
	for _, text := range texts {
		if strings.Contains(text, "<environment_context") || strings.Contains(text, "<permissions instructions>") {
			return codexDeviceProfile{}
		}
	}
	var environment codexDeviceProfile
	if cwd := strings.TrimSpace(account.GetExtraString(codexWorkspaceCWDExtraKey)); strings.HasPrefix(cwd, "/") && !strings.Contains(cwd, "..") && filepath.Clean(cwd) == cwd {
		environment.WorkspaceCWD = cwd
	}
	switch mode := strings.TrimSpace(account.GetExtraString(codexSandboxModeExtraKey)); mode {
	case "read-only", "workspace-write", "danger-full-access":
		environment.SandboxMode = mode
	}
	if timezone := strings.TrimSpace(account.GetExtraString(codexTimezoneExtraKey)); timezone != "" {
		if _, err := time.LoadLocation(timezone); err == nil {
			environment.Timezone = timezone
		}
	}
	return environment
}

func codexSynthesisPermissionsText(environment codexDeviceProfile) string {
	switch environment.SandboxMode {
	case "read-only", "workspace-write", "danger-full-access":
		// A sandbox mode alone does not specify shell, network, approval policy,
		// or writable roots. Leave those to the downstream's own declarations.
		return "<permissions instructions>\nThe downstream filesystem sandbox mode is `" + environment.SandboxMode + "`.\n</permissions instructions>"
	default:
		return ""
	}
}

func codexSynthesisEnvironmentText(environment codexDeviceProfile, now time.Time) string {
	var fields []string
	if cwd := strings.TrimSpace(environment.WorkspaceCWD); cwd != "" {
		fields = append(fields, "  <cwd>"+html.EscapeString(cwd)+"</cwd>")
	}
	if timezone := strings.TrimSpace(environment.Timezone); timezone != "" {
		if location, err := time.LoadLocation(timezone); err == nil {
			fields = append(fields, "  <current_date>"+now.In(location).Format("2006-01-02")+"</current_date>", "  <timezone>"+html.EscapeString(timezone)+"</timezone>")
		}
	}
	if len(fields) == 0 {
		return ""
	}
	return "<environment_context>\n" + strings.Join(fields, "\n") + "\n</environment_context>"
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
	_, _ = buf.WriteString(`{"type":"message","id":`)
	writeCodexJSONString(&buf, id)
	_, _ = buf.WriteString(`,"role":`)
	writeCodexJSONString(&buf, role)
	_, _ = buf.WriteString(`,"content":[`)
	for i, text := range texts {
		if i > 0 {
			_ = buf.WriteByte(',')
		}
		_, _ = buf.WriteString(`{"type":"input_text","text":`)
		writeCodexJSONString(&buf, text)
		_ = buf.WriteByte('}')
	}
	_, _ = buf.WriteString(`],"internal_chat_message_metadata_passthrough":{`)
	first := true
	if turnID != "" {
		_, _ = buf.WriteString(`"turn_id":`)
		writeCodexJSONString(&buf, turnID)
		first = false
	}
	if createTime > 0 {
		if !first {
			_ = buf.WriteByte(',')
		}
		_, _ = buf.WriteString(`"create_time":`)
		_, _ = buf.WriteString(strconv.FormatFloat(createTime, 'f', -1, 64))
		first = false
	}
	if len(kinds) > 0 {
		if !first {
			_ = buf.WriteByte(',')
		}
		_, _ = buf.WriteString(`"content_item_kinds":[`)
		for i, kind := range kinds {
			if i > 0 {
				_ = buf.WriteByte(',')
			}
			writeCodexJSONString(&buf, kind)
		}
		_ = buf.WriteByte(']')
	}
	_, _ = buf.WriteString(`}}`)
	return json.RawMessage(escapeCodexNonASCII(buf.Bytes()))
}

// codexSynthesisPrepareTools preserves downstream tool contracts. Official
// names require both argument and result adapters, not just a reverse name
// map. Reserved-name escaping remains in the OAuth transform, which restores
// those names on responses without changing their schemas or execution model.
func codexSynthesisPrepareTools(
	reqBody map[string]any,
	descriptor openai.CodexCatalogModel,
	hasDescriptor bool,
	capability codexCapability,
	responsesLite bool,
) map[string]string {
	if capability == codexCapabilityGeneric && !codexSynthesisToolChoiceNone(reqBody) &&
		!codexSynthesisHasToolType(reqBody, "web_search") && codexDescriptorSupportsWebSearch(descriptor, hasDescriptor) {
		// Hosted search executes upstream and needs no downstream executor.
		codexSynthesisAppendTools(reqBody, []any{map[string]any{"type": "web_search"}}, responsesLite)
	}
	return nil
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
