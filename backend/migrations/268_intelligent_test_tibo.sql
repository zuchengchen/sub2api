-- Admin intelligent-test question matching the OpenAI Tibo HTTP probe.
-- Same prompt as openAITiboProbePrompt; user-invisible, click-to-run only.
INSERT INTO test_settings(test_type, enabled, user_visible, config) VALUES
('tibo', true, false, '{"prompt":"Tell me whether you know who Tibo is in OpenAI based on your knowledge without searching online. Only return True or  False.","model":"gpt-6-astra","evaluator":"tibo_answer","expected_answer":"True","answer_type":"text","answer_format":"free_text","timeout_seconds":180}')
ON CONFLICT(test_type) DO NOTHING;
