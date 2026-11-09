package service

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserCacheDiagnosticRequestedModelAmbiguousAndBounded(t *testing.T) {
	target := []byte(`{"model":"gpt-6.1-sol","messages":[]}`)
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"duplicate_model", `{"model":"gpt-6.1-sol","model":"gpt-6-astra"}`, false},
		{"duplicate_identical_model", `{"model":"gpt-6.1-sol","model":"gpt-6.1-sol"}`, false},
		{"Model_override", `{"model":"gpt-6.1-sol","Model":"gpt-6-astra"}`, false},
		{"MODEL_override", `{"model":"gpt-6.1-sol","MODEL":"gpt-6-astra"}`, false},
		{"alias_before_canonical", `{"Model":"gpt-6-astra","model":"gpt-6.1-sol"}`, false},
		{"escaped_duplicate", `{"model":"gpt-6.1-sol","mo\u0064el":"gpt-6-astra"}`, false},
		{"only_Model", `{"Model":"gpt-6.1-sol"}`, false},
		{"only_MODEL", `{"MODEL":"gpt-6.1-sol"}`, false},
		{"truncated_object", `{"model":"gpt-6.1-sol","messages":[`, false},
		{"array_root", `[{"model":"gpt-6.1-sol"}]`, false},
		{"trailing_document", `{"model":"gpt-6.1-sol"} {}`, false},
		{"oversized", `{"model":"gpt-6.1-sol","padding":"` + strings.Repeat("x", userCacheDiagnosticMaxBytes) + `"}`, false},
		{"nested_model_is_unambiguous", `{"model":"gpt-6.1-sol","metadata":{"model":"gpt-6-astra","Model":"another"}}`, true},
		{"single_escaped_model", `{"mo\u0064el":"gpt-6.1-sol"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", "gpt-6.1-sol")
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
			request := userCacheDiagnosticTestContext(428)
			diagnostic := beginUserCacheDiagnostic(request, []byte(tc.body))
			if tc.want {
				require.NotNil(t, diagnostic)
			} else {
				require.Nil(t, diagnostic)
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
				require.Nil(t, beginUserCacheDiagnostic(request, target), "an ambiguous request must stay rejected on retry")
				require.NotNil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), target), "a rejected model carrier must not consume the only sample slot")
			}
			require.Nil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), target), "the existing request limit must remain effective")
			require.Len(t, userCacheDiagnosticReadEvents(t, path), 1)
		})
	}
}

func TestUserCacheDiagnosticRequestedModelUnsetKeepsLegacy(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-6.1-sol","Model":"gpt-6-astra"}`,
		`{"Model":"gpt-6.1-sol"}`,
		`{"model":"gpt-6.1-sol","messages":[`,
	} {
		path := userCacheDiagnosticTestEnv(t)
		t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", "")
		require.NotNil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(body)), "disabled model filtering must retain legacy diagnostic admission")
		require.Len(t, userCacheDiagnosticReadEvents(t, path), 1)
	}
}
