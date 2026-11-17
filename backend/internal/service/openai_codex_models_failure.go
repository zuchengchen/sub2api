package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

// CodexModelsFailureStage is a fixed operation label, never provider data.
type CodexModelsFailureStage string

const (
	CodexModelsStageUnknown            CodexModelsFailureStage = "unknown"
	CodexModelsStageGroupAccountRead   CodexModelsFailureStage = "group_account_read"
	CodexModelsStageConfiguredBuild    CodexModelsFailureStage = "configured_build"
	CodexModelsStageConfiguredMerge    CodexModelsFailureStage = "configured_merge"
	CodexModelsStagePinnedOrder        CodexModelsFailureStage = "pinned_order"
	CodexModelsStageRetiredFilter      CodexModelsFailureStage = "retired_filter"
	CodexModelsStageGroupFilter        CodexModelsFailureStage = "group_filter"
	CodexModelsStageCacheRevalidation  CodexModelsFailureStage = "cache_revalidation"
	CodexModelsStageUpstreamFetch      CodexModelsFailureStage = "upstream_fetch"
	CodexModelsStagePinnedFetch        CodexModelsFailureStage = "pinned_fetch"
	CodexModelsStageMetadataCompletion CodexModelsFailureStage = "metadata_completion"
	CodexModelsStageAccountMapping     CodexModelsFailureStage = "account_mapping"
	CodexModelsStageUserFilter         CodexModelsFailureStage = "user_filter"
)

type codexModelsStageError struct {
	stage CodexModelsFailureStage
	err   error
}

func (e *codexModelsStageError) Error() string {
	return "Codex models manifest failed at " + string(e.stage)
}
func (e *codexModelsStageError) Unwrap() error { return e.err }

func withCodexModelsFailureStage(err error, stage CodexModelsFailureStage) error {
	if err == nil {
		return nil
	}
	return &codexModelsStageError{stage: normalizeCodexModelsFailureStage(stage), err: err}
}

// CodexModelsFailureMetadata exposes only fixed, bounded labels. It deliberately
// never formats err: repository messages, URLs and invalid JSON may be private.
func CodexModelsFailureMetadata(err error, fallback CodexModelsFailureStage) (stage, class string) {
	selected := normalizeCodexModelsFailureStage(fallback)
	var staged *codexModelsStageError
	if errors.As(err, &staged) {
		selected = normalizeCodexModelsFailureStage(staged.stage)
	}
	stage = string(selected)
	switch {
	case errors.Is(err, context.Canceled):
		return stage, "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return stage, "deadline"
	}
	var syntax *json.SyntaxError
	var shape *json.UnmarshalTypeError
	var encodingType *json.UnsupportedTypeError
	var encodingValue *json.UnsupportedValueError
	var marshaler *json.MarshalerError
	switch {
	case errors.As(err, &syntax):
		return stage, "invalid_json"
	case errors.As(err, &shape):
		return stage, "invalid_shape"
	case errors.As(err, &encodingType), errors.As(err, &encodingValue), errors.As(err, &marshaler):
		return stage, "encoding"
	}
	if selected == CodexModelsStageGroupAccountRead {
		return stage, "repository"
	}
	var upstream *codexModelsManifestUpstreamError
	if errors.As(err, &upstream) {
		switch infraerrors.Reason(err) {
		case "OPENAI_MODELS_UNEXPECTED_NOT_MODIFIED", "OPENAI_CODEX_MODELS_UPSTREAM_INVALID_MANIFEST", "OPENAI_MODELS_UPSTREAM_INVALID":
			return stage, "upstream_protocol"
		}
		if upstream.statusCode >= http.StatusBadRequest {
			return stage, "upstream_status"
		}
		return stage, "upstream_transport"
	}
	return stage, "unknown"
}

func normalizeCodexModelsFailureStage(stage CodexModelsFailureStage) CodexModelsFailureStage {
	switch stage {
	case CodexModelsStageGroupAccountRead, CodexModelsStageConfiguredBuild, CodexModelsStageConfiguredMerge,
		CodexModelsStagePinnedOrder, CodexModelsStageRetiredFilter, CodexModelsStageGroupFilter,
		CodexModelsStageCacheRevalidation, CodexModelsStageUpstreamFetch, CodexModelsStagePinnedFetch,
		CodexModelsStageMetadataCompletion, CodexModelsStageAccountMapping, CodexModelsStageUserFilter:
		return stage
	default:
		return CodexModelsStageUnknown
	}
}
