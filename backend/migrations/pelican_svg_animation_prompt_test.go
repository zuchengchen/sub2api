package migrations

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMigration258PelicanSvgAnimationPrompt(t *testing.T) {
	body, err := FS.ReadFile("258_pelican_svg_animation_prompt.sql")
	require.NoError(t, err)
	sql := string(body)
	require.Contains(t, sql, "WHERE test_type = 'pelican'")
	require.Contains(t, sql, "jsonb_set")
	require.Contains(t, sql, `'{prompt}'`)
	require.Contains(t, sql, "创建一个SVG文件，内容是绘制一个鹈鹕骑自行车的2D动画。")
	require.NotContains(t, sql, "请只输出一个独立、有效的 SVG")
	require.NotContains(t, sql, "HTML")
}
