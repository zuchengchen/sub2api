package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

const (
	GuideMaxContentBytes = 256 * 1024
	GuideRevisionLimit   = 20
)

// GuideRevision is a restorable snapshot of the public usage guide.
// An empty content value means that the bundled guide was active.
type GuideRevision struct {
	Version   int    `json:"version"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
}

// GuideSettings contains the current guide and its bounded revision history.
type GuideSettings struct {
	Content          string          `json:"content"`
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

// SaveGuideSettings publishes Markdown as a new guide version. expectedVersion
// prevents an older browser tab from silently overwriting a newer edit.
func (s *SettingService) SaveGuideSettings(ctx context.Context, content string, expectedVersion int) (*GuideSettings, error) {
	return s.saveGuideSettings(ctx, content, expectedVersion, false)
}

// ResetGuideSettings switches the public page back to the guide bundled with
// the application while preserving the action as a restorable revision.
func (s *SettingService) ResetGuideSettings(ctx context.Context, expectedVersion int) (*GuideSettings, error) {
	return s.saveGuideSettings(ctx, "", expectedVersion, true)
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
	if err := validateGuideContent(target.Content, true); err != nil {
		return nil, err
	}

	return s.persistGuideSettings(ctx, current, target.Content)
}

func (s *SettingService) saveGuideSettings(ctx context.Context, content string, expectedVersion int, allowEmpty bool) (*GuideSettings, error) {
	if err := validateGuideContent(content, allowEmpty); err != nil {
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
	if content == current.Content {
		return current, nil
	}

	return s.persistGuideSettings(ctx, current, content)
}

func (s *SettingService) loadGuideSettings(ctx context.Context) (*GuideSettings, error) {
	values, err := s.settingRepo.GetMultiple(ctx, []string{
		SettingKeyGuideContent,
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
	return &GuideSettings{
		Content:          content,
		Version:          version,
		UpdatedAt:        strings.TrimSpace(values[SettingKeyGuideUpdatedAt]),
		HasCustomContent: strings.TrimSpace(content) != "",
		Revisions:        revisions,
	}, nil
}

func (s *SettingService) persistGuideSettings(ctx context.Context, current *GuideSettings, content string) (*GuideSettings, error) {
	version := current.Version + 1
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	revisions := append(append([]GuideRevision(nil), current.Revisions...), GuideRevision{
		Version:   version,
		Content:   content,
		UpdatedAt: updatedAt,
	})
	if len(revisions) > GuideRevisionLimit {
		revisions = revisions[len(revisions)-GuideRevisionLimit:]
	}

	revisionsJSON, err := json.Marshal(revisions)
	if err != nil {
		return nil, fmt.Errorf("marshal guide revisions: %w", err)
	}
	if err := s.settingRepo.SetMultiple(ctx, map[string]string{
		SettingKeyGuideContent:   content,
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
		Version:          version,
		UpdatedAt:        updatedAt,
		HasCustomContent: strings.TrimSpace(content) != "",
		Revisions:        revisions,
	}, nil
}

func validateGuideContent(content string, allowEmpty bool) error {
	if !allowEmpty && strings.TrimSpace(content) == "" {
		return infraerrors.BadRequest("GUIDE_CONTENT_REQUIRED", "guide content is required")
	}
	if len(content) > GuideMaxContentBytes {
		return infraerrors.BadRequest("GUIDE_CONTENT_TOO_LARGE", "guide content must not exceed 256 KiB")
	}
	if strings.ContainsRune(content, '\x00') {
		return infraerrors.BadRequest("GUIDE_CONTENT_INVALID", "guide content contains an invalid character")
	}
	return nil
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
