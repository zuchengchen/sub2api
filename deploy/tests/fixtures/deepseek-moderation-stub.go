package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type channelBehavior struct {
	Mode     string `json:"mode"`
	Status   int    `json:"status,omitempty"`
	DelayMS  int    `json:"delay_ms,omitempty"`
	Category string `json:"category,omitempty"`
}

type controlRequest struct {
	Channel  string `json:"channel"`
	Mode     string `json:"mode"`
	Status   int    `json:"status,omitempty"`
	DelayMS  int    `json:"delay_ms,omitempty"`
	Category string `json:"category,omitempty"`
}

type requestEvent struct {
	Sequence int64  `json:"sequence"`
	Channel  string `json:"channel"`
	Outcome  string `json:"outcome"`
	Status   int    `json:"status"`
}

type stubStats struct {
	InstanceID         string           `json:"instance_id"`
	StartedAt          string           `json:"started_at"`
	Requests           int64            `json:"requests"`
	ContractViolations int64            `json:"contract_violations"`
	Active             int              `json:"active"`
	MaxActive          int              `json:"max_active"`
	CallsByChannel     map[string]int64 `json:"calls_by_channel"`
	Events             []requestEvent   `json:"events"`
}

type moderationStub struct {
	mu         sync.Mutex
	behaviors  map[string]channelBehavior
	stats      stubStats
	nextSeq    int64
	instanceID string
	startedAt  string
}

func newModerationStub() *moderationStub {
	now := time.Now().UTC()
	return &moderationStub{
		behaviors: make(map[string]channelBehavior),
		stats: stubStats{
			CallsByChannel: make(map[string]int64),
		},
		instanceID: fmt.Sprintf("%d-%d", os.Getpid(), now.UnixNano()),
		startedAt:  now.Format(time.RFC3339Nano),
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:0", "listen address")
	flag.Parse()

	stub := newModerationStub()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /stats", stub.handleStats)
	mux.HandleFunc("POST /reset", stub.handleReset)
	mux.HandleFunc("POST /control", stub.handleControl)
	mux.HandleFunc("POST /", stub.handleCompletion)

	server := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("deepseek moderation release stub listening on %s", *listen)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func (s *moderationStub) handleReset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.behaviors = make(map[string]channelBehavior)
	s.stats = stubStats{CallsByChannel: make(map[string]int64)}
	s.nextSeq = 0
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}

func (s *moderationStub) handleControl(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	var request controlRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid control request"})
		return
	}
	request.Channel = strings.Trim(strings.TrimSpace(request.Channel), "/")
	request.Mode = strings.TrimSpace(request.Mode)
	request.Category = strings.TrimSpace(request.Category)
	if request.Channel == "" || !allowedMode(request.Mode) || request.DelayMS < 0 || request.DelayMS > 30000 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported control values"})
		return
	}
	if request.Mode == "status" && (request.Status < 100 || request.Status > 599) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid status"})
		return
	}
	if request.Mode == "category" && !allowedRiskCategory(request.Category) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid category"})
		return
	}
	s.mu.Lock()
	s.behaviors[request.Channel] = channelBehavior{
		Mode: request.Mode, Status: request.Status, DelayMS: request.DelayMS, Category: request.Category,
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"configured": true, "channel": request.Channel})
}

func (s *moderationStub) handleStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	stats := stubStats{
		InstanceID:         s.instanceID,
		StartedAt:          s.startedAt,
		Requests:           s.stats.Requests,
		ContractViolations: s.stats.ContractViolations,
		Active:             s.stats.Active,
		MaxActive:          s.stats.MaxActive,
		CallsByChannel:     make(map[string]int64, len(s.stats.CallsByChannel)),
		Events:             append([]requestEvent(nil), s.stats.Events...),
	}
	for channel, calls := range s.stats.CallsByChannel {
		stats.CallsByChannel[channel] = calls
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, stats)
}

