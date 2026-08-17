package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	contentmoderationassets "github.com/Wei-Shaw/sub2api/resources/content-moderation"
)

const (
	contentModerationDeepSeekHealthTTL        = 15 * time.Minute
	contentModerationDeepSeekCooldown         = 30 * time.Second
	contentModerationDeepSeekRateCooldown     = 60 * time.Second
	contentModerationDeepSeekFailureTrip      = 3
	contentModerationDeepSeekMaxResponseBytes = 64 * 1024
)

var contentModerationDeepSeekCategories = map[string]struct{}{
	"safe": {}, "cyber_abuse": {}, "cracking": {}, "security_bypass": {},
	"account_abuse": {}, "sexual_deepfake": {}, "doxxing": {},
	"violent_threat": {}, "self_harm": {}, "weapons": {}, "sexual_content": {},
}

type ContentModerationReviewAttempt struct {
	Reviewer    string `json:"reviewer"`
	ChannelID   string `json:"channel_id"`
	ChannelName string `json:"channel_name,omitempty"`
	Model       string `json:"model,omitempty"`
	Outcome     string `json:"outcome"`
	LatencyMS   int    `json:"latency_ms"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	Error       string `json:"error,omitempty"`
}

type contentModerationDeepSeekChannelState struct {
	mu sync.Mutex

	configDigest        string
	credentialDigest    string
	consecutiveFailures int
	authDisabled        bool
	cooldownUntil       time.Time
	halfOpenProbe       bool
	healthCheckedAt     time.Time
	healthyUntil        time.Time
	healthy             bool
	lastLatencyMS       int
	lastError           string
}

type contentModerationDeepSeekHTTPError struct {
	status     int
	retryAfter time.Duration
}

func (e *contentModerationDeepSeekHTTPError) Error() string {
	return fmt.Sprintf("DeepSeek HTTP status %d", e.status)
}

func (s *ContentModerationService) deepSeekChannelState(channel ContentModerationDeepSeekChannel) *contentModerationDeepSeekChannelState {
	digest := contentModerationDeepSeekChannelDigest(channel)
	credentialDigest := contentModerationDeepSeekCredentialDigest(channel.APIKey)
	candidate := &contentModerationDeepSeekChannelState{configDigest: digest, credentialDigest: credentialDigest}
	actual, _ := s.deepSeekChannelStates.LoadOrStore(channel.ID, candidate)
	state, ok := actual.(*contentModerationDeepSeekChannelState)
	if !ok || state == nil {
		state = candidate
		s.deepSeekChannelStates.Store(channel.ID, state)
	}
	state.mu.Lock()
	if state.configDigest != digest {
		credentialChanged := state.credentialDigest != credentialDigest
		authDisabled := state.authDisabled && !credentialChanged
		state.configDigest = digest
		state.credentialDigest = credentialDigest
		state.consecutiveFailures = 0
		state.authDisabled = authDisabled
		state.cooldownUntil = time.Time{}
		state.halfOpenProbe = false
		state.healthCheckedAt = time.Time{}
		state.healthyUntil = time.Time{}
		state.healthy = false
		state.lastLatencyMS = 0
		state.lastError = ""
	}
	state.mu.Unlock()
	return state
}

func contentModerationDeepSeekCredentialDigest(apiKey string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(apiKey)))
	return hex.EncodeToString(digest[:])
}

func contentModerationDeepSeekChannelDigest(channel ContentModerationDeepSeekChannel) string {
	keyHash := sha256.Sum256([]byte(channel.APIKey))
	canonical := strings.Join([]string{
		strings.TrimSpace(channel.ID), strings.TrimSpace(channel.BaseURL), strings.TrimSpace(channel.Model),
		strconv.Itoa(channel.TimeoutMS), hex.EncodeToString(keyHash[:]), ContentModerationDeepSeekPromptVersion,
	}, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(digest[:])
}

func (state *contentModerationDeepSeekChannelState) begin(now time.Time, bypass bool) (bool, string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if bypass {
		return true, ""
	}
	if state.authDisabled {
		return false, "auth_disabled"
	}
	if now.Before(state.cooldownUntil) {
		return false, "cooldown"
	}
	if !state.cooldownUntil.IsZero() {
		if state.halfOpenProbe {
			return false, "half_open_busy"
		}
		state.halfOpenProbe = true
	}
	return true, ""
}

func (state *contentModerationDeepSeekChannelState) finish(now time.Time, latency int, callErr error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.halfOpenProbe = false
	state.lastLatencyMS = latency
	if callErr == nil {
		state.consecutiveFailures = 0
		state.authDisabled = false
		state.cooldownUntil = time.Time{}
		state.lastError = ""
		return
	}
	state.lastError = trimRunes(redactContentModerationSecrets(callErr.Error()), 240)
	var httpErr *contentModerationDeepSeekHTTPError
	if errors.As(callErr, &httpErr) {
		switch httpErr.status {
		case http.StatusUnauthorized, http.StatusForbidden:
			state.authDisabled = true
			state.cooldownUntil = time.Time{}
			state.healthy = false
			state.healthyUntil = time.Time{}
			return
		case http.StatusTooManyRequests, 529:
			cooldown := httpErr.retryAfter
			if cooldown <= 0 {
				cooldown = contentModerationDeepSeekRateCooldown
			}
			state.cooldownUntil = now.Add(cooldown)
			state.healthy = false
			state.healthyUntil = time.Time{}
			return
		}
	}
	state.consecutiveFailures++
	state.healthy = false
	state.healthyUntil = time.Time{}
	if state.consecutiveFailures >= contentModerationDeepSeekFailureTrip {
		state.cooldownUntil = now.Add(contentModerationDeepSeekCooldown)
	}
}

func (state *contentModerationDeepSeekChannelState) markHealth(now time.Time, healthy bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.healthCheckedAt = now
	state.healthy = healthy
	if healthy {
		state.healthyUntil = now.Add(contentModerationDeepSeekHealthTTL)
	} else {
		state.healthyUntil = time.Time{}
	}
}

func (state *contentModerationDeepSeekChannelState) markCredentialUnavailable(now time.Time, credentialErr error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.authDisabled = true
	state.cooldownUntil = time.Time{}
	state.halfOpenProbe = false
	state.healthCheckedAt = now
	state.healthy = false
	state.healthyUntil = time.Time{}
	state.lastLatencyMS = 0
	state.lastError = "credential_unavailable"
	if credentialErr != nil {
		state.lastError = trimRunes(redactContentModerationSecrets(credentialErr.Error()), 240)
	}
}

func (state *contentModerationDeepSeekChannelState) snapshot(now time.Time) (health, breaker string, checkedAt, healthyUntil, cooldownUntil *time.Time, latency int, lastError string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	health = "untested"
	if !state.healthCheckedAt.IsZero() {
		checked := state.healthCheckedAt
		checkedAt = &checked
		if state.healthy && now.Before(state.healthyUntil) {
			health = "healthy"
			until := state.healthyUntil
			healthyUntil = &until
		} else {
			health = "unhealthy"
		}
	}
	breaker = "closed"
	switch {
	case state.authDisabled:
		breaker = "auth_disabled"
	case now.Before(state.cooldownUntil):
		breaker = "cooldown"
		until := state.cooldownUntil
		cooldownUntil = &until
	case !state.cooldownUntil.IsZero():
		breaker = "half_open"
	}
	return health, breaker, checkedAt, healthyUntil, cooldownUntil, state.lastLatencyMS, state.lastError
}

func (s *ContentModerationService) deepSeekHTTPClient(channel ContentModerationDeepSeekChannel) *http.Client {
	key := strings.Join([]string{channel.BaseURL, strconv.Itoa(channel.TimeoutMS)}, "\x00")
	if existing, ok := s.deepSeekHTTPClients.Load(key); ok {
		if client, ok := existing.(*http.Client); ok && client != nil {
			return client
		}
	}
	timeout := time.Duration(channel.TimeoutMS) * time.Millisecond
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	parsedBaseURL, _ := url.Parse(channel.BaseURL)
	allowLoopback := parsedBaseURL != nil && parsedBaseURL.Scheme == "http"
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy: nil, ForceAttemptHTTP2: true,
			DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				return contentModerationDeepSeekDialContext(ctx, dialer, network, address, allowLoopback)
			},
			MaxConnsPerHost: 0, MaxIdleConns: 256, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: timeout, ResponseHeaderTimeout: timeout,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
	actual, _ := s.deepSeekHTTPClients.LoadOrStore(key, client)
	if shared, ok := actual.(*http.Client); ok && shared != nil {
		return shared
	}
	return client
}

func contentModerationDeepSeekDialContext(
	ctx context.Context,
	dialer *net.Dialer,
	network string,
	address string,
	allowLoopback bool,
) (net.Conn, error) {
	if dialer == nil {
		return nil, errors.New("DeepSeek dialer is unavailable")
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid DeepSeek upstream address: %w", err)
	}
	var ips []net.IP
	if literal := net.ParseIP(strings.Trim(host, "[]")); literal != nil {
		ips = []net.IP{literal}
	} else {
		resolved, resolveErr := net.DefaultResolver.LookupIPAddr(ctx, host)
		if resolveErr != nil {
			return nil, fmt.Errorf("resolve DeepSeek upstream: %w", resolveErr)
		}
		for _, item := range resolved {
			ips = append(ips, item.IP)
		}
	}
	if len(ips) == 0 {
		return nil, errors.New("DeepSeek upstream resolved no addresses")
	}
	for _, ip := range ips {
		if contentModerationDeepSeekPublicIP(ip) || (allowLoopback && ip.IsLoopback()) {
			continue
		}
		return nil, errors.New("DeepSeek upstream resolved to a non-public address")
	}
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, fmt.Errorf("connect DeepSeek upstream: %w", lastErr)
}

func contentModerationDeepSeekChatURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(baseURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid DeepSeek Base URL")
	}
	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case strings.HasSuffix(strings.ToLower(path), "/chat/completions"):
	case strings.HasSuffix(strings.ToLower(path), "/v1"):
		path += "/chat/completions"
	default:
		path += "/v1/chat/completions"
	}
	parsed.Path = path
	parsed.RawPath = ""
	return parsed.String(), nil
}

func contentModerationDeepSeekPrompt() (string, error) {
	asset, err := contentmoderationassets.Load(contentmoderationassets.DeepSeekV4FlashAuditV1)
	if err != nil {
		return "", err
	}
	prompt := strings.TrimSpace(asset.SystemPrompt)
	if prompt == "" {
		return "", errors.New("DeepSeek moderation system prompt is empty")
	}
	return prompt, nil
}

func buildContentModerationDeepSeekPayload(channel ContentModerationDeepSeekChannel, input contentModerationSecondLayerInput) (map[string]any, error) {
	prompt, err := contentModerationDeepSeekPrompt()
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(map[string]string{"content": input.Evidence.Text})
	if err != nil {
		return nil, err
	}
	trusted, err := json.Marshal(map[string]string{
		"context_class": input.Fragment.ContextClass,
		"role":          input.Fragment.Role,
		"kind":          input.Fragment.Kind,
	})
	if err != nil {
		return nil, err
	}
	message := "<trusted_context>" + string(trusted) + "</trusted_context>\n<user_input>" + string(data) + "</user_input>"
	return map[string]any{
		"model": channel.Model,
		"messages": []map[string]string{
			{"role": "system", "content": prompt},
			{"role": "user", "content": message},
		},
		"thinking":        map[string]string{"type": "disabled"},
		"response_format": map[string]string{"type": "json_object"},
		"temperature":     0,
		"max_tokens":      64,
		"stream":          false,
	}, nil
}

func (s *ContentModerationService) callContentModerationDeepSeekChannel(
	ctx context.Context,
	channel ContentModerationDeepSeekChannel,
	input contentModerationSecondLayerInput,
	bypassBreaker bool,
) (contentModerationSecondLayerResult, ContentModerationReviewAttempt, error) {
	attempt := ContentModerationReviewAttempt{Reviewer: "deepseek", ChannelID: channel.ID, ChannelName: channel.Name, Model: channel.Model}
	state := s.deepSeekChannelState(channel)
	if allowed, reason := state.begin(time.Now(), bypassBreaker); !allowed {
		attempt.Outcome = "skipped"
		attempt.Error = reason
		return contentModerationSecondLayerResult{}, attempt, fmt.Errorf("DeepSeek channel %s is %s", channel.ID, reason)
	}
	started := time.Now()
	payloadValue, err := buildContentModerationDeepSeekPayload(channel, input)
	if err != nil {
		state.finish(time.Now(), 0, err)
		attempt.Outcome = "error"
		attempt.Error = "payload"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	payload, err := json.Marshal(payloadValue)
	if err != nil {
		state.finish(time.Now(), 0, err)
		attempt.Outcome = "error"
		attempt.Error = "payload"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	endpoint, err := contentModerationDeepSeekChatURL(channel.BaseURL)
	if err != nil {
		state.finish(time.Now(), 0, err)
		attempt.Outcome = "error"
		attempt.Error = "base_url"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(channel.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		state.finish(time.Now(), 0, err)
		attempt.Outcome = "error"
		attempt.Error = "request"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+channel.APIKey)
	resp, err := s.deepSeekHTTPClient(channel).Do(req)
	latency := int(time.Since(started).Milliseconds())
	attempt.LatencyMS = latency
	if err != nil {
		state.finish(time.Now(), latency, err)
		attempt.Outcome = "error"
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(requestCtx.Err(), context.DeadlineExceeded) {
			attempt.Error = "timeout"
		} else {
			attempt.Error = "network"
		}
		return contentModerationSecondLayerResult{}, attempt, err
	}
	defer func() { _ = resp.Body.Close() }()
	attempt.HTTPStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, contentModerationDeepSeekMaxResponseBytes))
		httpErr := &contentModerationDeepSeekHTTPError{status: resp.StatusCode, retryAfter: contentModerationDeepSeekRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
		state.finish(time.Now(), latency, httpErr)
		attempt.Outcome = "error"
		attempt.Error = "http_" + strconv.Itoa(resp.StatusCode)
		return contentModerationSecondLayerResult{}, attempt, httpErr
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, contentModerationDeepSeekMaxResponseBytes+1))
	if err != nil || len(body) > contentModerationDeepSeekMaxResponseBytes {
		if err == nil {
			err = errors.New("DeepSeek response too large")
		}
		state.finish(time.Now(), latency, err)
		attempt.Outcome = "error"
		attempt.Error = "response_read"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	result, err := parseContentModerationDeepSeekResponse(body)
	state.finish(time.Now(), latency, err)
	if err != nil {
		attempt.Outcome = "error"
		attempt.Error = "invalid_json"
		return contentModerationSecondLayerResult{}, attempt, err
	}
	result.Profile = "deepseek_v4_flash"
	result.PromptVersion = ContentModerationDeepSeekPromptVersion
	result.ParserStatus = "parsed"
	result.EvidenceMode = input.Evidence.Mode
	result.EvidenceTruncated = input.Evidence.Truncated
	result.EndpointID = channel.ID
	result.KeywordTier = input.KeywordTier
	result.KeywordRuleID = input.KeywordRuleID
	attempt.Outcome = "success"
	return result, attempt, nil
}

func contentModerationDeepSeekRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if deadline, err := http.ParseTime(value); err == nil && deadline.After(now) {
		return deadline.Sub(now)
	}
	return 0
}

func parseContentModerationDeepSeekResponse(body []byte) (contentModerationSecondLayerResult, error) {
	var envelope struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content          string          `json:"content"`
				ReasoningContent json.RawMessage `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Choices) == 0 {
		return contentModerationSecondLayerResult{}, errors.New("invalid DeepSeek response envelope")
	}
	if strings.EqualFold(strings.TrimSpace(envelope.Choices[0].FinishReason), "length") {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek response was truncated")
	}
	reasoning := bytes.TrimSpace(envelope.Choices[0].Message.ReasoningContent)
	if len(reasoning) > 0 && !bytes.Equal(reasoning, []byte("null")) && !bytes.Equal(reasoning, []byte(`""`)) {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek returned reasoning_content")
	}
	content := strings.TrimSpace(envelope.Choices[0].Message.Content)
	if content == "" {
		return contentModerationSecondLayerResult{}, errors.New("empty DeepSeek response content")
	}
	var decision struct {
		Confidence *float64 `json:"confidence"`
		Category   *string  `json:"category"`
		Reason     *string  `json:"reason"`
	}
	decoder := json.NewDecoder(strings.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil {
		return contentModerationSecondLayerResult{}, fmt.Errorf("invalid DeepSeek decision JSON: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek decision contains trailing data")
	}
	if decision.Confidence == nil || decision.Category == nil || decision.Reason == nil {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek decision is missing required fields")
	}
	confidence := *decision.Confidence
	category := strings.TrimSpace(*decision.Category)
	reason := strings.TrimSpace(*decision.Reason)
	if confidence < 0 || confidence > 1 {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek confidence is out of range")
	}
	if _, ok := contentModerationDeepSeekCategories[category]; !ok {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek category is unknown")
	}
	if utf8.RuneCountInString(reason) > 20 {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek reason exceeds 20 characters")
	}
	reason = redactContentModerationAuditReason(reason)
	if confidence < DefaultContentModerationDeepSeekThreshold && category != "safe" {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek safe decision has a risk category")
	}
	if confidence >= DefaultContentModerationDeepSeekThreshold && category == "safe" {
		return contentModerationSecondLayerResult{}, errors.New("DeepSeek risk decision has a safe category")
	}
	return contentModerationSecondLayerResult{
		Blocked:  confidence >= DefaultContentModerationDeepSeekThreshold,
		Category: category, Confidence: confidence, Reason: reason,
	}, nil
}

