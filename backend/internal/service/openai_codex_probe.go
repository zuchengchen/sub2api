package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/google/uuid"
)

// Diagnostics stay on HTTP to measure that route, but share the Codex request
// contract and transport with business traffic. This interface avoids a
// constructor dependency cycle between the gateway and rate-limit service.
type openAICodexProbeSender interface {
	doOpenAICodexProbe(*http.Request, string, *Account) (*http.Response, error)
}

type codexProbePreparedKey struct{}

func codexProbeConfig(cfg *config.Config) *config.Config {
	if cfg != nil {
		return cfg
	}
	// Standalone diagnostics must use the production transport defaults too.
	cfg = &config.Config{}
	cfg.Gateway.OpenAIWS.ExactFidelityEnabled = true
	jitter := true
	cfg.Gateway.CodexWire.MapOrderJitter = &jitter
	return cfg
}

func (s *OpenAIGatewayService) doOpenAICodexProbe(req *http.Request, proxyURL string, account *Account) (*http.Response, error) {
	if s == nil || s.httpUpstream == nil || req == nil || account == nil {
		return nil, fmt.Errorf("codex probe transport is unavailable")
	}
	if s.cfg == nil {
		s = &OpenAIGatewayService{
			cfg: codexProbeConfig(nil), httpUpstream: s.httpUpstream,
			accountRepo: s.accountRepo, pluginManager: s.pluginManager,
		}
	}
	if err := s.prepareOpenAICodexProbeRequest(req, account); err != nil {
		return nil, err
	}
	// Preserve each diagnostic caller's admission and error policy. A health
	// sample must not refresh credentials or record a permanent auth failure.
	s.maybeCompressCodexUpstreamRequest(req, account)
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(req.Context(), req, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.doOpenAIHTTPTransport(req, proxyURL, account, account)
}

func (s *OpenAIGatewayService) prepareOpenAICodexProbeRequest(req *http.Request, account *Account) error {
	if account == nil || !account.IsOpenAI() || !account.IsOpenAIOAuthLike() {
		return nil
	}
	if prepared, _ := req.Context().Value(codexProbePreparedKey{}).(bool); prepared {
		return nil
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	identity := resolveCodexOutboundIdentity(s.codexIdentityOverrideUA(account))
	req.Header.Set("User-Agent", identity.userAgent)
	req.Header.Set("originator", identity.originator)
	req.Header.Set("version", identity.version)
	// Direct image endpoints have a different body and negotiation contract.
	if req.URL == nil || !strings.HasSuffix(strings.TrimRight(req.URL.Path, "/"), "/responses") {
		return nil
	}
	if req.Body == nil || req.Body == http.NoBody || req.Header.Get("Content-Encoding") != "" {
		return fmt.Errorf("codex probe requires an unencoded Responses body")
	}
	raw, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return fmt.Errorf("read Codex probe: %w", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return fmt.Errorf("invalid Codex probe JSON object")
	}
	model := normalizeOpenAIModelForUpstream(account, firstNonEmptyString(body["model"]))
	body["model"] = model
	descriptor, hasDescriptor := openai.CodexCatalogModelForSlug(model)
	profile, _ := resolveCodexWireProfile(identity.version, codexProbeConfig(s.cfg).Gateway.CodexWire.Profile)
	device, ok := codexDeviceProfileForRequest(account)
	if !ok {
		// Setup tokens use the Codex endpoint without OAuth-only device extras.
		device = codexDeviceProfile{SandboxMode: codexDefaultWorkspaceMode, WorkspaceCWD: codexDefaultWorkspaceCWD, Timezone: codexDefaultTimezone}
	}
	now := time.Now()
	session, err := uuid.NewV7()
	if err != nil {
		return err
	}
	turn, err := uuid.NewV7()
	if err != nil {
		return err
	}
	// Each standalone diagnostic is a fresh conversation. Retries reuse the
	// prepared request; installation identity still belongs to the credential.
	sessionID := session.String()
	installationID := account.GetOpenAIDeviceID()
	if seed, ok := codexFingerprintSeed(account.Extra); ok {
		installationID = resolveConvergedInstallationID(account, seed)
	}
	if installationID == "" {
		installationID = deriveStableUUIDv4("sub2api:codex-native-installation:" + firstNonEmptyString(codexAccountIdentityNamespace(account), codexIdentityClockSeed(account)))
	}
	body["client_metadata"] = map[string]any{
		"session_id": sessionID, "thread_id": sessionID, "turn_id": turn.String(),
		"x-codex-installation-id": installationID, "x-codex-window-id": sessionID + ":0",
	}
	switch strings.ToLower(strings.TrimSpace(account.GetExtraString(codexPromptCacheKeyExtraKey))) {
	case string(codexPromptCacheKeyOmit):
		delete(body, "prompt_cache_key")
	case string(codexPromptCacheKeyCustom):
		// A custom policy leaves the caller's optional key unchanged.
	default:
		body["prompt_cache_key"] = sessionID
	}
	opts := codexSynthOptions{WireProfile: profile, Device: device, Now: now, TurnID: turn.String(), TurnStartedMs: now.UnixMilli()}
	opts.Environment = codexSynthesisConfiguredEnvironment(account, body)
	opts.ResponsesLite = hasDescriptor && descriptor.UseResponsesLite
	// Probes have no local tool executor. Preserve declared hosted tools, but
	// never inject shell/edit tools merely to imitate an installed CLI.
	original, _ := codexSynthesisCollectOriginalInput(body)
	prefix, _ := codexSynthesisPrefixItems(descriptor, hasDescriptor, codexCapabilityGeneric,
		codexSynthesisBaseInstructions(model, descriptor, hasDescriptor, profile), opts)
	body["input"] = append(prefix, original...)
	if _, exists := body["tools"]; !exists && !opts.ResponsesLite {
		body["tools"] = []any{}
	}
	codexSynthesisApplyDefaults(body, descriptor, hasDescriptor, opts)
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if hasDescriptor && descriptor.SupportsReasoningSummaryParameter && descriptor.DefaultReasoningSummary != "" && descriptor.DefaultReasoningSummary != "none" {
			if _, exists := reasoning["summary"]; !exists {
				reasoning["summary"] = descriptor.DefaultReasoningSummary
			}
		}
		if opts.ResponsesLite {
			reasoning["context"] = "all_turns"
		}
	}
	if opts.ResponsesLite {
		if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
			delete(body, "tools")
			codexSynthesisAppendTools(body, tools, true)
		}
		delete(body, "tools")
		body["parallel_tool_calls"] = false
	}
	result := applyCodexOAuthTransformWithOptions(body, codexOAuthTransformOptions{
		IsCodexCLI: true, SkipDefaultInstructions: true, ResponsesLite: opts.ResponsesLite, WireProfile: &profile,
	})
	if result.Error != nil {
		return result.Error
	}
	// Keep supplemental instructions as input without duplicating the base prompt.
	if instruction, ok := body["instructions"].(string); ok {
		delete(body, "instructions")
		if strings.TrimSpace(instruction) != "" {
			input, ok := body["input"].([]any)
			if !ok || len(input) == 0 {
				return fmt.Errorf("codex probe input is missing its base instructions")
			}
			item := codexSynthMessageRaw(codexSynthItemID("probe-guidance", instruction), "developer", []string{instruction}, nil, turn.String(), float64(now.UnixMilli())/1000)
			body["input"] = append([]any{input[0], item}, input[1:]...)
		}
	}
	if opts.ResponsesLite {
		req.Header.Set(responsesLiteHeader, "true")
	} else {
		req.Header.Del(responsesLiteHeader)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-codex-beta-features", profile.DefaultBetaFeatures)
	for _, name := range []string{"session_id", "conversation_id", "session-id", "thread-id", "turn-id", "x-codex-turn-state", "x-codex-turn-metadata", "x-codex-installation-id", "x-codex-window-id", "x-client-request-id"} {
		deleteHeaderAllForms(req.Header, name)
	}
	if err := normalizeCodexRequestMetadataMap(body, req.Header, account); err != nil {
		return err
	}
	applyCodexWireMetadata(body, req.Header, account, "turn", false)
	metadata, ok := body["client_metadata"].(map[string]any)
	if !ok {
		return fmt.Errorf("codex probe metadata is invalid")
	}
	delete(metadata, "x-codex-ws-stream-request-start-ms")
	delete(metadata, "ws_request_header_x_openai_internal_codex_responses_lite")
	// Reuse the existing serde-shaped field writers for nested structs as well
	// as the HTTP top-level order; json.Marshal alone sorts struct fields.
	for _, key := range []string{"input", "reasoning", "text", "client_metadata"} {
		if value, exists := body[key]; exists {
			var field bytes.Buffer
			if err := writeCodexWSFrameFieldValue(&field, nil, key, value, s.codexMapOrderJitterEnabled()); err != nil {
				return err
			}
			body[key] = json.RawMessage(field.Bytes())
		}
	}
	encoded, err := marshalOpenAIUpstreamJSON(body)
	if err != nil {
		return err
	}
	encoded, _ = reorderCodexRequestTopLevelJSON(encoded)
	encoded = escapeCodexNonASCII(encoded)
	projectCodexOutboundHeaders(req.Header, false)
	setOpenAICodexRoutingHintFromBody(req.Header, account, encoded)
	req.Body = io.NopCloser(bytes.NewReader(encoded))
	req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(encoded)), nil }
	req.ContentLength = int64(len(encoded))
	req.Header.Del("Content-Length")
	*req = *req.WithContext(context.WithValue(WithHTTPUpstreamProfile(req.Context(), HTTPUpstreamProfileOpenAI), codexProbePreparedKey{}, true))
	return nil
}
