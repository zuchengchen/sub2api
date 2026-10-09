package service

// This file contains an opt-in trace for controlled local gateway comparisons.
// It deliberately records hashes and bounded metadata only; request content and
// credentials are never written to the trace.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	diagnosticTraceEnabledEnv = "SUB2_DIAGNOSTIC_TRACE"
	diagnosticTracePathEnv    = "SUB2_DIAGNOSTIC_TRACE_PATH"
	diagnosticTraceRunEnv     = "SUB2_DIAGNOSTIC_TRACE_RUN_ID"
)

var (
	diagnosticTraceMu                  sync.Mutex
	diagnosticTraceHeaderDigestKey     [32]byte
	diagnosticTraceHeaderDigestKeyOnce sync.Once
)

type diagnosticTraceEvent struct {
	Time            string                  `json:"time"`
	RunID           string                  `json:"run_id,omitempty"`
	Stage           string                  `json:"stage"`
	RequestID       string                  `json:"request_id_hash,omitempty"`
	ClientRequestID string                  `json:"client_request_id_hash,omitempty"`
	Attempt         int                     `json:"attempt,omitempty"`
	AccountID       int64                   `json:"account_id,omitempty"`
	Account         string                  `json:"account_type,omitempty"`
	Platform        string                  `json:"platform,omitempty"`
	BodySHA256      string                  `json:"body_sha256"`
	BodyBytes       int                     `json:"body_bytes"`
	Body            diagnosticBodySummary   `json:"body"`
	URL             *diagnosticURLSummary   `json:"url,omitempty"`
	Headers         map[string]headerDigest `json:"headers,omitempty"`
	PublicHeaders   map[string]string       `json:"public_headers,omitempty"`
	Status          int                     `json:"status,omitempty"`
	Outcome         string                  `json:"outcome,omitempty"`
}

type diagnosticURLSummary struct {
	Scheme string `json:"scheme,omitempty"`
	Host   string `json:"host,omitempty"`
	Path   string `json:"path,omitempty"`
}

