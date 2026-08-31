package service

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/stretchr/testify/require"
)

type guideSettingRepo struct {
	mu     sync.Mutex
	values map[string]string
}

func (r *guideSettingRepo) Get(context.Context, string) (*Setting, error) {
	return nil, ErrSettingNotFound
}

func (r *guideSettingRepo) GetValue(_ context.Context, key string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.values[key]
	if !ok {
		return "", ErrSettingNotFound
	}
	return value, nil
}

func (r *guideSettingRepo) Set(_ context.Context, key, value string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.values[key] = value
	return nil
}

func (r *guideSettingRepo) GetMultiple(_ context.Context, keys []string) (map[string]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make(map[string]string, len(keys))
	for _, key := range keys {
		if value, ok := r.values[key]; ok {
			values[key] = value
		}
	}
	return values, nil
}

func (r *guideSettingRepo) SetMultiple(_ context.Context, values map[string]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, value := range values {
		r.values[key] = value
	}
	return nil
}

func (r *guideSettingRepo) GetAll(context.Context) (map[string]string, error) {
	return map[string]string{}, nil
}

func (r *guideSettingRepo) Delete(_ context.Context, key string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.values, key)
	return nil
}

func newGuideSettingService() *SettingService {
	return NewSettingService(&guideSettingRepo{values: map[string]string{}}, &config.Config{})
}

func guideChaptersOf(contents ...string) []GuideChapter {
	chapters := make([]GuideChapter, 0, len(contents))
	for i, content := range contents {
		chapters = append(chapters, GuideChapter{
			Slug:    "chapter-" + strconv.Itoa(i+1),
			Title:   "Chapter " + strconv.Itoa(i+1),
			Content: content,
		})
	}
	return chapters
}

func TestGuideSettingsSaveRestoreAndReset(t *testing.T) {
	ctx := context.Background()
	service := newGuideSettingService()

	initial, err := service.GetGuideSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, initial.Version)
	require.False(t, initial.HasCustomContent)
	require.Empty(t, initial.Revisions)

	first, err := service.SaveGuideSettings(ctx, guideChaptersOf("## First guide"), 0)
	require.NoError(t, err)
	require.Equal(t, 1, first.Version)
	require.True(t, first.HasCustomContent)
	require.Len(t, first.Revisions, 1)
	require.Len(t, first.Chapters, 1)

	second, err := service.SaveGuideSettings(ctx, guideChaptersOf("## Second guide"), 1)
	require.NoError(t, err)
	require.Equal(t, 2, second.Version)

	restored, err := service.RestoreGuideSettings(ctx, 1, 2)
	require.NoError(t, err)
	require.Equal(t, 3, restored.Version)
	require.Equal(t, "## First guide", restored.Content)
	require.Equal(t, "chapter-1", restored.Chapters[0].Slug)

	reset, err := service.ResetGuideSettings(ctx, 3)
	require.NoError(t, err)
	require.Equal(t, 4, reset.Version)
	require.False(t, reset.HasCustomContent)
	require.Empty(t, reset.Content)

	bundled, err := service.RestoreGuideSettings(ctx, 4, 4)
	require.NoError(t, err)
	require.Equal(t, 5, bundled.Version)
	require.False(t, bundled.HasCustomContent)
}

