-- User-facing pelican test: HTML+SVG 2D animation, visible to all users,
-- default model gpt-6-astra. Hourly GPT-PRO runner reads this prompt.
UPDATE test_settings
SET
    user_visible = true,
    config = jsonb_set(
        jsonb_set(
            config,
            '{prompt}',
            '"创建一个HTML，内容是SVG绘制一个鹈鹕骑自行车的2D动画，你不需要任何测试，只靠你自己完成，不要使用任何skill，不要依赖我本地的AGENTS.md"'::jsonb
        ),
        '{model}',
        '"gpt-6-astra"'::jsonb
    ),
    updated_at = NOW()
WHERE test_type = 'pelican';
