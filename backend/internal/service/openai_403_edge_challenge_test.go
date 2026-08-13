package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIsOpenAIEdgeChallenge403(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"holding_page", `<html><head><meta http-equiv="refresh" content="360"></head></html>`, true},
		{"cloudflare", `<!doctype html><html><title>Just a moment...</title></html>`, true},
		{"bare_challenge", `Attention Required! Enable JavaScript and cookies to continue`, true},
		{"plain_html", `<html><body>Forbidden</body></html>`, false},
		{"json_account_error", `{"error":{"message":"workspace forbidden"}}`, false},
		{"json_marker_collision", `{"error":{"message":"cloudflare account policy"}}`, false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isOpenAIEdgeChallenge403([]byte(tt.body)))
		})
	}
}

func TestIsOpenAIEdgeChallenge403BoundsInput(t *testing.T) {
	body := "<html><body>" + strings.Repeat("x", edgeChallengeBodyScanMaxBytes*2) + "just a moment</body></html>"
	require.False(t, isOpenAIEdgeChallenge403([]byte(body)))
	require.True(t, isOpenAIEdgeChallenge403([]byte("<html><title>Just a moment</title></html>")))
}
