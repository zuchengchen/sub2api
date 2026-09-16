package service

import (
	"encoding/json"
	"maps"
	"time"
)

// extra429NearLimitKeys are existing OpenAI extra keys that protection backfill
// must never overwrite or delete. The list is the live 429 near-limit / auto-pause
// contract; adding a protection key to the merge must not include these names.
var extra429NearLimitKeys = []string{
	"auto_pause_5h_disabled",
	"auto_pause_7d_disabled",
	"auto_pause_5h_threshold",
	"auto_pause_7d_threshold",
}

// OpenAILegacyProtectionBackfillEligible is true only for independent OpenAI
// accounts that do not already have protection enabled. Shadow, non-OpenAI,
// random-proxy, and already-protected rows are skipped so the upgrade is
// extra-merge-only and idempotent.
func OpenAILegacyProtectionBackfillEligible(platform string, parentAccountID *int64, extra map[string]any) bool {
	if platform != PlatformOpenAI || parentAccountID != nil {
		return false
	}
	if extra != nil {
		if mode, _ := extra[accountProxyModeExtraKey].(string); mode == "random" {
			return false
		}
		if enabled, ok := extra[AntiDegradationExtraKey].(bool); ok && enabled {
			return false
		}
		if marker, _ := extra[AntiDegradeMarkerExtraKey].(map[string]any); marker != nil {
			if enabled, _ := marker["enabled"].(bool); enabled {
				return false
			}
			if mode, _ := marker["mode"].(string); mode == string(AntiDegradeModeLegacy) || mode == string(AntiDegradeMode1) {
				return false
			}
		}
		if scope, _ := extra[ProtectionScopeExtraKey].(string); scope == "legacy" || scope == "codex_v3" {
			return false
		}
	}
	return true
}

// MergeLegacyProtectionIntoExtra writes only protection keys. Existing extra
// entries (including 429 near-limit / auto_pause_* ) stay unless they are the
// protection keys this strategy owns.
func MergeLegacyProtectionIntoExtra(extra map[string]any) map[string]any {
	result := maps.Clone(extra)
	if result == nil {
		result = map[string]any{}
	}
	prev := map[string]any{}
	for _, key := range protectionManagedExtraKeys {
		if key == AntiDegradeMarkerExtraKey || key == AntiDegradationExtraKey || key == ProtectionScopeExtraKey {
			continue
		}
		if value, exists := result[key]; exists {
			prev[key] = value
		} else {
			prev[key] = nil
		}
	}
	result[AntiDegradationExtraKey] = true
	result[ProtectionScopeExtraKey] = "legacy"
	result[codexFingerprintModeExtraKey] = string(codexFingerprintSession)
	result[tlsFingerprintEnabledKey] = true
	result[tlsFingerprintBuiltinKey] = "nodejs24"
	delete(result, tlsFingerprintProfileIDKey)
	result[AntiDegradeMarkerExtraKey] = map[string]any{
		"enabled":         true,
		"mode":            string(AntiDegradeModeLegacy),
		"max_concurrency": 0,
		"applied_at":      time.Now().UTC().Format(time.RFC3339),
		"prev":            prev,
	}
	return result
}

// ApplyOpenAILegacyProtectionBackfill is the Go form of migration 253: skip
// ineligible rows, otherwise merge legacy protection keys and leave every other
// extra key (especially 429 near-limit) untouched.
func ApplyOpenAILegacyProtectionBackfill(platform string, parentAccountID *int64, extra map[string]any) (map[string]any, bool) {
	if !OpenAILegacyProtectionBackfillEligible(platform, parentAccountID, extra) {
		return extra, false
	}
	return MergeLegacyProtectionIntoExtra(extra), true
}

func extraHas429NearLimitKeys(extra map[string]any) bool {
	for _, key := range extra429NearLimitKeys {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

func cloneExtraJSON(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return maps.Clone(extra)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return maps.Clone(extra)
	}
	return out
}
