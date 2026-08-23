package service

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/tidwall/gjson"
)

// ContentModerationScopeSnapshot is captured once from the API key's group at
// request entry. Account failover and fallback-group routing must not change
// the group used for configured scope checks. Group names are metadata only;
// moderation eligibility is controlled by the persisted group/model filters.
type ContentModerationScopeSnapshot struct {
	GroupID   *int64 `json:"group_id,omitempty"`
	GroupName string `json:"group_name"`
	InScope   bool   `json:"in_scope"`
}

func NewContentModerationScopeSnapshot(groupID *int64, groupName string) ContentModerationScopeSnapshot {
	return ContentModerationScopeSnapshot{
		GroupID:   cloneInt64Ptr(groupID),
		GroupName: groupName,
		InScope:   true,
	}
}

type ContentModerationFragment struct {
	Role          string `json:"role"`
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	ContextClass  string `json:"context_class"`
	Text          string `json:"text,omitempty"`
	Hash          string `json:"hash"`
	leadingSpace  bool
	trailingSpace bool
}

const (
	contentModerationFragmentHashDomain = "sub2api/content-moderation/fragment/v3\x00"
	contentModerationLineageHashDomain  = "sub2api/content-moderation/lineage/v1\x00"
)

func newContentModerationFragment(role, kind, path, text string) (ContentModerationFragment, bool) {
	rawText := text
	text = strings.TrimSpace(text)
	if text == "" {
		return ContentModerationFragment{}, false
	}
	role = strings.ToLower(strings.TrimSpace(role))
	kind = strings.ToLower(strings.TrimSpace(kind))
	path = strings.TrimSpace(path)
	fragment := ContentModerationFragment{
		Role:          role,
		Kind:          kind,
		Path:          path,
		Text:          text,
		leadingSpace:  len(strings.TrimLeftFunc(rawText, unicode.IsSpace)) != len(rawText),
		trailingSpace: len(strings.TrimRightFunc(rawText, unicode.IsSpace)) != len(rawText),
	}
	fragment.ContextClass = classifyContentModerationContext(fragment)
	updateContentModerationFragmentHash(&fragment)
	return fragment, true
}

func updateContentModerationFragmentHash(fragment *ContentModerationFragment) {
	if fragment == nil {
		return
	}
	digest := sha256.Sum256([]byte(contentModerationFragmentHashDomain + fragment.Role + "\x00" + fragment.Kind + "\x00" + fragment.ContextClass + "\x00" + fragment.Path + "\x00" + fragment.Text))
	fragment.Hash = hex.EncodeToString(digest[:])
}

type contentModerationLineageMessageUnit struct {
	root  string
	index int
	key   string
	ok    bool
}

func contentModerationLineageMessageUnitForFragment(fragment ContentModerationFragment) contentModerationLineageMessageUnit {
	parts := strings.Split(strings.TrimSpace(fragment.Path), ".")
	if len(parts) >= 2 {
		root := strings.ToLower(parts[0])
		if root == "messages" || root == "input" || root == "contents" {
			index, err := strconv.Atoi(parts[1])
			if err == nil && index >= 0 {
				return contentModerationLineageMessageUnit{root: root, index: index, key: root + "." + parts[1], ok: true}
			}
		}
	}
	return contentModerationLineageMessageUnit{root: "top", key: "top", ok: true}
}

