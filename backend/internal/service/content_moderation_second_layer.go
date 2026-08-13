package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxContentModerationSecondLayerResponseBytes int64 = 256 * 1024

type contentModerationSecondLayerResult struct {
	Blocked  bool
	Category string
}

func (s *ContentModerationService) scanUnifiedSecondLayer(ctx context.Context, cfg *ContentModerationConfig, text string) (contentModerationSecondLayerResult, bool, error) {
	endpoints := cfg.enabledSecondLayerEndpoints()
	if len(endpoints) == 0 {
		return contentModerationSecondLayerResult{}, false, nil
	}
	scanners := normalizeContentModerationScannerIDs(cfg.SecondLayerScanners)
	if len(scanners) == 0 {
		scanners = append([]string(nil), contentModerationScannerIDs...)
	}

	limit := endpoints[0].InputLimit
	for _, endpoint := range endpoints[1:] {
		if endpoint.InputLimit < limit {
			limit = endpoint.InputLimit
		}
	}
	chunks := splitContentModerationRunes(text, limit)
	for _, chunk := range chunks {
		result, err := scanContentModerationSecondLayerChunk(ctx, endpoints, chunk, scanners)
		if err != nil {
			return contentModerationSecondLayerResult{}, true, err
		}
		if result.Blocked {
			return result, true, nil
		}
	}
	return contentModerationSecondLayerResult{}, true, nil
}

func scanContentModerationSecondLayerChunk(ctx context.Context, endpoints []ContentModerationEndpoint, chunk string, scanners []string) (contentModerationSecondLayerResult, error) {
	var lastErr error
	for _, endpoint := range endpoints {
		result, err := callContentModerationSecondLayer(ctx, endpoint, chunk, scanners)
		if err == nil {
			return result, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no content moderation second-layer endpoint available")
	}
	return contentModerationSecondLayerResult{}, lastErr
}

func callContentModerationSecondLayer(ctx context.Context, endpoint ContentModerationEndpoint, chunk string, scanners []string) (contentModerationSecondLayerResult, error) {
	baseURL, err := normalizeContentModerationSecondLayerBaseURL(endpoint.BaseURL)
	if err != nil {
		return contentModerationSecondLayerResult{}, err
	}
	payload, err := json.Marshal(map[string]any{
		"model":       endpoint.Model,
		"messages":    []map[string]string{{"role": "user", "content": chunk}},
		"temperature": 0,
		"max_tokens":  64,
		"seed":        42,
	})
	if err != nil {
		return contentModerationSecondLayerResult{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(endpoint.TimeoutMS)*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, baseURL+"/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return contentModerationSecondLayerResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if endpoint.Token != "" {
		req.Header.Set("Authorization", "Bearer "+endpoint.Token)
	}
	client := newContentModerationSecondLayerClient(endpoint.TimeoutMS)
	resp, err := client.Do(req)
	if err != nil {
		return contentModerationSecondLayerResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return contentModerationSecondLayerResult{}, fmt.Errorf("second-layer HTTP status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxContentModerationSecondLayerResponseBytes+1))
	if err != nil {
		return contentModerationSecondLayerResult{}, err
	}
	if int64(len(body)) > maxContentModerationSecondLayerResponseBytes {
		return contentModerationSecondLayerResult{}, errors.New("second-layer response too large")
	}
	content, err := extractContentModerationSecondLayerContent(body)
	if err != nil {
		return contentModerationSecondLayerResult{}, err
	}
	return parseContentModerationSecondLayerOutput(content, scanners)
}

func normalizeContentModerationSecondLayerBaseURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid second-layer base URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("second-layer base URL must use HTTP(S)")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("unsafe second-layer base URL")
	}
	path := strings.TrimRight(parsed.EscapedPath(), "/")
	if strings.EqualFold(path, "/v1") {
		path = ""
	}
	parsed.Path = path
	parsed.RawPath = ""
	return strings.TrimRight(parsed.String(), "/"), nil
}

func newContentModerationSecondLayerClient(timeoutMS int) *http.Client {
	timeout := time.Duration(timeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultContentModerationSecondLayerTimeoutMS * time.Millisecond
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			Proxy: nil, ForceAttemptHTTP2: true, DialContext: dialer.DialContext,
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second,
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: timeout,
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
	}
}

func extractContentModerationSecondLayerContent(body []byte) (string, error) {
	var response struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &response); err != nil || len(response.Choices) == 0 {
		return "", errors.New("invalid second-layer response envelope")
	}
	switch content := response.Choices[0].Message.Content.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return "", errors.New("empty second-layer response")
		}
		return content, nil
	case []any:
		parts := make([]string, 0, len(content))
		for _, item := range content {
			if object, ok := item.(map[string]any); ok {
				if text, ok := object["text"].(string); ok && strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n"), nil
		}
	}
	return "", errors.New("invalid second-layer response content")
}

