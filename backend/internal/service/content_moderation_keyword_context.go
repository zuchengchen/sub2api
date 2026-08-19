package service

import (
	"strings"
	"unicode"
)

const (
	powerShellEncodedCommand                      = "powershell -encodedcommand"
	powerShellShortEncodedCommand                 = "powershell -enc"
	maxDocumentationCommandPlaceholderBytes       = 128
	contentModerationKeywordContextPolicyRevision = "keyword-context-v5"
	maliciousMacroContextWindowRunes              = 192
	maliciousMacroIntentDistanceRunes             = 32
)

type contentModerationKeywordContextDisposition uint8

const (
	contentModerationKeywordContextUnspecified contentModerationKeywordContextDisposition = iota
	contentModerationKeywordContextHardBlock
	contentModerationKeywordContextAllow
	contentModerationKeywordContextReview
)

var (
	policyRestrictionChineseContextSignals = [...]string{
		"安全测试", "输入校验", "检测规则", "测试用例",
	}
	policyRestrictionEnglishContextSignals = [...]string{
		"security test", "security tests", "security testing", "input validation",
		"detection rule", "detection rules", "test case", "test cases",
	}
	maliciousMacroContextKeywords = map[string]struct{}{
		"恶意宏":              {},
		"malicious macro":  {},
		"malicious macros": {},
	}
	maliciousMacroChineseConstructionVerbs = [...]string{
		"制作", "制造", "编写", "生成", "创建", "构造", "开发", "植入", "注入", "投递", "部署", "传播",
	}
	maliciousMacroEnglishConstructionVerbs = [...]string{
		"build", "create", "write", "generate", "craft", "develop", "inject", "embed", "deliver", "deploy", "spread",
	}
	maliciousMacroChineseDefensiveSignals = [...]string{
		"检测", "扫描", "查杀", "防范", "防止", "阻止", "避免", "禁止", "拦截", "抵御", "阻断", "识别", "审计", "隔离", "清除", "移除", "防护", "防御", "告警", "特征库",
	}
	maliciousMacroEnglishDefensiveSignals = [...]string{
		"detect", "detected", "detecting", "detection", "detector", "scan", "scanned", "scanning", "scanner", "block", "blocked", "blocking", "prevent", "prevented", "preventing", "prevention", "avoid", "avoided", "avoiding", "protect", "protected", "protecting", "protection", "defend", "defended", "defending", "defense", "audit", "audited", "auditing", "quarantine", "quarantined", "quarantining", "remove", "removed", "removing", "remediate", "remediated", "remediating", "remediation", "indicator", "signature", "alert", "sanitize", "sanitized", "sanitizing", "sanitization",
	}
	maliciousMacroChineseBehaviorSignals = [...]string{
		"自动执行", "静默执行", "自启动", "窃取", "盗取", "外传", "投递", "传播", "感染", "回连", "下载并执行", "绕过检测", "绕过查杀", "逃避检测", "逃避查杀", "规避检测", "规避查杀", "免杀",
	}
	maliciousMacroEnglishBehaviorSignals = [...]string{
		"execute automatically", "executing automatically", "run automatically", "running automatically", "silent execution", "steal", "exfiltrate", "exfiltrates", "exfiltrating", "exfiltration", "deliver", "spread", "infect", "call back", "connect back", "download and execute", "bypass detection", "bypass antivirus", "evade detection", "evade antivirus", "undetectable",
	}
	maliciousMacroChineseEvasionSignals = [...]string{
		"绕过", "规避", "逃避", "免杀",
	}
	maliciousMacroEnglishEvasionSignals = [...]string{
		"bypass", "evade", "circumvent", "undetectable",
	}
	maliciousMacroChineseProtectiveGovernors = [...]string{
		"防止", "阻止", "避免", "禁止", "拦截", "防范", "抵御", "阻断",
	}
	maliciousMacroEnglishProtectiveGovernors = [...]string{
		"prevent", "stop", "block", "avoid", "protect against", "defend against",
	}
	maliciousMacroChineseNegatedGovernors = [...]string{
		"不要防止", "不要阻止", "不要避免", "不要禁止", "不要拦截", "不要阻断",
		"无需防止", "无需阻止", "无需避免", "无需禁止", "无需拦截", "无需阻断",
		"无须防止", "无须阻止", "无须避免", "无须禁止", "无须拦截", "无须阻断",
		"不需要防止", "不需要阻止", "不需要避免", "不需要禁止", "不需要拦截", "不需要阻断",
		"不防止", "不阻止", "不避免", "不禁止", "不拦截", "不阻断",
		"别防止", "别阻止", "别避免", "别禁止", "别拦截", "别阻断",
		"勿防止", "勿阻止", "勿避免", "勿禁止", "勿拦截", "勿阻断",
		"取消阻止", "取消拦截", "关闭拦截", "关闭防护", "跳过拦截", "绕开拦截",
	}
	maliciousMacroEnglishNegatedGovernors = [...]string{
		"do not prevent", "do not stop", "do not block", "don't prevent", "don't stop", "don't block",
		"dont prevent", "dont stop", "dont block",
		"without preventing", "without stopping", "without blocking", "disable blocking", "turn off protection",
	}
	maliciousMacroChineseConstructionProhibitions = [...]string{
		"不要", "不得", "严禁",
	}
	maliciousMacroEnglishConstructionProhibitions = [...]string{
		"do not", "don't", "never",
	}
	maliciousMacroChineseAcquisitionSignals = [...]string{
		"给我", "给出", "给一份", "来一个", "求一个", "我要", "我想要", "我需要", "提供", "输出", "发给", "发送给", "返回", "写出", "交付",
	}
	maliciousMacroEnglishAcquisitionSignals = [...]string{
		"give me", "i need", "i want", "provide", "show me", "send me", "output", "return", "write out", "deliver me",
	}
	maliciousMacroChineseAcquisitionVerbs = [...]string{
		"获取", "取得", "拿到", "下载",
	}
	maliciousMacroEnglishAcquisitionVerbs = [...]string{
		"get", "fetch", "obtain", "download",
	}
	maliciousMacroChineseDirectActionVerbs = [...]string{
		"执行", "运行", "启用", "打开", "加载",
	}
	maliciousMacroEnglishDirectActionVerbs = [...]string{
		"execute", "run", "enable", "open", "load",
	}
	maliciousMacroChineseDescriptiveSignals = [...]string{
		"示例", "引用", "原文", "提到", "解释", "分析", "说明", "讨论", "攻击者", "防守方", "防御者", "安全人员",
	}
	maliciousMacroEnglishDescriptiveSignals = [...]string{
		"example", "quote", "quoted", "original text", "mentions", "mentioned", "explain", "analyze", "analyse", "describe", "discuss", "attacker", "attackers", "defender", "defenders",
	}
)

