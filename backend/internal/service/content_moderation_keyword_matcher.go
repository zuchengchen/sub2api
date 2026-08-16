package service

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const contentModerationHardKeywordMatcherPolicyVersion = "ascii-word-boundary-v2-all-offsets"
const contentModerationKeywordMatchLimit = 64

// contentModerationKeywordMatch offsets are Unicode rune indexes in the input
// passed to MatchAll. Rune indexes are stable across UTF-8 byte widths and are
// also what the evidence API exposes for client-side highlighting.
type contentModerationKeywordMatch struct {
	Keyword         string `json:"keyword"`
	Start           int    `json:"start"`
	End             int    `json:"end"`
	keywordIndex    int
	normalizedStart int
	normalizedEnd   int
}

type contentModerationKeywordMatcher struct {
	nodes                 []contentModerationKeywordNode
	edges                 []contentModerationKeywordEdge
	rootTransitions       [256]int32
	keywords              []string
	patterns              []contentModerationKeywordPattern
	requireWordBoundaries bool
	maxPatternByteLength  int
}

type contentModerationKeywordNode struct {
	failure         int32
	outputLink      int32
	terminalKeyword int32
	bestKeyword     int32
	edgeStart       uint32
	edgeCount       uint16
}

type contentModerationKeywordEdge struct {
	target int32
	label  byte
}

type contentModerationKeywordBuildEdge struct {
	target      int32
	nextSibling int32
	label       byte
}

type contentModerationKeywordPattern struct {
	byteLength    int
	leftBoundary  bool
	rightBoundary bool
}

func newContentModerationKeywordMatcher(keywords []string) *contentModerationKeywordMatcher {
	return newContentModerationKeywordMatcherWithMode(keywords, true)
}

func newContentModerationSubstringMatcher(keywords []string) *contentModerationKeywordMatcher {
	return newContentModerationKeywordMatcherWithMode(keywords, false)
}

