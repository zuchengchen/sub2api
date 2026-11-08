package service

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserCacheDiagnosticTerminalPrefixBoundaries(t *testing.T) {
	for _, fault := range []string{"clean", "bom", "invalid_json", "duplicate_json", "invalid_utf8", "response_error", "root_error", "nested_error", "failed", "id_conflict", "metadata_id_conflict", "event_type_conflict", "truncated", "limit", "missing_id", "missing_usage", "usage_wrong_type", "missing_status", "other_status", "suffix_partial", "suffix_mutation"} {
		t.Run(fault, func(t *testing.T) {
			wire := terminalDiagnosticWire()
			switch fault {
			case "bom":
				wire = append([]byte{0xef, 0xbb, 0xbf}, wire...)
			case "invalid_json":
				wire = append([]byte("data: {broken}\n\n"), wire...)
			case "duplicate_json":
				wire = append([]byte("data: {\"type\":\"response.created\",\"type\":\"response.created\"}\n\n"), wire...)
			case "invalid_utf8":
				wire = append([]byte{'d', 'a', 't', 'a', ':', ' ', '{', '"', 'x', '"', ':', '"', 0xff, '"', '}', '\n', '\n'}, wire...)
			case "response_error":
				wire = append([]byte("data: {\"type\":\"response.error\",\"code\":\"invalid_api_key\",\"message\":\"SECRET_ERROR\"}\n\n"), wire...)
			case "failed":
				wire = append([]byte("data: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_private\",\"error\":{\"code\":\"insufficient_quota\"}}}\n\n"), wire...)
			case "root_error":
				wire = append([]byte("data: {\"type\":\"response.metadata\",\"error\":{\"code\":\"insufficient_quota\",\"message\":\"SECRET_ERROR\"}}\n\n"), wire...)
			case "nested_error":
				wire = append([]byte("data: {\"type\":\"response.in_progress\",\"response\":{\"id\":\"resp_private\",\"error\":{\"code\":\"invalid_api_key\"}}}\n\n"), wire...)
			case "metadata_id_conflict":
				wire = append([]byte("data: {\"type\":\"response.metadata\",\"response_id\":\"other_private\"}\n\n"), wire...)
			case "event_type_conflict":
				wire = append([]byte("event: response.created\ndata: {\"type\":\"response.metadata\"}\n\n"), wire...)
			case "id_conflict":
				wire = bytes.Replace(wire, []byte(`"id":"resp_private"`), []byte(`"id":"other_private"`), 1)
			case "truncated":
				wire = append([]byte("data: "+strings.Repeat("x", userCacheDiagnosticMaxLine+1)+"\n\n"), wire...)
			case "missing_id", "missing_usage", "usage_wrong_type", "missing_status", "other_status":
				parts := bytes.Split(wire, []byte("\n\n"))
				var event map[string]any
				require.NoError(t, json.Unmarshal(bytes.TrimPrefix(parts[1], []byte("data: ")), &event))
				response := event["response"].(map[string]any)
				switch fault {
				case "missing_id":
					delete(response, "id")
				case "missing_usage":
					delete(response, "usage")
				case "usage_wrong_type":
					response["usage"] = "SECRET_USAGE_TYPE"
				case "missing_status":
					delete(response, "status")
				case "other_status":
					response["status"] = "in_progress"
				}
				encoded, err := json.Marshal(event)
				require.NoError(t, err)
				parts[1] = append([]byte("data: "), encoded...)
				wire = bytes.Join(parts, []byte("\n\n"))
				if fault == "missing_usage" || fault == "usage_wrong_type" {
					wire = bytes.Replace(wire, []byte(`"status":"in_progress"`), []byte(`"status":"in_progress","usage":{"input_tokens":999,"output_tokens":999}`), 1)
				}
			case "suffix_partial":
				wire = append(wire, []byte("data: [SECRET_UNFINISHED")...)
			case "suffix_mutation":
				wire = append(wire, []byte("data: [DONE]\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"other_private\",\"usage\":{\"input_tokens\":999},\"output\":[{\"type\":\"message\",\"id\":\"WRONG_SUFFIX_SECRET\"}]}}\n\n")...)
			}
			o, path := terminalDiagnosticObserver(t, io.NopCloser(bytes.NewReader(wire)))
			if fault == "limit" {
				o.observedBytes = userCacheDiagnosticMaxBytes
			}
			got, err := io.ReadAll(o)
			require.NoError(t, err)
			require.Equal(t, wire, got)
			require.NoError(t, o.Close())
			row := userCacheDiagnosticReadEvents(t, path)[2]
			if fault == "limit" || fault == "missing_id" {
				require.Nil(t, row["terminal_candidate"])
				return
			}
			candidate := row["terminal_candidate"].(map[string]any)
			switch fault {
			case "invalid_json", "duplicate_json", "invalid_utf8", "event_type_conflict":
				require.Positive(t, candidate["prefix_invalid_json_events"])
			case "response_error", "root_error", "nested_error", "failed":
				require.Equal(t, true, candidate["prefix_error_event"])
			case "id_conflict", "metadata_id_conflict":
				require.Equal(t, true, candidate["prefix_response_id_conflict"])
			case "truncated":
				require.Positive(t, candidate["prefix_truncated_events"])
			case "missing_usage", "usage_wrong_type":
				require.Nil(t, candidate["usage"])
				require.NotNil(t, row["usage"])
			case "missing_status":
				require.Equal(t, "missing", candidate["status_class"])
			case "other_status":
				require.Equal(t, "other", candidate["status_class"])
			case "suffix_partial", "suffix_mutation":
				require.Positive(t, row["post_terminal_bytes"])
				require.Equal(t, false, row["partial_event"])
				require.EqualValues(t, 0, candidate["prefix_invalid_json_events"])
				require.Equal(t, diagnosticSHA256([]byte("resp_private")), candidate["response_id_sha256"])
				usage := candidate["usage"].(map[string]any)["response.usage"].(map[string]any)
				require.EqualValues(t, 17, usage["input_tokens"])
				items := candidate["output_items"].(map[string]any)
				require.Equal(t, diagnosticSHA256([]byte("msg_private")), items["0"].(map[string]any)["id_sha256"])
			default:
				require.Equal(t, "completed", candidate["status_class"])
			}
			encoded, err := json.Marshal(row)
			require.NoError(t, err)
			for _, secret := range []string{"PRIVATE_ANSWER", "SECRET_ERROR", "SECRET_UNFINISHED", "resp_private", "other_private", "msg_private", "SECRET_USAGE_TYPE", "WRONG_SUFFIX_SECRET"} {
				require.NotContains(t, string(encoded), secret)
			}
		})
	}
}
