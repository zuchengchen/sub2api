//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Public synthetic fixture values, never real account credentials. The UUID was
// independently calculated from SHA-256("sub2api:codex-install-id:v2:" + seed)
// with RFC 4122 variant/version 4 bits, rather than read from a service helper.
const gatewayGoldenFingerprintSeed = "11111111-1111-4111-8111-111111111111"
const gatewayGoldenInstallationID = "3a479c33-60a4-4c91-8479-62da0fecf605"
const gatewayFingerprintContract = "seeded_full_identity_v1"

// Only identity fields are retained from the ingress; prompts, tool arguments,
// Authorization and arbitrary client metadata must never enter this evidence.
type gatewayFingerprintPayload struct {
	Type            string                     `json:"type,omitempty"`
	Metadata        map[string]string          `json:"metadata"`
	TurnMetadata    map[string]json.RawMessage `json:"turn_metadata,omitempty"`
	HasTurnMetadata bool                       `json:"has_turn_metadata"`
	PromptCacheKey  *string                    `json:"prompt_cache_key,omitempty"`
}
type gatewayFingerprintHeaders struct {
	Identity        map[string]string          `json:"identity"`
	TurnMetadata    map[string]json.RawMessage `json:"turn_metadata,omitempty"`
	HasTurnMetadata bool                       `json:"has_turn_metadata"`
}
type gatewayFingerprintIngress struct {
	Sequence  int                       `json:"sequence"`
	Transport string                    `json:"transport"`
	Headers   gatewayFingerprintHeaders `json:"headers"`
	Body      gatewayFingerprintPayload `json:"body"`
}
type gatewayFingerprintExecution struct {
	HeaderBindings       int `json:"header_bindings"`
	BodyBindings         int `json:"body_bindings"`
	Handshakes           int `json:"handshakes"`
	ChangedSessions      int `json:"changed_sessions"`
	ChangedInstallations int `json:"changed_installations"`
}
type gatewayFingerprintEvidence struct {
	Contract  string                      `json:"contract"`
	Mode      string                      `json:"mode"`
	Seed      string                      `json:"seed"`
	Ingress   []gatewayFingerprintIngress `json:"ingress"`
	Execution gatewayFingerprintExecution `json:"execution"`
}

var gatewayFingerprintPairs = [][2]string{
	{"session_id", "session_id"}, {"thread_id", "thread_id"},
	{"x-codex-installation-id", "installation_id"}, {"x-codex-window-id", "window_id"},
	{"turn_id", "turn_id"}, {"root_turn_id", "root_turn_id"},
	{"parent_turn_id", "parent_turn_id"}, {"x-codex-parent-thread-id", "parent_thread_id"},
}
var gatewayFingerprintHeaderNames = []string{"session-id", "thread-id", "x-client-request-id", "x-codex-window-id", "x-codex-installation-id"}

func gatewayFingerprintExtra(extra map[string]any) map[string]any {
	if extra == nil {
		extra = map[string]any{}
	}
	extra["codex_fingerprint_mode"] = "full"
	extra["codex_fingerprint_seed"] = gatewayGoldenFingerprintSeed
	return extra
}

func gatewayFingerprintTurnFields(raw string) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &object); err != nil || object == nil {
		return nil, fmt.Errorf("invalid identity turn metadata JSON")
	}
	selected := map[string]json.RawMessage{}
	for _, pair := range gatewayFingerprintPairs {
		if value, ok := object[pair[1]]; ok {
			selected[pair[1]] = value
		}
	}
	for _, key := range []string{"request_kind", "window_number"} {
		if value, ok := object[key]; ok {
			selected[key] = value
		}
	}
	return selected, nil
}