func hasContentModerationPolicyRestrictionContext(text string) bool {
	text = strings.ToLower(text)
	return containsAnyString(text, policyRestrictionChineseContextSignals[:]) ||
		containsAnyASCIIWordStem(text, policyRestrictionEnglishContextSignals[:])
}

// classifyContentModerationKeywordContext applies a narrowly scoped policy to
// selected ambiguous layer-one terms. It uses fragment metadata for window
// selection and never infers a trusted role from role-like text in the body.
// The bool reports whether the keyword has a contextual policy.
func classifyContentModerationKeywordContext(fragment ContentModerationFragment, keyword string) (contentModerationKeywordContextDisposition, bool) {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if _, configured := maliciousMacroContextKeywords[keyword]; !configured {
		return contentModerationKeywordContextUnspecified, false
	}

	matcher := newContentModerationKeywordMatcher([]string{keyword})
	if matcher == nil {
		return contentModerationKeywordContextReview, true
	}
	matches := matcher.MatchAll(fragment.Text)
	if len(matches) == 0 {
		return contentModerationKeywordContextReview, true
	}

	text := []rune(fragment.Text)
	// MatchAll is deliberately bounded. Local allow is limited to a small set
	// of complete-fragment templates so appended instructions cannot hide
	// outside the keyword's bounded intent window.
	allDefensive := len(matches) < contentModerationKeywordMatchLimit &&
		maliciousMacroFragmentIsClosedDefensiveTemplate(fragment.Text)
	for _, match := range matches {
		span := maliciousMacroKeywordContextSpan(fragment, text, match)
		window := strings.ToLower(string(text[span.start:span.end]))
		matchStart := match.Start - span.start
		matchEnd := match.End - span.start
		switch maliciousMacroWindowDisposition(window, matchStart, matchEnd) {
		case contentModerationKeywordContextHardBlock:
			return contentModerationKeywordContextHardBlock, true
		case contentModerationKeywordContextReview:
			allDefensive = false
		}
	}
	if allDefensive {
		return contentModerationKeywordContextAllow, true
	}
	return contentModerationKeywordContextReview, true
}

