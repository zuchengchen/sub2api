-- Pin empty intelligent-test models to gpt-6-astra. Reasoning effort is
-- applied by the runner (low); this only makes the saved default visible.
UPDATE test_settings
SET config = jsonb_set(config, '{model}', '"gpt-6-astra"'),
    updated_at = NOW()
WHERE btrim(COALESCE(config->>'model', '')) = '';
