package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	GuideMaxContentBytes        = 256 * 1024
	GuideMaxChapterContentBytes = 64 * 1024
	GuideMaxChapters            = 64
	GuideRevisionLimit          = 20
)

var guideChapterSlugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// guideLegacySlugs maps the bundled guide's headings to the anchors that were
// published before the guide was split into chapters. Splitting a legacy
// document consults this map first so links such as /guide#recharge survive.
// Mirrors sectionIds in frontend/src/utils/guideMarkdown.ts.
var guideLegacySlugs = map[string]string{
	"先照着做一遍":               "quick-start",
	"注册与登录":                "account",
	"充值：先买兑换码，再兑换余额":       "recharge",
	"API Key：给软件使用的专用密码":   "api-key",
	"检查 API Key 能不能用":      "first-request",
	"三个可用网址":               "domains",
	"在 Codex 中使用本站":        "codex",
	"自动选择速度最快的网址":          "speed-script",
	"使用 goal-workflow 小助手": "goal-workflow",
	"SVIP 能得到什么、需要注意什么":    "svip",
	"查看余额和使用记录":            "usage",
	"错误码含义与处理方案":           "error-codes",
	"遇到问题先看这里":             "faq",
	"保护账户和密钥":              "security",
	"联系管理员时要准备什么":          "support",
	"教程版本":                 "version",
}