func contentModerationLineageUnitHashes(input ContentModerationCheckInput, fragments []ContentModerationFragment) ([]string, map[string]ContentModerationFragment) {
	principal := ""
	switch {
	case input.UserID > 0:
		principal = "user:" + strconv.FormatInt(input.UserID, 10)
	case input.APIKeyID > 0:
		principal = "api_key:" + strconv.FormatInt(input.APIKeyID, 10)
	default:
		return nil, nil
	}
	type lineageUnit struct {
		key       string
		fragment  ContentModerationFragment
		fragments []ContentModerationFragment
	}
	units := make([]lineageUnit, 0, len(fragments))
	unitIndexes := make(map[string]int, len(fragments))
	for _, fragment := range fragments {
		unit := contentModerationLineageMessageUnitForFragment(fragment)
		unitKey := unit.key + "\x00" + fragment.Role
		index, found := unitIndexes[unitKey]
		if !found {
			index = len(units)
			unitIndexes[unitKey] = index
			units = append(units, lineageUnit{key: unitKey, fragment: fragment})
		}
		units[index].fragments = append(units[index].fragments, fragment)
	}
	hashes := make([]string, 0, len(units))
	representatives := make(map[string]ContentModerationFragment, len(units))
	for _, unit := range units {
		hasher := sha256.New()
		writeField := func(value string) {
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			_, _ = hasher.Write(size[:])
			_, _ = hasher.Write([]byte(value))
		}
		writeField(contentModerationLineageHashDomain)
		writeField(principal)
		for _, fragment := range unit.fragments {
			writeField(fragment.Role)
			writeField(fragment.Kind)
			writeField(fragment.ContextClass)
			writeField(fragment.Text)
			writeField(strconv.FormatBool(fragment.leadingSpace))
			writeField(strconv.FormatBool(fragment.trailingSpace))
		}
		hash := hex.EncodeToString(hasher.Sum(nil))
		hashes = append(hashes, hash)
		representatives[hash] = unit.fragment
	}
	return hashes, representatives
}

func contentModerationLineageUnitForFragment(input ContentModerationCheckInput, source ContentModerationFragment, fragments []ContentModerationFragment) (string, ContentModerationFragment) {
	sourceUnit := contentModerationLineageMessageUnitForFragment(source)
	unitFragments := make([]ContentModerationFragment, 0, len(fragments))
	for _, fragment := range fragments {
		if contentModerationLineageMessageUnitForFragment(fragment).key == sourceUnit.key && fragment.Role == source.Role {
			unitFragments = append(unitFragments, fragment)
		}
	}
	if len(unitFragments) == 0 {
		unitFragments = append(unitFragments, source)
	}
	hashes, representatives := contentModerationLineageUnitHashes(input, unitFragments)
	if len(hashes) == 0 {
		return "", ContentModerationFragment{}
	}
	return hashes[0], representatives[hashes[0]]
}

// ExtractContentModerationFragments extracts every directly parseable,
// client-controlled text, filename, and URL without OCR, transcription, or
// document decoding. It deliberately retains message boundaries for per-
// fragment cache decisions.
func ExtractContentModerationFragments(protocol string, body []byte) []ContentModerationFragment {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	fragments := make([]ContentModerationFragment, 0, 16)
	appendValue := func(value gjson.Result, role, path string) {
		collectUnifiedModerationValue(value, role, path, "text", &fragments)
	}

	for _, field := range []struct {
		name string
		role string
	}{
		{"system", "system"},
		{"system_instruction", "system"},
		{"systemInstruction", "system"},
		{"developer", "developer"},
		{"instructions", "developer"},
		{"prompt", "user"},
		{"negative_prompt", "user"},
	} {
		if value := root.Get(field.name); value.Exists() {
			appendValue(value, field.role, field.name)
		}
	}

	collectUnifiedModerationMessages(root.Get("messages"), "messages", &fragments)
	collectUnifiedModerationResponsesInput(root.Get("input"), "input", &fragments)
	collectUnifiedModerationGeminiContents(root.Get("contents"), "contents", &fragments)

	for _, field := range []string{"tools", "tool_choice", "files", "attachments", "images", "image_url", "url"} {
		if value := root.Get(field); value.Exists() {
			collectUnifiedModerationValue(value, "tool", field, fieldKind(field), &fragments)
		}
	}

	return fragments
}

// SelectContentModerationReviewFragments keeps every user-authored turn plus
// the tool calls/results that belong to the latest one.
func SelectContentModerationReviewFragments(fragments []ContentModerationFragment) []ContentModerationFragment {
	current, _ := partitionContentModerationReviewFragments(fragments)
	return current
}