func TestGuideSettingsRejectsStaleAndInvalidUpdates(t *testing.T) {
	ctx := context.Background()
	service := newGuideSettingService()

	_, err := service.SaveGuideSettings(ctx, guideChaptersOf("## Current"), 0)
	require.NoError(t, err)

	_, err = service.SaveGuideSettings(ctx, guideChaptersOf("## Stale"), 0)
	require.Error(t, err)
	require.Equal(t, "GUIDE_VERSION_CONFLICT", infraerrors.Reason(err))

	_, err = service.SaveGuideSettings(ctx, guideChaptersOf("  "), 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CONTENT_REQUIRED", infraerrors.Reason(err))

	_, err = service.SaveGuideSettings(ctx, guideChaptersOf(strings.Repeat("a", GuideMaxChapterContentBytes+1)), 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CHAPTER_TOO_LARGE", infraerrors.Reason(err))

	// Each chapter is within its own cap, but together they exceed the total.
	oversizedTotal := make([]GuideChapter, 0, 8)
	for i := 0; i < 8; i++ {
		oversizedTotal = append(oversizedTotal, GuideChapter{
			Slug:    "bulk-" + strconv.Itoa(i),
			Title:   "Bulk",
			Content: strings.Repeat("b", GuideMaxChapterContentBytes),
		})
	}
	_, err = service.SaveGuideSettings(ctx, oversizedTotal, 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CONTENT_TOO_LARGE", infraerrors.Reason(err))

	_, err = service.SaveGuideSettings(ctx, []GuideChapter{{Slug: "Bad Slug", Title: "t", Content: "## t"}}, 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CHAPTER_SLUG_INVALID", infraerrors.Reason(err))

	_, err = service.SaveGuideSettings(ctx, []GuideChapter{
		{Slug: "dupe", Title: "a", Content: "## a"},
		{Slug: "dupe", Title: "b", Content: "## b"},
	}, 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CHAPTER_SLUG_DUPLICATE", infraerrors.Reason(err))

	tooMany := make([]GuideChapter, 0, GuideMaxChapters+1)
	for i := 0; i <= GuideMaxChapters; i++ {
		tooMany = append(tooMany, GuideChapter{
			Slug:    "many-" + strconv.Itoa(i),
			Title:   "Many",
			Content: "## Many",
		})
	}
	_, err = service.SaveGuideSettings(ctx, tooMany, 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CHAPTER_LIMIT_EXCEEDED", infraerrors.Reason(err))

	_, err = service.RestoreGuideSettings(ctx, 999, 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_REVISION_NOT_FOUND", infraerrors.Reason(err))
}

func TestSplitGuideIntoChaptersKeepsLegacyAnchorsAndPreface(t *testing.T) {
	legacy := "开头的一段前言。\n\n## 充值：先买兑换码，再兑换余额\n\n买码然后兑换。\n\n" +
		"## 错误码含义与处理方案\n\n看表格。\n\n## 未登记的新章节\n\n正文。\n"

	chapters := SplitGuideIntoChapters(legacy)
	require.Len(t, chapters, 4)

	require.Equal(t, "preface", chapters[0].Slug)
	require.Contains(t, chapters[0].Content, "开头的一段前言。")

	require.Equal(t, "recharge", chapters[1].Slug)
	require.Equal(t, "充值：先买兑换码，再兑换余额", chapters[1].Title)
	require.Equal(t, "error-codes", chapters[2].Slug)

	// A heading absent from the legacy map still gets a deterministic slug.
	require.Regexp(t, `^section-[a-z0-9]+$`, chapters[3].Slug)
	require.Equal(t, chapters[3].Slug, SplitGuideIntoChapters(legacy)[3].Slug)

	// Splitting then joining must not lose any chapter body.
	rejoined := JoinGuideChapters(chapters)
	for _, fragment := range []string{"开头的一段前言。", "买码然后兑换。", "看表格。", "正文。"} {
		require.Contains(t, rejoined, fragment)
	}
}

func TestGuideSettingsMigratesLegacyDocumentOnRead(t *testing.T) {
	ctx := context.Background()
	repo := &guideSettingRepo{values: map[string]string{
		SettingKeyGuideContent:   "## 充值：先买兑换码，再兑换余额\n\n旧的整篇内容。\n\n## 遇到问题先看这里\n\n旧的 FAQ。",
		SettingKeyGuideVersion:   "5",
		SettingKeyGuideUpdatedAt: "2026-08-30T12:00:00Z",
	}}
	service := NewSettingService(repo, &config.Config{})

	settings, err := service.GetGuideSettings(ctx)
	require.NoError(t, err)
	require.True(t, settings.HasCustomContent)
	require.Len(t, settings.Chapters, 2)
	require.Equal(t, "recharge", settings.Chapters[0].Slug)
	require.Equal(t, "faq", settings.Chapters[1].Slug)
	require.Contains(t, settings.Chapters[0].Content, "旧的整篇内容。")

	// Editing one migrated chapter must leave the other untouched.
	edited := append([]GuideChapter(nil), settings.Chapters...)
	edited[1].Content = "## 遇到问题先看这里\n\n新的 FAQ。"
	updated, err := service.SaveGuideSettings(ctx, edited, 5)
	require.NoError(t, err)
	require.Equal(t, 6, updated.Version)
	require.Contains(t, updated.Chapters[0].Content, "旧的整篇内容。")
	require.Contains(t, updated.Chapters[1].Content, "新的 FAQ。")
}

func TestGuideSettingsKeepsBoundedHistory(t *testing.T) {
	ctx := context.Background()
	service := newGuideSettingService()

	version := 0
	for i := 0; i < GuideRevisionLimit+3; i++ {
		updated, err := service.SaveGuideSettings(ctx, guideChaptersOf("## Revision "+strconv.Itoa(i+1)), version)
		require.NoError(t, err)
		version = updated.Version
	}

	settings, err := service.GetGuideSettings(ctx)
	require.NoError(t, err)
	require.Len(t, settings.Revisions, GuideRevisionLimit)
	require.Equal(t, 4, settings.Revisions[0].Version)
	require.Equal(t, GuideRevisionLimit+3, settings.Revisions[len(settings.Revisions)-1].Version)
}

func TestGuideSettingsIgnoresCorruptHistoryWithoutHidingPublishedContent(t *testing.T) {
	ctx := context.Background()
	repo := &guideSettingRepo{values: map[string]string{
		SettingKeyGuideContent:   "## Still public",
		SettingKeyGuideVersion:   "2",
		SettingKeyGuideUpdatedAt: "2026-08-30T12:00:00Z",
		SettingKeyGuideRevisions: "not-json",
	}}
	service := NewSettingService(repo, &config.Config{})

	settings, err := service.GetGuideSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, "## Still public", settings.Content)
	require.Empty(t, settings.Revisions)

	updated, err := service.SaveGuideSettings(ctx, guideChaptersOf("## Repaired history"), 2)
	require.NoError(t, err)
	require.Equal(t, 3, updated.Version)
	require.Len(t, updated.Revisions, 1)
}
