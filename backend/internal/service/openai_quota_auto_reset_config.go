package service

const (
	OpenAIAutoResetCreditEnabledExtraKey     = "auto_reset_credit_enabled"
	OpenAIAutoResetCredit5hThresholdExtraKey = "auto_reset_credit_5h_threshold"
	OpenAIAutoResetCredit7dThresholdExtraKey = "auto_reset_credit_7d_threshold"
	OpenAIAutoResetCreditStateExtraKey       = "codex_auto_reset_credit_state"
)

// normalizeOpenAIAutoResetCreditExtra drops the retired automatic reset-credit
// settings so leftover extra keys cannot re-enable the feature after an update.
func normalizeOpenAIAutoResetCreditExtra(_ string, _ string, _ bool, extra map[string]any) (map[string]any, error) {
	return stripOpenAIAutoResetCreditManagedExtra(extra, true), nil
}

func stripOpenAIAutoResetCreditManagedExtra(extra map[string]any, stripConfig bool) map[string]any {
	if extra == nil {
		return nil
	}
	delete(extra, OpenAIAutoResetCreditStateExtraKey)
	if stripConfig {
		delete(extra, OpenAIAutoResetCreditEnabledExtraKey)
		delete(extra, OpenAIAutoResetCredit5hThresholdExtraKey)
		delete(extra, OpenAIAutoResetCredit7dThresholdExtraKey)
	}
	return extra
}
