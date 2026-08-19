package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

func isSevereContentModerationAction(action string) bool {
	switch strings.TrimSpace(action) {
	case ContentModerationActionBlock,
		ContentModerationActionHashBlock,
		ContentModerationActionKeywordBlock,
		ContentModerationActionSecondLayerBlock,
		ContentModerationActionCacheBlock,
		ContentModerationActionCyberPolicy:
		return true
	default:
		return false
	}
}

func (s *ContentModerationService) persistContentModerationArchive(ctx context.Context, log *ContentModerationLog, input ContentModerationCheckInput) error {
	if s == nil || s.archiveRuntime == nil || log == nil {
		return errors.New("content moderation archive runtime unavailable")
	}
	archiveID := uuid.NewString()
	capturedAt := log.CreatedAt
	if capturedAt.IsZero() {
		capturedAt = time.Now().UTC()
	}
	request := input.RawRequest.CloneMetadata()
	body := input.RawRequest.Body
	if body == nil {
		body = input.Body
	}
	transport := strings.TrimSpace(request.Transport)
	if transport == "" {
		transport = "http"
	}
	stage := strings.TrimSpace(request.Stage)
	if stage == "" {
		stage = "http"
	}
	envelope := ContentModerationArchiveEnvelope{
		ArchiveID:  archiveID,
		Version:    ContentModerationArchiveVersion,
		CapturedAt: capturedAt,
		Request: ContentModerationArchiveRequest{
			Method:     request.Method,
			Target:     request.Target,
			Headers:    request.Headers,
			BodyBase64: base64.StdEncoding.EncodeToString(body),
			Transport:  transport,
			Stage:      stage,
		},
		InputHash:         log.InputHash,
		Action:            log.Action,
		DispositionStatus: log.DispositionStatus,
		DispositionTarget: log.DispositionTarget,
		Incomplete:        log.ArchiveIncomplete,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	return s.archiveRuntime.storeWithArchiveID(ctx, log, archiveID, raw)
}

func (s *ContentModerationService) CloseContentModerationRuntime() {
	if s == nil {
		return
	}
	s.stopBackgroundHealthWorkers()
	if s.archiveRuntime != nil {
		s.archiveRuntime.Close()
	}
}
