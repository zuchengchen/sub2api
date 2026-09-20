package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration259PelicanHTMLAnimationPrompt(t *testing.T) {
	body, err := FS.ReadFile("259_pelican_html_animation_prompt.sql")
	require.NoError(t, err)
	sql := string(body)
	require.Contains(t, sql, "WHERE test_type = 'pelican'")
	require.Contains(t, sql, "user_visible = true")
	require.Contains(t, sql, "gpt-6-astra")
	require.Contains(t, sql, "创建一个HTML，内容是SVG绘制一个鹈鹕骑自行车的2D动画")
	require.Contains(t, sql, "不要使用任何skill")
	require.Contains(t, sql, "AGENTS.md")
}
