package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const contentModerationRemoteConsensusVotes = 1

func normalizeContentModerationRemoteResult(result contentModerationSecondLayerResult, threshold float64) contentModerationSecondLayerResult {
	disposition := result.normalizedDisposition()
	if disposition == ContentModerationReviewDispositionViolation && result.Confidence < threshold {
		// A configured threshold can be stricter than the prompt default. A
		// below-threshold violation is safe. Restricted content remains blocked
		// independently because it is an enforcement decision, not an abuse
		// confidence threshold.
		result.setDisposition(ContentModerationReviewDispositionAllow)
		result.Category = "safe"
		result.Reason = ""
		return result
	}
	result.setDisposition(disposition)
	return result
}

func contentModerationConsensusDisposition(primary, confirmation contentModerationSecondLayerResult) string {
	first := primary.normalizedDisposition()
	second := confirmation.normalizedDisposition()
	if first == ContentModerationReviewDispositionViolation && second == ContentModerationReviewDispositionViolation {
		return ContentModerationReviewDispositionViolation
	}
	if first == ContentModerationReviewDispositionRestricted && second == ContentModerationReviewDispositionRestricted {
		return ContentModerationReviewDispositionRestricted
	}
	// Any disagreement involving a blocked verdict remains blocked, but is
	// deliberately non-violating until an independent reviewer confirms both
	// votes as violation.
	if first != ContentModerationReviewDispositionAllow || second != ContentModerationReviewDispositionAllow {
		return ContentModerationReviewDispositionRestricted
	}
	return ContentModerationReviewDispositionAllow
}

type contentModerationRemoteProviderGroup struct {
	provider string
	channels []ContentModerationDeepSeekChannel
}

func contentModerationRemoteProvider(channel ContentModerationDeepSeekChannel) string {
	provider := normalizeContentModerationRemoteProvider(channel.Provider)
	if provider == "" {
		return ContentModerationRemoteProviderDeepSeek
	}
	return provider
}

func contentModerationRemoteProviderGroups(channels []ContentModerationDeepSeekChannel) []contentModerationRemoteProviderGroup {
	channels = normalizeContentModerationDeepSeekChannels(channels)
	groups := make([]contentModerationRemoteProviderGroup, 0, len(channels))
	indexes := make(map[string]int, len(channels))
	for _, channel := range channels {
		if !channel.Enabled {
			continue
		}
		provider := contentModerationRemoteProvider(channel)
		if !isSupportedContentModerationRemoteProvider(provider) {
			continue
		}
		if index, ok := indexes[provider]; ok {
			groups[index].channels = append(groups[index].channels, channel)
			continue
		}
		indexes[provider] = len(groups)
		groups = append(groups, contentModerationRemoteProviderGroup{
			provider: provider,
			channels: []ContentModerationDeepSeekChannel{channel},
		})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return contentModerationRemoteProviderOrder(groups[i].provider) < contentModerationRemoteProviderOrder(groups[j].provider)
	})
	return groups
}

func contentModerationRemoteUsesResponses(provider string) bool {
	switch normalizeContentModerationRemoteProvider(provider) {
	case ContentModerationRemoteProviderQwen, ContentModerationRemoteProviderMiMo:
		return true
	default:
		return false
	}
}

func contentModerationRemoteResponsesURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid remote reviewer Base URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(strings.ToLower(path), "/responses") {
		path += "/responses"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func contentModerationRemoteChatURL(channel ContentModerationDeepSeekChannel) (string, error) {
	if contentModerationRemoteProvider(channel) != ContentModerationRemoteProviderGLM {
		return contentModerationDeepSeekChatURL(channel.BaseURL)
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(channel.BaseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid GLM Base URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if !strings.HasSuffix(strings.ToLower(path), "/chat/completions") {
		path += "/chat/completions"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func buildContentModerationRemotePayload(channel ContentModerationDeepSeekChannel, input contentModerationSecondLayerInput) (map[string]any, error) {
	chatPayload, err := buildContentModerationDeepSeekPayload(channel, input)
	if err != nil {
		return nil, err
	}
	provider := contentModerationRemoteProvider(channel)
	if !contentModerationRemoteUsesResponses(provider) {
		if provider == ContentModerationRemoteProviderQwen {
			delete(chatPayload, "thinking")
			chatPayload["enable_thinking"] = false
		}
		return chatPayload, nil
	}
	messages, ok := chatPayload["messages"].([]map[string]string)
	if !ok || len(messages) != 2 {
		return nil, errors.New("remote reviewer prompt is invalid")
	}
	return map[string]any{
		"model":             channel.Model,
		"instructions":      messages[0]["content"],
		"input":             messages[1]["content"],
		"stream":            false,
		"max_output_tokens": 96,
		"reasoning":         map[string]string{"effort": "none"},
		"text":              map[string]any{"format": map[string]string{"type": "json_object"}},
	}, nil
}

func parseContentModerationRemoteResponses(body []byte) (contentModerationSecondLayerResult, error) {
	var envelope struct {
		Status     string `json:"status"`
		OutputText string `json:"output_text"`
		Output     []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return contentModerationSecondLayerResult{}, errors.New("invalid remote reviewer Responses envelope")
	}
	if envelope.Status != "" && !strings.EqualFold(strings.TrimSpace(envelope.Status), "completed") {
		return contentModerationSecondLayerResult{}, errors.New("remote reviewer Responses request did not complete")
	}
	parts := make([]string, 0, 2)
	if strings.TrimSpace(envelope.OutputText) != "" {
		parts = append(parts, strings.TrimSpace(envelope.OutputText))
	}
	for _, item := range envelope.Output {
		if strings.EqualFold(strings.TrimSpace(item.Type), "reasoning") {
			return contentModerationSecondLayerResult{}, errors.New("remote reviewer returned reasoning output")
		}
		if !strings.EqualFold(strings.TrimSpace(item.Type), "message") {
			continue
		}
		for _, content := range item.Content {
			if (content.Type == "output_text" || content.Type == "text") && strings.TrimSpace(content.Text) != "" {
				parts = append(parts, strings.TrimSpace(content.Text))
			}
		}
	}
	if len(parts) == 0 {
		return contentModerationSecondLayerResult{}, errors.New("remote reviewer Responses output is empty")
	}
	content := parts[len(parts)-1]
	fakeEnvelope, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": content},
		}},
	})
	if err != nil {
		return contentModerationSecondLayerResult{}, err
	}
	return parseContentModerationDeepSeekResponse(fakeEnvelope)
}

func (s *ContentModerationService) callContentModerationRemoteChannel(
	ctx context.Context,
	channel ContentModerationDeepSeekChannel,
	input contentModerationSecondLayerInput,
	bypassBreaker bool,
) (contentModerationSecondLayerResult, ContentModerationReviewAttempt, error) {
	provider := contentModerationRemoteProvider(channel)
	attempt := ContentModerationReviewAttempt{
		Reviewer: provider, Provider: provider, ChannelID: channel.ID, ChannelName: channel.Name, Model: channel.Model,
	}
	state := s.deepSeekChannelState(channel)
	configDigest := contentModerationDeepSeekChannelDigest(channel)
	if allowed, reason := state.begin(time.Now(), bypassBreaker); !allowed {
		attempt.Outcome = "skipped"
		attempt.Error = reason
		return contentModerationSecondLayerResult{}, attempt, fmt.Errorf("remote reviewer %s is %s", channel.ID, reason)
	}
	started := time.Now()
	finish := func(callErr error) {
		latency := int(time.Since(started).Milliseconds())
		attempt.LatencyMS = latency
		state.finish(time.Now(), latency, callErr, configDigest)
	}
	payloadValue, err := buildContentModerationRemotePayload(channel, input)
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "payload"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "payload"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	endpoint := ""
	if contentModerationRemoteUsesResponses(provider) {
		endpoint, err = contentModerationRemoteResponsesURL(channel.BaseURL)
	} else {
		endpoint, err = contentModerationRemoteChatURL(channel)
	}
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "base_url"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(channel.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = "request"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+channel.APIKey)
	resp, err := s.deepSeekHTTPClient(channel).Do(req)
	if err != nil {
		finish(err)
		attempt.Outcome = "error"
		attempt.Error = contentModerationDeepSeekCallErrorKind(ctx, requestCtx, err, "network")
		return contentModerationSecondLayerResult{}, attempt, err
	}
	defer func() { _ = resp.Body.Close() }()
	attempt.HTTPStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, contentModerationDeepSeekMaxResponseBytes))
		httpErr := &contentModerationDeepSeekHTTPError{
			status:     resp.StatusCode,
			retryAfter: contentModerationDeepSeekRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		}
		finish(httpErr)
		attempt.Outcome = "error"
		attempt.Error = "http_" + strconv.Itoa(resp.StatusCode)
		return contentModerationSecondLayerResult{}, attempt, httpErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, contentModerationDeepSeekMaxResponseBytes+1))
	if err != nil || len(body) > contentModerationDeepSeekMaxResponseBytes {
		if err == nil {
			err = errors.New("remote reviewer response too large")
			attempt.Error = "response_too_large"
		} else {
			attempt.Error = contentModerationDeepSeekCallErrorKind(ctx, requestCtx, err, "response_read")
		}
		finish(err)
		attempt.Outcome = "error"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	if contentModerationRemoteUsesResponses(provider) {
		result, err := parseContentModerationRemoteResponses(body)
		finish(err)
		if err != nil {
			attempt.Outcome = "error"
			attempt.Error = "invalid_json"
			return contentModerationSecondLayerResult{}, attempt, fmt.Errorf("%w: %v", errContentModerationSecondLayerParse, err)
		}
		return contentModerationRemoteSuccess(result, attempt, channel, input)
	}
	result, err := parseContentModerationDeepSeekResponse(body)
	finish(err)
	if err != nil {
		attempt.Outcome = "error"
		attempt.Error = "invalid_json"
		return contentModerationSecondLayerResult{}, attempt, fmt.Errorf("%w: %v", errContentModerationSecondLayerParse, err)
	}
	return contentModerationRemoteSuccess(result, attempt, channel, input)
}

