package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeDeepSeekResponsesRequestBodyStripsHostedTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"deepseek-v4.1-flash","input":"hi","tools":[` +
		`{"type":"function","name":"read","parameters":{"type":"object"}},` +
		`{"type":"web_search"},` +
		`{"type":"image_generation"},` +
		`{"type":"custom","name":"apply_patch"}` +
		`],"tool_choice":"auto"}`)

	for name, account := range map[string]*Account{
		"native": deepSeekNativeResponsesImageAccount(),
		"mapped": openaiMappedDeepSeekResponsesImageAccount(),
	} {
		out := normalizeDeepSeekResponsesRequestBody(account, body)
		types := gjson.GetBytes(out, "tools.#.type").Array()
		require.Len(t, types, 2, name)
		require.Equal(t, "function", types[0].String(), name)
		require.Equal(t, "custom", types[1].String(), name)
		require.Equal(t, "auto", gjson.GetBytes(out, "tool_choice").String(), name)
	}
}

func TestNormalizeDeepSeekResponsesRequestBodyDropsToolsAndChoiceWhenOnlyHosted(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"deepseek-v4.1-flash","input":"hi","tools":[{"type":"web_search"}],"tool_choice":"required"}`)
	out := normalizeDeepSeekResponsesRequestBody(deepSeekNativeResponsesImageAccount(), body)
	require.False(t, gjson.GetBytes(out, "tools").Exists())
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())
}

func TestNormalizeDeepSeekResponsesRequestBodyDropsHostedToolChoice(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"deepseek-v4.1-flash","input":"hi","tools":[{"type":"function","name":"f"},{"type":"web_search"}],"tool_choice":{"type":"web_search"}}`)
	out := normalizeDeepSeekResponsesRequestBody(deepSeekNativeResponsesImageAccount(), body)
	require.Equal(t, int64(1), gjson.GetBytes(out, "tools.#").Int())
	require.False(t, gjson.GetBytes(out, "tool_choice").Exists())
}

func TestNormalizeDeepSeekResponsesRequestBodyKeepsHostedToolsForOpenAI(t *testing.T) {
	t.Parallel()

	body := []byte(`{"model":"gpt-5","input":"hi","tools":[{"type":"web_search"}]}`)
	openai := &Account{Platform: PlatformOpenAI, Type: AccountTypeAPIKey}
	require.Equal(t, string(body), string(normalizeDeepSeekResponsesRequestBody(openai, body)))
}
