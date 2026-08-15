#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
reference_binary=${REFERENCE_BINARY:-/home/czc/sub2api-guard-work/backend/sub2api-new}
reference_sha256=34d6b28105a5d5cce0e216b47902048edf006ff3ca85c61148771de1d2c27634
evidence_dir=${REHEARSAL_EVIDENCE_DIR:-$(mktemp -d /tmp/sub2api-runtime-risk-rehearsal.XXXXXX)}
suffix=$(basename "$evidence_dir" | tr -cd 'A-Za-z0-9_.-')
postgres_name="sub2api-risk-rehearsal-pg-$suffix"
redis_name="sub2api-risk-rehearsal-redis-$suffix"

old_pid=
new_pid=
stub_pid=
http_client_pid=
sse_client_pid=
ws_client_pid=
nginx_started=false

fail() {
    printf 'runtime risk isolated rehearsal failed: %s\n' "$*" >&2
    exit 1
}

stop_pid() {
    local pid=${1:-}
    local signal=${2:-TERM}
    [ -n "$pid" ] || return 0
    kill -0 "$pid" 2>/dev/null || return 0
    kill -s "$signal" "$pid" 2>/dev/null || true
    for _ in $(seq 1 50); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.1
    done
    if kill -0 "$pid" 2>/dev/null; then
        kill -KILL "$pid" 2>/dev/null || true
    fi
    wait "$pid" 2>/dev/null || true
}

cleanup() {
    set +e
    stop_pid "$http_client_pid"
    stop_pid "$sse_client_pid"
    stop_pid "$ws_client_pid"
    stop_pid "$stub_pid"
    stop_pid "$new_pid"
    stop_pid "$old_pid"
    if [ "$nginx_started" = true ]; then
        nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -s quit >/dev/null 2>&1 || true
    fi
    for container_name in "$postgres_name" "$redis_name"; do
        if docker inspect "$container_name" >/dev/null 2>&1; then
            docker stop -t 1 "$container_name" >/dev/null 2>&1 || true
        fi
    done
}
trap cleanup EXIT HUP INT TERM

