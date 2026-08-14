package service

import (
	"strings"
	"unicode"
)

const powerShellEncodedCommand = "powershell -encodedcommand"

var documentationCommandPlaceholders = [...]string{
	"<base64>",
	"<payload>",
	"[base64]",
	"[payload]",
	"...",
	"\u2026",
}

// suppressToolDocumentationKeyword recognizes a non-executable command example
// in a tool-returned Markdown file. It deliberately accepts only explicit
// placeholders; real encoded payloads and user-authored messages remain blocked.
func suppressToolDocumentationKeyword(fragment ContentModerationFragment, keyword string) bool {
	if fragment.Role != "tool" || fragment.Kind != "text" || !isMarkdownFileView(fragment.Text) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(keyword)) {
	case "powershell -enc", powerShellEncodedCommand:
		return allPowerShellEncodedCommandsUsePlaceholders(fragment.Text)
	default:
		return false
	}
}

func isMarkdownFileView(text string) bool {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(strings.ToLower(text), "<file-view ") {
		return false
	}
	end := strings.IndexByte(text, '>')
	if end < 0 || end > 2048 {
		return false
	}
	startTag := text[:end]
	path, ok := quotedAttribute(startTag, "path")
	if !ok {
		return false
	}
	path = strings.ToLower(strings.TrimSpace(path))
	return strings.HasSuffix(path, ".md") || strings.HasSuffix(path, ".markdown")
}

func quotedAttribute(tag, name string) (string, bool) {
	lower := strings.ToLower(tag)
	marker := strings.ToLower(name) + "="
	start := strings.Index(lower, marker)
	if start < 0 {
		return "", false
	}
	start += len(marker)
	if start >= len(tag) || (tag[start] != '\'' && tag[start] != '"') {
		return "", false
	}
	quote := tag[start]
	start++
	end := strings.IndexByte(tag[start:], quote)
	if end < 0 {
		return "", false
	}
	return tag[start : start+end], true
}

func allPowerShellEncodedCommandsUsePlaceholders(text string) bool {
	lower := strings.ToLower(text)
	found := false
	for offset := 0; offset < len(lower); {
		index := strings.Index(lower[offset:], "powershell -enc")
		if index < 0 {
			break
		}
		index += offset
		found = true
		if !strings.HasPrefix(lower[index:], powerShellEncodedCommand) {
			return false
		}
		remainder := lower[index+len(powerShellEncodedCommand):]
		trimmed := strings.TrimLeftFunc(remainder, unicode.IsSpace)
		if len(trimmed) == len(remainder) {
			return false
		}
		if !hasDocumentationCommandPlaceholder(trimmed) {
			return false
		}
		offset = index + len(powerShellEncodedCommand)
	}
	return found
}

func hasDocumentationCommandPlaceholder(text string) bool {
	for _, placeholder := range documentationCommandPlaceholders {
		if strings.HasPrefix(text, placeholder) {
			return true
		}
	}
	return false
}
