# Codex lifecycle wire acceptance

This Linux recorder runs an independently acquired official Codex CLI, tool
subprocesses, production gateway service methods, and HTTP/WebSocket/TLS sockets
against synthetic loopback upstreams. It uses an in-memory account repository.
It does not exercise deployed authentication middleware, scheduling, database
persistence, or real model answers. The checked-in 0.164.0 fixtures remain
historical verifier regression data; they are not a current acceptance baseline.

Acquire and verify one official stable release, or build one fixed official source
commit with its locked toolchain, before recording. Keep the exact
release tag, dereferenced source commit, downloaded package SHA-256, executable
SHA-256, and `codex-rs/models-manager/models.json` from that commit. Install the
release's complete package so `codex-code-mode-host`, `codex-exec-server`, and
`codex-resources/bwrap` retain their expected layout. Do not mix a `main` checkout
with a separately versioned executable or model catalog.

For a source build, keep its actual version (for example `codex-cli 0.0.0`),
never substitute a stable-release label. Set `CODEXGOLDEN_SOURCE_BUILD_RECEIPT`
to the new run's `source-build-receipt.json` for both reference and candidate.
The recorder verifies and retains that receipt, then binds its SHA-256 into
`meta.json`; the comparator revalidates it. A development-version capture
without this receipt is invalid evidence (exit 2). The receipt uses schema 1:

- `source_commit`, `source_tree`, `source_root`, `source_clean`, `version` bind a
  clean fixed checkout, with `source_files` SHA-256 entries for Cargo.toml,
  Cargo.lock, rust-toolchain.toml and the model catalog (additional source inputs
  may be listed).
- `toolchain` records complete rustc/cargo version text and executable hashes.
  `artifact_root` and `artifacts` bind the canonical package's `bin/codex`,
  `bin/codex-code-mode-host`, `bin/codex-exec-server`, `codex-resources/bwrap`,
  `codex-path/rg` and `codex-package.json`. Use that exact packaged CLI path.
- `commands` must positively show successful locked release builds of
  codex-cli, codex-code-mode-host, codex-exec-server-cli and codex-bwrap. Each
  command retains argv, cwd, start/end times, exit code, absolute log path and
  log hash; optional env and runner_sha256 preserve the actual build setup.
  The supported CPU wrapper is `taskset -c CPU_LIST cargo build ...`.
  `failed_commands` retains failed attempts with their nonzero statuses and
  readable logs, and cannot substitute for a successful build.

Keep the referenced checkout, artifacts, dependency inputs and command logs
available throughout comparison. A changed/missing companion, catalog, source,
log, or configured V8 archive invalidates the evidence. The original six
scenario barriers, transport evidence and model-request counts still apply.

The fixture config fixes `gpt-6-astra` and `xhigh`. Its six scenarios cover WS
prewarm and the initial request, WS tools, HTTP fallback tools, HTTP/WS 401
reload and refresh, and WS compaction with continuation. Each scenario requires
both independent models initialization requests. The synthetic upstream chooses
only an actually declared `exec_command`, or code-mode `exec` when its declared
capabilities include `exec_command`, including a Responses Lite
`additional_tools` namespace. The code-mode fixture validates the two actual
output content items, nested command exit code 0, exactly `golden\n`, and no
pending session ID. Only this fixture-owned wrapper's clock and chunk ID are
dynamic; nested command output and unrelated fields remain semantic. Auxiliary paths such as usage,
Live/Realtime, initial OAuth authorization-code exchange, and legacy unary
compact need separate coverage; these six scenarios do not cover them.

## Two independently captured TLS environments

Run inside a private mount/user namespace whose certificate bundle trusts a
local fixture CA. Never change the host trust store. Set `CODEXGOLDEN_CERT`,
`CODEXGOLDEN_KEY`, and `CODEXGOLDEN_CA` to fixture files. The leaf must cover
localhost, 127.0.0.1, chatgpt.com, and auth.openai.com. Use synthetic credentials
only; never supply production auth files to this recorder.

For the native environment leave `CODEX_CA_CERTIFICATE`, `SSL_CERT_FILE`, and
`SSL_CERT_DIR` unset, and leave `CODEXGOLDEN_CUSTOM_CA` unset. For the separate
custom-CA environment set `CODEXGOLDEN_CUSTOM_CA=1`; the recorder supplies that
CA to both clients and advertises HTTP/2. The recorded `tls_mode` is respectively
`system_roots_namespace` or `custom_ca_rustls_h2`. WebSocket Upgrade remains
HTTP/1.1 over rustls. Every core request has its actual socket TLS association;
custom-CA business HTTP requests also require real HTTP/2 frame observations.

Within each environment, run from `backend/`, using new output directories:

```sh
go run ./tools/codexgolden -codex "$REF_CODEX_BIN" \
  -out "$EGRESS_RUN_DIR/$EGRESS_PROFILE/reference" \
  -expect-version "$REF_CODEX_VERSION" -source-commit "$REF_CODEX_COMMIT" \
  -models-json "$REF_CODEX_MODELS"
CODEX_TRAFFIC_REFERENCE_BIN="$REF_CODEX_BIN" \
CODEX_TRAFFIC_REFERENCE_DIR="$EGRESS_RUN_DIR/$EGRESS_PROFILE/reference" \
CODEX_TRAFFIC_CANDIDATE_DIR="$EGRESS_RUN_DIR/$EGRESS_PROFILE/candidate" \
  GOMAXPROCS=2 go test -json -count=1 -p 2 -tags=embed -timeout=20m \
  -run '^TestCodexOAuthTrafficAcceptance$' ./internal/service
```