require_command() {
    command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

allocate_port() {
    local candidate
    for _ in $(seq 1 200); do
        candidate=$(shuf -i 20000-60000 -n 1)
        if ! ss -H -ltn "sport = :$candidate" | grep -q .; then
            printf '%s\n' "$candidate"
            return 0
        fi
    done
    fail 'unable to allocate an isolated local port'
}

wait_http() {
    local url=$1
    local output=$2
    for _ in $(seq 1 160); do
        if curl -fsS --max-time 1 "$url" >"$output" 2>/dev/null; then
            return 0
        fi
        sleep 0.25
    done
    return 1
}

wait_file_contains() {
    local path=$1
    local value=$2
    for _ in $(seq 1 100); do
        if grep -a -q "$value" "$path" 2>/dev/null; then
            return 0
        fi
        sleep 0.1
    done
    return 1
}

now_ms() {
	date +%s%N | cut -c1-13
}

for command_name in docker curl jq nginx psql pg_dump pg_restore pg_isready redis-cli openssl flock sha256sum ss shuf install sed cmp; do
    require_command "$command_name"
done
[ -f "$reference_binary" ] || fail "reference binary is missing: $reference_binary"
actual_reference_sha=$(sha256sum "$reference_binary" | awk '{print $1}')
[ "$actual_reference_sha" = "$reference_sha256" ] || fail 'reference binary SHA-256 changed'

mkdir -p "$evidence_dir/build" "$evidence_dir/app-work" "$evidence_dir/archive/retry" \
    "$evidence_dir/archive/emergency" "$evidence_dir/nginx"
chmod 0700 "$evidence_dir" "$evidence_dir/app-work" "$evidence_dir/archive" \
    "$evidence_dir/archive/retry" "$evidence_dir/archive/emergency" "$evidence_dir/nginx"
chmod 0777 "$evidence_dir/build"
printf 'evidence_dir=%s\n' "$evidence_dir"

docker run --rm \
    -v "$repo_root:/repo" \
    -v "$evidence_dir/build:/out" \
    -v sub2api-go-mod-1266:/go/pkg/mod \
    -v sub2api-go-build-1266:/root/.cache/go-build \
    -w /repo/backend golang:1.26.6 \
    sh -c 'export PATH=/usr/local/go/bin:$PATH; set -eu; CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/sub2api-new ./cmd/server; CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/unified-risk-migration ./cmd/unified-risk-migration; CGO_ENABLED=0 go build -buildvcs=false -trimpath -o /out/connection-stub /repo/deploy/tests/fixtures/runtime-risk-connection-stub.go'
chmod 0755 "$evidence_dir/build/sub2api-new" "$evidence_dir/build/unified-risk-migration" "$evidence_dir/build/connection-stub"
chmod 0700 "$evidence_dir/build"
sha256sum "$reference_binary" "$evidence_dir/build/sub2api-new" \
    "$evidence_dir/build/unified-risk-migration" "$evidence_dir/build/connection-stub" \
    >"$evidence_dir/binary-sha256.txt"

db_password=$(openssl rand -hex 24)
admin_password=$(openssl rand -hex 24)
archive_key=$(openssl rand -base64 32 | tr -d '\n')
jq -n --arg key "$archive_key" '{current_key_id:"rehearsal-k1",keys:{"rehearsal-k1":$key}}' \
    >"$evidence_dir/keyring.json"
chmod 0600 "$evidence_dir/keyring.json"
unset archive_key

docker run -d --rm --name "$postgres_name" \
    -e POSTGRES_USER=sub2api_rehearsal \
    -e POSTGRES_PASSWORD="$db_password" \
    -e POSTGRES_DB=sub2api_rehearsal \
    -p 127.0.0.1::5432 postgres:18-alpine >"$evidence_dir/postgres.cid"
docker run -d --rm --name "$redis_name" \
    -p 127.0.0.1::6379 redis:8-alpine >"$evidence_dir/redis.cid"
postgres_port=$(docker port "$postgres_name" 5432/tcp | sed 's/.*://')
redis_port=$(docker port "$redis_name" 6379/tcp | sed 's/.*://')

for _ in $(seq 1 120); do
    if PGPASSWORD="$db_password" pg_isready -h 127.0.0.1 -p "$postgres_port" \
        -U sub2api_rehearsal -d sub2api_rehearsal >/dev/null 2>&1; then
        break
    fi
    sleep 0.25
done
PGPASSWORD="$db_password" pg_isready -h 127.0.0.1 -p "$postgres_port" \
    -U sub2api_rehearsal -d sub2api_rehearsal >/dev/null || fail 'isolated PostgreSQL did not become ready'
for _ in $(seq 1 120); do
    redis-cli -h 127.0.0.1 -p "$redis_port" ping 2>/dev/null | grep -q PONG && break
    sleep 0.25
done
redis-cli -h 127.0.0.1 -p "$redis_port" ping 2>/dev/null | grep -q PONG || fail 'isolated Redis did not become ready'

app_port=$(allocate_port)
proxy_port=$(allocate_port)
stub_port=$(allocate_port)
config_file="$evidence_dir/app-work/config.yaml"

(
    cd "$evidence_dir/app-work"
    exec env \
        AUTO_SETUP=true DATA_DIR="$evidence_dir/app-work" \
        SERVER_HOST=127.0.0.1 SERVER_PORT="$app_port" GIN_MODE=release \
        DATABASE_HOST=127.0.0.1 DATABASE_PORT="$postgres_port" \
        DATABASE_USER=sub2api_rehearsal DATABASE_PASSWORD="$db_password" \
        DATABASE_DBNAME=sub2api_rehearsal DATABASE_SSLMODE=disable \
        REDIS_HOST=127.0.0.1 REDIS_PORT="$redis_port" \
        ADMIN_EMAIL=admin@rehearsal.invalid ADMIN_PASSWORD="$admin_password" \
        "$reference_binary"
) >"$evidence_dir/old-forward.log" 2>&1 &
old_pid=$!
wait_http "http://127.0.0.1:$app_port/health" "$evidence_dir/old-forward-health.json" || {
    tail -100 "$evidence_dir/old-forward.log" >&2 || true
    fail 'reference old binary did not become healthy'
}
jq -e '.status == "ok"' "$evidence_dir/old-forward-health.json" >/dev/null
[ -s "$config_file" ] || fail 'reference old binary did not create its isolated configuration'
chmod 0600 "$config_file"

psql_rehearsal() {
    PGPASSWORD="$db_password" psql -X -v ON_ERROR_STOP=1 -h 127.0.0.1 \
        -p "$postgres_port" -U sub2api_rehearsal -d sub2api_rehearsal "$@"
}

psql_rehearsal >/dev/null <<'SQL'
INSERT INTO settings (key, value) VALUES ('prompt_audit_config', '{"enabled":false}')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;

DO $$
DECLARE
    job_one BIGINT;
    job_two BIGINT;
    job_three BIGINT;
BEGIN
    INSERT INTO prompt_audit_jobs (request_id, group_name, prompt_hash, redacted_preview, status, execution_mode)
    VALUES ('rehearsal-request-1', 'GPT rehearsal', 'rehearsal-hash-1', 'redacted one', 'done', 'async_audit') RETURNING id INTO job_one;
    INSERT INTO prompt_audit_jobs (request_id, group_name, prompt_hash, redacted_preview, status, execution_mode)
    VALUES ('rehearsal-request-2', 'GPT rehearsal', 'rehearsal-hash-2', 'redacted two', 'failed', 'async_audit') RETURNING id INTO job_two;
    INSERT INTO prompt_audit_jobs (request_id, group_name, prompt_hash, redacted_preview, status, execution_mode)
    VALUES ('rehearsal-request-3', 'GPT rehearsal', 'rehearsal-hash-3', 'redacted three', 'done', 'async_audit') RETURNING id INTO job_three;

    INSERT INTO prompt_audit_events (job_id, request_id, decision, risk_level, action, full_prompt)
    VALUES
        (job_one, 'rehearsal-event-1', 'critical', 'critical', 'Block', 'REHEARSAL_PROMPT_ONE'),
        (job_one, 'rehearsal-event-2', 'flag', 'high', 'Warn', 'REHEARSAL_PROMPT_TWO'),
        (job_two, 'rehearsal-event-3', 'critical', 'critical', 'Block', 'REHEARSAL_PROMPT_THREE'),
        (job_three, 'rehearsal-event-4', 'pass', 'low', 'Allow', 'REHEARSAL_PROMPT_FOUR');
END $$;
SQL

psql_rehearsal -c "COPY (SELECT value FROM settings WHERE key = 'prompt_audit_config') TO STDOUT WITH (FORMAT csv)" \
    >"$evidence_dir/prompt-audit-config.csv"
chmod 0600 "$evidence_dir/prompt-audit-config.csv"
test -s "$evidence_dir/prompt-audit-config.csv" || fail 'rollback configuration export is empty'

migration_env=(env CONFIG_FILE="$config_file")
"${migration_env[@]}" "$evidence_dir/build/unified-risk-migration" backup \
    --archive "$evidence_dir/prompt-audit.dump" \
    --proof "$evidence_dir/prompt-audit-proof.json" \
    >"$evidence_dir/backup-report.json"
jq -e '.list_verified == true and .restore_verified == true and .source_job_count == 3 and .source_event_count == 4 and .restored_job_count == 3 and .restored_event_count == 4' \
    "$evidence_dir/backup-report.json" >/dev/null
[ "$(stat -c '%a' "$evidence_dir/prompt-audit.dump")" = 600 ] || fail 'backup archive permissions are not 0600'
[ "$(stat -c '%a' "$evidence_dir/prompt-audit-proof.json")" = 600 ] || fail 'backup proof permissions are not 0600'
"${migration_env[@]}" "$evidence_dir/build/unified-risk-migration" verify \
    --proof "$evidence_dir/prompt-audit-proof.json" >"$evidence_dir/verify-report.json"

for iteration in 1 2; do
    "${migration_env[@]}" "$evidence_dir/build/unified-risk-migration" prepare \
        --proof "$evidence_dir/prompt-audit-proof.json" \
        --key-ring "$evidence_dir/keyring.json" \
        >"$evidence_dir/prepare-$iteration.json"
done
jq -S 'del(.prepared_at)' "$evidence_dir/prepare-1.json" >"$evidence_dir/prepare-1.stable.json"
jq -S 'del(.prepared_at)' "$evidence_dir/prepare-2.json" >"$evidence_dir/prepare-2.stable.json"
cmp -s "$evidence_dir/prepare-1.stable.json" "$evidence_dir/prepare-2.stable.json" || fail 'online prepare was not repeatable'
jq -e '.staged_job_count == 3 and .staged_event_count == 4 and .archived_hit_count == 2' \
    "$evidence_dir/prepare-2.json" >/dev/null

psql_rehearsal >/dev/null <<'SQL'
WITH added_job AS (
    INSERT INTO prompt_audit_jobs (request_id, group_name, prompt_hash, redacted_preview, status, execution_mode)
    VALUES ('rehearsal-request-delta', 'GPT rehearsal', 'rehearsal-hash-delta', 'redacted delta', 'done', 'async_audit')
    RETURNING id
)
INSERT INTO prompt_audit_events (job_id, request_id, decision, risk_level, action, full_prompt)
SELECT id, 'rehearsal-event-delta', 'critical', 'critical', 'Block', 'REHEARSAL_PROMPT_DELTA'
FROM added_job;
SQL

"$evidence_dir/build/connection-stub" --listen "127.0.0.1:$stub_port" \
    >"$evidence_dir/connection-stub.log" 2>&1 &
stub_pid=$!
wait_http "http://127.0.0.1:$stub_port/health" "$evidence_dir/stub-health.json" || fail 'connection stub did not become healthy'

sed "s#127.0.0.1:8080#127.0.0.1:$stub_port#" \
    "$repo_root/deploy/nginx/runtime-risk-live.conf" >"$evidence_dir/nginx/live-stub.conf"
sed "s#127.0.0.1:8080#127.0.0.1:$app_port#" \
    "$repo_root/deploy/nginx/runtime-risk-live.conf" >"$evidence_dir/nginx/live-app.conf"
install -m 0644 "$evidence_dir/nginx/live-stub.conf" "$evidence_dir/nginx/active.conf"
cat >"$evidence_dir/nginx/nginx.conf" <<EOF
pid $evidence_dir/nginx/nginx.pid;
error_log $evidence_dir/nginx/error.log info;
events { worker_connections 64; }
http {
    map \$http_upgrade \$connection_upgrade { default upgrade; '' ''; }
    access_log $evidence_dir/nginx/access.log;
    server {
        listen 127.0.0.1:$proxy_port;
        server_name localhost;
        include $evidence_dir/nginx/active.conf;
    }
}
EOF
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -t \
    >"$evidence_dir/nginx/config-test.log" 2>&1
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf"
nginx_started=true
wait_http "http://127.0.0.1:$proxy_port/health" "$evidence_dir/nginx-live-stub-health.json" || fail 'Nginx live route did not reach the stub'

(
	set +e
	curl -sS -N "http://127.0.0.1:$proxy_port/http" >"$evidence_dir/http-stream.out" 2>"$evidence_dir/http-stream.err"
	printf '%s\n' "$?" >"$evidence_dir/http-stream.status"
	now_ms >"$evidence_dir/http-stream.closed-ms"
) &
http_client_pid=$!
(
	set +e
	curl -sS -N "http://127.0.0.1:$proxy_port/sse" >"$evidence_dir/sse-stream.out" 2>"$evidence_dir/sse-stream.err"
	printf '%s\n' "$?" >"$evidence_dir/sse-stream.status"
	now_ms >"$evidence_dir/sse-stream.closed-ms"
) &
sse_client_pid=$!
(
	set +e
	"$evidence_dir/build/connection-stub" --connect "127.0.0.1:$proxy_port" \
		>"$evidence_dir/ws-stream.out" 2>"$evidence_dir/ws-stream.err"
	printf '%s\n' "$?" >"$evidence_dir/ws-stream.status"
	now_ms >"$evidence_dir/ws-stream.closed-ms"
) &
ws_client_pid=$!
wait_file_contains "$evidence_dir/http-stream.out" 'tick-' || fail 'HTTP stream was not established'
wait_file_contains "$evidence_dir/sse-stream.out" 'data: tick-' || fail 'SSE stream was not established'
wait_file_contains "$evidence_dir/ws-stream.out" '101 Switching Protocols' || fail 'WebSocket stream was not established'

window_started_ms=$(now_ms)
install -m 0644 "$repo_root/deploy/nginx/runtime-risk-maintenance.conf" "$evidence_dir/nginx/active.conf.new"
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -t >/dev/null 2>&1
mv "$evidence_dir/nginx/active.conf.new" "$evidence_dir/nginx/active.conf"
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -s reload >/dev/null
maintenance_code=
for _ in $(seq 1 50); do
    maintenance_code=$(curl -sS -o "$evidence_dir/maintenance-body.json" -D "$evidence_dir/maintenance-headers.txt" \
        -w '%{http_code}' "http://127.0.0.1:$proxy_port/health")
    [ "$maintenance_code" = 503 ] && break
    sleep 0.1
done
[ "$maintenance_code" = 503 ] || fail 'maintenance response was not HTTP 503'
retry_after=$(awk -F': ' 'tolower($1)=="retry-after" {gsub("\r", "", $2); print $2}' "$evidence_dir/maintenance-headers.txt")
[ "$retry_after" = 30 ] || fail 'maintenance response did not include Retry-After: 30'

termination_started_ms=$(now_ms)
kill -KILL "$stub_pid" "$old_pid"
wait "$stub_pid" 2>/dev/null || true
wait "$old_pid" 2>/dev/null || true
stub_pid=
old_pid=
for _ in $(seq 1 50); do
	if [ -s "$evidence_dir/http-stream.closed-ms" ] && \
		[ -s "$evidence_dir/sse-stream.closed-ms" ] && \
		[ -s "$evidence_dir/ws-stream.closed-ms" ]; then
		break
	fi
	sleep 0.1
done
[ -s "$evidence_dir/http-stream.closed-ms" ] || fail 'the existing HTTP connection was not terminated promptly'
[ -s "$evidence_dir/sse-stream.closed-ms" ] || fail 'the existing SSE connection was not terminated promptly'
[ -s "$evidence_dir/ws-stream.closed-ms" ] || fail 'the existing WebSocket connection was not terminated promptly'
wait "$http_client_pid" 2>/dev/null || true
wait "$sse_client_pid" 2>/dev/null || true
wait "$ws_client_pid" 2>/dev/null || true
[ "$(cat "$evidence_dir/http-stream.status")" = 18 ] || fail 'HTTP stream did not exit with the expected forced-close status'
[ "$(cat "$evidence_dir/sse-stream.status")" = 18 ] || fail 'SSE stream did not exit with the expected forced-close status'
[ "$(cat "$evidence_dir/ws-stream.status")" = 0 ] || fail 'WebSocket probe failed while observing downstream closure'
grep -Fq 'transfer closed' "$evidence_dir/http-stream.err" || fail 'HTTP stream did not report forced termination'
grep -Fq 'transfer closed' "$evidence_dir/sse-stream.err" || fail 'SSE stream did not report forced termination'
grep -Fq '101 Switching Protocols' "$evidence_dir/ws-stream.out" || fail 'WebSocket termination evidence lost its successful handshake'
grep -Fq 'connection closed' "$evidence_dir/ws-stream.out" || fail 'WebSocket probe did not observe downstream closure'
http_client_pid=
sse_client_pid=
ws_client_pid=
termination_closed_ms=$(sort -n "$evidence_dir/http-stream.closed-ms" \
	"$evidence_dir/sse-stream.closed-ms" "$evidence_dir/ws-stream.closed-ms" | tail -n 1)
termination_elapsed_ms=$(( termination_closed_ms - termination_started_ms ))
[ "$termination_elapsed_ms" -ge 0 ] || fail 'connection termination timestamp preceded service termination'
[ "$termination_elapsed_ms" -lt 5000 ] || fail 'connection termination exceeded five seconds'

"${migration_env[@]}" "$evidence_dir/build/unified-risk-migration" finalize \
    --maintenance-confirmed \
    --proof "$evidence_dir/prompt-audit-proof.json" \
    --key-ring "$evidence_dir/keyring.json" \
    >"$evidence_dir/finalize-report.json"
jq -e '.job_count == 4 and .event_count == 5 and .archived_hit_count == 3 and .status_counts.done == 3 and .status_counts.failed == 1' \
    "$evidence_dir/finalize-report.json" >/dev/null
[ "$(psql_rehearsal -At -c "SELECT to_regclass('public.prompt_audit_jobs') IS NULL AND to_regclass('public.prompt_audit_events') IS NULL")" = t ] || fail 'finalize did not remove legacy tables'
[ "$(psql_rehearsal -At -c "SELECT COUNT(*) FROM settings WHERE key='prompt_audit_config'")" = 0 ] || fail 'finalize did not remove legacy configuration'
[ "$(psql_rehearsal -At -c 'SELECT COUNT(*) FROM content_moderation_logs WHERE legacy_source_job_id IS NOT NULL')" = 4 ] || fail 'finalize did not create exactly one unified row per legacy job'

new_start_count=0
new_start_count=$((new_start_count + 1))
(
    cd "$evidence_dir/app-work"
    exec env \
        CONFIG_FILE="$config_file" DATA_DIR="$evidence_dir/app-work" \
        SERVER_HOST=127.0.0.1 SERVER_PORT="$app_port" GIN_MODE=release GOMEMLIMIT=4GiB \
        CONTENT_MODERATION_ARCHIVE_KEY_RING_PATH="$evidence_dir/keyring.json" \
        CONTENT_MODERATION_ARCHIVE_RETRY_DIR="$evidence_dir/archive/retry" \
        CONTENT_MODERATION_ARCHIVE_EMERGENCY_DIR="$evidence_dir/archive/emergency" \
        "$evidence_dir/build/sub2api-new"
) >"$evidence_dir/new.log" 2>&1 &
new_pid=$!
wait_http "http://127.0.0.1:$app_port/health" "$evidence_dir/new-health.json" || {
    tail -100 "$evidence_dir/new.log" >&2 || true
    fail 'new binary did not become healthy'
}
jq -e '.status == "ok"' "$evidence_dir/new-health.json" >/dev/null
"${migration_env[@]}" "$evidence_dir/build/unified-risk-migration" status \
    --require-finalized \
    --key-ring "$evidence_dir/keyring.json" \
    --retry-dir "$evidence_dir/archive/retry" \
    --emergency-dir "$evidence_dir/archive/emergency" \
    >"$evidence_dir/status-report.json"
jq -e '.ready == true and .status == "finalized" and .archive_key_references == "ready" and .runtime_directories == "ready" and .runtime_queue_lock == "held"' \
    "$evidence_dir/status-report.json" >/dev/null
[ "$new_start_count" = 1 ] || fail 'forward path started the new service more than once'

install -m 0644 "$evidence_dir/nginx/live-app.conf" "$evidence_dir/nginx/active.conf.new"
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -t >/dev/null 2>&1
mv "$evidence_dir/nginx/active.conf.new" "$evidence_dir/nginx/active.conf"
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -s reload >/dev/null
wait_http "http://127.0.0.1:$proxy_port/health" "$evidence_dir/nginx-live-new-health.json" || fail 'Nginx did not restore the healthy new route'
window_elapsed_ms=$(( $(now_ms) - window_started_ms ))
[ "$window_elapsed_ms" -lt 30000 ] || fail 'forward cutover exceeded 30 seconds'

install -m 0644 "$repo_root/deploy/nginx/runtime-risk-maintenance.conf" "$evidence_dir/nginx/active.conf.new"
mv "$evidence_dir/nginx/active.conf.new" "$evidence_dir/nginx/active.conf"
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -s reload >/dev/null
stop_pid "$new_pid" TERM
new_pid=

PGPASSWORD="$db_password" pg_restore --clean --if-exists --exit-on-error --no-owner --no-privileges \
    -h 127.0.0.1 -p "$postgres_port" -U sub2api_rehearsal -d sub2api_rehearsal \
    "$evidence_dir/prompt-audit.dump" >"$evidence_dir/rollback-pg-restore.log" 2>&1
{
    printf '%s\n' 'BEGIN;' \
        'CREATE TEMP TABLE prompt_config_restore (value text NOT NULL);' \
        'COPY prompt_config_restore (value) FROM STDIN WITH (FORMAT csv);'
    cat "$evidence_dir/prompt-audit-config.csv"
    printf '%s\n' '\.' \
        "INSERT INTO settings (key,value,updated_at) SELECT 'prompt_audit_config', value, NOW() FROM prompt_config_restore ON CONFLICT (key) DO UPDATE SET value=EXCLUDED.value, updated_at=NOW();" \
        'COMMIT;'
} | psql_rehearsal >"$evidence_dir/rollback-config-restore.log"

[ "$(psql_rehearsal -At -c 'SELECT COUNT(*) FROM prompt_audit_jobs')" = 3 ] || fail 'rollback did not restore the verified job count'
[ "$(psql_rehearsal -At -c 'SELECT COUNT(*) FROM prompt_audit_events')" = 4 ] || fail 'rollback did not restore the verified event count'
[ "$(psql_rehearsal -At -c "SELECT COUNT(*) FROM settings WHERE key='prompt_audit_config'")" = 1 ] || fail 'rollback did not restore legacy configuration'
[ "$(psql_rehearsal -At -c "SELECT COUNT(*) FROM information_schema.columns WHERE table_name='content_moderation_logs' AND column_name='archive_id'")" = 1 ] || fail 'rollback removed unified archive columns'
[ "$(psql_rehearsal -At -c 'SELECT COUNT(*) FROM content_moderation_logs WHERE legacy_source_job_id IS NOT NULL')" = 4 ] || fail 'rollback removed unified migrated rows'

(
    cd "$evidence_dir/app-work"
    exec env CONFIG_FILE="$config_file" DATA_DIR="$evidence_dir/app-work" \
        SERVER_HOST=127.0.0.1 SERVER_PORT="$app_port" GIN_MODE=release \
        "$reference_binary"
) >"$evidence_dir/old-rollback.log" 2>&1 &
old_pid=$!
wait_http "http://127.0.0.1:$app_port/health" "$evidence_dir/old-rollback-health.json" || {
    tail -100 "$evidence_dir/old-rollback.log" >&2 || true
    fail 'reference old binary did not start after legacy-table restore'
}
jq -e '.status == "ok"' "$evidence_dir/old-rollback-health.json" >/dev/null

psql_rehearsal >/dev/null <<'SQL'
BEGIN;
WITH rollback_job AS (
    INSERT INTO prompt_audit_jobs (request_id, status, execution_mode)
    VALUES ('rollback-smoke', 'done', 'async_audit')
    RETURNING id
)
INSERT INTO prompt_audit_events (job_id, request_id, decision, risk_level, action, full_prompt)
SELECT id, 'rollback-smoke-event', 'pass', 'low', 'Allow', '' FROM rollback_job;
DELETE FROM prompt_audit_jobs WHERE request_id = 'rollback-smoke';
COMMIT;
SQL

install -m 0644 "$evidence_dir/nginx/live-app.conf" "$evidence_dir/nginx/active.conf.new"
mv "$evidence_dir/nginx/active.conf.new" "$evidence_dir/nginx/active.conf"
nginx -p "$evidence_dir/nginx/" -c "$evidence_dir/nginx/nginx.conf" -s reload >/dev/null
wait_http "http://127.0.0.1:$proxy_port/health" "$evidence_dir/nginx-live-old-rollback-health.json" || fail 'Nginx did not restore the healthy rollback route'

jq -n \
    --arg evidence_dir "$evidence_dir" \
    --arg reference_sha256 "$reference_sha256" \
    --arg new_sha256 "$(sha256sum "$evidence_dir/build/sub2api-new" | awk '{print $1}')" \
    --arg migration_sha256 "$(sha256sum "$evidence_dir/build/unified-risk-migration" | awk '{print $1}')" \
    --argjson forward_window_ms "$window_elapsed_ms" \
    --argjson connection_termination_ms "$termination_elapsed_ms" \
    --argjson new_start_count "$new_start_count" \
    '{status:"passed",evidence_dir:$evidence_dir,reference_sha256:$reference_sha256,new_sha256:$new_sha256,migration_sha256:$migration_sha256,backup_restore_verified:true,prepare_repeatable:true,final_delta_captured:true,maintenance_503:true,retry_after_30:true,http_sse_websocket_terminated:true,connection_termination_ms:$connection_termination_ms,health_gates_passed:true,new_start_count:$new_start_count,forward_window_ms:$forward_window_ms,under_30_seconds:($forward_window_ms < 30000),rollback_tables_and_config_restored:true,unified_archives_preserved:true,old_binary_healthy_after_restore:true}' \
    >"$evidence_dir/rehearsal-result.json"
cat "$evidence_dir/rehearsal-result.json"
