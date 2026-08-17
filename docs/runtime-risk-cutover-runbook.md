# Unified Risk Control cutover runbook

This runbook rehearses and executes the one-restart systemd cutover from Prompt
Audit to unified Risk Control. Production execution is outside the migration
Goal and requires separate authorization. Commands below use operator-supplied
paths and never place credentials in the repository or shell history.

## Preconditions

- Build immutable old, new, and `unified-risk-migration` binaries from reviewed
  commits. Record their SHA-256 values outside the repository.
- Install `deploy/systemd/sub2api-runtime-risk.conf` as a systemd drop-in and
  verify `GOMEMLIMIT=4GiB`, `MemoryHigh=6G`, and `MemoryMax=8G` with
  `systemctl show`. Do not edit the running unit from this repository.
- Keep Nginx's site file pointed through one include path, for example
  `/etc/nginx/snippets/sub2api-active.conf`. Stage copies of
  `deploy/nginx/runtime-risk-live.conf` and
  `deploy/nginx/runtime-risk-maintenance.conf`; never edit a live file in place.
- Keep `client_max_body_size 256m`, `proxy_request_buffering off`, and
  `proxy_buffering off`. Do not lower the application 256 MiB request limit.
- Provision the key ring outside the repository as a `0600` regular file. It
  must contain the active 32-byte key and every historical Key ID still
  referenced by either `content_moderation_logs` or a DeepSeek API-key envelope
  in `settings.content_moderation_config`. Do not remove a historical key until
  the finalized readiness check confirms that neither reference remains.
- Provision retry and emergency directories as owned by the service account,
  mode `0700`, on monitored storage. Only one service instance may use them.
- Ensure PostgreSQL has room for the encrypted staging payload and the backup;
  alert on disk safety threshold, archive/retry/disposition queue depth,
  `content_lost`, budget rejection, request-body maximum, and cache errors.
- Confirm the current old-table counts and status distribution. A count or
  enum that cannot be explained is a stop condition.

Set private operator paths in the current shell. These examples are placeholders:

```bash
umask 077
export CUTOVER_DIR=/secure/operator-owned/sub2api-cutover
export CUTOVER_DUMP="$CUTOVER_DIR/prompt-audit.dump"
export CUTOVER_PROOF="$CUTOVER_DIR/prompt-audit-proof.json"
export CUTOVER_CONFIG="$CUTOVER_DIR/prompt-audit-config.csv"
export ARCHIVE_KEY_RING=/etc/sub2api/secrets/content-moderation-keyring.json
export ARCHIVE_RETRY_DIR=/var/lib/sub2api/content-moderation/retry
export ARCHIVE_EMERGENCY_DIR=/var/lib/sub2api/content-moderation/emergency
install -d -m 0700 "$CUTOVER_DIR"
```

## Verified backup

Export the old configuration as a separate private rollback artifact. Do not
print its value to the terminal:

```bash
psql -X -v ON_ERROR_STOP=1 -c \
  "COPY (SELECT value FROM settings WHERE key = 'prompt_audit_config') TO STDOUT WITH (FORMAT csv)" \
  >"$CUTOVER_CONFIG"
chmod 0600 "$CUTOVER_CONFIG"
test -s "$CUTOVER_CONFIG"
```

Create a custom-format dump, list it, restore it into a random isolated
database, compare job/event/status counts, drop the isolated database, and
write a database-identity-bound proof:

```bash
/opt/sub2api/bin/unified-risk-migration backup \
  --archive "$CUTOVER_DUMP" \
  --proof "$CUTOVER_PROOF"
/opt/sub2api/bin/unified-risk-migration verify --proof "$CUTOVER_PROOF"
test "$(stat -c '%a' "$CUTOVER_DUMP")" = 600
test "$(stat -c '%a' "$CUTOVER_PROOF")" = 600
```

Any backup, listing, restore, digest, identity, or count failure stops the
cutover. Keep both the table dump and configuration artifact until the rollback
period ends. The encrypted archive backup lifecycle is separate; deleting a
live archive does not immediately erase existing backups.

## Online preparation

Run while the old service remains live. It is idempotent and re-encrypts only
new or changed source fingerprints, but performs full source/stage validation:

```bash
/opt/sub2api/bin/unified-risk-migration prepare \
  --proof "$CUTOVER_PROOF" \
  --key-ring "$ARCHIVE_KEY_RING"
/opt/sub2api/bin/unified-risk-migration prepare \
  --proof "$CUTOVER_PROOF" \
  --key-ring "$ARCHIVE_KEY_RING"
```

Record both JSON reports. Job/event/status counts must match the live source;
the second run must be stable. Referenced-key validation covers both encrypted
archives and DeepSeek credential envelopes. Do not continue with a missing
referenced key, invalid archive, plaintext in staging, or unexplained count
change.

## Final window

Have rollback command lines prepared before maintenance. Measure from the Nginx
maintenance commit until the live include is restored. Target is under 30
seconds. The forward path starts the service exactly once.

1. Atomically enable maintenance; Nginx must return `503` plus
   `Retry-After: 30` before stopping the service:

```bash
sudo install -m 0644 deploy/nginx/runtime-risk-maintenance.conf \
  /etc/nginx/snippets/sub2api-active.conf.new
sudo nginx -t
sudo mv /etc/nginx/snippets/sub2api-active.conf.new \
  /etc/nginx/snippets/sub2api-active.conf
sudo nginx -s reload
curl -fsS -o /dev/null http://127.0.0.1/health && exit 1 || true
test "$(curl -sS -o /dev/null -w '%{http_code}' http://127.0.0.1/health)" = 503
test "$(curl -sSI http://127.0.0.1/health | awk -F': ' 'tolower($1)=="retry-after" {gsub("\\r", "", $2); print $2}')" = 30
```