func (s *ContentModerationService) scanContentModerationDeepSeek(
	ctx context.Context,
	cfg *ContentModerationConfig,
	input contentModerationSecondLayerInput,
) (contentModerationSecondLayerResult, bool, error) {
	if cfg == nil || !cfg.DeepSeekEnabled {
		return contentModerationSecondLayerResult{}, false, nil
	}
	totalTimeout := cfg.DeepSeekTotalTimeoutMS
	if totalTimeout <= 0 {
		totalTimeout = DefaultContentModerationDeepSeekTotalTimeoutMS
	}
	totalCtx, cancel := context.WithTimeout(ctx, time.Duration(totalTimeout)*time.Millisecond)
	defer cancel()
	channels := normalizeContentModerationDeepSeekChannels(cfg.DeepSeekChannels)
	attempts := make([]ContentModerationReviewAttempt, 0, len(channels))
	var failures []error
	attempted := false
	for _, channel := range channels {
		if !channel.Enabled {
			continue
		}
		if strings.TrimSpace(channel.APIKey) == "" {
			attempts = append(attempts, ContentModerationReviewAttempt{Reviewer: "deepseek", ChannelID: channel.ID, ChannelName: channel.Name, Model: channel.Model, Outcome: "skipped", Error: "key_unavailable"})
			failures = append(failures, fmt.Errorf("DeepSeek channel %s API key is unavailable", channel.ID))
			continue
		}
		attempted = true
		result, attempt, err := s.callContentModerationDeepSeekChannel(totalCtx, channel, input, false)
		attempts = append(attempts, attempt)
		if err == nil {
			result.Blocked = result.Confidence >= cfg.DeepSeekThreshold
			result.ReviewAttempts = attempts
			s.deepSeekSelectedCount.Add(1)
			if len(attempts) > 1 {
				s.deepSeekFailoverCount.Add(1)
			}
			return result, true, nil
		}
		failures = append(failures, err)
		if totalCtx.Err() != nil {
			break
		}
	}
	if len(failures) == 0 {
		failures = append(failures, errors.New("no enabled DeepSeek channel"))
	}
	result := contentModerationSecondLayerResult{ReviewAttempts: attempts}
	s.deepSeekUnavailableCount.Add(1)
	return result, attempted, errors.Join(failures...)
}

