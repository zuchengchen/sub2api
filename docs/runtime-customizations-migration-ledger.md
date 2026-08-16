# Runtime customization migration ledger

This ledger records the disposition of every tracked and relevant untracked
change in `/home/czc/sub2api-guard-work` at commit `99c8e4bf7564823bafbab369acab6539e734c1bb`.
The reference worktree and production binary were verified before migration;
the reference tree is read-only and no credentials or traffic samples are
copied here.

## Tracked files

| File | Disposition | Reason |
| --- | --- | --- |
| `backend/internal/handler/gateway_handler.go` | adapted | Add explicit client session ID to sticky-session context. |
| `backend/internal/handler/openai_chat_completions.go` | adapted | Pass protocol/body metadata to unified cyber-policy archival. |
| `backend/internal/handler/openai_gateway_cyber_test.go` | adapted | Update regression coverage for archival arguments. |
| `backend/internal/handler/openai_gateway_handler.go` | adapted | Wire complete cyber-policy request snapshot and account/key disposition. |
| `backend/internal/repository/group_repo.go` | adapted | Transactionally clear fallback references and enqueue scheduler outbox events. |
| `backend/internal/repository/group_repo_integration_test.go` | adapted | Verify fallback cleanup on group deletion. |
| `backend/internal/securityaudit/prompt_config.go` | excluded | Prompt Audit configuration is removed; its candidate keyword assets are versioned under unified Risk Control. |
| `backend/internal/securityaudit/prompt_config_store.go` | excluded | Independent Prompt Audit persistence is removed. |
| `backend/internal/securityaudit/prompt_enqueue.go` | excluded | Independent Prompt Audit queue is removed. |
| `backend/internal/securityaudit/prompt_guard.go` | adapted | Pure two-layer keyword/model evaluation is folded into Content Moderation. |
| `backend/internal/securityaudit/prompt_logging.go` | excluded | Prompt Audit event logger is removed; unified audit logging is used. |
| `backend/internal/securityaudit/prompt_logging_test.go` | excluded | Tests for the removed logger are replaced by unified redaction/audit tests. |
| `backend/internal/securityaudit/prompt_metrics.go` | adapted | Metrics are re-expressed as unified moderation metrics. |
| `backend/internal/securityaudit/prompt_types.go` | adapted | Scanner result types are reused only where they fit unified moderation contracts. |
| `backend/internal/securityaudit/prompt_worker.go` | excluded | Independent worker lifecycle is removed. |
| `backend/internal/service/account.go` | adapted | Preserve account metadata needed by the Anthropic cache-control rewrite. |
| `backend/internal/service/billing_service.go` | already equivalent | GPT-5.6 Luna already has the exact 20% pricing in the target baseline. |
| `backend/internal/service/content_moderation.go` | adapted | Single source of truth for GPT-scoped two-layer moderation, cache, archive, and disposition. |
| `backend/internal/service/content_moderation_cyber_test.go` | adapted | Extend unified cyber-policy behavior and administrator handling. |
| `backend/internal/service/gateway_claude_oauth_body.go` | adapted | Preserve account/client metadata during cache-control rewriting. |
| `backend/internal/service/gateway_forward.go` | adapted | Attach complete request snapshots to unified moderation. |
| `backend/internal/service/gateway_forward_as_chat_completions.go` | adapted | Connect request snapshot and session metadata. |
| `backend/internal/service/gateway_forward_as_responses.go` | adapted | Connect request snapshot and session metadata. |
| `backend/internal/service/gateway_messages_cache.go` | adapted | Support account cache-control rewrite and stable client metadata. |
| `backend/internal/service/gateway_request.go` | adapted | Extract and prioritize explicit client session IDs. |
| `backend/internal/service/gateway_service.go` | adapted | Preserve sticky-session and failover semantics. |
| `backend/internal/service/gateway_tool_rewrite_test.go` | adapted | Regression coverage for metadata-preserving rewrites. |
| `backend/internal/service/generate_session_hash_test.go` | adapted | Verify explicit session ID precedence. |
| `backend/internal/service/openai_cyber_policy.go` | adapted | Normalize and redact diagnostic cyber-policy bodies while preserving raw archive bytes. |
| `backend/internal/service/openai_gateway_chat_completions.go` | adapted | Record unified cyber-policy events with protocol/body metadata. |
| `backend/internal/service/openai_gateway_messages.go` | adapted | Record unified cyber-policy events with protocol/body metadata. |
| `backend/internal/service/openai_gateway_passthrough.go` | adapted | Record unified cyber-policy events with protocol/body metadata. |
| `backend/internal/service/openai_gateway_response_handling.go` | adapted | Preserve status/body semantics for cyber-policy archival. |
| `backend/internal/service/openai_gateway_upstream_errors.go` | adapted | Keep structured account errors distinct from edge challenges. |
| `backend/internal/service/openai_ws_forwarder_ingress.go` | adapted | Capture the triggering WebSocket message only. |
| `backend/internal/service/openai_ws_forwarder_v2.go` | adapted | Capture the triggering WebSocket message only. |
| `backend/internal/service/pricing_service.go` | already equivalent | Luna fallback pricing already matches the approved rates. |
| `backend/internal/service/pricing_service_test.go` | already equivalent | Exact Luna regression coverage is present. |
| `backend/internal/service/ratelimit_service.go` | adapted | Ignore OpenAI HTML/anti-bot 403 challenges for account health. |
| `backend/resources/model-pricing/model_prices_and_context_window.json` | already equivalent | Luna standard, batch, flex, and priority rates are already exact. |
| `frontend/src/components/payment/AmountInput.vue` | adapted | Default quick amounts are 50/100/150/200 in a four-column layout. |
| `frontend/src/views/user/PaymentView.vue` | adapted | Recharge view uses the approved quick amounts while retaining backend bounds. |

