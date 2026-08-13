#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

fail() {
  printf 'runtime risk cutover test failed: %s\n' "$1" >&2
  exit 1
}

assert_exact_line() {
  file=$1
  line=$2
  count=$(grep -Fxc "$line" "$file" || true)
  [ "$count" -eq 1 ] || fail "$file must contain exactly once: $line"
}

assert_order() {
  file=$1
  shift
  previous=0
  for marker in "$@"; do
    line=$(grep -n -F -x "$marker" "$file" | cut -d: -f1)
    [ -n "$line" ] || fail "$file is missing ordered marker: $marker"
    [ "$line" -gt "$previous" ] || fail "$file has markers out of order at: $marker"
    previous=$line
  done
}

live=deploy/nginx/runtime-risk-live.conf
maintenance=deploy/nginx/runtime-risk-maintenance.conf
resources=deploy/systemd/sub2api-runtime-risk.conf
sequence=deploy/runtime-risk-cutover.sequence
runbook=docs/runtime-risk-cutover-runbook.md
config=deploy/config.example.yaml
keyring_example=deploy/content-moderation-keyring.example.json
rehearsal=deploy/tests/runtime-risk-isolated-rehearsal.sh

assert_exact_line "$live" '    client_max_body_size 256m;'
assert_exact_line "$live" '    proxy_request_buffering off;'
assert_exact_line "$live" '    proxy_buffering off;'
assert_exact_line "$maintenance" '    add_header Retry-After 30 always;'
assert_exact_line "$maintenance" "    return 503 '{\"error\":{\"type\":\"service_unavailable\",\"code\":\"runtime_risk_cutover\",\"message\":\"Service temporarily unavailable\"}}';"
assert_exact_line "$resources" 'Environment=GOMEMLIMIT=4GiB'
assert_exact_line "$resources" 'MemoryHigh=6G'
assert_exact_line "$resources" 'MemoryMax=8G'
assert_exact_line "$config" '  key_ring_path: "/etc/sub2api/secrets/content-moderation-keyring.json"'
assert_exact_line "$config" '  retry_dir: "/var/lib/sub2api/content-moderation/retry"'
assert_exact_line "$config" '  emergency_dir: "/var/lib/sub2api/content-moderation/emergency"'
grep -Fq 'REPLACE_WITH_BASE64_ENCODED_32_BYTE_KEY' "$keyring_example" || \
  fail 'key-ring example must contain a nonfunctional placeholder'
if grep -Eq '"[A-Za-z0-9+/]{43}=?"' "$keyring_example"; then
  fail 'key-ring example appears to contain a usable 32-byte base64 key'
fi
[ -x "$rehearsal" ] || fail 'isolated cutover rehearsal must be executable'
grep -Fq 'TestRuntimeCustomizationsAcceptance' "$runbook" || \
  fail 'runbook must name the runtime acceptance evidence'
grep -Fq 'TestUnifiedRiskMigrationAcceptance' "$runbook" || \
  fail 'runbook must name the migration rollback evidence'
grep -Fq 'forward_window_ms' "$rehearsal" || \
  fail 'isolated rehearsal must measure the forward maintenance window'
grep -Fq 'new_start_count' "$rehearsal" || \
  fail 'isolated rehearsal must assert a single new-service start'
grep -Fq 'http_sse_websocket_terminated' "$rehearsal" || \
  fail 'isolated rehearsal must report forced connection termination'
grep -Fq 'old_binary_healthy_after_restore' "$rehearsal" || \
  fail 'isolated rehearsal must report old-binary rollback health'

assert_order "$sequence" \
  '01 verify-backup' \
  '02 online-prepare' \
  '03 enable-maintenance-503' \
  '04 terminate-connections' \
  '05 stop-old-service' \
  '06 finalize-transaction' \
  '07 start-new-service-once' \
  '08 health-check' \
  '09 finalized-keyring-queue-gate' \
  '10 restore-live-route'

assert_order "$sequence" \
  'rollback-01 stop-new-service' \
  'rollback-02 restore-old-tables' \
  'rollback-03 restore-old-prompt-config' \
  'rollback-04 start-old-service' \
  'rollback-05 health-check-old' \
  'rollback-06 restore-live-route'

start_count=$(grep -Ec '^sudo systemctl start sub2api$' "$runbook" || true)
[ "$start_count" -eq 1 ] || fail "runbook must start sub2api exactly once in the forward cutover"
restart_count=$(grep -Ec '^sudo systemctl restart sub2api$' "$runbook" || true)
[ "$restart_count" -eq 0 ] || fail "runbook must not restart sub2api during cutover"
grep -Fq 'sudo systemctl kill --kill-who=all --signal=SIGKILL sub2api' "$runbook" || \
  fail 'runbook must immediately terminate HTTP/SSE/WS connections'
grep -Fq 'status --require-finalized' "$runbook" || \
  fail 'runbook must gate live routing on finalized/key-ring/queue readiness'
grep -Fq 'pg_restore --clean --if-exists --exit-on-error' "$runbook" || \
  fail 'runbook must restore old tables before starting the old binary'
grep -Fq 'COPY prompt_config_restore (value) FROM STDIN WITH (FORMAT csv);' "$runbook" || \
  fail 'runbook must restore Prompt Audit config through CSV stdin'
if grep -Fq -- '--set=prompt_config=' "$runbook"; then
  fail 'runbook must not expose Prompt Audit config through process arguments'
fi

# Negative checks prove the verifier is sensitive to each critical property.
tmp_dir=$(mktemp -d)
trap 'rm -rf "$tmp_dir"' EXIT HUP INT TERM
sed 's/client_max_body_size 256m/client_max_body_size 128m/' "$live" >"$tmp_dir/live"
grep -Fqx '    client_max_body_size 256m;' "$tmp_dir/live" && fail 'negative 256m mutation was not detected'
sed 's/proxy_request_buffering off/proxy_request_buffering on/' "$live" >"$tmp_dir/buffering"
grep -Fqx '    proxy_request_buffering off;' "$tmp_dir/buffering" && fail 'negative buffering mutation was not detected'
sed '/Retry-After/d' "$maintenance" >"$tmp_dir/maintenance"
grep -Fqx '    add_header Retry-After 30 always;' "$tmp_dir/maintenance" && fail 'negative Retry-After mutation was not detected'
sed 's/MemoryMax=8G/MemoryMax=9G/' "$resources" >"$tmp_dir/resources"
grep -Fqx 'MemoryMax=8G' "$tmp_dir/resources" && fail 'negative MemoryMax mutation was not detected'
awk '
  $0 == "rollback-02 restore-old-tables" { saved=$0; next }
  $0 == "rollback-04 start-old-service" { print; print saved; next }
  { print }
' "$sequence" >"$tmp_dir/rollback-order"
restore_line=$(grep -n -F -x 'rollback-02 restore-old-tables' "$tmp_dir/rollback-order" | cut -d: -f1)
start_line=$(grep -n -F -x 'rollback-04 start-old-service' "$tmp_dir/rollback-order" | cut -d: -f1)
[ -n "$restore_line" ] && [ -n "$start_line" ] || fail 'negative rollback-order mutation lost a marker'
[ "$restore_line" -gt "$start_line" ] || fail 'negative rollback-order mutation was not constructed'

printf 'runtime risk cutover test passed\n'
