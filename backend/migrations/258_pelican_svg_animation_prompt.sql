-- Update the built-in pelican intelligent-test prompt to request an SVG
-- 2D animation. Only the prompt text changes; evaluator and other config stay.
UPDATE test_settings
SET config = jsonb_set(
        config,
        '{prompt}',
        '"创建一个SVG文件，内容是绘制一个鹈鹕骑自行车的2D动画。"'::jsonb
    ),
    updated_at = NOW()
WHERE test_type = 'pelican';