func newContentModerationKeywordMatcherWithMode(keywords []string, requireWordBoundaries bool) *contentModerationKeywordMatcher {
	if len(keywords) == 0 {
		return nil
	}

	buildNodes := []contentModerationKeywordNode{newContentModerationKeywordNode()}
	buildEdges := make([]contentModerationKeywordBuildEdge, 0)
	originalKeywords := append([]string(nil), keywords...)
	patterns := make([]contentModerationKeywordPattern, len(keywords))
	maxPatternByteLength := 0

	for keywordIndex, keyword := range keywords {
		if keyword == "" {
			continue
		}
		lowerKeyword := strings.ToLower(keyword)
		patterns[keywordIndex] = newContentModerationKeywordPattern(lowerKeyword)
		if len(lowerKeyword) > maxPatternByteLength {
			maxPatternByteLength = len(lowerKeyword)
		}
		state := int32(0)
		for _, label := range []byte(lowerKeyword) {
			next := contentModerationKeywordBuildTransition(buildNodes, buildEdges, state, label)
			if next < 0 {
				next = int32(len(buildNodes))
				buildNodes = append(buildNodes, newContentModerationKeywordNode())
				buildEdges = append(buildEdges, contentModerationKeywordBuildEdge{
					target:      next,
					nextSibling: contentModerationKeywordBuildFirstEdge(buildNodes[state]),
					label:       label,
				})
				buildNodes[state].edgeStart = uint32(len(buildEdges))
			}
			state = next
		}
		if current := buildNodes[state].terminalKeyword; current < 0 || int32(keywordIndex) < current {
			buildNodes[state].terminalKeyword = int32(keywordIndex)
		}
		if current := buildNodes[state].bestKeyword; current < 0 || int32(keywordIndex) < current {
			buildNodes[state].bestKeyword = int32(keywordIndex)
		}
	}

	if len(buildNodes) == 1 {
		return nil
	}

	queue := make([]int32, 0, len(buildNodes)-1)
	var rootTransitions [256]int32
	for edgeIndex := contentModerationKeywordBuildFirstEdge(buildNodes[0]); edgeIndex >= 0; edgeIndex = buildEdges[edgeIndex].nextSibling {
		edge := buildEdges[edgeIndex]
		rootTransitions[edge.label] = edge.target
		queue = append(queue, edge.target)
	}

	for queueIndex := 0; queueIndex < len(queue); queueIndex++ {
		state := queue[queueIndex]
		for edgeIndex := contentModerationKeywordBuildFirstEdge(buildNodes[state]); edgeIndex >= 0; edgeIndex = buildEdges[edgeIndex].nextSibling {
			edge := buildEdges[edgeIndex]
			failure := buildNodes[state].failure
			fallback := contentModerationKeywordBuildTransition(buildNodes, buildEdges, failure, edge.label)
			for fallback < 0 && failure != 0 {
				failure = buildNodes[failure].failure
				fallback = contentModerationKeywordBuildTransition(buildNodes, buildEdges, failure, edge.label)
			}
			if fallback >= 0 {
				buildNodes[edge.target].failure = fallback
			}
			failureNode := buildNodes[buildNodes[edge.target].failure]
			if failureNode.terminalKeyword >= 0 {
				buildNodes[edge.target].outputLink = buildNodes[edge.target].failure
			} else {
				buildNodes[edge.target].outputLink = failureNode.outputLink
			}
			buildNodes[edge.target].bestKeyword = minKeywordIndex(
				buildNodes[edge.target].bestKeyword,
				failureNode.bestKeyword,
			)
			queue = append(queue, edge.target)
		}
	}

	edges := make([]contentModerationKeywordEdge, 0, len(buildEdges))
	var outgoing [256]contentModerationKeywordEdge
	for nodeIndex := range buildNodes {
		count := 0
		for edgeIndex := contentModerationKeywordBuildFirstEdge(buildNodes[nodeIndex]); edgeIndex >= 0; edgeIndex = buildEdges[edgeIndex].nextSibling {
			edge := buildEdges[edgeIndex]
			outgoing[count] = contentModerationKeywordEdge{target: edge.target, label: edge.label}
			count++
		}
		for index := 1; index < count; index++ {
			current := outgoing[index]
			insertAt := index
			for insertAt > 0 && current.label < outgoing[insertAt-1].label {
				outgoing[insertAt] = outgoing[insertAt-1]
				insertAt--
			}
			outgoing[insertAt] = current
		}
		buildNodes[nodeIndex].edgeStart = uint32(len(edges))
		buildNodes[nodeIndex].edgeCount = uint16(count)
		edges = append(edges, outgoing[:count]...)
	}

	return &contentModerationKeywordMatcher{
		nodes:                 buildNodes,
		edges:                 edges,
		rootTransitions:       rootTransitions,
		keywords:              originalKeywords,
		patterns:              patterns,
		requireWordBoundaries: requireWordBoundaries,
		maxPatternByteLength:  maxPatternByteLength,
	}
}

func newContentModerationKeywordNode() contentModerationKeywordNode {
	return contentModerationKeywordNode{terminalKeyword: -1, bestKeyword: -1}
}

func contentModerationKeywordBuildFirstEdge(node contentModerationKeywordNode) int32 {
	if node.edgeStart == 0 {
		return -1
	}
	return int32(node.edgeStart - 1)
}

func contentModerationKeywordBuildTransition(
	nodes []contentModerationKeywordNode,
	edges []contentModerationKeywordBuildEdge,
	state int32,
	label byte,
) int32 {
	if state < 0 || int(state) >= len(nodes) {
		return -1
	}
	for edgeIndex := contentModerationKeywordBuildFirstEdge(nodes[state]); edgeIndex >= 0; edgeIndex = edges[edgeIndex].nextSibling {
		if edges[edgeIndex].label == label {
			return edges[edgeIndex].target
		}
	}
	return -1
}

func minKeywordIndex(left, right int32) int32 {
	if left < 0 {
		return right
	}
	if right < 0 || left < right {
		return left
	}
	return right
}

func (m *contentModerationKeywordMatcher) Match(text string) (string, bool) {
	if m == nil || text == "" || len(m.nodes) == 0 || len(m.keywords) == 0 {
		return "", false
	}
	scanner := newContentModerationKeywordScanner(m, false)
	runeIndex := 0
	for _, original := range text {
		if !emitLowerContentModerationRune(scanner, unicode.ToLower(original), runeIndex, runeIndex+1) {
			break
		}
		runeIndex++
	}
	scanner.finish()
	if scanner.bestKeyword < 0 || int(scanner.bestKeyword) >= len(m.keywords) {
		return "", false
	}
	return m.keywords[scanner.bestKeyword], true
}