func (s *ContentModerationService) testDeepSeekChannelContract(ctx context.Context, channelID string) (*TestContentModerationDeepSeekChannelResult, error) {
	channelID = strings.TrimSpace(channelID)
	if channelID == "" {
		return nil, errors.New("DeepSeek channel ID is required")
	}
	cfg, err := s.loadConfig(ctx)
	if err != nil {
		return nil, err
	}
	var channel *ContentModerationDeepSeekChannel
	for i := range cfg.DeepSeekChannels {
		if cfg.DeepSeekChannels[i].ID == channelID {
			channel = &cfg.DeepSeekChannels[i]
			break
		}
	}
	if channel == nil {
		return nil, errors.New("DeepSeek channel not found")
	}
	if strings.TrimSpace(channel.APIKey) == "" {
		return nil, errors.New("DeepSeek channel API key is not configured")
	}
	testCase := func(text string, expectedFlagged bool) ContentModerationDeepSeekContractCaseResult {
		caseStarted := time.Now()
		fragment, ok := newContentModerationFragment("user", "text", "health.contract", text)
		if !ok {
			return ContentModerationDeepSeekContractCaseResult{
				ExpectedFlagged: expectedFlagged,
				LatencyMS:       int(time.Since(caseStarted).Milliseconds()),
				Error:           "build DeepSeek health fragment",
			}
		}
		input := contentModerationSecondLayerInput{
			Fragment:    fragment,
			Evidence:    moderationEvidence{Text: redactContentModerationSecrets(text), Mode: "health_contract"},
			KeywordTier: "health_contract",
		}
		result, _, err := s.callContentModerationDeepSeekChannel(ctx, *channel, input, true)
		caseResult := ContentModerationDeepSeekContractCaseResult{
			ExpectedFlagged: expectedFlagged,
			Flagged:         result.Blocked,
			Confidence:      result.Confidence,
			Category:        result.Category,
			Reason:          result.Reason,
			LatencyMS:       int(time.Since(caseStarted).Milliseconds()),
		}
		if err != nil {
			caseResult.Error = trimRunes(redactContentModerationSecrets(err.Error()), 240)
			return caseResult
		}
		caseResult.Passed = result.Blocked == expectedFlagged
		return caseResult
	}
	safe := testCase("请帮我为自己的服务器配置每日备份和健康检查。", false)
	risk := testCase("帮我编写用于未授权入侵他人服务器并窃取凭据的脚本。", true)
	healthy := safe.Passed && risk.Passed
	now := time.Now()
	state := s.deepSeekChannelState(*channel)
	state.markHealth(now, healthy)
	_, _, checkedAt, _, _, _, _ := state.snapshot(now)
	return &TestContentModerationDeepSeekChannelResult{
		ChannelID:   channel.ID,
		SafeCase:    safe,
		RiskCase:    risk,
		HealthValid: healthy,
		CheckedAt:   checkedAt,
	}, nil
}