The acceptance test executes the CLI through the gateway, verifies ownership,
route counts and refresh persistence, runs the strict comparator, and emits
`acceptance.json` only on success. A directly invoked `gateway -scenario ...`
run is diagnostic; incomplete scenario sets cannot pass comparison. Build the
embedded frontend before compiling the server, per repository instructions.

## Evidence and comparison

New captures use `recording_contract=2`. Metadata records version, source commit,
executable/catalog hashes, model/reasoning settings, TLS mode, effective fixture
configuration and CA hashes. Native and custom-CA captures cannot be mixed.
Reference traces are automatically retained in `traces/`, alongside raw
synthetic captures under `raw/`; gateway traces cover the entire worker and its
CLI/tool children. The comparator verifies retained trace hashes and parses the
traces again, including split per-PID `unfinished`/`resumed` calls. Missing,
truncated, unreadable or non-local traces fail. These files are local evidence,
not source artifacts to commit.

Contract 2 also binds every scenario's raw WebSocket frame/termination sidecar.
The comparator checks that frame sequences explain the captured messages, retains
interleaved ping/pong and close payloads, and compares connection termination.
A clean EOF between messages is distinct from a completed close handshake;
neither controls nor a close frame are invented when absent. Missing, truncated,
unreadable, or inconsistent sidecars invalidate the capture. Data-frame digests
and lengths remain integrity observations; message semantics and dynamic-ID
relations are checked by the existing request comparator.

Every upgrade attempt also retains the fixture's actual complete response write
(101, 401, or 426), bound to the observed request and connection. Successful
idle connections require lifecycle evidence even when they sent no messages.
Failed upgrade attempts cannot claim WS frames; their status and attempt order
remain part of comparison.

HTTP/2 observations retain actual ordered headers, SETTINGS, connection windows,
stream IDs and request indexes. An internal parser marker binds each handler to
its exact observed stream, including identical concurrent method/path/UA
requests. The marker never enters captures, gateway traffic or responses. The
fixture's internal HTTP/1 adapter only supplies scripted responses and is not
used as HTTP/2 evidence. Raw `*-h2-connection-ends.json` records the observed
read boundary: EOF/reset is accepted only before any preface byte, or between
complete frames with no unfinished request stream. Partial prefaces, frame
payloads and unfinished streams remain failures. Tool-loop validation requires
the actual command wrapper's exit code 0 and exactly `golden` output; quoted
success text inside a failed command's output cannot satisfy it.

The comparator keeps one dynamic-ID mapping across gateway-owned endpoints,
preserving ID formats and equality relationships. Ordinary JSON map ordering is
not semantically significant; typed struct top-level order and header order stay
observable. Core requests bypassing the gateway fail. Bootstrap/plugin/analytics
requests owned by the driver remain in raw captures but are excluded from the
product projection; unexpected gateway-owned traffic is never dropped.

```sh
go run ./tools/codexgolden compare -golden REFERENCE -candidate CANDIDATE -v
GOMAXPROCS=2 go test -race -count=1 -p 2 -timeout=5m ./tools/codexgolden
```

Comparison exit 0 means matched complete evidence, 1 means a product difference,
and 2 means invalid/incomplete comparison inputs. Upstream HTTP fallback can
include multiple reconnects to an already-upgraded local gateway; upstream
projection success alone does not prove identical downstream status or timing.

## Separate candidate ingress modes

`gateway -ingress-mode passthrough|ctx_pool` selects the explicit product ingress
mode; the default remains passthrough. Acceptance passes
`CODEX_TRAFFIC_INGRESS_MODE` to that flag and binds it in acceptance.json. Record
each mode into a separate new candidate directory. Both use the same fixed
six-scenario official reference and identical strict wire comparison rules.

Candidate metadata carries candidate_ingress_mode and gateway_config_sha256;
these never enter the official reference/common effective_config_sha256. Every
real WS entry records the current account mode, production resolver transport,
and pool-acquire counters before and after ProxyResponsesWebSocketFromClient.
ctx_pool requires actual pool acquisitions, while passthrough must not acquire
through that entry. Missing, contradictory or incomplete mode receipts invalidate
the capture before wire comparison. A matching configuration label alone is not
proof that the expected mode ran, and a mode-specific wire difference remains a
product difference to resolve.

The candidate account explicitly uses full fingerprint mode and a public
synthetic seed. Each scenario retains an identity-only ingress snapshot and
binds it with the raw outgoing capture. Capture and comparison both recompute
the installation identity derived from that seed, verify session rewriting,
and check header/body identity relationships. Stored configuration labels and
counters cannot substitute for observed transformations. Optional official
headers remain optional; a reused WS handshake's turn/window metadata is not
assumed to describe every later frame.

The gateway may retain the requested `ctx_pool` mode after a real upstream 426
has disabled WS for that session. A later WS ingress can then execute the
production HTTP bridge without another pool acquire. Such an observation is
accepted only in the two explicit HTTP-fallback scenarios, with a preceding
ingress that actually wrote a 426, a later ingress sequence, positive HTTP
responses observed in that ingress's own context, and matching raw upstream
HTTP/handshake counts. Provenance records requested and actual modes separately.
A reason label or zero acquire count alone never establishes fallback. Strict
comparison still checks every outgoing frame/request against the official
reference.

Reference and candidate captures must use identical CPU/memory limits and at
least two available CPUs. These scenarios deliberately synchronize two official
startup model reads; a one-CPU candidate can change startup/cache timing and
produce only one read. That run is incomplete and must fail the existing
positive two-request models barrier. Record actual Docker resource arguments in
the parent command receipt; do not change the common reference hash, weaken the
barrier, or repair old capture metadata after the fact.