// MatchAll returns a bounded set of accepted occurrences in source order. The
// scanner keeps only enough byte-to-rune mapping for the longest configured
// pattern, so a large request cannot amplify into input-sized offset tables.
func (m *contentModerationKeywordMatcher) MatchAll(text string) []contentModerationKeywordMatch {
	if m == nil || text == "" || len(m.nodes) == 0 || len(m.keywords) == 0 {
		return nil
	}
	scanner := newContentModerationKeywordScanner(m, true)
	runeIndex := 0
	for _, original := range text {
		if !emitLowerContentModerationRune(scanner, unicode.ToLower(original), runeIndex, runeIndex+1) {
			break
		}
		runeIndex++
	}
	scanner.finish()
	return sortAndDeduplicateContentModerationKeywordMatches(scanner.matches)
}

type contentModerationKeywordScanner struct {
	matcher     *contentModerationKeywordMatcher
	state       int32
	position    int
	labels      []byte
	runeStarts  []int
	pending     []contentModerationKeywordMatch
	matches     []contentModerationKeywordMatch
	bestKeyword int32
	collect     bool
	matchLimit  int
	onMatch     func(contentModerationKeywordMatch) bool
	stopped     bool
}

func newContentModerationKeywordScanner(m *contentModerationKeywordMatcher, collect bool) *contentModerationKeywordScanner {
	ringSize := 1
	if m != nil && m.maxPatternByteLength >= ringSize {
		ringSize = m.maxPatternByteLength + 1
	}
	return &contentModerationKeywordScanner{
		matcher: m, labels: make([]byte, ringSize), runeStarts: make([]int, ringSize),
		bestKeyword: -1, collect: collect, matchLimit: contentModerationKeywordMatchLimit,
	}
}

func emitLowerContentModerationRune(scanner *contentModerationKeywordScanner, value rune, runeStart, runeEnd int) bool {
	if scanner == nil {
		return false
	}
	var encoded [utf8.UTFMax]byte
	length := utf8.EncodeRune(encoded[:], value)
	for index := 0; index < length; index++ {
		if !scanner.emit(encoded[index], runeStart, runeEnd) {
			return false
		}
	}
	return true
}

func (s *contentModerationKeywordScanner) emit(label byte, runeStart, runeEnd int) bool {
	if s == nil || s.matcher == nil {
		return false
	}
	if !s.resolvePending(label, false) {
		return false
	}
	ringIndex := s.position % len(s.labels)
	s.labels[ringIndex] = label
	s.runeStarts[ringIndex] = runeStart

	for {
		next := s.matcher.next(s.state, label)
		if next != 0 {
			s.state = next
			break
		}
		if s.state == 0 {
			break
		}
		s.state = s.matcher.nodes[s.state].failure
	}
	for outputState := s.state; outputState != 0; outputState = s.matcher.nodes[outputState].outputLink {
		keywordIndex := s.matcher.nodes[outputState].terminalKeyword
		if keywordIndex < 0 || int(keywordIndex) >= len(s.matcher.patterns) {
			continue
		}
		pattern := s.matcher.patterns[keywordIndex]
		endByte := s.position + 1
		startByte := endByte - pattern.byteLength
		if startByte < 0 || s.matcher.requireWordBoundaries && pattern.leftBoundary && startByte > 0 && isASCIIContentModerationWordByte(s.labelAt(startByte-1)) {
			continue
		}
		match := contentModerationKeywordMatch{
			Keyword: s.matcher.keywords[keywordIndex], Start: s.runeStartAt(startByte), End: runeEnd,
			keywordIndex: int(keywordIndex), normalizedStart: startByte, normalizedEnd: endByte,
		}
		if s.matcher.requireWordBoundaries && pattern.rightBoundary {
			if s.canKeepAnotherMatch() {
				s.pending = append(s.pending, match)
			}
			continue
		}
		if !s.accept(match) {
			return false
		}
	}
	s.position++
	return !s.done()
}

func (s *contentModerationKeywordScanner) finish() {
	if s == nil {
		return
	}
	_ = s.resolvePending(0, true)
}

