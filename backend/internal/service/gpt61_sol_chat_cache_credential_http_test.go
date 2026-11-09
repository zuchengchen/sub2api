package service_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestGPT61SolChatCacheModelScopeHTTPCredentialRotation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%t", stream), func(t *testing.T) {
			h := newGPT61ScopeHTTPHarness(t, gpt61ScopeAllUsersPolicy(), nil, "")
			owner := gpt61ScopeHTTPClient{101, 501, "same-session", "", ""}
			rotated := owner
			rotated.account = "rotated_credentials"
			history := nativeTrafficInitialHistory()
			first, firstRow, err := h.call(history, "gpt-6.1-sol", stream, owner)
			require.NoError(t, err)
			continued := nativeTrafficContinue(history, first)
			second, rotatedRow, err := h.call(continued, "gpt-6.1-sol", stream, rotated)
			require.NoError(t, err)
			require.Zero(t, strings.Count(string(rotatedRow.Body), `"type":"reasoning"`), "the same site account ID with another credential must not inherit original native state")
			require.NotEqual(t, firstRow.AuthorizationHash, rotatedRow.AuthorizationHash, "the real upstream HTTP credential must change")

			_, rotatedFollow, err := h.call(nativeTrafficContinue(continued, second), "gpt-6.1-sol", stream, rotated)
			require.NoError(t, err)
			require.Zero(t, nativeTrafficComparisonExit(rotatedFollow.Body, rotatedRow.Output), "the rotated credential must retain its own complete native state")
			require.Equal(t, 1, nativeTrafficComparisonExit(rotatedFollow.Body, firstRow.Output), "the original credential's native block must remain isolated")
			require.Equal(t, rotatedRow.AuthorizationHash, rotatedFollow.AuthorizationHash)

			_, originalFollow, err := h.call(continued, "gpt-6.1-sol", stream, owner)
			require.NoError(t, err)
			require.Zero(t, nativeTrafficComparisonExit(originalFollow.Body, firstRow.Output), "switching credentials must not corrupt the original branch")
			require.Equal(t, 1, nativeTrafficComparisonExit(originalFollow.Body, rotatedRow.Output))
			require.Equal(t, firstRow.AuthorizationHash, originalFollow.AuthorizationHash)
			for _, row := range []nativeTrafficRow{firstRow, rotatedRow, rotatedFollow, originalFollow} {
				require.Equal(t, "gpt-6.1-sol", gjson.GetBytes(row.Body, "model").String())
			}
			h.assertNoForwardErrors(t)
		})
	}
}
