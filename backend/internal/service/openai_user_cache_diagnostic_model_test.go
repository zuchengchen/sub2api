package service

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUserCacheDiagnosticRequestedModelFilter(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", "gpt-6.1-sol")
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
	target := []byte(`{"model":"gpt-6.1-sol","messages":[]}`)
	other := []byte(`{"model":"gpt-6-astra","messages":[]}`)
	rejected := userCacheDiagnosticTestContext(428)
	require.Nil(t, beginUserCacheDiagnostic(rejected, other))
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "excluded model must not create a diagnostic file")
	require.Nil(t, beginUserCacheDiagnostic(rejected, target), "retry must retain the original non-admission")
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "retry must not bypass the requested-model decision")

	accepted := userCacheDiagnosticTestContext(428)
	diagnostic := beginUserCacheDiagnostic(accepted, target)
	require.NotNil(t, diagnostic, "excluded requests must leave the only sample slot available")
	require.Same(t, diagnostic, beginUserCacheDiagnostic(accepted, other), "admitted retries retain their existing trace")
	require.Nil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), target), "the request cap must still apply")
	events := userCacheDiagnosticReadEvents(t, path)
	require.Len(t, events, 1)
	require.Equal(t, "ingress", events[0]["stage"])
}

func TestUserCacheDiagnosticRequestedModelExactAndOptional(t *testing.T) {
	for _, tc := range []struct {
		name, filter, body string
		want               bool
	}{
		{"exact", "gpt-6.1-sol", `{"model":"gpt-6.1-sol"}`, true},
		{"different_case", "gpt-6.1-sol", `{"model":"GPT-6.1-SOL"}`, false},
		{"body_space", "gpt-6.1-sol", `{"model":" gpt-6.1-sol "}`, false},
		{"filter_space", " gpt-6.1-sol ", `{"model":"gpt-6.1-sol"}`, false},
		{"missing", "gpt-6.1-sol", `{}`, false},
		{"not_string", "61", `{"model":61}`, false},
		{"unconfigured_other_model", "", `{"model":"gpt-6-astra"}`, true},
		{"unconfigured_missing_model", "", `{}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", tc.filter)
			diagnostic := beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), []byte(tc.body))
			if tc.want {
				require.NotNil(t, diagnostic)
				require.Len(t, userCacheDiagnosticReadEvents(t, path), 1)
			} else {
				require.Nil(t, diagnostic)
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}
