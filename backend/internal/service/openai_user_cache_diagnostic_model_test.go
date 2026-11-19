package service

import (
	"bytes"
	"os"
	"testing"
	"time"

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

func TestUserCacheDiagnosticOriginalSurvivesMultipleRewrites(t *testing.T) {
	path := userCacheDiagnosticTestEnv(t)
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", "gpt-6.2-sol")
	t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
	c := userCacheDiagnosticTestContext(428)
	body := []byte(`{"model":"gpt-6.2-sol","reasoning_effort":"high","messages":[]}`)
	RememberUserCacheDiagnosticIngress(c, 428, body)
	afterPolicy, changed, err := ApplyOpenAIReasoningEffortPolicy(body, "low", nil, ReasoningEffortOverLimitDowngrade)
	require.NoError(t, err)
	require.True(t, changed)
	RememberUserCacheDiagnosticIngress(c, 428, afterPolicy)
	mapped := ReplaceModelInBody(afterPolicy, "gpt-6.2-astra")
	raw, _ := c.Get(userCacheDiagnosticIngressKey)
	stashed, ok := raw.(userCacheDiagnosticIngress)
	require.True(t, ok)
	require.JSONEq(t, `{"model":"gpt-6.2-sol","reasoning_effort":"high","messages":[]}`, string(stashed.body))
	_, err = os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist)
	d := beginUserCacheDiagnostic(c, mapped)
	require.NotNil(t, d)
	require.False(t, d.matchesAssistantModel("gpt-6.2-astra"))
	require.True(t, d.matchesAssistantModel("gpt-6.2-sol"))
	require.Equal(t, diagnosticSHA256(body), userCacheDiagnosticReadEvents(t, path)[0]["body"].(map[string]any)["sha256"])
	require.Nil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), body))
}

func TestUserCacheDiagnosticRemembersIngressOnlyInActiveWindow(t *testing.T) {
	body := []byte(`{"model":"gpt-6.2-sol","messages":[]}`)
	for _, condition := range []string{"disabled", "expired", "wrong_user", "exhausted"} {
		t.Run(condition, func(t *testing.T) {
			userCacheDiagnosticTestEnv(t)
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", "")
			userID := int64(428)
			switch condition {
			case "disabled":
				t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_USER_ID", "")
			case "expired":
				t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_UNTIL", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339))
			case "wrong_user":
				userID++
			case "exhausted":
				t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
				require.NotNil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(userID), body))
			}
			c := userCacheDiagnosticTestContext(userID)
			RememberUserCacheDiagnosticIngress(c, userID, body)
			_, stored := c.Get(userCacheDiagnosticIngressKey)
			require.False(t, stored)
		})
	}
}

func TestUserCacheDiagnosticOversizedOriginalAfterRewrite(t *testing.T) {
	body := bytes.Repeat([]byte{' '}, userCacheDiagnosticMaxBytes+1)
	mapped := []byte(`{"model":"gpt-6.2-sol","messages":[]}`)
	for _, filter := range []string{"", "gpt-6.2-sol"} {
		t.Run("filter="+filter, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", filter)
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MAX_REQUESTS", "1")
			c := userCacheDiagnosticTestContext(428)
			RememberUserCacheDiagnosticIngress(c, 428, body)
			RememberUserCacheDiagnosticIngress(c, 428, mapped)
			if raw, ok := c.Get(userCacheDiagnosticIngressKey); ok {
				require.Empty(t, raw.(userCacheDiagnosticIngress).body)
			}
			d := beginUserCacheDiagnostic(c, mapped)
			if filter != "" {
				require.Nil(t, d)
				_, err := os.Stat(path)
				require.ErrorIs(t, err, os.ErrNotExist)
				require.NotNil(t, beginUserCacheDiagnostic(userCacheDiagnosticTestContext(428), mapped))
				return
			}
			require.NotNil(t, d)
			require.False(t, d.matchesAssistantModel("gpt-6.2-sol"))
			event := userCacheDiagnosticReadEvents(t, path)[0]
			require.NotContains(t, event, "assistant_tail")
			summary := event["body"].(map[string]any)
			require.Equal(t, true, summary["summary_unavailable"])
			require.EqualValues(t, len(body), summary["bytes"])
			require.Equal(t, diagnosticSHA256(body), summary["sha256"])
		})
	}
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

func TestUserCacheDiagnosticAssistantModelScope(t *testing.T) {
	for _, tc := range []struct {
		name, body, upstreamModel string
		wantTail, wantComparison  bool
	}{
		{"exact", `{"model":"gpt-6.2-sol","messages":[]}`, "gpt-6.2-sol", true, true},
		{"different_6_2", `{"model":"gpt-6.2-sol","messages":[]}`, "gpt-6.2-astra", true, false},
		{"mapped_to_6_1", `{"model":"gpt-6.2-astra","messages":[]}`, "gpt-6.1-sol", true, false},
		{"mapped_from_6_1", `{"model":"gpt-6.1-sol","messages":[]}`, "gpt-6.2-astra", true, false},
		{"mapped_alias", `{"model":"public-model","messages":[]}`, "gpt-6.2-sol", false, false},
		{"case_alias", `{"model":"GPT-6.2-SOL","messages":[]}`, "gpt-6.2-sol", false, false},
		{"space", `{"model":" gpt-6.2-sol ","messages":[]}`, "gpt-6.2-sol", false, false},
		{"ambiguous_field", `{"model":"gpt-6.2-sol","Model":"gpt-6.2-astra","messages":[]}`, "gpt-6.2-sol", false, false},
		{"responses_shape", `{"model":"gpt-6.2-sol","input":[]}`, "gpt-6.2-sol", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := userCacheDiagnosticTestEnv(t)
			t.Setenv("SUB2_USER_CACHE_DIAGNOSTIC_MODEL", "")
			c := userCacheDiagnosticTestContext(428)
			d := beginUserCacheDiagnostic(c, []byte(tc.body))
			require.NotNil(t, d)
			delivery := NewNativeReasoningChatCapture(c.Writer, true, DefaultNativeReasoningReplayLimits())
			_, err := delivery.Write(nativeCaptureChatWire())
			require.NoError(t, err)
			attempt := &userCacheDiagnosticAttempt{diagnostic: d, attempt: 1}
			attempt.chatDelivery(c, delivery, &OpenAIForwardResult{Stream: true}, nil, nil, tc.upstreamModel)
			events := userCacheDiagnosticReadEvents(t, path)
			require.Len(t, events, 2)
			require.Equal(t, true, events[1]["downstream_delivered_complete"])
			_, tail := events[0]["assistant_tail"]
			_, comparison := events[1]["assistant_comparison"]
			require.Equal(t, tc.wantTail, tail)
			require.Equal(t, tc.wantComparison, comparison)
		})
	}
}