func (s *contentModerationKeywordScanner) resolvePending(next byte, eof bool) bool {
	if len(s.pending) == 0 {
		return true
	}
	pending := s.pending
	s.pending = s.pending[:0]
	if !eof && isASCIIContentModerationWordByte(next) {
		return true
	}
	for _, match := range pending {
		if !s.accept(match) {
			return false
		}
	}
	return true
}

func (s *contentModerationKeywordScanner) accept(match contentModerationKeywordMatch) bool {
	s.bestKeyword = minKeywordIndex(s.bestKeyword, int32(match.keywordIndex))
	if s.onMatch != nil {
		if !s.onMatch(match) {
			s.stopped = true
			return false
		}
		return true
	}
	if s.collect && len(s.matches) < s.matchLimit {
		s.matches = append(s.matches, match)
	}
	return !s.done()
}

func (s *contentModerationKeywordScanner) canKeepAnotherMatch() bool {
	if s.onMatch != nil {
		return !s.stopped
	}
	return !s.collect || len(s.matches)+len(s.pending) < s.matchLimit
}

func (s *contentModerationKeywordScanner) done() bool {
	if s.onMatch != nil {
		return s.stopped
	}
	if s.collect {
		return len(s.matches) >= s.matchLimit
	}
	return s.bestKeyword == 0
}

func (s *contentModerationKeywordScanner) labelAt(position int) byte {
	return s.labels[position%len(s.labels)]
}

func (s *contentModerationKeywordScanner) runeStartAt(position int) int {
	return s.runeStarts[position%len(s.runeStarts)]
}

func sortAndDeduplicateContentModerationKeywordMatches(matches []contentModerationKeywordMatch) []contentModerationKeywordMatch {
	if len(matches) < 2 {
		return matches
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].Start != matches[j].Start {
			return matches[i].Start < matches[j].Start
		}
		if matches[i].End != matches[j].End {
			return matches[i].End < matches[j].End
		}
		return matches[i].keywordIndex < matches[j].keywordIndex
	})
	out := matches[:0]
	for _, match := range matches {
		if len(out) > 0 {
			last := out[len(out)-1]
			if last.Start == match.Start && last.End == match.End && last.Keyword == match.Keyword {
				continue
			}
		}
		out = append(out, match)
	}
	return out
}

func newContentModerationKeywordPattern(keyword string) contentModerationKeywordPattern {
	pattern := contentModerationKeywordPattern{byteLength: len(keyword)}
	if keyword == "" {
		return pattern
	}
	pattern.leftBoundary = isASCIIContentModerationWordByte(keyword[0])
	pattern.rightBoundary = isASCIIContentModerationWordByte(keyword[len(keyword)-1])
	return pattern
}

func contentModerationKeywordMatchHasBoundaries(text string, start, end int, pattern contentModerationKeywordPattern) bool {
	if start < 0 || end < start || end > len(text) {
		return false
	}
	if pattern.leftBoundary && start > 0 && isASCIIContentModerationWordByte(text[start-1]) {
		return false
	}
	if pattern.rightBoundary && end < len(text) && isASCIIContentModerationWordByte(text[end]) {
		return false
	}
	return true
}

func containsContentModerationHardKeyword(text, keyword string) bool {
	if text == "" || keyword == "" {
		return false
	}
	pattern := newContentModerationKeywordPattern(keyword)
	for searchFrom := 0; searchFrom <= len(text)-len(keyword); {
		relative := strings.Index(text[searchFrom:], keyword)
		if relative < 0 {
			return false
		}
		start := searchFrom + relative
		end := start + len(keyword)
		if contentModerationKeywordMatchHasBoundaries(text, start, end, pattern) {
			return true
		}
		searchFrom = start + 1
	}
	return false
}

func isASCIIContentModerationWordByte(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_'
}

func (m *contentModerationKeywordMatcher) next(state int32, label byte) int32 {
	if state == 0 {
		return m.rootTransitions[label]
	}
	if state < 0 || int(state) >= len(m.nodes) {
		return 0
	}
	node := m.nodes[state]
	left := int(node.edgeStart)
	right := left + int(node.edgeCount)
	for left < right {
		middle := left + (right-left)/2
		edge := m.edges[middle]
		if edge.label < label {
			left = middle + 1
			continue
		}
		right = middle
	}
	end := int(node.edgeStart) + int(node.edgeCount)
	if left < end && m.edges[left].label == label {
		return m.edges[left].target
	}
	return 0
}