func maliciousMacroKeywordContextSpan(fragment ContentModerationFragment, text []rune, match contentModerationKeywordMatch) contentModerationRuneSpan {
	// ContextClass is derived by the request extractor, but recomputing from
	// role/kind/path prevents a caller-provided ContextClass or body text from
	// upgrading user content into a trusted line-oriented context.
	metadata := ContentModerationFragment{Role: fragment.Role, Kind: fragment.Kind, Path: fragment.Path}
	contextClass := classifyContentModerationContext(metadata)
	spans := contentModerationSentenceSpans(text)
	switch contextClass {
	case ContentModerationContextTool, ContentModerationContextServiceLog, ContentModerationContextCode, ContentModerationContextConfig:
		spans = contentModerationLineSpans(text)
	}
	index := containingContentModerationSpan(spans, match.Start, match.End)
	if index < 0 {
		return contentModerationRuneSpan{start: match.Start, end: match.End}
	}
	span := spans[index]
	return cropCandidateSpan(span, []contentModerationKeywordMatch{match}, maliciousMacroContextWindowRunes)
}

func maliciousMacroWindowDisposition(window string, matchStart, matchEnd int) contentModerationKeywordContextDisposition {
	before, after, ok := maliciousMacroIntentSlices(window, matchStart, matchEnd)
	if !ok {
		return contentModerationKeywordContextReview
	}

	if maliciousMacroConstructionIsDescriptive(before, after) {
		return contentModerationKeywordContextReview
	}
	if maliciousMacroHasConstructionIntent(before, after) {
		return contentModerationKeywordContextHardBlock
	}
	if disposition := maliciousMacroPostposedConstructionDisposition(after); disposition != contentModerationKeywordContextUnspecified {
		return disposition
	}
	if containsAnyString(after, maliciousMacroChineseBehaviorSignals[:]) ||
		containsAnyASCIIPhrase(after, maliciousMacroEnglishBehaviorSignals[:]) {
		if containsAnyString(before, maliciousMacroChineseNegatedGovernors[:]) ||
			containsAnyASCIIPhrase(before, maliciousMacroEnglishNegatedGovernors[:]) {
			return contentModerationKeywordContextHardBlock
		}
		if containsAnyString(before, maliciousMacroChineseProtectiveGovernors[:]) ||
			containsAnyASCIIPhrase(before, maliciousMacroEnglishProtectiveGovernors[:]) {
			return contentModerationKeywordContextUnspecified
		}
		if containsAnyString(before, maliciousMacroChineseDefensiveSignals[:]) ||
			containsAnyASCIIWordStem(before, maliciousMacroEnglishDefensiveSignals[:]) ||
			containsAnyString(after, maliciousMacroChineseProtectiveGovernors[:]) ||
			containsAnyASCIIPhrase(after, maliciousMacroEnglishProtectiveGovernors[:]) {
			return contentModerationKeywordContextReview
		}
		return contentModerationKeywordContextHardBlock
	}
	if maliciousMacroHasDirectAction(before, after) {
		return contentModerationKeywordContextReview
	}
	if containsAnyString(before+after, maliciousMacroChineseAcquisitionSignals[:]) ||
		containsAnyASCIIPhrase(before+" "+after, maliciousMacroEnglishAcquisitionSignals[:]) ||
		maliciousMacroHasAcquisitionIntent(before, after) {
		return contentModerationKeywordContextReview
	}
	if containsAnyString(before+after, maliciousMacroChineseEvasionSignals[:]) ||
		containsAnyASCIIPhrase(before+" "+after, maliciousMacroEnglishEvasionSignals[:]) {
		// Evasion language mixed with a defensive noun is not safe enough for a
		// local allow, but absent clear construction intent it still needs the
		// contextual reviewer rather than an unconditional keyword block.
		return contentModerationKeywordContextReview
	}
	return contentModerationKeywordContextUnspecified
}