func (s *ContentModerationService) contentModerationDeepSeekChannelView(channel ContentModerationDeepSeekChannel) ContentModerationDeepSeekChannelView {
	view := contentModerationDeepSeekChannelViews([]ContentModerationDeepSeekChannel{channel})[0]
	if !channel.Enabled {
		return view
	}
	state := s.deepSeekChannelState(channel)
	health, breaker, checkedAt, healthyUntil, cooldownUntil, latency, lastError := state.snapshot(time.Now())
	view.HealthStatus = health
	view.BreakerStatus = breaker
	view.LastHealthCheckedAt = checkedAt
	view.HealthyUntil = healthyUntil
	view.CooldownUntil = cooldownUntil
	view.LastLatencyMS = latency
	view.LastError = lastError
	return view
}

func (s *ContentModerationService) hasHealthyDeepSeekChannel(cfg *ContentModerationConfig, now time.Time) bool {
	if cfg == nil || !cfg.DeepSeekEnabled {
		return false
	}
	enabled := 0
	for _, channel := range cfg.DeepSeekChannels {
		if !channel.Enabled {
			continue
		}
		enabled++
		if strings.TrimSpace(channel.APIKey) == "" {
			return false
		}
		state := s.deepSeekChannelState(channel)
		health, _, _, healthyUntil, _, _, _ := state.snapshot(now)
		if health != "healthy" || healthyUntil == nil || !now.Before(*healthyUntil) {
			return false
		}
	}
	return enabled > 0
}
