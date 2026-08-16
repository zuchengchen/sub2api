# YuFeng XGuard llama.cpp moderation runbook

This runbook prepares a local or controlled-host shadow endpoint. It does not
authorize Sub2API production deployment, service replacement, branch
promotion, or policy enforcement.

## Pinned artifacts

Validated on 2026-08-15:

| Artifact | Provenance | Revision | SHA-256 |
| --- | --- | --- | --- |
| `llama-server` | official llama.cpp Linux x64 release `b10430` | `4c1a0af40d88c7fbb3b15c85bf2e8016d1d5b64c` | `05fcaf46bb8b58e1e6cd80d3199ba619c98a1842bd1b59ac70fb6d18e7a26788` |
| release archive | official llama.cpp `b10430` | same | `1f1e0b9dae6a79b952fe82d13fbd6112516824228bdd7a0f4890e3dde18a93b5` |
| YuFeng base model | `Alibaba-AAIG/YuFeng-XGuard-Reason-0.6B` | `9016029653fa2994e8efe5f2c007fc2ac172287d` | source revision only |
| Q4 GGUF | `mradermacher/YuFeng-XGuard-Reason-0.6B-GGUF` | `a457e581bb00997ff1eb1f9ae0bf21488c6a632c` | `c9766937ea76717be85261fe5518d328d4209fecc291bfa24d18b521d897adde` |

The 484,217,984-byte Q4 file is a community conversion, not an official
Alibaba GGUF. Its repository metadata records a static HF conversion,
`quantize_version=2`, `output_tensor_quantised=1`, `convert_type=hf`, and
`Q4_K_M`; no weighted/imatrix quantization was used. The upstream base and
community repository both declare Apache-2.0. Treat the revision and file hash
as the artifact identity.

The validated binary reports `0.1.0-dev (build 10430, commit 4c1a0af40)`.
The GGUF reports Q4_K Medium, 751,632,384 parameters, a 40,960-token training
context, and the embedded YuFeng chat template. The service deliberately uses
an 8,192-token shared runtime context, preserving approximately 4,096 tokens
for each of its two parallel slots.

## Host prerequisites

The validation host has an AMD EPYC 7571, 32 logical CPUs, 31 GiB RAM, and no
swap. Plan for a process baseline of approximately 1.55 GiB with two parallel
slots before the prompt cache fills. Required commands are `sha256sum`, `awk`,
`curl`, and `jq`; the benchmark requires Python 3.9 or newer. The official
archive also requires an OpenMP runtime. Set `OPENMP_LIB_DIR` to the directory
containing `libgomp.so.1` when it is not in the system loader path.

Place the extracted official release and model in read-only, administrator
managed directories. Do not put model binaries in this repository. Verify the
release archive before extraction and retain its download provenance.

## Start and verify

The launcher verifies both artifact hashes and the llama.cpp build/commit. It
refuses a non-loopback address unless `YUFENG_ALLOW_NON_LOOPBACK=true` is
explicitly set. The systemd file is an example and must be reviewed for the
target host; installing or starting it is a separate, approval-gated action.

```bash
export LLAMA_CPP_DIR=/opt/llama.cpp/b10430
export YUFENG_MODEL_PATH=/var/lib/sub2api/models/YuFeng-XGuard-Reason-0.6B.Q4_K_M.gguf
export OPENMP_LIB_DIR=/path/to/openmp/lib
scripts/content-moderation/run-yufeng-llama.sh
```

The current default server arguments are:

```text
--alias yufeng-xguard-q4 --host 127.0.0.1 --port 8088
--ctx-size 8192 --parallel 2 --batch-size 512 --ubatch-size 512
--threads 14 --threads-batch 14 --flash-attn on --load-mode none
--cache-ram 8192 --cache-prompt --no-context-shift --no-mmproj --offline
--no-webui --no-slots --metrics --n-gpu-layers 0
--cors-origins localhost --no-cors-credentials --jinja --log-verbosity 2
```

