# Runtime Risk Control consolidation

This release consolidates GPT-prefix request auditing into Content Moderation
and removes the independent Prompt Audit subsystem.

## Breaking changes

- The `/admin/prompt-audit/*` API surface is removed and returns `404`.
- The Prompt Audit navigation entry and administration page are removed.
- `prompt_audit_config`, Prompt Audit workers, Redis payloads, and the
  `prompt_audit_jobs` / `prompt_audit_events` runtime store are retired by the
  explicit cutover tool. They are not dual-written or retained as a fallback.
- Historical jobs become one `content_moderation_logs` row each. All events for
  a job are merged into that row; historical hit prompts are encrypted as
  `legacy_prompt_only` archives marked `incomplete`.

The database cutover is not performed by ordinary application startup. Follow
`docs/runtime-risk-cutover-runbook.md`, retain its verified rollback artifacts,
and obtain explicit production-deployment authorization first.
