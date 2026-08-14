package service

import (
	"strings"
	"unicode"
)

const (
	powerShellEncodedCommand                      = "powershell -encodedcommand"
	powerShellShortEncodedCommand                 = "powershell -enc"
	maxDocumentationCommandPlaceholderBytes       = 128
	contentModerationKeywordContextPolicyRevision = "powershell-doc-v2"
)

var documentationCommandPlaceholders = [...]string{
	"<base64>",
	"<payload>",
	"[base64]",
	"[payload]",
	"...",
	"\u2026",
}

// suppressToolDocumentationKeyword recognizes a non-executable command example
// in a tool-returned Markdown file. Real encoded payloads and user-authored
// messages remain blocked.
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
		index := strings.Index(lower[offset:], powerShellShortEncodedCommand)
		if index < 0 {
			break
		}
		index += offset
		found = true
		commandBytes := powerShellEncodedCommandBytes(lower[index:])
		if commandBytes == 0 {
			return false
		}
		remainder, separated := trimHorizontalCommandSpace(lower[index+commandBytes:])
		if !separated {
			return false
		}
		if !hasDocumentationCommandPlaceholder(remainder) {
			return false
		}
		offset = index + commandBytes
	}
	return found
}

func withoutPowerShellDocumentationCommands(text string) string {
	lower := strings.ToLower(text)
	var filtered strings.Builder
	filtered.Grow(len(text))
	for offset := 0; offset < len(text); {
		index := strings.Index(lower[offset:], powerShellShortEncodedCommand)
		if index < 0 {
			_, _ = filtered.WriteString(text[offset:])
			break
		}
		index += offset
		_, _ = filtered.WriteString(text[offset:index])
		commandBytes := powerShellEncodedCommandBytes(lower[index:])
		if commandBytes == 0 {
			_ = filtered.WriteByte(text[index])
			offset = index + 1
			continue
		}
		_, _ = filtered.WriteString(strings.Repeat(" ", commandBytes))
		offset = index + commandBytes
	}
	return filtered.String()
}

func powerShellEncodedCommandBytes(text string) int {
	for _, command := range [...]string{powerShellEncodedCommand, powerShellShortEncodedCommand} {
		if !strings.HasPrefix(text, command) {
			continue
		}
		if len(text) == len(command) || text[len(command)] == ' ' || text[len(command)] == '\t' {
			return len(command)
		}
	}
	return 0
}

func trimHorizontalCommandSpace(text string) (string, bool) {
	index := 0
	for index < len(text) && (text[index] == ' ' || text[index] == '\t') {
		index++
	}
	return text[index:], index > 0
}

func hasDocumentationCommandPlaceholder(text string) bool {
	for _, placeholder := range documentationCommandPlaceholders {
		if strings.HasPrefix(text, placeholder) {
			return true
		}
	}
	return hasBoundedDocumentationCommandPlaceholder(text, '<', '>') ||
		hasBoundedDocumentationCommandPlaceholder(text, '[', ']')
}

func hasBoundedDocumentationCommandPlaceholder(text string, open, close byte) bool {
	if len(text) < 3 || text[0] != open {
		return false
	}
	end := strings.IndexByte(text[1:], close)
	if end < 0 {
		return false
	}
	end++
	if end+1 > maxDocumentationCommandPlaceholderBytes {
		return false
	}
	if lineBreak := strings.IndexAny(text[:end+1], "\r\n"); lineBreak >= 0 {
		return false
	}
	content := strings.TrimSpace(text[1:end])
	if content == "" {
		return false
	}
	for _, field := range strings.FieldsFunc(strings.ToLower(content), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if field == "base64" || field == "payload" {
			return true
		}
	}
	return false
}
