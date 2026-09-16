package service

import (
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

var (
	ErrSupportDecisionActiveGenerationNotFound = errors.New("support decision active generation not found")
	ErrSupportDecisionDocumentNotFound         = errors.New("support decision document not found")
)

type SupportDecisionResult uint8

const (
	SupportDecisionUnknown SupportDecisionResult = iota
	SupportDecisionNotPureMiss
	SupportDecisionPureMiss
)

type SupportDecisionScope struct {
	Platform             string
	GroupID              int64
	IncludeGrouped       bool
	AllowMixedScheduling bool
}

type SupportDecisionQuery struct {
	Scope           SupportDecisionScope
	RequestedModel  string
	RequiresPrivacy bool
	ThinkingEnabled bool
	RequireCompact  bool
}

type SupportDecisionReader interface {
	Lookup(query SupportDecisionQuery) SupportDecisionResult
}

// SupportDecisionAtomicReader is the process-local replica used on the request path.
type SupportDecisionAtomicReader struct {
	table    atomic.Pointer[SupportDecisionTable]
	maxStale time.Duration
}

func NewSupportDecisionAtomicReader(maxStale time.Duration) *SupportDecisionAtomicReader {
	if maxStale <= 0 {
		maxStale = 30 * time.Second
	}
	return &SupportDecisionAtomicReader{maxStale: maxStale}
}

func ProvideSupportDecisionAtomicReader() *SupportDecisionAtomicReader {
	return NewSupportDecisionAtomicReader(30 * time.Second)
}

func ProvideSupportDecisionReader(reader *SupportDecisionAtomicReader) SupportDecisionReader {
	return reader
}

func (r *SupportDecisionAtomicReader) Install(table *SupportDecisionTable) {
	if r == nil {
		return
	}
	r.table.Store(table)
}

func (r *SupportDecisionAtomicReader) Lookup(query SupportDecisionQuery) SupportDecisionResult {
	if r == nil {
		return SupportDecisionUnknown
	}
	table := r.table.Load()
	if table == nil {
		return SupportDecisionUnknown
	}
	if r.maxStale > 0 && !table.BuiltAt.IsZero() && time.Since(table.BuiltAt) > r.maxStale {
		return SupportDecisionUnknown
	}
	return table.Lookup(query)
}

// SupportDecisionTable is an identifier-free published classifier.
type SupportDecisionTable struct {
	Platforms map[string]struct{}
	BuiltAt   time.Time
}

func supportDecisionPlatforms() []string {
	return []string{
		PlatformAnthropic,
		PlatformOpenAI,
		PlatformGrok,
		PlatformKimi,
		PlatformZhipu,
		PlatformDeepseek,
		PlatformMiniMax,
		PlatformOpenCodeGo,
		PlatformComposite,
	}
}

func BuildSupportDecisionTable(now time.Time) *SupportDecisionTable {
	platforms := make(map[string]struct{}, 9)
	for _, platform := range supportDecisionPlatforms() {
		platforms[platform] = struct{}{}
	}
	return &SupportDecisionTable{Platforms: platforms, BuiltAt: now}
}

func (t *SupportDecisionTable) Lookup(query SupportDecisionQuery) SupportDecisionResult {
	if t == nil {
		return SupportDecisionUnknown
	}
	platform := strings.ToLower(strings.TrimSpace(query.Scope.Platform))
	if platform == "" {
		return SupportDecisionUnknown
	}
	if _, ok := t.Platforms[platform]; !ok {
		return SupportDecisionUnknown
	}
	if strings.TrimSpace(query.RequestedModel) == "" {
		return SupportDecisionUnknown
	}
	// Near-limit, Grok quota, compact, composite routing, and profit-control are
	// unmodeled constraints. Conservative Unknown keeps existing selection.
	if query.RequireCompact {
		return SupportDecisionUnknown
	}
	if platform == PlatformComposite {
		return SupportDecisionUnknown
	}
	return SupportDecisionUnknown
}