func maliciousMacroPostposedConstructionDisposition(after string) contentModerationKeywordContextDisposition {
	for _, verb := range maliciousMacroChineseConstructionVerbs {
		index := strings.Index(after, verb)
		if index < 0 || len([]rune(after[:index])) > maliciousMacroIntentDistanceRunes {
			continue
		}
		prefix := after[:index]
		suffix := runePrefix(after[index+len(verb):], 16)
		if containsAnyString(prefix+suffix, maliciousMacroChineseEvasionSignals[:]) ||
			containsAnyASCIIPhrase(prefix+" "+suffix, maliciousMacroEnglishEvasionSignals[:]) {
			return contentModerationKeywordContextHardBlock
		}
		if containsAnyString(prefix+suffix, maliciousMacroChineseDefensiveSignals[:]) {
			return contentModerationKeywordContextReview
		}
		return contentModerationKeywordContextHardBlock
	}
	for _, verb := range maliciousMacroEnglishConstructionVerbs {
		for offset := 0; offset < len(after); {
			index := strings.Index(after[offset:], verb)
			if index < 0 {
				break
			}
			index += offset
			if hasASCIIWordBoundary(after, index, index+len(verb)) &&
				len([]rune(after[:index])) <= maliciousMacroIntentDistanceRunes {
				prefix := after[:index]
				suffix := runePrefix(after[index+len(verb):], 16)
				if containsAnyString(prefix+suffix, maliciousMacroChineseEvasionSignals[:]) ||
					containsAnyASCIIPhrase(prefix+" "+suffix, maliciousMacroEnglishEvasionSignals[:]) {
					return contentModerationKeywordContextHardBlock
				}
				if containsAnyASCIIWordStem(prefix+" "+suffix, maliciousMacroEnglishDefensiveSignals[:]) {
					return contentModerationKeywordContextReview
				}
				return contentModerationKeywordContextHardBlock
			}
			offset = index + 1
		}
	}
	return contentModerationKeywordContextUnspecified
}

func maliciousMacroIntentSlices(window string, matchStart, matchEnd int) (string, string, bool) {
	windowRunes := []rune(window)
	if matchStart < 0 || matchEnd <= matchStart || matchEnd > len(windowRunes) {
		return "", "", false
	}
	beforeStart := matchStart - maliciousMacroIntentDistanceRunes
	if beforeStart < 0 {
		beforeStart = 0
	}
	afterEnd := matchEnd + maliciousMacroIntentDistanceRunes
	if afterEnd > len(windowRunes) {
		afterEnd = len(windowRunes)
	}
	return string(windowRunes[beforeStart:matchStart]), string(windowRunes[matchEnd:afterEnd]), true
}

func maliciousMacroFragmentIsClosedDefensiveTemplate(text string) bool {
	normalized := strings.TrimFunc(strings.ToLower(text), isMaliciousMacroTerminalPunctuation)
	const reviewedUploadPolicy = "上传文件执行类型、大小、压缩炸弹、恶意宏和病毒扫描；解析器运行在受限 worker"
	if normalized == reviewedUploadPolicy {
		return true
	}

	for _, prefix := range [...]string{
		"扫描", "请扫描", "检测", "请检测", "查杀", "请查杀", "拦截", "请拦截", "阻止", "请阻止",
		"上传时扫描", "下载时再次查杀", "上传文件扫描", "上传文件检测", "需要扫描", "需要检测",
	} {
		if normalized == prefix+"恶意宏" {
			return true
		}
	}
	for _, prefix := range [...]string{
		"scan", "please scan", "detect", "please detect", "block", "please block", "prevent", "please prevent",
	} {
		if normalized == prefix+" malicious macro" || normalized == prefix+" malicious macros" {
			return true
		}
	}
	return false
}

func isMaliciousMacroTerminalPunctuation(r rune) bool {
	return unicode.IsSpace(r) || strings.ContainsRune("。！？!?.,，；;:：、", r)
}

func maliciousMacroHasDirectAction(before, after string) bool {
	const directActionDistanceRunes = 8
	for _, verb := range maliciousMacroChineseDirectActionVerbs {
		if _, ok := boundedSuffixAfter(before, verb, directActionDistanceRunes); ok {
			return true
		}
		if index := strings.Index(after, verb); index >= 0 && len([]rune(after[:index])) <= directActionDistanceRunes {
			return true
		}
	}
	for _, verb := range maliciousMacroEnglishDirectActionVerbs {
		if _, ok := boundedSuffixAfterASCIIWord(before, verb, directActionDistanceRunes); ok {
			return true
		}
		for offset := 0; offset < len(after); {
			index := strings.Index(after[offset:], verb)
			if index < 0 {
				break
			}
			index += offset
			if len([]rune(after[:index])) <= directActionDistanceRunes && hasASCIIWordBoundary(after, index, index+len(verb)) {
				return true
			}
			offset = index + 1
		}
	}
	return false
}