The launcher uses 14 inference and batch threads with two server slots. The
8,192-token shared context preserves approximately 4,096 tokens per slot. It
also enables prompt caching with an 8,192 MiB in-memory cache ceiling. Together
with the approximately 1.55 GiB process baseline, the cache can bring the
steady footprint near 9.55 GiB before transient allocator and request overhead.
The example systemd unit uses `MemoryHigh=11G` and `MemoryMax=12G`, leaving
about 19 GiB available to the rest of the 31 GiB host even at the hard limit.
It caps the service at 2,800% CPU, equivalent to 28 fully utilized logical
CPUs. A 28-thread setting was rejected after concurrent probes exceeded the
25-second endpoint timeout. With 14 threads, three pairs of concurrent health
checks all completed successfully; the complete safe-plus-unsafe chains took
14.76 to 24.83 seconds. After the v2 prompt was added, two more concurrent
pairs ran the expanded safe, unsafe, benign-media, and explicit-content checks.
All four chains returned the expected labels without a per-request timeout;
each four-request chain took 49.67 to 54.69 seconds. The historical benchmark
below used the previous 16-thread, single-slot setting and is not a two-slot
performance guarantee.

From another shell:

```bash
YUFENG_BASE_URL=http://127.0.0.1:8088 \
  scripts/content-moderation/check-yufeng-health.sh
```

The check requires `/health=status:ok`, the pinned model alias, a `sec` result
for a normal service status, and a known non-`sec` result for an embedded
secret-exfiltration and shell-execution instruction. It also requires ordinary
FFmpeg/FFprobe output to remain `sec` and an explicit sexual-content sample to
return `pc`. It sends no credential.

## Sub2API shadow configuration

Create one enabled endpoint with these values in the existing Risk Control
second-layer settings:

```text
name: YuFeng XGuard Q4
base_url: http://127.0.0.1:8088
model: yufeng-xguard-q4
profile: yufeng_xguard
model_revision: a457e581bb00997ff1eb1f9ae0bf21488c6a632c
prompt_version: yufeng-xguard-v3
timeout_ms: 25000
input_limit: 4000
first_layer_stage: enforce
second_layer_stage: shadow
context_policy_version: context-v3
evidence_policy_version: evidence-v1
keyword_policy_version: keyword-v4
fragment_ttl_policy_version: ttl-v2
block_ttl_seconds: 36000
allow_ttl_seconds: 36000
```

The 25-second endpoint timeout exceeds the observed 4K p95 plus margin while
remaining below the 30-second configuration limit. Evidence selection and
bounded fallback are still required; timeout is not a substitute for them.

The v3 prompt tells the model not to infer `pc` (Pornographic Contraband) from
ordinary FFmpeg/media editing, file names, paths, rendering, transcoding,
probing, or verification text. Before YuFeng is called, every context uses the
same two-layer policy. With `first_layer_stage=enforce`, Layer 1
high-confidence keywords block directly. With `first_layer_stage=shadow`, a
Layer 1 hit is recorded and, while Layer 2 is enabled, always continues to
YuFeng, including when the Layer 2 candidate prefilter misses. Otherwise Layer
2 candidate keywords route to YuFeng, and candidate misses are allowed and
cached. If the Layer 2 matcher
is unavailable or empty, the service disables that fast-allow path and sends
second-layer content to YuFeng as a health fallback. The two stage switches are
independent; neither changes the `cyber_policy` hard-block path.

## Replay benchmark

The harness sends the exact context-sensitive single-message envelope and
dynamic-policy arguments used by Sub2API. User requests precede a structured
metadata trailer; non-user content remains inside the JSON `quoted_data`
boundary. The harness never stores case input text in its output. Use a
controlled temporary directory because result IDs and model labels can still
be operational evidence.

