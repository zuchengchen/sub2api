# DeepSeek V4 Flash Audit Policy v2

`deepseek-v4-flash-audit-v2` is the immutable three-state text-moderation
policy used by Sub2API. The application owns enforcement and account side
effects; reviewer output never applies them directly.

## Canonical assets

| Asset | Entries | SHA-256 |
| --- | ---: | --- |
| `backend/resources/content-moderation/deepseek-v4-flash-audit-v2/system-prompt.txt` | n/a | `5ecc9c2e03d9fd7d2191e4a618ab25e272f1f5cf4fc0cead9a023b3da308c35b` |
| `backend/resources/content-moderation/deepseek-v4-flash-audit-v2/layer1-high-confidence-keywords.json` | 114 | `bbacfed1994fcc70f24bbd9d9171f11d8fbe700e650bc61e32001a7e1d7413a8` |
| `backend/resources/content-moderation/deepseek-v4-flash-audit-v2/layer2-candidate-keywords.json` | 457 | `ed9adfbd8b00dc3dcc7b440a8d770f729c683da3994605e4673ddc4fd34c07c6` |

The v2 files replace the inherited v1 keyword layers. The manifest binds the
counts and hashes, so a changed prompt or list cannot load without a matching
manifest update.

## Decision contract

The reviewer must return exactly one JSON object with exactly these fields:

```json
{"disposition":"allow","confidence":0.00,"category":"safe","reason":""}
```

- `allow` requires `category=safe` and confidence below `0.80`.
- `restricted` requires `category=restricted_security_content` and confidence
  at least `0.80`. It blocks the request without counting a user violation.
- `violation` requires one violation category and confidence at least `0.80`.
  It blocks the request and may apply configured account side effects.
- Unknown or mismatched fields, categories, dispositions, and thresholds fail
  strict parsing.
- The prompt limits `reason` to 20 Unicode code points. A valid decision with
  an overlong reason remains usable after the server redacts and bounds the
  audit-only reason field.
- Non-empty `reasoning_content`, truncated output, trailing data, and prose
  outside the JSON object are rejected.

The violation categories are:

- `cyber_abuse`, `cracking`, `security_bypass`, `account_abuse`
- `sexual_deepfake`, `doxxing`, `violent_threat`, `self_harm`
- `weapons`, `sexual_content`
- `fraud_financial_crime`, `controlled_substances`, `human_exploitation`
- `terrorism_extremism`, `illegal_gambling`, `forgery_counterfeit`
- `corruption_tax_evasion`, `hate_harassment`

## Keyword tiers

Layer 2 contains broad recall terms. A match only selects bounded evidence for
model review and does not itself mark the user as violating policy. This layer
includes common Chinese and English expressions for all 18 violation
categories.

Layer 1 contains explicit request or implementation phrases. Version 2 adds 11
high-confidence Chinese combinations for terrorism, human exploitation,
illegal gambling, financial crime, controlled substances, forgery,
corruption, tax evasion, and coordinated harassment. Bare words such as
`洗钱`, `毒品`, `赌博`, `行贿`, and `网络霸凌` remain Layer 2 only.

Changing either layer requires a new manifest checksum, focused collision
tests, full moderation tests, and release replay before promotion.