func maliciousMacroHasAcquisitionIntent(before, after string) bool {
	const acquisitionDistanceRunes = 8
	for _, verb := range maliciousMacroChineseAcquisitionVerbs {
		if gap, ok := boundedSuffixAfter(before, verb, acquisitionDistanceRunes); ok {
			if verb == "下载" && strings.HasPrefix(strings.TrimSpace(gap), "时") &&
				containsAnyString(gap, maliciousMacroChineseDefensiveSignals[:]) {
				continue
			}
			return true
		}
		if index := strings.Index(after, verb); index >= 0 && len([]rune(after[:index])) <= acquisitionDistanceRunes {
			return true
		}
	}
	for _, verb := range maliciousMacroEnglishAcquisitionVerbs {
		if _, ok := boundedSuffixAfterASCIIWord(before, verb, acquisitionDistanceRunes); ok {
			return true
		}
		for offset := 0; offset < len(after); {
			index := strings.Index(after[offset:], verb)
			if index < 0 {
				break
			}
			index += offset
			if len([]rune(after[:index])) <= acquisitionDistanceRunes && hasASCIIWordBoundary(after, index, index+len(verb)) {
				return true
			}
			offset = index + 1
		}
	}
	return false
}

func maliciousMacroHasConstructionIntent(before, after string) bool {
	defensiveSuffix := startsWithAnyTrimmed(after, maliciousMacroChineseDefensiveSignals[:]) ||
		startsWithAnyASCIIWordStem(after, maliciousMacroEnglishDefensiveSignals[:])
	for _, verb := range maliciousMacroChineseConstructionVerbs {
		if gap, ok := boundedSuffixAfter(before, verb, maliciousMacroIntentDistanceRunes); ok {
			if maliciousMacroChineseConstructionIsProtectivelyGoverned(before, verb) ||
				maliciousMacroConstructionIsDescriptive(before, after) {
				continue
			}
			if containsAnyString(gap, maliciousMacroChineseBehaviorSignals[:]) ||
				containsAnyASCIIPhrase(gap, maliciousMacroEnglishBehaviorSignals[:]) {
				return true
			}
			if !defensiveSuffix && !isMaliciousMacroChineseDefensiveConstruction(gap, after) {
				return true
			}
		}
	}
	for _, verb := range maliciousMacroEnglishConstructionVerbs {
		if gap, ok := boundedSuffixAfterASCIIWord(before, verb, maliciousMacroIntentDistanceRunes); ok {
			if maliciousMacroEnglishConstructionIsProtectivelyGoverned(before, verb) ||
				maliciousMacroConstructionIsDescriptive(before, after) {
				continue
			}
			if containsAnyString(gap, maliciousMacroChineseBehaviorSignals[:]) ||
				containsAnyASCIIPhrase(gap, maliciousMacroEnglishBehaviorSignals[:]) {
				return true
			}
			if !defensiveSuffix && !isMaliciousMacroEnglishDefensiveConstruction(gap) {
				return true
			}
		}
	}
	return false
}

func maliciousMacroChineseConstructionIsProtectivelyGoverned(before, verb string) bool {
	index := strings.LastIndex(before, verb)
	if index < 0 {
		return false
	}
	prefix := runeSuffix(before[:index], maliciousMacroIntentDistanceRunes)
	if containsAnyString(prefix, maliciousMacroChineseConstructionProhibitions[:]) {
		return true
	}
	if containsAnyString(prefix, maliciousMacroChineseNegatedGovernors[:]) {
		return false
	}
	return containsAnyString(prefix, maliciousMacroChineseProtectiveGovernors[:])
}

func maliciousMacroEnglishConstructionIsProtectivelyGoverned(before, verb string) bool {
	searchEnd := len(before)
	for searchEnd > 0 {
		index := strings.LastIndex(before[:searchEnd], verb)
		if index < 0 {
			return false
		}
		if hasASCIIWordBoundary(before, index, index+len(verb)) {
			prefix := runeSuffix(before[:index], maliciousMacroIntentDistanceRunes)
			if containsAnyASCIIPhrase(prefix, maliciousMacroEnglishConstructionProhibitions[:]) {
				return true
			}
			if containsAnyASCIIPhrase(prefix, maliciousMacroEnglishNegatedGovernors[:]) {
				return false
			}
			return containsAnyASCIIPhrase(prefix, maliciousMacroEnglishProtectiveGovernors[:])
		}
		searchEnd = index
	}
	return false
}