// GuideChapter is one independently editable section of the usage guide. Slug
// is the permanent anchor id on /guide, so it is validated but never rewritten.
type GuideChapter struct {
	Slug    string `json:"slug"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

// GuideRevision is a restorable snapshot of the public usage guide.
// An empty chapter list means that the bundled guide was active.
type GuideRevision struct {
	Version  int            `json:"version"`
	Chapters []GuideChapter `json:"chapters"`
	// Content carries snapshots taken before the guide was split into chapters.
	Content   string `json:"content,omitempty"`
	UpdatedAt string `json:"updated_at"`
}

// GuideSettings contains the current guide and its bounded revision history.
type GuideSettings struct {
	// Content is the whole-document form retained for guides published before
	// chapters existed; Chapters is authoritative for everything published now.
	Content          string          `json:"content"`
	Chapters         []GuideChapter  `json:"chapters"`
	Version          int             `json:"version"`
	UpdatedAt        string          `json:"updated_at"`
	HasCustomContent bool            `json:"has_custom_content"`
	Revisions        []GuideRevision `json:"revisions,omitempty"`
}

// GetGuideSettings returns the current database-backed guide configuration.
func (s *SettingService) GetGuideSettings(ctx context.Context) (*GuideSettings, error) {
	s.guideMu.Lock()
	defer s.guideMu.Unlock()

	return s.loadGuideSettings(ctx)
}

// SaveGuideSettings publishes a chapter list as a new guide version.
// expectedVersion prevents an older browser tab from silently overwriting a
// newer edit.
func (s *SettingService) SaveGuideSettings(ctx context.Context, chapters []GuideChapter, expectedVersion int) (*GuideSettings, error) {
	return s.saveGuideSettings(ctx, chapters, expectedVersion, false)
}

// ResetGuideSettings switches the public page back to the guide bundled with
// the application while preserving the action as a restorable revision.
func (s *SettingService) ResetGuideSettings(ctx context.Context, expectedVersion int) (*GuideSettings, error) {
	return s.saveGuideSettings(ctx, nil, expectedVersion, true)
}

// RestoreGuideSettings republishes a stored snapshot as a new version.
func (s *SettingService) RestoreGuideSettings(ctx context.Context, revisionVersion, expectedVersion int) (*GuideSettings, error) {
	s.guideMu.Lock()
	defer s.guideMu.Unlock()

	current, err := s.loadGuideSettings(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateGuideExpectedVersion(current.Version, expectedVersion); err != nil {
		return nil, err
	}

	var target *GuideRevision
	for i := range current.Revisions {
		if current.Revisions[i].Version == revisionVersion {
			target = &current.Revisions[i]
			break
		}
	}
	if target == nil {
		return nil, infraerrors.NotFound("GUIDE_REVISION_NOT_FOUND", "guide revision was not found")
	}

	// A snapshot taken before chapters existed only carries Content; split it so
	// restoring an old revision still yields an editable chapter list.
	chapters := target.Chapters
	if len(chapters) == 0 && strings.TrimSpace(target.Content) != "" {
		chapters = SplitGuideIntoChapters(target.Content)
	}
	if err := validateGuideChapters(chapters, true); err != nil {
		return nil, err
	}

	return s.persistGuideSettings(ctx, current, chapters)
}

func (s *SettingService) saveGuideSettings(ctx context.Context, chapters []GuideChapter, expectedVersion int, allowEmpty bool) (*GuideSettings, error) {
	if err := validateGuideChapters(chapters, allowEmpty); err != nil {
		return nil, err
	}

	s.guideMu.Lock()
	defer s.guideMu.Unlock()

	current, err := s.loadGuideSettings(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateGuideExpectedVersion(current.Version, expectedVersion); err != nil {
		return nil, err
	}
	if guideChaptersEqual(current.Chapters, chapters) {
		return current, nil
	}

	return s.persistGuideSettings(ctx, current, chapters)
}

func (s *SettingService) loadGuideSettings(ctx context.Context) (*GuideSettings, error) {
	values, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyGuideContent,
		SettingKeyGuideChapters,
		SettingKeyGuideVersion,
		SettingKeyGuideUpdatedAt,
		SettingKeyGuideRevisions,
	})
	if err != nil {
		return nil, fmt.Errorf("get guide settings: %w", err)
	}

	version := 0
	if raw := strings.TrimSpace(values[SettingKeyGuideVersion]); raw != "" {
		parsed, parseErr := strconv.Atoi(raw)
		if parseErr != nil || parsed < 0 {
			return nil, fmt.Errorf("parse guide version %q", raw)
		}
		version = parsed
	}

	revisions := make([]GuideRevision, 0)
	if raw := strings.TrimSpace(values[SettingKeyGuideRevisions]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &revisions); err != nil {
			slog.Warn("invalid guide revision history; ignoring it", "error", err)
			revisions = make([]GuideRevision, 0)
		}
	}
	if len(revisions) > GuideRevisionLimit {
		revisions = revisions[len(revisions)-GuideRevisionLimit:]
	}

	content := values[SettingKeyGuideContent]
	chapters := make([]GuideChapter, 0)
	if raw := strings.TrimSpace(values[SettingKeyGuideChapters]); raw != "" {
		if err := json.Unmarshal([]byte(raw), &chapters); err != nil {
			slog.Warn("invalid guide chapter list; falling back to the stored document", "error", err)
			chapters = make([]GuideChapter, 0)
		}
	}

	// Migrate a guide published before chapters existed. The split happens on
	// read so no data is rewritten until the admin publishes again, which keeps
	// guide_content intact as a rollback path.
	if len(chapters) == 0 && strings.TrimSpace(content) != "" {
		chapters = SplitGuideIntoChapters(content)
	}
	if content == "" && len(chapters) > 0 {
		content = JoinGuideChapters(chapters)
	}

	return &GuideSettings{
		Content:          content,
		Chapters:         chapters,
		Version:          version,
		UpdatedAt:        strings.TrimSpace(values[SettingKeyGuideUpdatedAt]),
		HasCustomContent: len(chapters) > 0,
		Revisions:        revisions,
	}, nil
}

func (s *SettingService) persistGuideSettings(ctx context.Context, current *GuideSettings, chapters []GuideChapter) (*GuideSettings, error) {
	version := current.Version + 1
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	normalized := normalizeGuideChapters(chapters)
	content := JoinGuideChapters(normalized)
	revisions := append(append([]GuideRevision(nil), current.Revisions...), GuideRevision{
		Version:   version,
		Chapters:  normalized,
		UpdatedAt: updatedAt,
	})
	if len(revisions) > GuideRevisionLimit {
		revisions = revisions[len(revisions)-GuideRevisionLimit:]
	}

	revisionsJSON, err := json.Marshal(revisions)
	if err != nil {
		return nil, fmt.Errorf("marshal guide revisions: %w", err)
	}
	chaptersJSON, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("marshal guide chapters: %w", err)
	}
	if err := s.settingRepo.SetMultiple(ctx, map[string]string{
		SettingKeyGuideContent:   content,
		SettingKeyGuideChapters:  string(chaptersJSON),
		SettingKeyGuideVersion:   strconv.Itoa(version),
		SettingKeyGuideUpdatedAt: updatedAt,
		SettingKeyGuideRevisions: string(revisionsJSON),
	}); err != nil {
		return nil, fmt.Errorf("save guide settings: %w", err)
	}
	if s.onUpdate != nil {
		s.onUpdate()
	}

	return &GuideSettings{
		Content:          content,
		Chapters:         normalized,
		Version:          version,
		UpdatedAt:        updatedAt,
		HasCustomContent: len(normalized) > 0,
		Revisions:        revisions,
	}, nil
}

// normalizeGuideChapters drops blank chapters and trims trailing whitespace so
// stored chapters join back into a stable document.
func normalizeGuideChapters(chapters []GuideChapter) []GuideChapter {
	normalized := make([]GuideChapter, 0, len(chapters))
	for _, chapter := range chapters {
		content := strings.TrimRight(chapter.Content, " \t\r\n")
		if strings.TrimSpace(content) == "" {
			continue
		}
		normalized = append(normalized, GuideChapter{
			Slug:    chapter.Slug,
			Title:   strings.TrimSpace(chapter.Title),
			Content: content,
		})
	}
	return normalized
}

func guideChaptersEqual(a, b []GuideChapter) bool {
	left, right := normalizeGuideChapters(a), normalizeGuideChapters(b)
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validateGuideChapters(chapters []GuideChapter, allowEmpty bool) error {
	normalized := normalizeGuideChapters(chapters)
	if len(normalized) == 0 {
		if allowEmpty {
			return nil
		}
		return infraerrors.BadRequest("GUIDE_CONTENT_REQUIRED", "guide content is required")
	}
	if len(normalized) > GuideMaxChapters {
		return infraerrors.BadRequest("GUIDE_CHAPTER_LIMIT_EXCEEDED", "guide must not exceed 64 chapters")
	}

	total := 0
	seen := make(map[string]struct{}, len(normalized))
	for _, chapter := range normalized {
		if !guideChapterSlugPattern.MatchString(chapter.Slug) {
			return infraerrors.BadRequest("GUIDE_CHAPTER_SLUG_INVALID", "chapter slug must match [a-z0-9] words joined by hyphens")
		}
		if _, duplicate := seen[chapter.Slug]; duplicate {
			return infraerrors.BadRequest("GUIDE_CHAPTER_SLUG_DUPLICATE", "chapter slugs must be unique")
		}
		seen[chapter.Slug] = struct{}{}

		if len(chapter.Content) > GuideMaxChapterContentBytes {
			return infraerrors.BadRequest("GUIDE_CHAPTER_TOO_LARGE", "a chapter must not exceed 64 KiB")
		}
		if strings.ContainsRune(chapter.Content, '\x00') || strings.ContainsRune(chapter.Title, '\x00') {
			return infraerrors.BadRequest("GUIDE_CONTENT_INVALID", "guide content contains an invalid character")
		}
		total += len(chapter.Content)
	}
	if total > GuideMaxContentBytes {
		return infraerrors.BadRequest("GUIDE_CONTENT_TOO_LARGE", "guide content must not exceed 256 KiB")
	}
	return nil
}

// SplitGuideIntoChapters splits a whole-document guide at every `##` heading.
// Text before the first heading is preserved as a `preface` chapter instead of
// being dropped. Slugs are derived from the heading so anchors stay stable.
func SplitGuideIntoChapters(content string) []GuideChapter {
	chapters := make([]GuideChapter, 0, 16)
	used := make(map[string]struct{}, 16)
	var preface []string
	var currentTitle string
	var currentLines []string

	flush := func() {
		if currentLines == nil {
			return
		}
		body := strings.TrimRight(strings.Join(currentLines, "\n"), " \t\r\n")
		if strings.TrimSpace(body) != "" {
			slug := ""
			if legacy, ok := guideLegacySlugs[currentTitle]; ok {
				if _, taken := used[legacy]; !taken {
					slug = legacy
				}
			}
			if slug == "" {
				slug = deriveGuideChapterSlug(currentTitle, used)
			}
			used[slug] = struct{}{}
			chapters = append(chapters, GuideChapter{Slug: slug, Title: currentTitle, Content: body})
		}
		currentTitle, currentLines = "", nil
	}

	for _, line := range strings.Split(content, "\n") {
		if title, ok := guideHeadingTitle(line); ok {
			flush()
			currentTitle, currentLines = title, []string{line}
			continue
		}
		if currentLines != nil {
			currentLines = append(currentLines, line)
		} else {
			preface = append(preface, line)
		}
	}
	flush()

	if body := strings.TrimRight(strings.Join(preface, "\n"), " \t\r\n"); strings.TrimSpace(body) != "" {
		chapters = append([]GuideChapter{{Slug: "preface", Title: "前言", Content: body}}, chapters...)
	}

	return chapters
}

// JoinGuideChapters renders chapters back into the single document the public
// page and the legacy `guide_content` key expect.
func JoinGuideChapters(chapters []GuideChapter) string {
	parts := make([]string, 0, len(chapters))
	for _, chapter := range normalizeGuideChapters(chapters) {
		parts = append(parts, chapter.Content)
	}
	return strings.Join(parts, "\n\n")
}

func guideHeadingTitle(line string) (string, bool) {
	if !strings.HasPrefix(line, "## ") && !strings.HasPrefix(line, "##\t") {
		return "", false
	}
	title := strings.TrimSpace(strings.TrimPrefix(line, "##"))
	if title == "" {
		return "", false
	}
	return strings.Join(strings.Fields(title), " "), true
}

// deriveGuideChapterSlug mirrors the frontend derivation so a chapter created
// on either side gets the same anchor: ASCII words from the title when present,
// otherwise a deterministic FNV-1a digest of the title.
func deriveGuideChapterSlug(title string, used map[string]struct{}) string {
	var builder strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(title) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			builder.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			builder.WriteRune('-')
			lastHyphen = true
		}
	}
	base := strings.Trim(builder.String(), "-")
	if len(base) > 48 {
		base = strings.Trim(base[:48], "-")
	}
	if base == "" {
		var hash uint32 = 0x811c9dc5
		for _, r := range title {
			hash ^= uint32(r)
			hash *= 0x01000193
		}
		base = "section-" + strconv.FormatUint(uint64(hash), 36)
	}

	if _, taken := used[base]; !taken {
		return base
	}
	for suffix := 2; ; suffix++ {
		candidate := base + "-" + strconv.Itoa(suffix)
		if _, taken := used[candidate]; !taken {
			return candidate
		}
	}
}

func validateGuideExpectedVersion(current, expected int) error {
	if expected < 0 {
		return infraerrors.BadRequest("GUIDE_VERSION_INVALID", "expected version must not be negative")
	}
	if current != expected {
		return infraerrors.Conflict("GUIDE_VERSION_CONFLICT", "the guide was changed in another session; reload and try again").WithMetadata(map[string]string{
			"current_version": strconv.Itoa(current),
		})
	}
	return nil
}
