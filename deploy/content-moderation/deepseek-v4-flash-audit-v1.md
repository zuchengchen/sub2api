# DeepSeek V4 Flash Audit Policy v1

`deepseek-v4-flash-audit-v1` is the versioned text-moderation policy used by
Sub2API. The application, not the model, owns the final Shadow or Enforce
decision.

## Canonical assets

The embedded files below are the only runtime source of truth. Do not copy the
system prompt into configuration or make it editable in the admin UI.

| Asset | Entries | SHA-256 |
| --- | ---: | --- |
| `backend/resources/content-moderation/deepseek-v4-flash-audit-v1/system-prompt.txt` | n/a | `820613a1927e5c2bf1d1d4eb4f40b24a5bee73c43334e147d87aa6e217308c01` |
| `backend/resources/content-moderation/deepseek-v4-flash-audit-v1/layer1-high-confidence-keywords.json` | 103 | `525e4e089ed9fe8352dc3281d10ec6b332db60e46e9b23a014a26c3c03092a9c` |
| `backend/resources/content-moderation/deepseek-v4-flash-audit-v1/layer2-candidate-keywords.json` | 306 | `2dd5aced91fd799a0efe961c3d33a8fa36f29be226c17f4d838249366b7fb8c5` |

The manifest binds these hashes, the policy categories, and the model contract.
Changing any policy text or keyword requires a new policy version, updated
fixtures, historical calibration, and release verification.

## Model request contract

Use the first healthy enabled channel in configured order. Each channel may
override the model name; the default model is `deepseek-v4-flash`.

```json
{
  "model": "deepseek-v4-flash",
  "messages": [
    {"role": "system", "content": "<embedded system-prompt.txt>"},
    {"role": "user", "content": "<trusted template below>"}
  ],
  "thinking": {"type": "disabled"},
  "response_format": {"type": "json_object"},
  "temperature": 0,
  "max_tokens": 64,
  "stream": false
}
```

The client must reject the attempt and follow channel failure handling when any
of these conditions occurs:

- the response is not one JSON object or has keys other than `confidence`,
  `category`, and `reason`;
- `confidence` is not a JSON number in `[0,1]`;
- `category` is outside the manifest enum, or conflicts with the `0.80`
  threshold;
- `reason` is not a string, is longer than 20 Unicode code points, or exposes
  sensitive input;
- `reasoning_content` is present and non-empty;
- the response is truncated or contains prose outside the JSON object.

`confidence >= 0.80` is a policy match. Values from `0.50` through `0.79` are
recorded as uncertain but do not block. Values below `0.80` must use category
`safe`; the service derives the disposition and must never ask the model to
return `flagged`.

## Trusted user template

`CONTEXT_CLASS`, `ROLE`, and `KIND` come only from server-side extraction.
`CONTENT_JSON` is a JSON string produced after evidence-window minimization and
secret redaction. JSON encoding must escape angle brackets so audited text
cannot manufacture a delimiter.

```text
<trusted_context>
context_class={{CONTEXT_CLASS}}
role={{ROLE}}
kind={{KIND}}
</trusted_context>

<user_input>
{"content":{{CONTENT_JSON}}}
</user_input>
```

Only the latest user text and text from tool calls/results associated with that
turn are eligible. Media metadata may be included as text. Never fetch or send
an image, audio, video, media binary, or media URL to the reviewer.

## Policy boundary

The prompt covers these risk categories:

- `cyber_abuse`
- `cracking`
- `security_bypass`
- `account_abuse`
- `sexual_deepfake`
- `doxxing`
- `violent_threat`
- `self_harm`
- `weapons`
- `sexual_content`

Ownership, authorization, context, intent, and actionability are mandatory
signals. Authorized work on the user's own resources, CTF and defensive work,
self-harm help or prevention, weapons safety/history/law, medical or educational
sexual content, non-explicit romance, and mere keyword mentions are safe.

Layer 1 contains only explicit request or implementation phrases and can make a
direct decision when Enforce is enabled. Layer 2 contains relevant or ambiguous
terms only; a hit admits the evidence window to model review and is never a
decision by itself.

## Operational defaults

- DeepSeek enabled; YuFeng disabled.
- Layer 1 Shadow; Layer 2 Shadow.
- Per-channel timeout: 3000 ms.
- Total DeepSeek review budget: 10000 ms.
- Sequential channel failover with one attempt per channel; no hedging.
- Policy version and summary are read-only in the UI.
- API keys are entered through the protected admin flow, encrypted at rest, and
  never returned, logged, embedded in an asset, or included in an audit record.

## Production review deduplication

Layer 2 uses Redis to cache successful parsed reviews. The cache key combines a
configuration-derived namespace with the SHA-256 digest of the complete,
redacted evidence bundle. The namespace changes when the policy, keyword,
evidence-window contract, rollout stages, reviewer configuration, threshold, or
channel credentials change. Identical concurrent requests are coalesced inside
the process; different evidence remains fully concurrent.

Safe, risky, uncertain, and reviewer-disagreement outcomes are cached. Invalid
JSON, truncated-context failures, timeouts, cancellation, and reviewer
unavailability are never cached. The allow and block TTL settings both default
to 36000 seconds (10 hours). Cache entries do not contain the evidence field or
evidence windows. They retain hashes, decision metadata, and the model's bounded
20-rune reason so a replayed audit preserves the original decision. Version 2
entries also carry `disposition_applied`, which separates reuse of the model
result from completion of Enforce side effects.

Every request still writes its own audit row. A reused result has
`cache_hit=true` and `decision_source=cache_replay`; an Enforce-mode risk replay
still blocks the request, but it does not repeat violation increments, account
disablement, email, archive, or flagged-hash side effects. Runtime status
reports dedicated `second_layer_cache_hits`, `second_layer_cache_misses`,
`second_layer_cache_writes`, and `second_layer_cache_errors` counters.

A risky result produced for a whitelist request or suppressed Enforce batch is
shared with `disposition_applied=false`. Other whitelist requests reuse it
without another model call. The first ordinary Enforce request serializes on the
evidence hash, writes a `cache_promotion` audit, performs the normal disposition
once, and updates the same entry to `disposition_applied=true`; only subsequent
requests use the side-effect-free replay path. The cache is published only after
the audit for the current state has persisted.

## Fixed historical calibration

The release calibration is pinned to source SHA-256
`b20980588c83b5a8e1046ce5ed0bc8473c8920fefc431b7b045fdf7b019bd8d6`
and maximum record ID `15528`. The source contains 10879 eligible rows: 10809
are readable and 70 are recorded as unreadable without a model call. Of the
readable rows, 7948 use complete redacted evidence windows and 2861 legacy rows
use only their already-redacted, length-bounded `input_excerpt`; the evaluator
labels these separately as `legacy_redacted_excerpt` and never reads an archive.
The readable rows form exactly 793 groups by target policy, trusted context
class, role, kind, and selected evidence. Each group is called once and its
result is mapped back to every member row. Records created after this fixed
snapshot are excluded from this calibration.

`backend/cmd/deepseek-moderation-eval` rejects a different source digest,
record count, unique-group count, endpoint, model, timeout, or worker count. It
uses only the official endpoint, `deepseek-v4-flash`, non-thinking JSON mode,
and a credential supplied on protected file descriptor 3. Result files contain
record IDs and hashes but no evidence text; the bounded model reason is stored
only as a SHA-256 digest.