func parseContentModerationSecondLayerOutput(content string, scanners []string) (contentModerationSecondLayerResult, error) {
	var safety, categoriesLine string
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(lower, "safety:"):
			if safety != "" {
				return contentModerationSecondLayerResult{}, errors.New("duplicate second-layer safety")
			}
			safety = strings.ToLower(strings.TrimSpace(line[len("safety:"):]))
		case strings.HasPrefix(lower, "categories:"):
			if categoriesLine != "" {
				return contentModerationSecondLayerResult{}, errors.New("duplicate second-layer categories")
			}
			categoriesLine = strings.TrimSpace(line[len("categories:"):])
		}
	}
	if safety != "safe" && safety != "controversial" && safety != "unsafe" {
		return contentModerationSecondLayerResult{}, errors.New("invalid second-layer safety")
	}
	if categoriesLine == "" {
		return contentModerationSecondLayerResult{}, errors.New("missing second-layer categories")
	}
	if safety == "safe" {
		return contentModerationSecondLayerResult{}, nil
	}
	enabled := make(map[string]struct{}, len(scanners))
	for _, scanner := range scanners {
		enabled[scanner] = struct{}{}
	}
	firstCategory := ""
	matched := false
	unknown := false
	for _, raw := range strings.Split(categoriesLine, ",") {
		category := normalizeContentModerationSecondLayerCategory(raw)
		if category == "" || category == "none" || category == "n_a" {
			continue
		}
		if firstCategory == "" {
			firstCategory = category
		}
		if !contentModerationSecondLayerCategoryKnown(category) {
			unknown = true
			continue
		}
		if _, ok := enabled[category]; ok {
			matched = true
		}
	}
	if safety == "unsafe" && (matched || unknown || firstCategory == "") {
		return contentModerationSecondLayerResult{Blocked: true, Category: defaultContentModerationString(firstCategory, "second_layer")}, nil
	}
	if safety == "controversial" && matched {
		if firstCategory == "jailbreak" || firstCategory == "pii" || firstCategory == "suicide_and_self_harm" {
			return contentModerationSecondLayerResult{Blocked: true, Category: firstCategory}, nil
		}
	}
	return contentModerationSecondLayerResult{}, nil
}

func normalizeContentModerationSecondLayerCategory(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.NewReplacer("_", " ", "&", " and ", "/", " ", "-", " ", "–", " ", "—", " ").Replace(value)
	value = strings.Join(strings.Fields(value), " ")
	aliases := map[string]string{
		"violent": "violent", "violence": "violent",
		"non violent illegal acts":      "non_violent_illegal_acts",
		"sexual content or sexual acts": "sexual_content_or_sexual_acts", "sexual": "sexual_content_or_sexual_acts",
		"pii": "pii", "personal identifying information": "pii", "personal identifiable information": "pii",
		"suicide and self harm": "suicide_and_self_harm", "unethical acts": "unethical_acts", "unethical": "unethical_acts",
		"politically sensitive topics": "politically_sensitive_topics", "political": "politically_sensitive_topics",
		"copyright violation": "copyright_violation", "copyright": "copyright_violation",
		"jailbreak": "jailbreak", "prompt injection": "jailbreak", "none": "none", "n a": "n_a",
	}
	if canonical, ok := aliases[value]; ok {
		return canonical
	}
	return strings.ReplaceAll(value, " ", "_")
}

func contentModerationSecondLayerCategoryKnown(category string) bool {
	for _, known := range contentModerationScannerIDs {
		if category == known {
			return true
		}
	}
	return false
}

func splitContentModerationRunes(text string, limit int) []string {
	if limit <= 0 {
		limit = defaultContentModerationSecondLayerInputLimit
	}
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	out := make([]string, 0, (len(runes)+limit-1)/limit)
	for start := 0; start < len(runes); start += limit {
		end := start + limit
		if end > len(runes) {
			end = len(runes)
		}
		out = append(out, string(runes[start:end]))
	}
	return out
}
