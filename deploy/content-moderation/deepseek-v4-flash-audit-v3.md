# DeepSeek V4 Flash Audit Policy v3

`deepseek-v4-flash-audit-v3` keeps the v2 three-state moderation policy and
keyword layers, while making the `confidence` field unambiguously a risk score.
It is an immutable policy asset and inherits both keyword layers from v2.

## Canonical assets

| Asset | Entries | SHA-256 |
| --- | ---: | --- |
| `backend/resources/content-moderation/deepseek-v4-flash-audit-v3/system-prompt.txt` | n/a | `8d307713d65ea33685f4b8092033cc6a27f4e7c1808ff34b14f3b2bff5135675` |
| inherited v2 Layer 1 | 114 | `bbacfed1994fcc70f24bbd9d9171f11d8fbe700e650bc61e32001a7e1d7413a8` |
| inherited v2 Layer 2 | 457 | `ed9adfbd8b00dc3dcc7b440a8d770f729c683da3994605e4673ddc4fd34c07c6` |

## Confidence contract

- `confidence` is the risk score that content requires a `restricted` or
  `violation` disposition. It is not certainty in an `allow` decision.
- `allow` requires `category=safe` and a risk score below `0.80`.
- `restricted` and `violation` require their matching risk category and a risk
  score of at least `0.80`.
- For compatibility with reviewers that return certainty in an explicit
  `allow + safe` decision, the parser converts a value at or above `0.80` to
  `1-confidence` and records `normalized_allow_confidence`.
- An `allow` response with any risk category remains invalid. All restricted
  and violation category and threshold mismatches still fail closed.

The policy version participates in channel and fragment-cache revisions, so
promotion to v3 invalidates stale reviewer readiness and cached decisions.