```bash
artifact_dir="$(mktemp -d)"
python3 scripts/content-moderation/benchmark_yufeng.py \
  --base-url http://127.0.0.1:8088 \
  --cases /controlled/path/yufeng-cases.json \
  --output "${artifact_dir}/yufeng-shadow.json" \
  --timeout-seconds 30 \
  --probe-repeats 3
```

The historical 90-case corpus used during this change has SHA-256
`fee304fdad5b4704f3edebc4e88093a1f23558d735e82250b4383f8433033d3d`
and contains 60 safe plus 30 unsafe cases. A case may explicitly set `role`,
`kind`, `context_class`, `origin_path`, `evidence_mode`, and
`evidence_truncated`; otherwise the harness uses conservative deterministic
defaults. Review every false positive, false negative, parser failure, and
timeout rather than changing a global threshold.

Record process RSS after model load and retain the benchmark JSON next to the
artifact hashes. A production candidate must also replay the separately
controlled exact 20:48 service-status source. Its expected context is
`service_log`, expected label is `sec`, and its evidence/truncation fields must
be retained. Do not reconstruct that source from memory and call it exact.

### 2026-08-15 local verification record

The final context-sensitive envelope replay artifact is
`result-yufeng06-context-envelope-v1.json` in the controlled temporary
benchmark directory. Its SHA-256 is
`b9a2d2f082e517f07e60f4ce200faedfec74ad8cb82c6868cc570e1452a162ff`.
Results for 60 safe and 30 unsafe cases were TP 29, TN 59, FP 1, FN 1,
FPR 1.67%, fail-open FNR 3.33%, parse rate 100%, and timeout rate 0%. The
remaining failures were `s18` (a quoted dangerous-weapons classifier fixture
labeled `dw`) and `u08` (a web-content instruction takeover labeled `sec`).
No threshold or broad allow rule was added for either case.

Sequential accuracy latency was p50 3458.547 ms, p95 4766.098 ms, and p99
6782.582 ms, with measured throughput 0.2779 cases/second. Three safe
service-log probes at 200, 1000, and 4000 characters all returned `sec`. Their
p50/p95 wall times were 3625.062/5335.792 ms, 5645.683/6250.174 ms, and
15356.377/15505.398 ms respectively. There were no probe timeouts. Loaded
`llama-server` RSS after the run was 1,040,648 KiB with 66 threads.

An earlier in-memory replay of the separately archived exact source log 4165
recorded 8,585 characters and Q4 `sec` outcomes for 4000/4000/585-character
chunks at 14268.819, 18695.198, and 3814.473 ms. The plaintext was not written
to disk and was not re-read after the final envelope change. The current code
has a deterministic 8,585-character service-log evidence regression, but that
synthetic regression is not a substitute for the exact controlled source.
Replaying that source again remains a production promotion gate.

## Promotion gates

1. Keep `second_layer_stage=shadow`; verify health, parser, cache, audit, and
   90-case results, then manually review disagreements.
2. Replay controlled historical false positives and the exact 20:48 source.
   Confirm repeated blocks create one counted source and uncounted replays.
3. Obtain explicit approval before any small-traffic enablement, migration on a
   live database, service installation/restart, executable replacement, or
   Sub2API deployment.
4. Only after the canary meets its error, timeout, and resource limits should
   `second_layer_stage=enforce` be considered.

## Rollback

For a policy rollback, leave stage at `shadow` or restore the previous
`qwen_guard` endpoint/profile and its exact model/prompt revision. Disable the
YuFeng endpoint. Verify the runtime endpoint/profile status and confirm a new
cache namespace is active. No cache purge is normally required because policy
identity is namespaced; use the existing manual flagged-input deletion when a
specific reviewed hash must miss immediately.

For a service rollback, stop only the separately approved YuFeng unit and
verify Sub2API uses the prior endpoint. Do not replace the Sub2API executable,
restart its production service, or alter production data as part of this
runbook without separate approval.
