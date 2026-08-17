package service

import (
	"maps"
	"net/http"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai_compat"
)

func normalizeOpenAICompatibleProviderConfig(
	platform string,
	accountType string,
	credentials map[string]any,
	extra map[string]any,
) (map[string]any, map[string]any, error) {
	if extra == nil {
		return credentials, extra, nil
	}
	raw, exists := extra[openai_compat.ExtraKeyCompatibleProvider]
	if !exists || raw == nil {
		return credentials, extra, nil
	}
	providerID, ok := raw.(string)
	if !ok {
		return nil, nil, infraerrors.New(http.StatusBadRequest, "INVALID_OPENAI_COMPATIBLE_PROVIDER", "openai_compatible_provider must be a string")
	}
	providerID = strings.TrimSpace(providerID)
	if providerID == "" {
		normalizedExtra := maps.Clone(extra)
		delete(normalizedExtra, openai_compat.ExtraKeyCompatibleProvider)
		return credentials, normalizedExtra, nil
	}
	preset, ok := openai_compat.CompatibleProviderPresetByID(providerID)
	if !ok {
		return nil, nil, infraerrors.New(http.StatusBadRequest, "INVALID_OPENAI_COMPATIBLE_PROVIDER", "unsupported OpenAI-compatible provider preset")
	}
	if platform != PlatformOpenAI || accountType != AccountTypeAPIKey {
		return nil, nil, infraerrors.New(http.StatusBadRequest, "INVALID_OPENAI_COMPATIBLE_PROVIDER_ACCOUNT", "OpenAI-compatible provider presets require an openai apikey account")
	}

	normalizedCredentials := maps.Clone(credentials)
	if normalizedCredentials == nil {
		normalizedCredentials = make(map[string]any, 3)
	}
	if configuredBaseURL, ok := normalizedCredentials["base_url"].(string); ok && strings.TrimSpace(configuredBaseURL) != "" {
		if strings.TrimRight(strings.TrimSpace(configuredBaseURL), "/") != strings.TrimRight(preset.BaseURL, "/") {
			return nil, nil, infraerrors.New(http.StatusBadRequest, "OPENAI_COMPATIBLE_PROVIDER_BASE_URL_MISMATCH", "base_url does not match the selected provider preset")
		}
	}
	normalizedCredentials["base_url"] = preset.BaseURL
	normalizedCredentials["model_mapping"] = preset.ModelMapping()
	normalizedCredentials[openAIEndpointCapabilitiesCredentialKey] = []string{string(OpenAIEndpointCapabilityChatCompletions)}

	normalizedExtra := maps.Clone(extra)
	normalizedExtra[openai_compat.ExtraKeyCompatibleProvider] = string(preset.ID)
	normalizedExtra[openai_compat.ExtraKeyResponsesMode] = string(preset.ResponsesMode)
	// These presets expose HTTP-compatible Chat/Responses APIs, not OpenAI's
	// Responses WebSocket protocol. Keep global WS rollout settings from
	// changing their transport behind the preset's back.
	normalizedExtra["openai_ws_force_http"] = true
	normalizedExtra["openai_apikey_responses_websockets_v2_mode"] = OpenAIWSIngressModeOff
	return normalizedCredentials, normalizedExtra, nil
}
