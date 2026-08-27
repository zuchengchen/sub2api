package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestPrepareOpenAIGPT56PromptCaching(t *testing.T) {
	t.Parallel()

	t.Run("auto configures implicit mode and preserves stable system input", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"system","content":[{"type":"input_text","text":"stable"}]},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", true)

		require.NoError(t, err)
		require.True(t, preparation.Enabled)
		require.True(t, preparation.AutoConfigured)
		require.True(t, preparation.EnsureBreakpoint)
		require.Equal(t, "implicit", gjson.GetBytes(updated, "prompt_cache_options.mode").String())
		require.Equal(t, "30m", gjson.GetBytes(updated, "prompt_cache_options.ttl").String())
		require.Equal(t, "developer", gjson.GetBytes(updated, "input.0.role").String())
	})

	t.Run("does not auto configure a user-only request", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-sol", true)

		require.NoError(t, err)
		require.False(t, preparation.Enabled)
		require.JSONEq(t, string(body), string(updated))
	})

	t.Run("explicit client mode requests a stable breakpoint", func(t *testing.T) {
		body := []byte(`{"prompt_cache_options":{"mode":"explicit"},"input":[{"role":"system","content":"stable"},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-terra", false)

		require.NoError(t, err)
		require.True(t, preparation.Enabled)
		require.False(t, preparation.AutoConfigured)
		require.True(t, preparation.EnsureBreakpoint)
		require.Equal(t, "developer", gjson.GetBytes(updated, "input.0.role").String())
	})

	t.Run("automatic path supplements client implicit mode", func(t *testing.T) {
		body := []byte(`{"prompt_cache_options":{"mode":"implicit"},"input":[{"role":"system","content":"stable"},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", true)

		require.NoError(t, err)
		require.True(t, preparation.Enabled)
		require.False(t, preparation.AutoConfigured)
		require.True(t, preparation.EnsureBreakpoint)
		require.Equal(t, "developer", gjson.GetBytes(updated, "input.0.role").String())
	})

	t.Run("empty options use implicit defaults", func(t *testing.T) {
		body := []byte(`{"prompt_cache_options":{},"input":[{"role":"developer","content":"stable"},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", false)

		require.NoError(t, err)
		require.True(t, preparation.Enabled)
		require.False(t, preparation.EnsureBreakpoint)
		require.JSONEq(t, string(body), string(updated))
	})

	t.Run("ttl-only options use implicit mode", func(t *testing.T) {
		body := []byte(`{"prompt_cache_options":{"ttl":"30m"},"input":[{"role":"developer","content":"stable"},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", false)

		require.NoError(t, err)
		require.True(t, preparation.Enabled)
		require.False(t, preparation.EnsureBreakpoint)
		require.JSONEq(t, string(body), string(updated))
	})

	t.Run("system and top-level instructions are canonicalized once in original order", func(t *testing.T) {
		body := []byte(`{"instructions":"top-level","input":[{"role":"system","content":"system text"},{"role":"developer","content":"developer text"},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", true)

		require.NoError(t, err)
		require.True(t, preparation.EnsureBreakpoint)
		require.False(t, gjson.GetBytes(updated, "instructions").Exists())
		require.Equal(t, "developer", gjson.GetBytes(updated, "input.0.role").String())
		require.Equal(t, "system text\n\ntop-level", gjson.GetBytes(updated, "input.0.content.0.text").String())
		require.Equal(t, "developer text", gjson.GetBytes(updated, "input.1.content").String())
		require.Equal(t, 1, strings.Count(string(updated), "system text"))
		require.Equal(t, 1, strings.Count(string(updated), "top-level"))
	})

	t.Run("mixed-content system prompt is not automatically rewritten", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"system","content":[{"type":"input_text","text":"stable"},{"type":"input_image","image_url":"https://example.com/a.png"}]},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", true)

		require.NoError(t, err)
		require.False(t, preparation.Enabled)
		require.JSONEq(t, string(body), string(updated))
	})

	t.Run("invalid client options remove breakpoints", func(t *testing.T) {
		body := []byte(`{"prompt_cache_options":{"mode":"explicit","ttl":"1h"},"input":[{"role":"developer","content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", false)

		require.NoError(t, err)
		require.False(t, preparation.Enabled)
		require.False(t, gjson.GetBytes(updated, "prompt_cache_options").Exists())
		require.False(t, gjson.GetBytes(updated, "input.0.content.0.prompt_cache_breakpoint").Exists())
	})

	t.Run("enum whitespace is rejected instead of forwarded", func(t *testing.T) {
		body := []byte(`{"prompt_cache_options":{"mode":" implicit ","ttl":"30m"},"input":[{"role":"developer","content":"stable"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.6-luna", true)

		require.NoError(t, err)
		require.False(t, preparation.Enabled)
		require.False(t, gjson.GetBytes(updated, "prompt_cache_options").Exists())
	})

	t.Run("older models remain unchanged", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"system","content":"stable"},{"role":"user","content":"dynamic"}]}`)

		updated, preparation, err := prepareOpenAIGPT56PromptCaching(body, "gpt-5.5", true)

		require.NoError(t, err)
		require.False(t, preparation.Enabled)
		require.Equal(t, string(body), string(updated))
	})
}

func TestEnsureOpenAIExplicitPromptCacheBreakpoint(t *testing.T) {
	t.Parallel()

	t.Run("uses last leading stable message", func(t *testing.T) {
		body := []byte(`{
			"input":[
				{"role":"system","content":[{"type":"input_text","text":"system"}]},
				{"role":"developer","content":[{"type":"input_text","text":"developer"}]},
				{"role":"user","content":[{"type":"input_text","text":"dynamic"}]}
			]
		}`)

		updated, changed, err := ensureOpenAIExplicitPromptCacheBreakpoint(body)
		require.NoError(t, err)
		require.True(t, changed)
		require.False(t, gjson.GetBytes(updated, "input.0.content.0.prompt_cache_breakpoint").Exists())
		require.Equal(t, "explicit", gjson.GetBytes(updated, "input.1.content.0.prompt_cache_breakpoint.mode").String())
		require.False(t, gjson.GetBytes(updated, "input.2.content.0.prompt_cache_breakpoint").Exists())
	})

	t.Run("converts stable string content", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"system","content":"stable"},{"role":"user","content":"dynamic"}]}`)

		updated, changed, err := ensureOpenAIExplicitPromptCacheBreakpoint(body)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "stable", gjson.GetBytes(updated, "input.0.content.0.text").String())
		require.Equal(t, "explicit", gjson.GetBytes(updated, "input.0.content.0.prompt_cache_breakpoint.mode").String())
	})

	t.Run("preserves client breakpoint", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"system","content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"role":"user","content":"dynamic"}]}`)

		updated, changed, err := ensureOpenAIExplicitPromptCacheBreakpoint(body)
		require.NoError(t, err)
		require.False(t, changed)
		require.JSONEq(t, string(body), string(updated))
	})

	t.Run("does not mark user-only request", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"user","content":"dynamic"}]}`)

		updated, changed, err := ensureOpenAIExplicitPromptCacheBreakpoint(body)
		require.NoError(t, err)
		require.False(t, changed)
		require.JSONEq(t, string(body), string(updated))
	})

	t.Run("uses the last stable message that contains text", func(t *testing.T) {
		body := []byte(`{"input":[{"role":"developer","content":"stable text"},{"role":"developer","content":[{"type":"input_image","image_url":"https://example.com/reference.png"}]},{"role":"user","content":"dynamic"}]}`)

		updated, changed, err := ensureOpenAIExplicitPromptCacheBreakpoint(body)
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, "explicit", gjson.GetBytes(updated, "input.0.content.0.prompt_cache_breakpoint.mode").String())
		require.False(t, gjson.GetBytes(updated, "input.1.content.0.prompt_cache_breakpoint").Exists())
	})
}

func TestRemoveOpenAIPromptCacheConfiguration(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"prompt_cache_options":{"mode":"explicit"},
		"prompt_cache_breakpoint":{"mode":"explicit"},
		"input":[
			{"role":"developer","prompt_cache_breakpoint":{"mode":"explicit"},"content":[{"type":"input_text","text":"stable","prompt_cache_breakpoint":{"mode":"explicit"}}]},
			{"role":"user","content":"keep"}
		]
	}`)

	updated, changed, err := removeOpenAIPromptCacheConfiguration(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(updated, "prompt_cache_options").Exists())
	require.False(t, gjson.GetBytes(updated, "prompt_cache_breakpoint").Exists())
	require.False(t, gjson.GetBytes(updated, "input.0.prompt_cache_breakpoint").Exists())
	require.False(t, gjson.GetBytes(updated, "input.0.content.0.prompt_cache_breakpoint").Exists())
	require.Equal(t, "keep", gjson.GetBytes(updated, "input.1.content").String())
}