func maliciousMacroConstructionIsDescriptive(before, after string) bool {
	local := before + " " + after
	return containsAnyString(local, maliciousMacroChineseDescriptiveSignals[:]) ||
		containsAnyASCIIPhrase(local, maliciousMacroEnglishDescriptiveSignals[:]) ||
		strings.ContainsAny(local, "\"'“”‘’")
}

func runeSuffix(value string, limit int) string {
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		runes = runes[len(runes)-limit:]
	}
	return string(runes)
}

func runePrefix(value string, limit int) string {
	runes := []rune(value)
	if limit > 0 && len(runes) > limit {
		runes = runes[:limit]
	}
	return string(runes)
}

func isMaliciousMacroChineseDefensiveConstruction(gap, after string) bool {
	for _, phrase := range [...]string{
		"用于检测", "用于扫描", "用来检测", "用来扫描", "以便检测", "以便扫描", "为了检测", "为了扫描",
		"检测器", "扫描器", "检测规则", "扫描规则", "检测工具", "扫描工具",
	} {
		if strings.Contains(gap, phrase) {
			return true
		}
	}
	after = strings.TrimLeftFunc(after, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("，、,:：-—()（）[]【】的", r)
	})
	return startsWithAnyTrimmed(after, maliciousMacroChineseDefensiveSignals[:])
}

func isMaliciousMacroEnglishDefensiveConstruction(gap string) bool {
	return containsAnyASCIIPhrase(gap, []string{
		"detector", "scanner", "detection rule", "scanning rule", "detection tool", "scanning tool",
		"to detect", "to scan", "for detecting", "for scanning", "for detection",
	})
}

func boundedSuffixAfter(value, marker string, limit int) (string, bool) {
	index := strings.LastIndex(value, marker)
	if index < 0 {
		return "", false
	}
	suffix := value[index+len(marker):]
	return suffix, len([]rune(suffix)) <= limit
}

func boundedSuffixAfterASCIIWord(value, marker string, limit int) (string, bool) {
	searchEnd := len(value)
	for searchEnd > 0 {
		index := strings.LastIndex(value[:searchEnd], marker)
		if index < 0 {
			return "", false
		}
		end := index + len(marker)
		if hasASCIIWordBoundary(value, index, end) {
			suffix := value[end:]
			return suffix, len([]rune(suffix)) <= limit
		}
		searchEnd = index
	}
	return "", false
}

func startsWithAnyTrimmed(value string, markers []string) bool {
	value = strings.TrimLeftFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("，、,:：-—()（）[]【】", r)
	})
	for _, marker := range markers {
		if strings.HasPrefix(value, marker) {
			return true
		}
	}
	return false
}

func startsWithAnyASCIIWordStem(value string, stems []string) bool {
	value = strings.TrimLeftFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",:-()[]", r)
	})
	for _, stem := range stems {
		if strings.HasPrefix(value, stem) && (len(value) == len(stem) || !isASCIIWordByte(value[len(stem)])) {
			return true
		}
	}
	return false
}

func containsAnyString(value string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func containsAnyASCIIPhrase(value string, phrases []string) bool {
	for _, phrase := range phrases {
		for offset := 0; offset < len(value); {
			index := strings.Index(value[offset:], phrase)
			if index < 0 {
				break
			}
			index += offset
			if hasASCIIWordBoundary(value, index, index+len(phrase)) {
				return true
			}
			offset = index + 1
		}
	}
	return false
}

func containsAnyASCIIWordStem(value string, stems []string) bool {
	for _, stem := range stems {
		for offset := 0; offset < len(value); {
			index := strings.Index(value[offset:], stem)
			if index < 0 {
				break
			}
			index += offset
			if (index == 0 || !isASCIIWordByte(value[index-1])) &&
				(index+len(stem) == len(value) || !isASCIIWordByte(value[index+len(stem)])) {
				return true
			}
			offset = index + 1
		}
	}
	return false
}

func hasASCIIWordBoundary(value string, start, end int) bool {
	return (start == 0 || !isASCIIWordByte(value[start-1])) &&
		(end == len(value) || !isASCIIWordByte(value[end]))
}

func isASCIIWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= '0' && value <= '9' || value == '_'
}

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
