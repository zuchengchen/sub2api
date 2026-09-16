package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSecurityPolicyHitsAreExcludedFromAutoBanCount(t *testing.T) {
	cfg := &ContentModerationConfig{AutoBanEnabled: true, BanThreshold: 1}
	svc := &ContentModerationService{}
	userID := int64(9)
	log := &ContentModerationLog{
		UserID:  &userID,
		Flagged: true,
		Action:  SecurityPolicyActionBlock,
	}
	transitioned, err := svc.applyFlaggedAccountSideEffectsWithRole(context.Background(), cfg, log, RoleUser)
	require.NoError(t, err)
	require.False(t, transitioned)
	require.False(t, log.AutoBanned)
	require.Equal(t, "not_counted", log.DispositionStatus)
	require.Equal(t, 0, log.ViolationCount)
	_ = time.Now()
}
