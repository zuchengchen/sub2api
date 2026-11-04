package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
)

func incrementalAccessEvent(t *testing.T, values map[string]any) *logger.LogEvent {
	t.Helper()
	sink := initMiddlewareTestLogger(t)
	router := gin.New()
	router.Use(Logger())
	router.POST("/v1/responses", func(c *gin.Context) {
		for key, value := range values {
			c.Set(key, value)
		}
		c.Status(http.StatusOK)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	events := sink.list()
	if len(events) != 1 || events[0].Message != "http request completed" {
		t.Fatalf("missing access event: %v", events)
	}
	return events[0]
}

func TestLoggerOpenAIWSIncrementalLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name                                             string
		applied, completed, replay, attempted, succeeded bool
		reason                                           string
	}{
		{name: "full_without_checkpoint", reason: "no_checkpoint"},
		{name: "delta_completed", applied: true, completed: true, attempted: true, succeeded: true, reason: "incremental"},
		{name: "delta_failed_after_apply", applied: true, attempted: true, reason: "incremental"},
		{name: "absent_attempt_not_inferred", applied: true, reason: "incremental"},
		{name: "full_replay_completed", replay: true, attempted: true, reason: "no_checkpoint"},
		{name: "full_replay_stale_success_flag", applied: true, completed: true, replay: true, attempted: true, reason: "no_checkpoint"},
		{name: "success_without_applied_is_not_incremental", completed: true, attempted: true, reason: "no_checkpoint"},
		{name: "prewarm_delta_completed", applied: true, completed: true, attempted: true, succeeded: true, reason: "prewarm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			event := incrementalAccessEvent(t, map[string]any{
				"openai_ws_auto_incremental_attempted": tc.attempted,
				"openai_ws_auto_incremental":           tc.applied,
				"openai_ws_auto_incremental_succeeded": tc.completed,
				"openai_ws_auto_incremental_replay":    tc.replay,
				"openai_ws_auto_incremental_reason":    tc.reason,
			})
			for key, want := range map[string]bool{
				"openai_ws_auto_incremental_attempted": tc.attempted,
				"openai_ws_auto_incremental":           tc.applied,
				"openai_ws_auto_incremental_succeeded": tc.succeeded,
				"openai_ws_auto_incremental_replay":    tc.replay,
			} {
				got, ok := event.Fields[key].(bool)
				if !ok || got != want {
					t.Fatalf("%s=%v, want %v", key, event.Fields[key], want)
				}
			}
			if event.Fields["openai_ws_auto_incremental_reason"] != tc.reason {
				t.Fatal("reason missing or changed")
			}
		})
	}
}

func TestLoggerOpenAIWSIncrementalDiagnosticsAreBounded(t *testing.T) {
	const secret = "DO_NOT_LOG_prompt_response_id_ciphertext_hash"
	for _, index := range []any{0, 2047, -1, 2048, secret, int64(1)} {
		event := incrementalAccessEvent(t, map[string]any{
			"openai_ws_auto_incremental":                    false,
			"openai_ws_auto_incremental_reason":             secret,
			"openai_ws_auto_incremental_item_index":         index,
			"openai_ws_auto_incremental_item_type":          secret,
			"openai_ws_auto_incremental_previous_item_type": "function_call",
			"openai_ws_auto_incremental_response_id":        secret,
			"openai_ws_auto_incremental_input":              secret,
			"openai_ws_auto_incremental_hash":               secret,
		})
		raw, err := json.Marshal(event.Fields)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), secret) {
			t.Fatal("incremental diagnostics leaked arbitrary payload")
		}
		if event.Fields["openai_ws_auto_incremental_reason"] != "unknown" {
			t.Fatal("reason must use a fixed vocabulary")
		}
		number, valid := index.(int)
		valid = valid && number >= 0 && number < 2048
		got, exists := event.Fields["openai_ws_auto_incremental_item_index"]
		if exists != valid {
			t.Fatalf("index presence=%v, valid=%v", exists, valid)
		}
		if valid {
			switch n := got.(type) {
			case int:
				if n != number {
					t.Fatal("index changed")
				}
			case int64:
				if n != int64(number) {
					t.Fatal("index changed")
				}
			default:
				t.Fatalf("bad index type %T", got)
			}
			if event.Fields["openai_ws_auto_incremental_item_type"] != "unknown" || event.Fields["openai_ws_auto_incremental_previous_item_type"] != "function_call" {
				t.Fatal("bounded item kinds missing")
			}
		} else {
			if _, ok := event.Fields["openai_ws_auto_incremental_item_type"]; ok {
				t.Fatal("invalid index must not imply an item diagnostic")
			}
		}
	}
}

func TestLoggerOpenAIWSIncrementalFieldsAbsentOnUnrelatedRequests(t *testing.T) {
	event := incrementalAccessEvent(t, nil)
	for key := range event.Fields {
		if strings.HasPrefix(key, "openai_ws_auto_incremental") {
			t.Fatal("unrelated request gained incremental state")
		}
	}
}