type diagnosticBodySummary struct {
	TopLevelKeys       []string          `json:"known_top_level_keys,omitempty"`
	Canonicalization   string            `json:"canonicalization,omitempty"`
	SemanticSHA256     string            `json:"semantic_sha256,omitempty"`
	FieldSHA256        map[string]string `json:"field_sha256,omitempty"`
	Model              string            `json:"model,omitempty"`
	Stream             *bool             `json:"stream,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	ReasoningEffort    string            `json:"reasoning_effort,omitempty"`
	ReasoningMode      string            `json:"reasoning_mode,omitempty"`
	ReasoningPresent   bool              `json:"reasoning_present,omitempty"`
	InputPresent       bool              `json:"input_present,omitempty"`
	InputItems         int               `json:"input_items,omitempty"`
	Tools              int               `json:"tools,omitempty"`
	PreviousIDPresent  bool              `json:"previous_response_id_present,omitempty"`
	MaxOutputPresent   bool              `json:"max_output_tokens_present,omitempty"`
	MaxCompletionExist bool              `json:"max_completion_tokens_present,omitempty"`
	IncludeValues      []string          `json:"include_values,omitempty"`
}

type headerDigest struct {
	Present bool   `json:"present"`
	Length  int    `json:"length"`
	Digest  string `json:"digest"`
}

func diagnosticTraceEnabled() (string, bool) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(diagnosticTraceEnabledEnv))) {
	case "1", "true", "yes", "on":
	default:
		return "", false
	}
	path := strings.TrimSpace(os.Getenv(diagnosticTracePathEnv))
	if path == "" {
		return "", false
	}
	return path, true
}

func diagnosticSHA256(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func diagnosticRequestIDHash(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	value := strings.TrimSpace(c.GetHeader("X-Request-ID"))
	if value == "" {
		return ""
	}
	return diagnosticSHA256([]byte(value))
}

func diagnosticClientRequestIDHash(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	value := strings.TrimSpace(c.GetHeader("X-Client-Request-ID"))
	if value == "" {
		return ""
	}
	return diagnosticSHA256([]byte(value))
}

// diagnosticHeaderValueDigest is process-keyed so logs cannot be used as a
// reusable oracle for a credential or opaque session value. It is only useful
// to compare events emitted by the same diagnostic process.
func diagnosticHeaderValueDigest(value string) string {
	diagnosticTraceHeaderDigestKeyOnce.Do(func() {
		if _, err := rand.Read(diagnosticTraceHeaderDigestKey[:]); err != nil {
			fallback := sha256.Sum256([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
			diagnosticTraceHeaderDigestKey = fallback
		}
	})
	mac := hmac.New(sha256.New, diagnosticTraceHeaderDigestKey[:])
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil))
}

func diagnosticHeaderDigests(headers http.Header) map[string]headerDigest {
	if len(headers) == 0 {
		return nil
	}
	keys := []string{
		"accept", "content-type", "originator", "user-agent", "version",
		"session_id", "conversation_id", "chatgpt-account-id",
		"x-codex-turn-state", "x-codex-beta-features", "x-codex-routing-hint",
		"authorization", "openai-beta", "x-openai-client-user-agent",
	}
	sort.Strings(keys)
	result := make(map[string]headerDigest, len(keys))
	for _, key := range keys {
		values := headers.Values(key)
		if len(values) == 0 {
			continue
		}
		joined := strings.Join(values, "\x00")
		result[key] = headerDigest{
			Present: true,
			Length:  len(joined),
			Digest:  diagnosticHeaderValueDigest(joined),
		}
	}
	return result
}

// diagnosticPublicHeaders includes only bounded, non-secret client identity
// fields. Credentials and opaque continuity headers remain HMAC-only above.
func diagnosticPublicHeaders(headers http.Header) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	result := make(map[string]string, 3)
	for _, key := range []string{"user-agent", "originator", "version"} {
		value := strings.TrimSpace(headers.Get(key))
		if value == "" || len(value) > 128 {
			continue
		}
		valid := true
		for _, char := range value {
			if char < 32 || char > 126 {
				valid = false
				break
			}
		}
		if !valid || (key != "user-agent" && !diagnosticSafeIdentifier(value)) {
			continue
		}
		result[key] = value
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func diagnosticSummarizeBody(raw []byte) diagnosticBodySummary {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return diagnosticBodySummary{}
	}
	knownKeys := []string{"model", "stream", "store", "reasoning", "input", "instructions", "tools", "tool_choice", "max_output_tokens", "max_completion_tokens", "previous_response_id", "parallel_tool_calls", "text", "service_tier", "include", "metadata", "prompt_cache_key", "truncation"}
	keys := make([]string, 0, len(knownKeys))
	for _, key := range knownKeys {
		if _, ok := object[key]; ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := diagnosticBodySummary{
		TopLevelKeys:     keys,
		Canonicalization: "sorted-key-compact-json",
		SemanticSHA256:   diagnosticSemanticSHA256(object),
		FieldSHA256:      diagnosticFieldSHA256(object),
	}
	var stringValue string
	if json.Unmarshal(object["model"], &stringValue) == nil && diagnosticSafeIdentifier(stringValue) {
		result.Model = stringValue
	}
	if _, ok := object["stream"]; ok {
		var boolValue *bool
		if json.Unmarshal(object["stream"], &boolValue) == nil {
			result.Stream = boolValue
		}
	}
	if _, ok := object["store"]; ok {
		var boolValue *bool
		if json.Unmarshal(object["store"], &boolValue) == nil {
			result.Store = boolValue
		}
	}
	if reasoning, ok := object["reasoning"]; ok {
		var value map[string]json.RawMessage
		if json.Unmarshal(reasoning, &value) == nil {
			result.ReasoningPresent = true
			stringValue = ""
			_ = json.Unmarshal(value["effort"], &stringValue)
			if diagnosticSafeIdentifier(stringValue) {
				result.ReasoningEffort = stringValue
			}
			stringValue = ""
			_ = json.Unmarshal(value["mode"], &stringValue)
			if diagnosticSafeIdentifier(stringValue) {
				result.ReasoningMode = stringValue
			}
		}
	}
	if input, ok := object["input"]; ok {
		result.InputPresent = true
		var items []json.RawMessage
		if json.Unmarshal(input, &items) == nil {
			result.InputItems = len(items)
		}
	}
	if tools, ok := object["tools"]; ok {
		var items []json.RawMessage
		if json.Unmarshal(tools, &items) == nil {
			result.Tools = len(items)
		}
	}
	_, result.PreviousIDPresent = object["previous_response_id"]
	_, result.MaxOutputPresent = object["max_output_tokens"]
	_, result.MaxCompletionExist = object["max_completion_tokens"]
	if include, ok := object["include"]; ok {
		var values []string
		if json.Unmarshal(include, &values) == nil {
			for _, value := range values {
				switch value {
				case "reasoning.encrypted_content", "message.output_text.logprobs":
					result.IncludeValues = append(result.IncludeValues, value)
				}
			}
			sort.Strings(result.IncludeValues)
		}
	}
	return result
}

func diagnosticSemanticSHA256(object map[string]json.RawMessage) string {
	if object == nil {
		return ""
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return ""
	}
	return diagnosticCanonicalSHA256(raw)
}

func diagnosticFieldSHA256(object map[string]json.RawMessage) map[string]string {
	if object == nil {
		return nil
	}
	result := make(map[string]string, 6)
	for _, key := range []string{"input", "instructions", "tools", "text", "reasoning", "include"} {
		value, ok := object[key]
		if !ok {
			continue
		}
		if digest := diagnosticCanonicalSHA256(value); digest != "" {
			result[key] = digest
		}
	}
	return result
}

func diagnosticCanonicalSHA256(raw []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return ""
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return ""
	}
	var canonicalBuffer bytes.Buffer
	encoder := json.NewEncoder(&canonicalBuffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(decoded); err != nil {
		return ""
	}
	canonical := bytes.TrimSuffix(canonicalBuffer.Bytes(), []byte{'\n'})
	if len(canonical) == 0 {
		return ""
	}
	return diagnosticSHA256(canonical)
}

func diagnosticSafeIdentifier(value string) bool {
	if len(value) == 0 || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' || char == '.' {
			continue
		}
		return false
	}
	return true
}

func diagnosticURLSummaryForRequest(request *http.Request) *diagnosticURLSummary {
	if request == nil || request.URL == nil {
		return nil
	}
	return &diagnosticURLSummary{
		Scheme: request.URL.Scheme,
		Host:   request.URL.Host,
		Path:   request.URL.Path,
	}
}

func writeDiagnosticTrace(event diagnosticTraceEvent) {
	path, enabled := diagnosticTraceEnabled()
	if !enabled {
		return
	}
	event.Time = time.Now().UTC().Format(time.RFC3339Nano)
	event.RunID = strings.TrimSpace(os.Getenv(diagnosticTraceRunEnv))
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	diagnosticTraceMu.Lock()
	defer diagnosticTraceMu.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_, _ = file.Write(append(encoded, '\n'))
}

func TraceOpenAIRequestIngress(c *gin.Context, body []byte) {
	traceOpenAIRequestBody(c, nil, body, "http_ingress")
}

func traceOpenAIInbound(ctx context.Context, c *gin.Context, account *Account, body []byte) {
	traceOpenAIRequestBody(c, account, body, "forward_entry")
}

func traceOpenAIRequestBody(c *gin.Context, account *Account, body []byte, stage string) {
	if _, enabled := diagnosticTraceEnabled(); !enabled {
		return
	}
	event := diagnosticTraceEvent{
		Stage:           stage,
		RequestID:       diagnosticRequestIDHash(c),
		ClientRequestID: diagnosticClientRequestIDHash(c),
		BodySHA256:      diagnosticSHA256(body),
		BodyBytes:       len(body),
		Body:            diagnosticSummarizeBody(body),
	}
	if account != nil {
		event.AccountID, event.Account, event.Platform = account.ID, account.Type, account.Platform
	}
	writeDiagnosticTrace(event)
}

func traceOpenAIUpstream(ctx context.Context, c *gin.Context, account *Account, attempt int, request *http.Request) {
	if request == nil {
		return
	}
	if _, enabled := diagnosticTraceEnabled(); !enabled {
		return
	}
	body := []byte{}
	if request.GetBody != nil {
		if reader, err := request.GetBody(); err == nil {
			body, _ = io.ReadAll(reader)
			_ = reader.Close()
		}
	}
	event := diagnosticTraceEvent{
		Stage:           "upstream",
		Attempt:         attempt,
		RequestID:       diagnosticRequestIDHash(c),
		ClientRequestID: diagnosticClientRequestIDHash(c),
		BodySHA256:      diagnosticSHA256(body),
		BodyBytes:       len(body),
		Body:            diagnosticSummarizeBody(body),
		URL:             diagnosticURLSummaryForRequest(request),
		Headers:         diagnosticHeaderDigests(request.Header),
		PublicHeaders:   diagnosticPublicHeaders(request.Header),
	}
	if account != nil {
		event.AccountID, event.Account, event.Platform = account.ID, account.Type, account.Platform
	}
	writeDiagnosticTrace(event)
}

func traceOpenAIUpstreamResult(c *gin.Context, account *Account, attempt int, response *http.Response, err error) {
	if _, enabled := diagnosticTraceEnabled(); !enabled {
		return
	}
	event := diagnosticTraceEvent{
		Stage:           "upstream_result",
		Attempt:         attempt,
		RequestID:       diagnosticRequestIDHash(c),
		ClientRequestID: diagnosticClientRequestIDHash(c),
	}
	if account != nil {
		event.AccountID, event.Account, event.Platform = account.ID, account.Type, account.Platform
	}
	if response != nil {
		event.Status = response.StatusCode
	}
	if err != nil {
		event.Outcome = "transport_error"
	} else if response == nil {
		event.Outcome = "no_response"
	} else {
		event.Outcome = "response_headers"
	}
	writeDiagnosticTrace(event)
}
