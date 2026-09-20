package service

import (
	"errors"
	"regexp"
	"strings"
)

const intelligentPelicanHTMLLimit = 256 << 10

var (
	intelligentBannedHTMLBlocks = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</\s*script\s*>`),
		regexp.MustCompile(`(?is)<iframe\b[^>]*>.*?</\s*iframe\s*>`),
		regexp.MustCompile(`(?is)<object\b[^>]*>.*?</\s*object\s*>`),
		regexp.MustCompile(`(?is)<embed\b[^>]*>.*?</\s*embed\s*>`),
		regexp.MustCompile(`(?is)<applet\b[^>]*>.*?</\s*applet\s*>`),
		regexp.MustCompile(`(?is)<form\b[^>]*>.*?</\s*form\s*>`),
	}
	intelligentBannedHTMLEmpty = regexp.MustCompile(`(?is)<(script|iframe|object|embed|applet|form|input|button|textarea|select|link|base)\b[^>]*/?>`)
	intelligentHTMLEventAttr   = regexp.MustCompile(`(?i)\s+on[a-z][a-z0-9_-]*\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	intelligentHTMLJSURL       = regexp.MustCompile(`(?i)javascript\s*:`)
)

func ExtractIntelligentTestHTMLDocument(output string) string {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return ""
	}
	lower := strings.ToLower(trimmed)
	if i := strings.Index(lower, "<html"); i >= 0 {
		end := strings.LastIndex(lower, "</html>")
		if end > i {
			return strings.TrimSpace(trimmed[i : end+len("</html>")])
		}
		return strings.TrimSpace(trimmed[i:])
	}
	if i := strings.Index(trimmed, "<svg"); i >= 0 {
		end := strings.LastIndex(trimmed, "</svg>")
		if end > i {
			return wrapIntelligentTestHTML(trimmed[i : end+len("</svg>")])
		}
	}
	return ""
}

func wrapIntelligentTestHTML(inner string) string {
	return `<!DOCTYPE html><html><head><meta charset="utf-8"><style>html,body{margin:0;height:100%;background:#0b1220;display:flex;align-items:center;justify-content:center;}svg{max-width:100%;max-height:100%;}</style></head><body>` + inner + `</body></html>`
}

func SanitizeIntelligentTestHTML(output string) (string, error) {
	doc := ExtractIntelligentTestHTMLDocument(output)
	if doc == "" {
		return "", errors.New("未返回 HTML 或 SVG")
	}
	if len(doc) > intelligentPelicanHTMLLimit {
		return "", errors.New("HTML 超过 256 KiB 上限")
	}
	for _, re := range intelligentBannedHTMLBlocks {
		doc = re.ReplaceAllString(doc, "")
	}
	doc = intelligentBannedHTMLEmpty.ReplaceAllString(doc, "")
	doc = intelligentHTMLEventAttr.ReplaceAllString(doc, "")
	doc = intelligentHTMLJSURL.ReplaceAllString(doc, "")
	if !strings.Contains(strings.ToLower(doc), "<svg") {
		return "", errors.New("HTML 中没有 SVG")
	}
	return doc, nil
}