2. Immediately terminate HTTP, SSE, and WebSocket connections, then keep the
   old service stopped. This intentionally does not drain:

```bash
sudo systemctl kill --kill-who=all --signal=SIGKILL sub2api
sudo systemctl stop sub2api
test "$(systemctl is-active sub2api || true)" = inactive
```

3. Finalize. The transaction takes access-exclusive locks, stages the delta,
   validates totals/statuses/payload hashes/decryption, merges exactly one row
   per job, deletes old config/tables, writes the audit, and commits atomically:

```bash
/opt/sub2api/bin/unified-risk-migration finalize \
  --maintenance-confirmed \
  --proof "$CUTOVER_PROOF" \
  --key-ring "$ARCHIVE_KEY_RING"
```

4. Install the already-built new immutable binary, reload the systemd drop-in,
   and start the service once:

```bash
sudo systemctl daemon-reload
sudo systemctl start sub2api
```

5. Gate route restoration on all four conditions: process health, finalized
   migration, a key ring containing every referenced Key ID, and private
   writable retry/emergency directories. The new service owns the queue lock:

```bash
curl -fsS http://127.0.0.1:8080/health | jq -e '.status == "ok"'
/opt/sub2api/bin/unified-risk-migration status --require-finalized \
  --key-ring "$ARCHIVE_KEY_RING" \
  --retry-dir "$ARCHIVE_RETRY_DIR" \
  --emergency-dir "$ARCHIVE_EMERGENCY_DIR" \
  | jq -e '.ready == true and .status == "finalized"'
```

6. Atomically restore the live include only after both gates pass:

```bash
sudo install -m 0644 deploy/nginx/runtime-risk-live.conf \
  /etc/nginx/snippets/sub2api-active.conf.new
sudo nginx -t
sudo mv /etc/nginx/snippets/sub2api-active.conf.new \
  /etc/nginx/snippets/sub2api-active.conf
sudo nginx -s reload
curl -fsS http://127.0.0.1/health | jq -e '.status == "ok"'
```

Record elapsed maintenance time, exact reports, health output, queue/degraded
metrics, and Nginx 503/live responses without recording requests or secrets.

## Rollback

Rollback immediately if finalization fails, health/key/queue gates fail, or the
window exceeds 30 seconds. Keep maintenance active. A failed finalization rolls
back the transaction and leaves old tables/config intact; start the old binary
only after verifying that state. After a committed finalization, restore the old
tables first, restore old configuration second, then start the old binary:

```bash
sudo systemctl stop sub2api
pg_restore --clean --if-exists --exit-on-error --no-owner --no-privileges \
  --dbname "$DATABASE_NAME" "$CUTOVER_DUMP"
{
  printf '%s\n' 'BEGIN;' \
    'CREATE TEMP TABLE prompt_config_restore (value text NOT NULL);' \
    'COPY prompt_config_restore (value) FROM STDIN WITH (FORMAT csv);'
  cat "$CUTOVER_CONFIG"
  printf '%s\n' '\.' \
    "INSERT INTO settings (key,value,updated_at) SELECT 'prompt_audit_config', value, NOW() FROM prompt_config_restore ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW();" \
    'COMMIT;'
} | psql -X -v ON_ERROR_STOP=1
psql -X -v ON_ERROR_STOP=1 -c \
  "SELECT COUNT(*) FROM prompt_audit_jobs; SELECT COUNT(*) FROM prompt_audit_events;"
sudo install -m 0755 /secure/operator-owned/sub2api-old /opt/sub2api/sub2api
sudo systemctl start sub2api-old-rollback
curl -fsS http://127.0.0.1:8080/health | jq -e '.status == "ok"'
```

The `sub2api-old-rollback` unit used in rehearsal/production must be a prepared
copy of the old unit pointing at the restored old binary; it is deliberately
named differently so the forward-path static assertion remains exactly one
`systemctl start sub2api`. Only after old health and Prompt Audit read/write
smoke checks pass may the live Nginx include be restored. Preserve new unified
columns and encrypted archives; the old binary ignores them. Do not roll back
the whole database.

## Rehearsal evidence

Before production authorization, repeat this runbook against isolated
PostgreSQL/Redis and a stub upstream. Verify GPT, whitespace/mixed-case GPT,
non-GPT, first-layer hit, second-layer block/failure, upstream cyber, ordinary
user/admin-Key disposition, HTTP/SSE/WS capture, preview truncation, download,
delete audit, immediate connection termination, maintenance headers, final
health gates, rollback restore, and old binary compatibility. Missing, skipped,
or stale evidence is a failed rehearsal.

Run the repository rehearsal from the reviewed feature branch. It uses random
loopback ports, temporary Docker PostgreSQL/Redis instances, the recorded old
binary, locally built new/migration binaries, and a local HTTP/SSE/WebSocket
stub. It never reads or changes the production database, service, or Nginx:

```bash
/bin/bash deploy/tests/runtime-risk-isolated-rehearsal.sh
```

Preserve the printed evidence directory and `rehearsal-result.json`. The
forward report must show one new-service start, a sub-30-second maintenance
window, forced HTTP/SSE/WebSocket termination, all health gates, restored old
tables/configuration, preserved unified archive columns/rows, and a healthy
old binary after rollback. The migration transaction failure path is injected
and verified separately by `TestUnifiedRiskMigrationAcceptance`; functional
GPT scope, two-layer stub behavior, cyber disposition, raw HTTP/SSE/WebSocket
archives, sensitive-surface isolation, preview/download/delete, and the body
budget are verified by `TestRuntimeCustomizationsAcceptance`.
