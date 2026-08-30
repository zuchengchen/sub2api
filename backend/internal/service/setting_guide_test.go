package service

import (
	"context"
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

func TestGuideSettingsSaveRestoreAndReset(t *testing.T) {
	ctx := context.Background()
	service := newGuideSettingService()

	initial, err := service.GetGuideSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, 0, initial.Version)
	require.False(t, initial.HasCustomContent)
	require.Empty(t, initial.Revisions)

	first, err := service.SaveGuideSettings(ctx, "# First guide", 0)
	require.NoError(t, err)
	require.Equal(t, 1, first.Version)
	require.True(t, first.HasCustomContent)
	require.Len(t, first.Revisions, 1)

	second, err := service.SaveGuideSettings(ctx, "# Second guide", 1)
	require.NoError(t, err)
	require.Equal(t, 2, second.Version)

	restored, err := service.RestoreGuideSettings(ctx, 1, 2)
	require.NoError(t, err)
	require.Equal(t, 3, restored.Version)
	require.Equal(t, "# First guide", restored.Content)

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

	_, err := service.SaveGuideSettings(ctx, "# Current", 0)
	require.NoError(t, err)

	_, err = service.SaveGuideSettings(ctx, "# Stale", 0)
	require.Error(t, err)
	require.Equal(t, "GUIDE_VERSION_CONFLICT", infraerrors.Reason(err))

	_, err = service.SaveGuideSettings(ctx, "  ", 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CONTENT_REQUIRED", infraerrors.Reason(err))

	_, err = service.SaveGuideSettings(ctx, strings.Repeat("a", GuideMaxContentBytes+1), 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_CONTENT_TOO_LARGE", infraerrors.Reason(err))

	_, err = service.RestoreGuideSettings(ctx, 999, 1)
	require.Error(t, err)
	require.Equal(t, "GUIDE_REVISION_NOT_FOUND", infraerrors.Reason(err))
}

func TestGuideSettingsKeepsBoundedHistory(t *testing.T) {
	ctx := context.Background()
	service := newGuideSettingService()

	version := 0
	for i := 0; i < GuideRevisionLimit+3; i++ {
		updated, err := service.SaveGuideSettings(ctx, strings.Repeat("#", i+1), version)
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
		SettingKeyGuideContent:   "# Still public",
		SettingKeyGuideVersion:   "2",
		SettingKeyGuideUpdatedAt: "2026-08-30T12:00:00Z",
		SettingKeyGuideRevisions: "not-json",
	}}
	service := NewSettingService(repo, &config.Config{})

	settings, err := service.GetGuideSettings(ctx)
	require.NoError(t, err)
	require.Equal(t, "# Still public", settings.Content)
	require.Empty(t, settings.Revisions)

	updated, err := service.SaveGuideSettings(ctx, "# Repaired history", 2)
	require.NoError(t, err)
	require.Equal(t, 3, updated.Version)
	require.Len(t, updated.Revisions, 1)
}
