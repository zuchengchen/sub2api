package service

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Tibo probe answer classes. Logs and tests keep the historical labels.
const (
	tiboAnswerTrue  = "True"
	tiboAnswerFalse = "False"
	tiboAnswerNone  = "none"
	tiboAnswerOther = "other"
)

// The probe asks for exactly True or False, but models decorate the verdict:
// "true", "True.", "**True**", "`True`", "Answer: True", "是的". Only formatting
// is forgiven; the verdict itself must be unambiguous.
var (
	tiboTrueWords  = map[string]bool{"true": true, "yes": true, "y": true, "t": true, "真": true, "对": true, "是": true, "是的": true, "正确": true, "对的": true}
	tiboFalseWords = map[string]bool{"false": true, "no": true, "n": true, "f": true, "假": true, "错": true, "否": true, "不是": true, "不对": true, "错误": true, "不知道": true}
	tiboTrueMarks  = map[string]bool{"✅": true, "✔": true, "☑": true, "✓": true, "👍": true}
	tiboFalseMarks = map[string]bool{"❌": true, "✖": true, "✗": true, "✘": true, "👎": true}

	tiboAnswerLabel = regexp.MustCompile(`^(?:(?:the\s+)?(?:final\s+)?(?:answer|result|output|verdict|response)(?:\s+is)?|最终答案|最终结果|答案|结果|回答|答)\s*["']?\s*(?:[:：=]|是|为)?\s*`)
	tiboAnswerTag   = regexp.MustCompile(`</?(?:b|strong|i|em|p|code|span|answer)\b[^>]*>`)
)

func classifyTiboAnswer(answer string) string {
	text := strings.ToLower(strings.TrimSpace(norm.NFKC.String(answer)))
	text = strings.ReplaceAll(text, "\ufe0f", "") // emoji variation selector
	if text == "" {
		return tiboAnswerNone
	}
	text = tiboAnswerTag.ReplaceAllString(text, " ")
	if verdict := tiboMarkVerdict(text); verdict != "" {
		return verdict
	}
	core := tiboAnswerCore(text)
	if core == "" {
		return tiboAnswerOther
	}
	if verdict := tiboWordVerdict(core); verdict != "" {
		return verdict
	}
	// "True, Tibo leads Codex." / "是的，我知道": the verdict leads the answer.
	// Single letters only count alone: "T. Sottiaux ..." is not a verdict.
	if lead, rest := tiboLeadingWord(core); lead != "" && (len([]rune(lead)) > 1 || unicode.Is(unicode.Han, []rune(lead)[0])) {
		if verdict := tiboWordVerdict(lead); verdict != "" && tiboStartsWithSeparator(rest) && !tiboContainsOpposite(rest, verdict) {
			return verdict
		}
	}
	// "I know him.\nTrue": a trailing segment that is only the verdict.
	segments := strings.FieldsFunc(text, func(r rune) bool { return strings.ContainsRune("\n.。!！;；", r) })
	for i := len(segments) - 1; i >= 0; i-- {
		segment := tiboAnswerCore(segments[i])
		if segment == "" {
			continue
		}
		verdict := tiboWordVerdict(segment)
		if verdict == "" {
			verdict = tiboMarkVerdict(strings.TrimSpace(segments[i]))
		}
		if verdict != "" && !tiboContainsOpposite(strings.Join(segments[:i], " "), verdict) {
			return verdict
		}
		break
	}
	return tiboAnswerOther
}

// tiboAnswerCore removes wrapping punctuation, markdown, quotes and a leading
// "answer:" label, leaving the words the model actually answered with.
func tiboAnswerCore(text string) string {
	trim := func(s string) string {
		return strings.TrimFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
	}
	core := trim(text)
	for range 2 {
		stripped := trim(tiboAnswerLabel.ReplaceAllString(core, ""))
		if stripped == core {
			break
		}
		core = stripped
	}
	return core
}

func tiboWordVerdict(word string) string {
	switch {
	case tiboTrueWords[word]:
		return tiboAnswerTrue
	case tiboFalseWords[word]:
		return tiboAnswerFalse
	}
	return ""
}

func tiboMarkVerdict(text string) string {
	switch {
	case tiboTrueMarks[text]:
		return tiboAnswerTrue
	case tiboFalseMarks[text]:
		return tiboAnswerFalse
	}
	return ""
}

// tiboLeadingWord splits the first run of Han characters or of Latin letters.
func tiboLeadingWord(core string) (string, string) {
	runes := []rune(core)
	if len(runes) == 0 {
		return "", ""
	}
	han := unicode.Is(unicode.Han, runes[0])
	end := 0
	for end < len(runes) {
		r := runes[end]
		if han != unicode.Is(unicode.Han, r) || (!han && !unicode.IsLetter(r)) {
			break
		}
		end++
	}
	return string(runes[:end]), string(runes[end:])
}

func tiboStartsWithSeparator(rest string) bool {
	for _, r := range rest {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	}
	return true
}

func tiboContainsOpposite(text, verdict string) bool {
	opposite := "false"
	if verdict == tiboAnswerFalse {
		opposite = "true"
	}
	for _, field := range strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) }) {
		if field == opposite {
			return true
		}
	}
	return false
}