func contentModerationRemoteSuccess(
	result contentModerationSecondLayerResult,
	attempt ContentModerationReviewAttempt,
	channel ContentModerationDeepSeekChannel,
	input contentModerationSecondLayerInput,
) (contentModerationSecondLayerResult, ContentModerationReviewAttempt, error) {
	provider := contentModerationRemoteProvider(channel)
	result.Profile = provider + ":" + channel.Model
	result.PromptVersion = ContentModerationDeepSeekPromptVersion
	if result.ParserStatus == "" {
		result.ParserStatus = "parsed"
	}
	result.EvidenceMode = input.Evidence.Mode
	result.EvidenceTruncated = input.Evidence.Truncated
	result.EndpointID = channel.ID
	result.KeywordTier = input.KeywordTier
	result.KeywordRuleID = input.KeywordRuleID
	attempt.Outcome = "success"
	attempt.Confidence = result.Confidence
	switch result.normalizedDisposition() {
	case ContentModerationReviewDispositionViolation:
		attempt.Verdict = "violation"
	case ContentModerationReviewDispositionRestricted:
		attempt.Verdict = "restricted"
	default:
		attempt.Verdict = "safe"
	}
	return result, attempt, nil
}

func (s *ContentModerationService) reviewContentModerationRemoteProvider(
	ctx context.Context,
	group contentModerationRemoteProviderGroup,
	input contentModerationSecondLayerInput,
) (contentModerationSecondLayerResult, []ContentModerationReviewAttempt, bool, error) {
	attempts := make([]ContentModerationReviewAttempt, 0, len(group.channels)*2)
	retryChannels := make([]ContentModerationDeepSeekChannel, 0, len(group.channels))
	var failures []error
	attempted := false
	for _, channel := range group.channels {
		if strings.TrimSpace(channel.APIKey) == "" {
			attempts = append(attempts, ContentModerationReviewAttempt{
				Reviewer: group.provider, Provider: group.provider, ChannelID: channel.ID,
				ChannelName: channel.Name, Model: channel.Model, Outcome: "skipped", Error: "key_unavailable",
			})
			failures = append(failures, fmt.Errorf("remote reviewer %s API key is unavailable", channel.ID))
			continue
		}
		attempted = true
		var result contentModerationSecondLayerResult
		var attempt ContentModerationReviewAttempt
		var err error
		if group.provider == ContentModerationRemoteProviderDeepSeek {
			result, attempt, err = s.callContentModerationDeepSeekChannel(ctx, channel, input, false)
		} else {
			result, attempt, err = s.callContentModerationRemoteChannel(ctx, channel, input, false)
		}
		attempts = append(attempts, attempt)
		if err == nil {
			return result, attempts, true, nil
		}
		failures = append(failures, err)
		if contentModerationDeepSeekAttemptRetryable(attempt) {
			retryChannels = append(retryChannels, channel)
		}
		if ctx.Err() != nil {
			break
		}
	}
	for _, channel := range retryChannels {
		if !contentModerationDeepSeekHasRetryBudget(ctx, channel) {
			continue
		}
		var result contentModerationSecondLayerResult
		var attempt ContentModerationReviewAttempt
		var err error
		if group.provider == ContentModerationRemoteProviderDeepSeek {
			result, attempt, err = s.callContentModerationDeepSeekChannel(ctx, channel, input, false)
		} else {
			result, attempt, err = s.callContentModerationRemoteChannel(ctx, channel, input, false)
		}
		attempts = append(attempts, attempt)
		if err == nil {
			return result, attempts, true, nil
		}
		failures = append(failures, err)
		if ctx.Err() != nil {
			break
		}
	}
	if len(failures) == 0 {
		failures = append(failures, fmt.Errorf("no enabled %s reviewer", group.provider))
	}
	return contentModerationSecondLayerResult{}, attempts, attempted, errors.Join(failures...)
}

