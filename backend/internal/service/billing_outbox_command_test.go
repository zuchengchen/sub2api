//go:build unit

package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBillingOutboxCommandNormalizeAndValidate(t *testing.T) {
	cmd := &BillingOutboxCommand{
		AttemptID:          "attempt-1",
		RequestID:          "attempt:attempt-1",
		APIKeyID:           7,
		RequestFingerprint: "fp-1",
		Billing: UsageBillingCommand{
			RequestID:          "attempt:attempt-1",
			APIKeyID:           7,
			RequestFingerprint: "fp-1",
			BalanceCost:        1.25,
		},
	}
	cmd.Normalize()
	require.NoError(t, cmd.Validate())
	require.Equal(t, "attempt-1", cmd.AttemptID)
	require.Equal(t, int64(7), cmd.APIKeyID)
}

func TestBillingOutboxCommandRejectsMissingIdentity(t *testing.T) {
	cmd := &BillingOutboxCommand{}
	require.ErrorIs(t, cmd.Validate(), ErrBillingOutboxAttemptIDRequired)
}
