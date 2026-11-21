UPDATE test_settings
SET config = jsonb_set(config, '{model}', '"gpt-6.2-astra"'::jsonb),
    updated_at = NOW()
WHERE test_type = 'pelican'
  AND btrim(config->>'model') = 'gpt-6-astra';
