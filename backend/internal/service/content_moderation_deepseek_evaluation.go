package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// ContentModerationDeepSeekEvaluationInput is the redacted text and trusted
// metadata accepted by the offline release evaluator. It intentionally has no
// request archive, URL, or media fields.
type ContentModerationDeepSeekEvaluationInput struct {
	Text         string
	ContextClass string
	Role         string
	Kind         string
}

type ContentModerationDeepSeekEvaluationResult struct {
	Blocked       bool                             `json:"blocked"`
	Flagged       bool                             `json:"flagged"`
	Disposition   string                           `json:"disposition"`
	Confidence    float64                          `json:"confidence"`
	Category      string                           `json:"category"`
	ReasonHash    string                           `json:"reason_hash,omitempty"`
	Profile       string                           `json:"profile"`
	PromptVersion string                           `json:"prompt_version"`
	LatencyMS     int                              `json:"latency_ms"`
	Attempts      []ContentModerationReviewAttempt `json:"attempts"`
}

// ContentModerationDeepSeekEvaluator reuses the production transport,
// non-thinking payload, strict parser, ordered failover, and breaker state.
// It is intended for release probes and historical calibration only.
type ContentModerationDeepSeekEvaluator struct {
	service *ContentModerationService
	config  *ContentModerationConfig
}

func NewContentModerationDeepSeekEvaluator(
	channels []ContentModerationDeepSeekChannel,
	totalTimeoutMS int,
) (*ContentModerationDeepSeekEvaluator, error) {
	channels = normalizeContentModerationDeepSeekChannels(cloneContentModerationDeepSeekChannels(channels))
	if err := validateContentModerationDeepSeekChannels(channels); err != nil {
		return nil, err
	}
	if totalTimeoutMS < minContentModerationSecondLayerTimeoutMS || totalTimeoutMS > 120000 {
		return nil, errors.New("DeepSeek evaluation total timeout must be between 100 and 120000 milliseconds")
	}
	cfg := defaultContentModerationConfig()
	cfg.Enabled = true
	cfg.DeepSeekEnabled = true
	// The offline evaluator may intentionally be constructed with one legacy
	// DeepSeek channel; production configs use schema version 1 and require
	// two distinct online votes.
	cfg.RemoteReviewersEnabled = false
	cfg.RemoteReviewersVersion = 0
	cfg.YuFengEnabled = false
	cfg.DeepSeekTotalTimeoutMS = totalTimeoutMS
	cfg.DeepSeekThreshold = DefaultContentModerationDeepSeekThreshold
	cfg.DeepSeekChannels = channels
	return &ContentModerationDeepSeekEvaluator{service: &ContentModerationService{}, config: cfg}, nil
}

func (e *ContentModerationDeepSeekEvaluator) Evaluate(
	ctx context.Context,
	input ContentModerationDeepSeekEvaluationInput,
) (ContentModerationDeepSeekEvaluationResult, error) {
	if e == nil || e.service == nil || e.config == nil {
		return ContentModerationDeepSeekEvaluationResult{}, errors.New("DeepSeek evaluator is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	text := strings.TrimSpace(input.Text)
	if text == "" {
		return ContentModerationDeepSeekEvaluationResult{}, errors.New("DeepSeek evaluation text is empty")
	}
	fragment := ContentModerationFragment{
		Role:         strings.ToLower(strings.TrimSpace(input.Role)),
		Kind:         strings.ToLower(strings.TrimSpace(input.Kind)),
		Path:         "evaluation.input",
		ContextClass: normalizeContentModerationEvaluationContext(input.ContextClass),
		Text:         text,
	}
	updateContentModerationFragmentHash(&fragment)
	started := time.Now()
	result, attempted, err := e.service.scanContentModerationDeepSeek(ctx, e.config, contentModerationSecondLayerInput{
		Fragment:    fragment,
		Evidence:    moderationEvidence{Text: text, Mode: "release_evaluation"},
		KeywordTier: "release_evaluation",
	})
	disposition := result.normalizedDisposition()
	unconfirmedRestriction := errors.Is(err, errContentModerationRestrictedConfirmationUnavailable)
	if unconfirmedRestriction {
		// The evaluator reports a single reviewer's calibration verdict without
		// turning it into an enforceable production decision.
		disposition = ContentModerationReviewDispositionRestricted
		err = nil
	}
	evaluation := ContentModerationDeepSeekEvaluationResult{
		Blocked:     result.Blocked && !unconfirmedRestriction,
		Flagged:     disposition == ContentModerationReviewDispositionViolation,
		Disposition: disposition, Confidence: result.Confidence, Category: result.Category,
		Profile: result.Profile, PromptVersion: result.PromptVersion,
		LatencyMS: int(time.Since(started).Milliseconds()),
		Attempts:  append([]ContentModerationReviewAttempt(nil), result.ReviewAttempts...),
	}
	if reason := strings.TrimSpace(result.Reason); reason != "" {
		digest := sha256.Sum256([]byte(reason))
		evaluation.ReasonHash = hex.EncodeToString(digest[:])
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return evaluation, ctxErr
	}
	if err != nil {
		return evaluation, err
	}
	if !attempted {
		return evaluation, errors.New("DeepSeek evaluation was not attempted")
	}
	return evaluation, nil
}

func normalizeContentModerationEvaluationContext(value string) string {
	switch normalized := strings.ToLower(strings.TrimSpace(value)); normalized {
	case ContentModerationContextUser, ContentModerationContextTool,
		ContentModerationContextAssistant, ContentModerationContextServiceLog,
		ContentModerationContextCode, ContentModerationContextConfig:
		return normalized
	default:
		return ContentModerationContextUnknown
	}
}
