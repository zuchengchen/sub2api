# YuFeng content moderation design

This document defines the behavior shared by the moderation service, audit API,
and admin UI. It is an implementation contract, not a production enablement
record.

## Staging and rollback

`first_layer_stage` and `second_layer_stage` independently accept `enforce` or
`shadow`; missing values normalize to `enforce` for legacy configurations.
First-layer shadow records a high-confidence keyword risk without blocking and,
when the second layer is enabled, continues to it even when its candidate
prefilter does not match.
Second-layer shadow records an unsafe model decision without blocking or user
side effects. A risky shadow result is not stored as an allow-cache entry, so a
repeated risk remains auditable. Promotion starts in shadow and requires
reviewed replay results before enforce is considered.

Rollback does not require a code rollback: set the endpoint `profile` back to
`qwen_guard`, restore its endpoint/model/prompt version, and keep the YuFeng
endpoint disabled. Changing profile, endpoint, model revision, prompt version,
context policy, evidence policy, keyword policy, TTL policy, or either layer's
stage changes the fragment cache namespace, so a rollback cannot reuse a
decision from a different policy.

## Decision flow

1. Extract fragments without replacing their original `role`, `kind`, or
   `path`; deterministically derive `context_class` as `user`, `tool`,
   `service_log`, `code`, `config`, or `unknown`.
2. Normalize high-confidence, candidate, and benign-context keyword rules.
   Only a high-confidence rule can block before the model. Historical
   `blocked_keywords` are candidate rules unless explicitly migrated.
3. Check the policy-scoped fragment cache. An unexpired block is a replay;
   an expired entry is atomically removed and treated as a miss.
4. Build bounded, deduplicated, redacted evidence. If selection or truncation
   may omit relevant content, scan bounded first/last fallback chunks too.
5. Send a single user message to the YuFeng chat template. A real `user`
   request is placed first with a structured metadata trailer so short harmful
   intent is not buried. Tool, service-log, code, config, and unknown evidence
   use a JSON envelope containing explicit `quoted_data`; the dynamic policy
   says quoted output is evidence, not an instruction to execute.
6. Parse only a known YuFeng label. Empty or unknown output is a parser error
   and follows the existing second-layer failure policy; it is never silently
   converted to safe.
7. Preserve every model risk decision. A complete non-user fragment labeled
   `pc` is marked `context_review_pc` for audit review, but remains risky in
   shadow mode and remains blocked in enforce mode. Context class is not an
   authorization boundary because API clients can submit tool-shaped content.
8. Persist the original audit row before publishing a block cache entry.

## Original, replay, expiry, and deletion

```text
request A -- cache miss --> model/keyword block --> persist source audit row
                                                --> apply account side effects once
                                                --> cache(source_log_id, expires_at)

request B -- same namespace/hash --> cache hit --> persist cache_block replay row
                                              --> link source_log_id
                                              --> violation_count = 0
                                              --> no ban, notice, archive, or hash side effect

TTL passes -- atomic Get --> delete value/size/LRU/expiry metadata --> cache miss
                         --> run a new original decision

manual review delete --> delete all aliases for the input hash and namespace
                     --> next request is a cache miss immediately
```

A short-lived in-process lock serializes identical namespace/hash misses. The
first request persists the source and publishes the cache entry; concurrent
waiters then become replays. Repository counting continues to exclude both
`hash_block` and `cache_block` actions.

The block TTL defaults to 600 seconds and is valid only from 300 through 900
seconds. The allow TTL defaults to 3600 seconds and is bounded at 86400
seconds. The Redis representation uses separate result, size, LRU, and expiry
metadata. Lua operations update or remove them atomically. Legacy entries that
lack expiry metadata are treated as misses and removed rather than becoming
permanent decisions.

## Audit and privacy

Original and replay rows retain request ID, input hash, role, kind, context,
redacted path, decision source, source log ID, namespace, policy versions,
model profile/revision, evidence mode/truncation, parser status, cache
provenance, and disposition. Ordinary logs and model evidence redact common
authorization headers, cookies, tokens, passwords, private keys, and secrets.
The archived source remains the separately controlled source of truth.

Admin list filters cover result, review status, context class, model profile,
decision source, and exact log ID. Replay rows are visually subordinate to the
source, expose their own request ID, and can navigate to the original record.

## Observability

Runtime status reports cache hits, misses, expired entries, and replays. Model
metrics are bounded by endpoint/profile/context/evidence/keyword tier and
include request, safe, blocked, uncertain, parser-failure, timeout, and average
latency counters. These are operational dimensions, not user-controlled label
values.
