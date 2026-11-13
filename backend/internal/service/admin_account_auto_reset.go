package service

import "context"

// SetOpenAIAutoResetCreditEnabled changes only the automatic reset opt-in. The
// quota worker owns consumption, recovery, and in-flight attempt state.
func (s *adminServiceImpl) SetOpenAIAutoResetCreditEnabled(ctx context.Context, id int64, enabled bool) (*Account, error) {
	account, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if account == nil {
		return nil, ErrAccountNotFound
	}

	config := map[string]any{OpenAIAutoResetCreditEnabledExtraKey: enabled}
	if enabled {
		// Enabling uses the same validation as the edit form. Disabling must
		// remain possible even if an old stored threshold is malformed.
		for _, key := range []string{OpenAIAutoResetCredit5hThresholdExtraKey, OpenAIAutoResetCredit7dThresholdExtraKey} {
			if value, ok := account.Extra[key]; ok {
				config[key] = value
			}
		}
	}
	normalized, err := normalizeOpenAIAutoResetCreditExtra(account.Platform, account.Type, config)
	if err != nil {
		return nil, err
	}

	// Merge only these two keys atomically. Rewriting thresholds from the
	// earlier snapshot could lose a concurrent edit; missing thresholds keep
	// the resolver's 100% defaults. A disabled opt-in is stored as JSON null.
	if err := s.accountRepo.UpdateExtra(ctx, id, map[string]any{
		OpenAIAutoResetCreditEnabledExtraKey:      enabled,
		OpenAIAutoResetCreditOptInVersionExtraKey: normalized[OpenAIAutoResetCreditOptInVersionExtraKey],
	}); err != nil {
		return nil, err
	}
	updated, err := s.accountRepo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if enabled && ResolveOpenAIAutoResetCreditConfig(updated).Enabled {
		AfterAccountCommit(ctx, func(context.Context) { notifyOpenAIAutoReset(id) })
	}
	return updated, nil
}
