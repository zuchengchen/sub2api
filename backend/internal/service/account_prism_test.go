package service

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPrismBrowserModelScope(t *testing.T) {
	for _, tc := range []struct {
		name       string
		scope      any
		configured bool
		model      string
		want       bool
	}{
		{"default sol", nil, false, "gpt-6.1-sol", true},
		{"default max spelling", nil, false, "gpt-6.1-sol-max", true},
		{"default 5.6 stays native", nil, false, "gpt-5.6-sol", false},
		{"compact spelling is not a prism model", nil, false, "gpt-6.1-sol-openai-compact", false},
		{"selected", []any{"gpt-6.1-sol"}, true, "gpt-6.1-sol", true},
		{"unselected native", []any{"gpt-5.6-sol"}, true, "gpt-6.1-sol", false},
		{"empty", []string{}, true, "gpt-6.1-sol", false},
		{"null", nil, true, "gpt-6.1-sol", false},
		{"malformed", "gpt-6.1-sol", true, "gpt-6.1-sol", false},
		{"wildcard never widens", []string{"*"}, true, "gpt-6.1-sol", false},
		{"explicit alias", []string{"gpt-6.1-sol"}, true, "my-sol", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, a := prismTestService("")
			a.Credentials["model_mapping"] = map[string]any{"my-sol": "gpt-6.1-sol"}
			a.Extra = map[string]any{}
			if tc.configured {
				a.Extra[PrismBrowserModelsKey] = tc.scope
			}
			require.Equal(t, tc.want, a.IsPrismBrowserEnabledForModel(tc.model))
			a.Extra["openai_prism_browser"] = false
			require.False(t, a.IsPrismBrowserEnabledForModel(tc.model))
		})
	}
	_, a := prismTestService("")
	require.Equal(t, []string{"gpt-6.1-sol"}, PrismBrowserSupportedModels())
	require.True(t, a.IsPrismBrowserEnabledForModel("gpt-6.1-sol"))
}