func gatewayFingerprintBody(raw []byte) (gatewayFingerprintPayload, error) {
	var result gatewayFingerprintPayload
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return result, fmt.Errorf("invalid identity request JSON")
	}
	if value, ok := body["type"]; ok {
		if err := json.Unmarshal(value, &result.Type); err != nil {
			return result, err
		}
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(body["client_metadata"], &metadata); err != nil || metadata == nil {
		return result, fmt.Errorf("missing client metadata identity object")
	}
	result.Metadata = map[string]string{}
	for _, pair := range gatewayFingerprintPairs {
		if value, ok := metadata[pair[0]]; ok {
			var text string
			if err := json.Unmarshal(value, &text); err != nil || string(value) == "null" {
				return result, fmt.Errorf("non-string identity %s", pair[0])
			}
			result.Metadata[pair[0]] = text
		}
	}
	if value, ok := metadata["x-codex-turn-metadata"]; ok {
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return result, err
		}
		fields, err := gatewayFingerprintTurnFields(text)
		if err != nil {
			return result, err
		}
		result.HasTurnMetadata, result.TurnMetadata = true, fields
	}
	if value, ok := body["prompt_cache_key"]; ok {
		var text string
		if err := json.Unmarshal(value, &text); err != nil || string(value) == "null" {
			return result, fmt.Errorf("non-string prompt cache identity")
		}
		result.PromptCacheKey = &text
	}
	return result, nil
}

func gatewayFingerprintHeaderFields(headers http.Header) (gatewayFingerprintHeaders, error) {
	result := gatewayFingerprintHeaders{Identity: map[string]string{}}
	for _, key := range append(append([]string{}, gatewayFingerprintHeaderNames...), "x-codex-turn-metadata") {
		values := headers.Values(key)
		if len(values) > 1 {
			return result, fmt.Errorf("duplicate identity header %s", key)
		}
		if len(values) == 0 {
			continue
		}
		if key != "x-codex-turn-metadata" {
			result.Identity[key] = values[0]
			continue
		}
		selected, err := gatewayFingerprintTurnFields(values[0])
		if err != nil {
			return result, err
		}
		result.HasTurnMetadata, result.TurnMetadata = true, selected
	}
	return result, nil
}

func gatewayIngressFingerprint(transport string, headers http.Header, body []byte) (gatewayFingerprintIngress, error) {
	result := gatewayFingerprintIngress{Transport: transport}
	var err error
	result.Headers, err = gatewayFingerprintHeaderFields(headers)
	if err != nil {
		return result, err
	}
	result.Body, err = gatewayFingerprintBody(body)
	return result, err
}

func gatewayFingerprintUUID(raw string, version uuid.Version) bool {
	id, err := uuid.Parse(raw)
	return err == nil && id != uuid.Nil && id.String() == raw && id.Version() == version && id.Variant() == uuid.RFC4122
}

func gatewayFingerprintWindow(window, thread string) bool {
	suffix, ok := strings.CutPrefix(window, thread+":")
	return ok && regexp.MustCompile(`^(0|[1-9][0-9]*)$`).MatchString(suffix)
}

func validateGatewayFingerprintBody(body gatewayFingerprintPayload, seeded bool) error {
	flat := body.Metadata
	for _, key := range []string{"session_id", "thread_id"} {
		if !gatewayFingerprintUUID(flat[key], 7) {
			return fmt.Errorf("%s is not canonical UUIDv7", key)
		}
	}
	if !gatewayFingerprintUUID(flat["x-codex-installation-id"], 4) || seeded && flat["x-codex-installation-id"] != gatewayGoldenInstallationID {
		return fmt.Errorf("installation identity does not match fixture seed")
	}
	if !gatewayFingerprintWindow(flat["x-codex-window-id"], flat["thread_id"]) {
		return fmt.Errorf("window identity lost thread binding")
	}
	// Root exec is the selected fixture: it uses session-derived cache keys, not
	// a custom/omit policy. Missing cache keys are not invented by this verifier.
	if body.PromptCacheKey != nil && *body.PromptCacheKey != flat["session_id"] {
		return fmt.Errorf("default prompt cache identity lost session binding")
	}
	for _, key := range []string{"turn_id", "root_turn_id", "parent_turn_id", "x-codex-parent-thread-id"} {
		if value := flat[key]; value != "" && !gatewayFingerprintUUID(value, 7) {
			return fmt.Errorf("%s lost UUIDv7 shape", key)
		}
	}
	if !body.HasTurnMetadata {
		if len(body.TurnMetadata) != 0 {
			return fmt.Errorf("unmarked embedded metadata")
		}
		return nil
	}
	// Official e54a5713 responses_metadata.rs emits turn fields only when set.
	// In-turn prewarm may have a turn ID; startup prewarm can omit it or use "".
	for _, pair := range gatewayFingerprintPairs {
		if raw, exists := body.TurnMetadata[pair[1]]; exists {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil || string(raw) == "null" {
				return fmt.Errorf("invalid embedded identity %s", pair[1])
			}
			flatValue, ok := flat[pair[0]]
			if !ok || value != flatValue {
				return fmt.Errorf("flat/embedded identity mismatch for %s", pair[1])
			}
		}
	}
	if raw, ok := body.TurnMetadata["window_number"]; ok {
		var number uint64
		if err := json.Unmarshal(raw, &number); err != nil || fmt.Sprintf("%s:%d", flat["thread_id"], number) != flat["x-codex-window-id"] {
			return fmt.Errorf("embedded window number lost binding")
		}
	}
	return nil
}