// partitionContentModerationReviewFragments splits client-controlled fragments
// into the reviewable set and the lineage-only set. Every user-authored text
// turn is reviewable regardless of position: a single request can smuggle an
// attack script as an earlier message and end on a benign final turn, so only
// tool results follow the latest-turn rule. system/developer/assistant text is
// dropped from direct review; history keeps lineage replay coverage and the
// hard-pattern history scan.
func partitionContentModerationReviewFragments(fragments []ContentModerationFragment) ([]ContentModerationFragment, []ContentModerationFragment) {
	latestUser := contentModerationLineageMessageUnit{}
	hasUser := false
	for _, fragment := range fragments {
		if fragment.Role != "user" || fragment.Kind == "url" {
			continue
		}
		latestUser = contentModerationLineageMessageUnitForFragment(fragment)
		hasUser = true
	}

	current := make([]ContentModerationFragment, 0, len(fragments))
	history := make([]ContentModerationFragment, 0, len(fragments))
	for _, fragment := range fragments {
		if fragment.Kind == "url" || fragment.Role == "system" || fragment.Role == "developer" || fragment.Role == "assistant" {
			continue
		}
		unit := contentModerationLineageMessageUnitForFragment(fragment)
		switch fragment.Role {
		case "user":
			current = append(current, fragment)
		case "tool":
			root := strings.ToLower(strings.SplitN(strings.TrimSpace(fragment.Path), ".", 2)[0])
			if root == "tools" || root == "tool_choice" {
				continue
			}
			if !hasUser || (unit.root == latestUser.root && unit.index >= latestUser.index) || latestUser.root == "top" {
				current = append(current, fragment)
			} else {
				history = append(history, fragment)
			}
		}
	}
	return current, history
}

func collectUnifiedModerationMessages(messages gjson.Result, path string, out *[]ContentModerationFragment) {
	if !messages.IsArray() {
		return
	}
	index := 0
	messages.ForEach(func(_, message gjson.Result) bool {
		role := normalizeModerationRole(message.Get("role").String())
		if role == "" {
			role = "user"
		}
		messagePath := path + "." + itoa(index)
		if content := message.Get("content"); content.Exists() {
			collectUnifiedModerationValue(content, role, messagePath+".content", "text", out)
		}
		for _, field := range []string{"tool_calls", "function_call", "function_calls"} {
			if value := message.Get(field); value.Exists() {
				collectUnifiedModerationValue(value, "tool", messagePath+"."+field, "tool", out)
			}
		}
		for _, field := range []string{"text", "name", "filename", "file_name", "url", "uri"} {
			if value := message.Get(field); value.Exists() {
				collectUnifiedModerationValue(value, role, messagePath+"."+field, fieldKind(field), out)
			}
		}
		index++
		return true
	})
}

func collectUnifiedModerationResponsesInput(input gjson.Result, path string, out *[]ContentModerationFragment) {
	switch {
	case !input.Exists():
		return
	case input.Type == gjson.String:
		appendUnifiedModerationFragment(out, "user", "text", path, input.String())
	case input.IsArray():
		index := 0
		input.ForEach(func(_, item gjson.Result) bool {
			role := normalizeModerationRole(item.Get("role").String())
			if role == "" {
				role = roleForContentType(item.Get("type").String())
			}
			collectUnifiedModerationValue(item, role, path+"."+itoa(index), "text", out)
			index++
			return true
		})
	case input.IsObject():
		role := normalizeModerationRole(input.Get("role").String())
		if role == "" {
			role = roleForContentType(input.Get("type").String())
		}
		collectUnifiedModerationValue(input, role, path, "text", out)
	}
}

func collectUnifiedModerationGeminiContents(contents gjson.Result, path string, out *[]ContentModerationFragment) {
	if !contents.IsArray() {
		return
	}
	index := 0
	contents.ForEach(func(_, content gjson.Result) bool {
		role := normalizeModerationRole(content.Get("role").String())
		if role == "" {
			role = "user"
		}
		collectUnifiedModerationValue(content.Get("parts"), role, path+"."+itoa(index)+".parts", "text", out)
		index++
		return true
	})
}