func (s *moderationStub) handleCompletion(w http.ResponseWriter, r *http.Request) {
	channel, ok := completionChannel(r.URL.Path)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unknown endpoint"})
		return
	}

	s.mu.Lock()
	behavior := s.behaviors[channel]
	if behavior.Mode == "" {
		behavior.Mode = "contract"
	}
	s.stats.Requests++
	s.stats.CallsByChannel[channel]++
	s.stats.Active++
	if s.stats.Active > s.stats.MaxActive {
		s.stats.MaxActive = s.stats.Active
	}
	s.nextSeq++
	sequence := s.nextSeq
	s.mu.Unlock()

	outcome := "completed"
	status := http.StatusOK
	defer func() {
		s.mu.Lock()
		s.stats.Active--
		s.stats.Events = append(s.stats.Events, requestEvent{
			Sequence: sequence, Channel: channel, Outcome: outcome, Status: status,
		})
		s.mu.Unlock()
	}()

	var text string
	var err error
	if behavior.Mode == "yufeng_safe" || behavior.Mode == "yufeng_risk" {
		err = validateYuFengRequest(r)
	} else {
		text, err = validateDeepSeekRequest(r)
	}
	if err != nil {
		s.mu.Lock()
		s.stats.ContractViolations++
		s.mu.Unlock()
		outcome = "contract_violation"
		status = http.StatusBadRequest
		writeJSON(w, status, map[string]any{"error": "request contract violation"})
		return
	}

	if behavior.DelayMS > 0 {
		timer := time.NewTimer(time.Duration(behavior.DelayMS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-r.Context().Done():
			outcome = "cancelled"
			status = 499
			return
		case <-timer.C:
		}
	}

	switch behavior.Mode {
	case "status":
		status = behavior.Status
		outcome = fmt.Sprintf("http_%d", status)
		writeJSON(w, status, map[string]any{"error": "controlled status"})
	case "invalid_json":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[`))
	case "reasoning":
		writeDeepSeekEnvelope(w, `{"confidence":0.05,"category":"safe","reason":""}`, "internal reasoning")
	case "truncated":
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []any{map[string]any{
				"finish_reason": "length",
				"message":       map[string]any{"content": `{"confidence":0.90,"category":"cyber_abuse","reason":"风险"}`},
			}},
		})
	case "yufeng_safe":
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "sec"}}},
		})
	case "yufeng_risk":
		writeJSON(w, http.StatusOK, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "pc"}}},
		})
	case "risk":
		writeDeepSeekEnvelope(w, `{"confidence":0.90,"category":"cyber_abuse","reason":"明确风险"}`, "")
	case "category":
		content, marshalErr := json.Marshal(map[string]any{
			"confidence": 0.90, "category": behavior.Category, "reason": "明确风险",
		})
		if marshalErr != nil {
			status = http.StatusInternalServerError
			writeJSON(w, status, map[string]any{"error": "encode response"})
			return
		}
		writeDeepSeekEnvelope(w, string(content), "")
	case "safe":
		writeDeepSeekEnvelope(w, `{"confidence":0.05,"category":"safe","reason":""}`, "")
	case "contract":
		if strings.Contains(text, "未授权入侵他人服务器") {
			writeDeepSeekEnvelope(w, `{"confidence":0.95,"category":"cyber_abuse","reason":"未授权攻击"}`, "")
			return
		}
		writeDeepSeekEnvelope(w, `{"confidence":0.03,"category":"safe","reason":""}`, "")
	default:
		status = http.StatusInternalServerError
		writeJSON(w, status, map[string]any{"error": "unhandled behavior"})
	}
}

func validateDeepSeekRequest(r *http.Request) (string, error) {
	defer func() { _ = r.Body.Close() }()
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(auth, "Bearer ") || strings.TrimSpace(strings.TrimPrefix(auth, "Bearer ")) == "" {
		return "", errors.New("missing bearer credential")
	}
	var payload struct {
		Model          string              `json:"model"`
		Messages       []map[string]string `json:"messages"`
		Thinking       map[string]string   `json:"thinking"`
		ResponseFormat map[string]string   `json:"response_format"`
		Temperature    float64             `json:"temperature"`
		MaxTokens      int                 `json:"max_tokens"`
		Stream         bool                `json:"stream"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 256*1024))
	if err := decoder.Decode(&payload); err != nil {
		return "", err
	}
	if strings.TrimSpace(payload.Model) == "" || payload.Thinking["type"] != "disabled" ||
		payload.ResponseFormat["type"] != "json_object" || payload.Temperature != 0 ||
		payload.MaxTokens != 64 || payload.Stream || len(payload.Messages) != 2 {
		return "", errors.New("invalid non-thinking JSON contract")
	}
	if payload.Messages[0]["role"] != "system" || payload.Messages[1]["role"] != "user" ||
		!strings.Contains(payload.Messages[0]["content"], "[SYSTEM - IMMUTABLE]") ||
		!strings.Contains(payload.Messages[0]["content"], "reasoning_content") {
		return "", errors.New("invalid moderation messages")
	}
	message := payload.Messages[1]["content"]
	const startTag = "<user_input>"
	const endTag = "</user_input>"
	start := strings.Index(message, startTag)
	end := strings.LastIndex(message, endTag)
	if start < 0 || end <= start || strings.Count(message, endTag) != 1 ||
		!strings.Contains(message, "<trusted_context>") || !strings.Contains(message, "</trusted_context>") {
		return "", errors.New("invalid untrusted-data envelope")
	}
	var wrapped struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(message[start+len(startTag):end]), &wrapped); err != nil || strings.TrimSpace(wrapped.Content) == "" {
		return "", errors.New("invalid wrapped input")
	}
	return wrapped.Content, nil
}

func validateYuFengRequest(r *http.Request) error {
	defer func() { _ = r.Body.Close() }()
	var payload struct {
		Model    string              `json:"model"`
		Messages []map[string]string `json:"messages"`
		Stream   bool                `json:"stream"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 256*1024))
	if err := decoder.Decode(&payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.Model) == "" || len(payload.Messages) == 0 || payload.Stream {
		return errors.New("invalid YuFeng request contract")
	}
	return nil
}

func completionChannel(path string) (string, bool) {
	const suffix = "/v1/chat/completions"
	if !strings.HasSuffix(path, suffix) {
		return "", false
	}
	channel := strings.Trim(strings.TrimSuffix(path, suffix), "/")
	if channel == "" || strings.Contains(channel, "/") {
		return "", false
	}
	return channel, true
}

func allowedMode(mode string) bool {
	switch mode {
	case "contract", "safe", "risk", "category", "status", "invalid_json", "reasoning", "truncated", "yufeng_safe", "yufeng_risk":
		return true
	default:
		return false
	}
}

func allowedRiskCategory(category string) bool {
	switch category {
	case "cyber_abuse", "cracking", "security_bypass", "account_abuse", "sexual_deepfake", "doxxing", "violent_threat", "self_harm", "weapons", "sexual_content":
		return true
	default:
		return false
	}
}

func writeDeepSeekEnvelope(w http.ResponseWriter, content, reasoning string) {
	message := map[string]any{"content": content}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"choices": []any{map[string]any{"finish_reason": "stop", "message": message}},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