func validateGatewayFingerprintHeaders(headers gatewayFingerprintHeaders, seeded bool) error {
	h := headers.Identity
	// build_session_headers always emits these for this root CLI fixture. An
	// installation header is NOT emitted by official Responses compatibility_headers.
	if !gatewayFingerprintUUID(h["session-id"], 7) || !gatewayFingerprintUUID(h["thread-id"], 7) {
		return fmt.Errorf("missing or invalid session/thread headers")
	}
	if value, ok := h["x-client-request-id"]; ok && value != h["thread-id"] {
		return fmt.Errorf("request header lost thread binding")
	}
	if value, ok := h["x-codex-window-id"]; ok && !gatewayFingerprintWindow(value, h["thread-id"]) {
		return fmt.Errorf("header window lost thread binding")
	}
	if value, ok := h["x-codex-installation-id"]; ok && (!gatewayFingerprintUUID(value, 4) || seeded && value != gatewayGoldenInstallationID) {
		return fmt.Errorf("wrong optional installation header")
	}
	if !headers.HasTurnMetadata {
		if len(headers.TurnMetadata) != 0 {
			return fmt.Errorf("unmarked header metadata")
		}
		return nil
	}
	for _, pair := range [][2]string{{"session-id", "session_id"}, {"thread-id", "thread_id"}, {"x-codex-window-id", "window_id"}, {"x-codex-installation-id", "installation_id"}} {
		raw, present := headers.TurnMetadata[pair[1]]
		if !present {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil || string(raw) == "null" {
			return fmt.Errorf("invalid embedded header identity")
		}
		if direct, exists := h[pair[0]]; exists && value != direct {
			return fmt.Errorf("direct/embedded header identity mismatch")
		}
		if pair[1] == "installation_id" && seeded && value != gatewayGoldenInstallationID {
			return fmt.Errorf("wrong embedded header installation")
		}
	}
	return nil
}

func bindGatewayFingerprintHeaders(headers gatewayFingerprintHeaders, body gatewayFingerprintPayload, transport string) error {
	if headers.Identity["session-id"] != body.Metadata["session_id"] || headers.Identity["thread-id"] != body.Metadata["thread_id"] {
		return fmt.Errorf("header/body session or thread mismatch")
	}
	if transport != "http" {
		return nil
	}
	// A WS handshake is retained across prewarm and later turns. Window/turn
	// metadata belongs to the handshake instant, not every subsequent frame.
	if value, ok := headers.Identity["x-codex-window-id"]; ok && value != body.Metadata["x-codex-window-id"] {
		return fmt.Errorf("HTTP header/body window mismatch")
	}
	for _, pair := range gatewayFingerprintPairs {
		if raw, ok := headers.TurnMetadata[pair[1]]; ok {
			var value string
			if err := json.Unmarshal(raw, &value); err != nil {
				return err
			}
			if bodyValue, exists := body.Metadata[pair[0]]; exists && bodyValue != value {
				return fmt.Errorf("HTTP header/body embedded identity mismatch")
			}
		}
	}
	return nil
}

// Recompute from observed raw rows every time. Configuration labels and stored
// counters cannot prove that the production parser/header/body path ran.
func validateGatewayFingerprintExecution(evidence gatewayFingerprintEvidence, records []record) (gatewayFingerprintExecution, error) {
	proof := gatewayFingerprintExecution{}
	fail := func(err error) (gatewayFingerprintExecution, error) {
		return proof, fmt.Errorf("seeded full fingerprint evidence: %w", err)
	}
	if evidence.Contract != gatewayFingerprintContract || evidence.Mode != "full" || evidence.Seed != gatewayGoldenFingerprintSeed || !gatewayFingerprintUUID(evidence.Seed, 4) {
		return fail(fmt.Errorf("missing explicit full mode or legal fixture seed"))
	}
	if len(evidence.Ingress) == 0 {
		return fail(fmt.Errorf("missing observed ingress identities"))
	}
	sessions, installations := map[string]bool{}, map[string]bool{}
	rootRelationship := true
	for i, entry := range evidence.Ingress {
		if entry.Sequence != i+1 || (entry.Transport != "http" && entry.Transport != "ws") || entry.Transport == "ws" && entry.Body.Type != "response.create" {
			return fail(fmt.Errorf("invalid ingress sequence or transport"))
		}
		if err := validateGatewayFingerprintBody(entry.Body, false); err != nil {
			return fail(err)
		}
		if err := validateGatewayFingerprintHeaders(entry.Headers, false); err != nil {
			return fail(err)
		}
		if err := bindGatewayFingerprintHeaders(entry.Headers, entry.Body, entry.Transport); err != nil {
			return fail(err)
		}
		sessions[entry.Body.Metadata["session_id"]] = true
		installations[entry.Body.Metadata["x-codex-installation-id"]] = true
		rootRelationship = rootRelationship && entry.Body.Metadata["session_id"] == entry.Body.Metadata["thread_id"]
	}
	if installations[gatewayGoldenInstallationID] {
		return fail(fmt.Errorf("fixture seed must differ from native ingress installation"))
	}
	rows := append([]record(nil), records...)
	sort.Slice(rows, func(i, j int) bool { return rows[i].Seq < rows[j].Seq })
	handshakes := map[int]gatewayFingerprintHeaders{}
	boundHandshake := map[int]bool{}
	outputSessions := map[string]bool{}
	for _, row := range rows {
		if row.Hop != "gateway_upstream" {
			continue
		}
		target, _, _ := strings.Cut(row.Target, "?")
		if row.Kind != "ws_message" && target != "/backend-api/codex/responses" {
			continue
		}
		headers := gatewayFingerprintHeaders{}
		if row.Kind != "ws_message" {
			h := http.Header{}
			for _, field := range row.Headers {
				h.Add(field.Name, field.Value)
			}
			var err error
			headers, err = gatewayFingerprintHeaderFields(h)
			if err != nil {
				return fail(err)
			}
			if err := validateGatewayFingerprintHeaders(headers, true); err != nil {
				return fail(err)
			}
		}
		if row.Kind == "ws_handshake" {
			handshakes[row.Conn] = headers
			boundHandshake[row.Conn] = false
			proof.Handshakes++
			continue // 401/426 attempts need not have a response.create on this socket.
		}
		transport := "http"
		if row.Kind == "ws_message" {
			transport = "ws"
			var ok bool
			headers, ok = handshakes[row.Conn]
			if !ok {
				return fail(fmt.Errorf("WS message has no prior observed handshake"))
			}
		} else if row.Kind != "http_request" || row.Method != "POST" {
			return fail(fmt.Errorf("unexpected Responses request shape"))
		}
		body, err := gatewayFingerprintBody(row.BodyJSON)
		if err != nil {
			return fail(err)
		}
		if transport == "ws" && body.Type != "response.create" {
			return fail(fmt.Errorf("unexpected WS identity message type"))
		}
		if err := validateGatewayFingerprintBody(body, true); err != nil {
			return fail(err)
		}
		if err := bindGatewayFingerprintHeaders(headers, body, transport); err != nil {
			return fail(err)
		}
		session := body.Metadata["session_id"]
		if rootRelationship && session != body.Metadata["thread_id"] {
			return fail(fmt.Errorf("full mode broke native root session/thread equality"))
		}
		if sessions[session] || installations[body.Metadata["x-codex-installation-id"]] {
			return fail(fmt.Errorf("observed identity was not rewritten"))
		}
		outputSessions[session] = true
		proof.BodyBindings++
		proof.ChangedSessions++
		proof.ChangedInstallations++
		if transport == "http" || !boundHandshake[row.Conn] {
			proof.HeaderBindings++
			boundHandshake[row.Conn] = true
		}
	}
	if proof.BodyBindings == 0 || proof.HeaderBindings == 0 {
		return fail(fmt.Errorf("no positive observed body/header bindings"))
	}
	if len(outputSessions) > len(sessions) {
		return fail(fmt.Errorf("one native session split into multiple output identities"))
	}
	return proof, nil
}

func expectedGatewayFingerprintPaths() []string {
	var paths []string
	for _, name := range fullScenarioNames {
		paths = append(paths, filepath.Join("raw", name+".jsonl"), name+"-provenance.json")
	}
	sort.Strings(paths)
	return paths
}
func finalizeGatewayFingerprintHashes(dir string, meta map[string]any) error {
	if meta["recorder"] != "backend/tools/codexgolden gateway" {
		return nil
	}
	hashes := map[string]string{}
	for _, path := range expectedGatewayFingerprintPaths() {
		data, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			return err
		}
		hashes[path] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	meta["gateway_fingerprint_evidence_sha256"] = hashes
	return nil
}

func readGatewayFingerprintEvidence(dir, scenario string, meta captureMetadata, normalized map[recordKey]record) error {
	paths := expectedGatewayFingerprintPaths()
	if len(meta.GatewayFingerprintHashes) != len(paths) {
		return fmt.Errorf("missing fingerprint evidence hashes")
	}
	for _, path := range paths {
		digest, ok := meta.GatewayFingerprintHashes[path]
		if !ok || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
			return fmt.Errorf("missing fingerprint evidence hash for %s", path)
		}
	}
	for _, path := range []string{filepath.Join("raw", scenario+".jsonl"), scenario + "-provenance.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(raw)) != meta.GatewayFingerprintHashes[path] {
			return fmt.Errorf("missing or changed fingerprint evidence: %s", path)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, scenario+"-provenance.json"))
	if err != nil {
		return err
	}
	var provenance struct {
		Scenario    string                     `json:"scenario"`
		Fingerprint gatewayFingerprintEvidence `json:"fingerprint_evidence"`
	}
	if err := json.Unmarshal(raw, &provenance); err != nil {
		return err
	}
	if provenance.Scenario != scenario {
		return fmt.Errorf("fingerprint scenario identity mismatch")
	}
	rows, err := readRecordMap(filepath.Join(dir, "raw", scenario+".jsonl"))
	if err != nil {
		return err
	}
	records := make([]record, 0, len(rows))
	for _, row := range rows {
		records = append(records, row)
	}
	execution, err := validateGatewayFingerprintExecution(provenance.Fingerprint, records)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(execution, provenance.Fingerprint.Execution) {
		return fmt.Errorf("stored fingerprint counters disagree with raw/ingress recomputation")
	}
	// The actual identity fields are unchanged by path/endpoint normalization.
	// Bind proof to the records being compared, not an unrelated raw capture.
	rawProjection, err := gatewayFingerprintProjection(rows)
	if err != nil {
		return err
	}
	normalizedProjection, err := gatewayFingerprintProjection(normalized)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(rawProjection, normalizedProjection) {
		return fmt.Errorf("raw fingerprint proof disagrees with compared records")
	}
	return nil
}

func gatewayFingerprintProjection(rows map[recordKey]record) (map[recordKey]any, error) {
	result := map[recordKey]any{}
	for key, row := range rows {
		target, _, _ := strings.Cut(row.Target, "?")
		if row.Hop != "gateway_upstream" || row.Kind != "ws_message" && target != "/backend-api/codex/responses" {
			continue
		}
		h := http.Header{}
		for _, field := range row.Headers {
			h.Add(field.Name, field.Value)
		}
		headers, err := gatewayFingerprintHeaderFields(h)
		if err != nil {
			return nil, err
		}
		var body gatewayFingerprintPayload
		if row.Kind != "ws_handshake" {
			body, err = gatewayFingerprintBody(row.BodyJSON)
			if err != nil {
				return nil, err
			}
		}
		result[key] = struct {
			Kind    string
			Headers gatewayFingerprintHeaders
			Body    gatewayFingerprintPayload
		}{row.Kind, headers, body}
	}
	return result, nil
}