func collectUnifiedModerationValue(value gjson.Result, role, path, kind string, out *[]ContentModerationFragment) {
	switch {
	case !value.Exists() || value.Type == gjson.Null:
		return
	case value.Type == gjson.String:
		appendUnifiedModerationFragment(out, role, kind, path, value.String())
	case value.IsArray():
		index := 0
		value.ForEach(func(_, item gjson.Result) bool {
			collectUnifiedModerationValue(item, role, path+"."+itoa(index), kind, out)
			index++
			return true
		})
	case value.IsObject():
		typeName := strings.ToLower(strings.TrimSpace(value.Get("type").String()))
		objectRole := role
		if objectRole == "" {
			objectRole = roleForContentType(typeName)
		}
		for _, field := range []string{"text", "prompt", "instructions", "description", "filename", "file_name", "fileName", "url", "uri", "file_url", "fileUrl", "file_uri", "fileUri"} {
			if child := value.Get(field); child.Exists() {
				collectUnifiedModerationValue(child, objectRole, path+"."+field, fieldKind(field), out)
			}
		}
		if objectRole == "tool" || typeName == "tool" || typeName == "function" || strings.Contains(typeName, "tool_") || strings.Contains(typeName, "function_") {
			if child := value.Get("name"); child.Exists() {
				collectUnifiedModerationValue(child, "tool", path+".name", "tool", out)
			}
		}
		for _, field := range []string{"content", "parts", "input", "output", "arguments", "function", "source", "image_url", "file", "files", "attachments", "file_data", "fileData"} {
			if child := value.Get(field); child.Exists() {
				childKind := kind
				if strings.Contains(strings.ToLower(field), "url") || strings.Contains(strings.ToLower(field), "uri") {
					childKind = "url"
				} else if field == "file" || field == "files" || field == "attachments" || field == "file_data" || field == "fileData" {
					childKind = "file"
				}
				collectUnifiedModerationValue(child, objectRole, path+"."+field, childKind, out)
			}
		}
	}
}

func appendUnifiedModerationFragment(out *[]ContentModerationFragment, role, kind, path, text string) {
	if isInlineBase64MediaURL(text) {
		return
	}
	fragment, ok := newContentModerationFragment(role, kind, path, text)
	if !ok {
		return
	}
	*out = append(*out, fragment)
}

func isInlineBase64MediaURL(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) < len("data:x;base64,") || !strings.HasPrefix(strings.ToLower(value), "data:") {
		return false
	}
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return false
	}
	header := strings.ToLower(value[len("data:"):comma])
	if !strings.Contains(header, ";base64") {
		return false
	}
	mediaType, _, _ := strings.Cut(header, ";")
	return strings.HasPrefix(mediaType, "image/") ||
		strings.HasPrefix(mediaType, "audio/") ||
		strings.HasPrefix(mediaType, "video/")
}

func normalizeModerationRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return "user"
	case "system":
		return "system"
	case "developer":
		return "developer"
	case "assistant", "model":
		return "assistant"
	case "tool", "function":
		return "tool"
	default:
		return ""
	}
}

func roleForContentType(contentType string) string {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	if strings.Contains(contentType, "tool") || strings.Contains(contentType, "function") {
		return "tool"
	}
	return "user"
}

func fieldKind(field string) string {
	field = strings.ToLower(strings.TrimSpace(field))
	switch {
	case strings.Contains(field, "url") || strings.Contains(field, "uri"):
		return "url"
	case strings.Contains(field, "file") || field == "attachments":
		return "file"
	case field == "name" || field == "tools" || field == "tool_choice":
		return "tool"
	default:
		return "text"
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}

// ContentModerationRawRequest is a reference snapshot of application-visible
// request data. Body is intentionally not copied; the gateway owns it for the
// reservation lifetime.
type ContentModerationRawRequest struct {
	Method    string      `json:"method"`
	Target    string      `json:"target"`
	Headers   http.Header `json:"headers"`
	Body      []byte      `json:"-"`
	Transport string      `json:"transport"`
	Stage     string      `json:"stage"`
}

func (r ContentModerationRawRequest) CloneMetadata() ContentModerationRawRequest {
	clone := r
	clone.Headers = r.Headers.Clone()
	return clone
}