func markContentModerationSuccessfulAttempt(attempts []ContentModerationReviewAttempt, role string) {
	for index := len(attempts) - 1; index >= 0; index-- {
		if attempts[index].Outcome == "success" {
			attempts[index].Role = role
			return
		}
	}
}

func (s *ContentModerationService) scanContentModerationRemotePool(
	ctx context.Context,
	cfg *ContentModerationConfig,
	input contentModerationSecondLayerInput,
) (contentModerationSecondLayerResult, bool, error) {
	if cfg == nil || !contentModerationRemoteReviewersEnabled(cfg) {
		return contentModerationSecondLayerResult{}, false, nil
	}
	totalTimeout := cfg.DeepSeekTotalTimeoutMS
	if totalTimeout <= 0 {
		totalTimeout = DefaultContentModerationDeepSeekTotalTimeoutMS
	}
	totalCtx, cancel := context.WithTimeout(ctx, time.Duration(totalTimeout)*time.Millisecond)
	defer cancel()
	groups := contentModerationRemoteProviderGroups(cfg.DeepSeekChannels)
	allAttempts := make([]ContentModerationReviewAttempt, 0, len(cfg.DeepSeekChannels)*2)
	var failures []error
	attempted := false
	var primary *contentModerationSecondLayerResult
	primaryProvider := ""
	failoverUsed := false
	threshold := cfg.DeepSeekThreshold
	if threshold <= 0 {
		threshold = DefaultContentModerationDeepSeekThreshold
	}
	requiredVotes := contentModerationRemoteConsensusVotesRequired(cfg)
	for _, group := range groups {
		result, attempts, groupAttempted, err := s.reviewContentModerationRemoteProvider(totalCtx, group, input)
		attempted = attempted || groupAttempted
		if err != nil {
			failoverUsed = true
			allAttempts = append(allAttempts, attempts...)
			failures = append(failures, err)
			if totalCtx.Err() != nil {
				break
			}
			continue
		}
		result = normalizeContentModerationRemoteResult(result, threshold)
		if len(attempts) > 0 {
			for index := range attempts {
				if attempts[index].Outcome == "success" {
					attempts[index].Confidence = result.Confidence
					switch result.normalizedDisposition() {
					case ContentModerationReviewDispositionViolation:
						attempts[index].Verdict = "violation"
					case ContentModerationReviewDispositionRestricted:
						attempts[index].Verdict = "restricted"
					default:
						attempts[index].Verdict = "safe"
					}
				}
			}
		}
		if primary == nil {
			markContentModerationSuccessfulAttempt(attempts, "primary")
			allAttempts = append(allAttempts, attempts...)
			result.ReviewAttempts = allAttempts
			result.RemoteVotes = 1
			if requiredVotes <= 1 {
				switch result.normalizedDisposition() {
				case ContentModerationReviewDispositionViolation:
					result.ConsensusStatus = "single_violation"
				case ContentModerationReviewDispositionRestricted:
					result.ConsensusStatus = "single_restricted"
				default:
					result.ConsensusStatus = "primary_safe"
				}
				s.deepSeekSelectedCount.Add(1)
				if failoverUsed {
					s.deepSeekFailoverCount.Add(1)
				}
				return result, true, nil
			}
			copyResult := result
			primary = &copyResult
			primaryProvider = group.provider
			continue
		}

		markContentModerationSuccessfulAttempt(attempts, "confirmation")
		allAttempts = append(allAttempts, attempts...)
		primary.ReviewAttempts = allAttempts
		primary.RemoteVotes = 2
		primaryDisposition := primary.normalizedDisposition()
		confirmationDisposition := result.normalizedDisposition()
		consensus := contentModerationConsensusDisposition(*primary, result)
		if consensus == ContentModerationReviewDispositionRestricted {
			switch {
			case primaryDisposition == ContentModerationReviewDispositionAllow &&
				confirmationDisposition != ContentModerationReviewDispositionAllow:
				primary.Confidence = result.Confidence
				primary.Reason = result.Reason
				primary.Label = result.Label
			case confirmationDisposition != ContentModerationReviewDispositionAllow && result.Confidence < primary.Confidence:
				primary.Confidence = result.Confidence
			}
		} else if result.Confidence < primary.Confidence {
			primary.Confidence = result.Confidence
		}
		primary.setDisposition(consensus)
		if consensus == ContentModerationReviewDispositionViolation {
			primary.ConsensusStatus = "confirmed_violation"
		} else if primaryDisposition == ContentModerationReviewDispositionRestricted &&
			confirmationDisposition == ContentModerationReviewDispositionRestricted {
			primary.ConsensusStatus = "confirmed_restricted"
		} else if consensus == ContentModerationReviewDispositionRestricted {
			primary.ReviewerMismatch = true
			primary.ConsensusStatus = "disagreement_restricted"
		} else {
			primary.ConsensusStatus = "confirmed_safe"
		}
		if consensus == ContentModerationReviewDispositionRestricted {
			primary.Category = ContentModerationRestrictedCategory
		}
		primary.Profile = "remote_consensus:" + primaryProvider + "+" + group.provider
		s.deepSeekSelectedCount.Add(1)
		if failoverUsed {
			s.deepSeekFailoverCount.Add(1)
		}
		return *primary, true, nil
	}
	if primary != nil {
		if primary.normalizedDisposition() == ContentModerationReviewDispositionAllow {
			primary.ConsensusStatus = "primary_safe"
			primary.ReviewAttempts = allAttempts
			primary.RemoteVotes = 1
			s.deepSeekSelectedCount.Add(1)
			if failoverUsed {
				s.deepSeekFailoverCount.Add(1)
			}
			return *primary, true, nil
		}
		primary.setDisposition(ContentModerationReviewDispositionAllow)
		primary.Category = "safe"
		primary.ConsensusStatus = "consensus_unavailable"
		primary.ReviewAttempts = allAttempts
		primary.RemoteVotes = 1
		s.deepSeekUnavailableCount.Add(1)
		failures = append(failures, errors.New("a second distinct remote reviewer verdict is required"))
		return *primary, true, errors.Join(failures...)
	}
	if len(failures) == 0 {
		failures = append(failures, errors.New("no enabled remote reviewer"))
	}
	s.deepSeekUnavailableCount.Add(1)
	return contentModerationSecondLayerResult{ReviewAttempts: allAttempts, ConsensusStatus: "unavailable"}, attempted, errors.Join(failures...)
}

func (s *ContentModerationService) countReachableContentModerationRemoteProviders(
	cfg *ContentModerationConfig,
	now time.Time,
) int {
	if cfg == nil || !contentModerationRemoteReviewersEnabled(cfg) {
		return 0
	}
	providers := make(map[string]struct{})
	for _, channel := range normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels) {
		if !channel.Enabled || !isSupportedContentModerationRemoteProvider(channel.Provider) ||
			strings.TrimSpace(channel.APIKey) == "" || !s.deepSeekChannelReviewReady(channel, now) {
			continue
		}
		providers[contentModerationRemoteProvider(channel)] = struct{}{}
	}
	return len(providers)
}

func countConfiguredContentModerationRemoteProviders(cfg *ContentModerationConfig) int {
	if cfg == nil || !contentModerationRemoteReviewersEnabled(cfg) {
		return 0
	}
	providers := make(map[string]struct{})
	for _, channel := range normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels) {
		if channel.Enabled && isSupportedContentModerationRemoteProvider(channel.Provider) && strings.TrimSpace(channel.APIKey) != "" {
			providers[contentModerationRemoteProvider(channel)] = struct{}{}
		}
	}
	return len(providers)
}
