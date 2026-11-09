//go:build unit

package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserErrorFinalRequestStatusKeepsLegacyStatus(t *testing.T) {
	got := ToUserErrorRequest(&OpsErrorLog{
		StatusCode: 503, FinalRequestStatus: 499, Phase: "upstream", Type: "api_error", AccountName: "internal-account",
	})
	require.Equal(t, 503, got.StatusCode)
	require.Equal(t, 499, got.FinalRequestStatus)
	payload, err := json.Marshal(got)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"final_request_status":499`)
	require.NotContains(t, string(payload), "internal-account")
}