## Relevant untracked files

| File | Disposition | Reason |
| --- | --- | --- |
| `backend/internal/securityaudit/prompt_claude_scope_test.go` | excluded | The old `all_groups` Prompt Audit contract is intentionally removed; GPT-prefix scope and non-GPT bypass are covered by unified moderation scope tests and `TestRuntimeCustomizationsAcceptance`. |
| `backend/internal/securityaudit/prompt_first_chunk_test.go` | excluded | “First chunk only” conflicts with the approved all-client-controlled-fragment policy; unified extraction tests cover every supported role and reference. |
| `backend/internal/securityaudit/prompt_hard_block_test.go` | adapted | Immediate keyword blocking, no-model behavior, persistence, and history/role extraction are covered by unified Content Moderation tests. |
| `backend/internal/securityaudit/prompt_keyword_only_test.go` | adapted | Keyword-only, API-only, and second-layer failure-to-allow behavior are covered by unified configuration and acceptance tests. |
| `backend/internal/securityaudit/prompt_keyword_table_test.go` | adapted | Candidate-table validation moved to versioned Content Moderation asset tests; candidates remain disabled by default. |
| `backend/internal/securityaudit/prompt_live_blocking_test.go` | excluded | Environment-dependent live Prompt Audit tests are replaced by deterministic unified tests and the isolated stub rehearsal. |
| `backend/internal/securityaudit/prompt_live_guard_test.go` | excluded | Environment-dependent Qwen live calls are not retained; the endpoint starts disabled and local stub tests prove its protocol and failure behavior. |
| `backend/internal/securityaudit/prompt_prefilter.go` | adapted | Canonical keyword matching and two-layer gating are implemented by the unified matcher and second-layer scanner. |
| `backend/internal/securityaudit/prompt_prefilter_gate_test.go` | adapted | Unified tests cover keyword/API strategy selection, cache behavior, endpoint validation, and fragment scope. |
| `backend/internal/securityaudit/prompt_prefilter_helpers_test.go` | excluded | Helpers only supported the removed Prompt Audit tests; unified extraction fixtures replace them. |
| `backend/internal/securityaudit/prompt_prefilter_persist_test.go` | adapted | Unified Risk Control configuration update and candidate-asset tests cover persistence without restoring a second configuration store. |
| `backend/internal/securityaudit/prompt_prefilter_sample.go` | excluded | Sampling unmatched prompts is not part of the approved deterministic all-unknown-fragment policy. |
| `backend/internal/securityaudit/prompt_prefilter_test.go` | adapted | Normalization, canonicalization, and matching behavior are covered by the unified keyword matcher parity tests. |
| `backend/internal/securityaudit/prompt_production_config_test.go` | excluded | The old private Prompt Audit production configuration is removed; repository examples contain no endpoints or tokens. |
| `backend/internal/securityaudit/prompt_responses_protocol_test.go` | adapted | OpenAI Responses extraction and keyword blocking are covered by unified input and route tests. |
| `backend/internal/securityaudit/testdata/prefilter_keywords.json` | adapted | Its candidate values are represented by `backend/resources/content-moderation/legacy-prompt-audit-v1/layer2-candidate-keywords.json`, disabled by the manifest. |
| `backend/internal/service/account_anthropic_cache_test.go` | adapted | Retained as regression coverage for the account cache-control flag. |
| `backend/internal/service/gateway_apikey_cache_metadata_test.go` | adapted | Retained as regression coverage for metadata preservation. |
| `backend/internal/service/openai_403_edge_challenge.go` | adapted | Retained as the bounded, format-aware edge challenge classifier. |
| `backend/internal/service/openai_cyber_policy_body_test.go` | adapted | Retained as diagnostic redaction and raw-byte archival coverage. |
| `backend/internal/service/ratelimit_service_403_edge_test.go` | adapted | Retained as account-health regression coverage. |
| `backend/sub2api-new` | excluded | Generated/deployed binary; never committed. |
| `keywords/__pycache__/generate_keywords.cpython-314.pyc` | excluded | Generated Python bytecode. |
| `keywords/__pycache__/layer2_broad.cpython-314.pyc` | excluded | Generated Python bytecode. |
| `keywords/__pycache__/validate_against_traffic.cpython-314.pyc` | excluded | Generated Python bytecode. |
| `keywords/generate_keywords.py` | excluded | Offline keyword-generation script; generator code and its external inputs are outside the runtime migration. |
| `keywords/generate_two_layer.py` | excluded | Offline candidate-generation script; only reviewed versioned output assets are migrated. |
| `keywords/layer1_hard_block.json` | adapted | Normalized into `legacy-prompt-audit-v1/layer1-high-confidence-keywords.json` as a disabled candidate asset, not merged into the 50 active production rules. |
| `keywords/layer2_broad.py` | excluded | Offline broad-list generator; executable generation tooling is outside scope. |
| `keywords/layer2_prefilter.json` | adapted | Normalized into `legacy-prompt-audit-v1/layer2-candidate-keywords.json` and disabled by default. |
| `keywords/prefilter_keywords.json` | adapted | Duplicate source for the reviewed layer-two candidate set; represented once by the versioned unified asset. |
| `keywords/validate_against_traffic.py` | excluded | Traffic-validation script and its private traffic inputs are explicitly outside scope. |
| `keywords/validate_two_layer.py` | excluded | Offline validation script; deterministic repository tests replace it for release gating. |

## Verification notes

- Luna rates are checked against the exact approved values in the backend
  pricing tests and resource JSON.
- Live production table counts are recorded in the Goal execution log only;
  this ledger never contains request bodies, headers, tokens, or secrets.
- A disposition may be changed only with an implementation and test reference;
  no reference file is copied wholesale over the current upstream baseline.
- The table has 42 tracked-file rows and 33 untracked-file rows, exactly
  matching `git diff --name-only HEAD` and `git ls-files --others
  --exclude-standard` in the reference worktree at the recorded commit.
