#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
run_dir=${1:-}
report_path=
work_dir=
postgres_started=false
redis_started=false
stub_pid=
app_pid=
pg_bin=
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
commit=$(git -C "$repo_root" rev-parse HEAD)

write_failure_report() {
  local reason=$1
  if [ -z "$report_path" ] || [ -e "$report_path" ]; then
    return 0
  fi
  jq -n \
    --arg run_id "$run_id" \
    --arg commit "$commit" \
    --arg started_at "$started_at" \
    --arg ended_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    --arg reason "$reason" \
    '{
      schema_version: 1,
      run_id: $run_id,
      commit: $commit,
      started_at: $started_at,
      ended_at: $ended_at,
      status: "failed",
      reason: $reason,
      config_summary: {
        model: "deepseek-v4-flash",
        thinking_type: "disabled",
        response_format: "json_object",
        confidence_threshold: 0.80,
        channel_timeout_ms: 3000,
        total_timeout_ms: 10000
      },
      actual_execution_count: {
        required_go_tests: 0,
        admin_api_calls: 0,
        stub_requests: 0
      }
    }' >"$report_path"
  chmod 0600 "$report_path"
}

fail() {
  local reason=$1
  write_failure_report "$reason"
  printf 'DeepSeek moderation release test failed: %s\n' "$reason" >&2
  exit 1
}

on_error() {
  local exit_code=$1
  local line=$2
  trap - ERR
  write_failure_report "unexpected command failure at line $line (exit $exit_code)"
  printf 'DeepSeek moderation release test failed at line %s (exit %s)\n' "$line" "$exit_code" >&2
  exit "$exit_code"
}
trap 'on_error "$?" "$LINENO"' ERR

stop_pid() {
  local pid=${1:-}
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null
  for _ in $(seq 1 50); do
    if ! kill -0 "$pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null
  fi
  if wait "$pid" 2>/dev/null; then
    :
  else
    :
  fi
  return 0
}

cleanup() {
  trap - ERR
  set +e
  stop_pid "$app_pid"
  stop_pid "$stub_pid"
  if [ "$redis_started" = true ] && [ -n "$work_dir" ]; then
    redis-cli -h 127.0.0.1 -p "$redis_port" shutdown nosave >/dev/null 2>&1
  fi
  if [ "$postgres_started" = true ] && [ -n "$pg_bin" ] && [ -n "$work_dir" ]; then
    "$pg_bin/pg_ctl" -D "$work_dir/postgres" -m immediate -w stop >/dev/null 2>&1
  fi
  if [ -n "$work_dir" ] && [ -d "$work_dir" ]; then
    case "$work_dir" in
      /tmp/sub2api-deepseek-contract.*) rm -rf -- "$work_dir" ;;
    esac
  fi
  return 0
}
trap cleanup EXIT
trap 'exit 130' HUP INT TERM

require_command() {
  command -v "$1" >/dev/null 2>&1 || fail "required command is unavailable: $1"
}

allocate_port() {
  local candidate
  for _ in $(seq 1 250); do
    candidate=$(shuf -i 20000-60000 -n 1)
    if ss -H -ltn "sport = :$candidate" | grep -q .; then
      continue
    fi
    printf '%s\n' "$candidate"
    return 0
  done
  fail "unable to allocate an isolated local port"
}

wait_http() {
  local url=$1
  local output=$2
  for _ in $(seq 1 240); do
    if curl --silent --show-error --fail --max-time 1 "$url" >"$output" 2>/dev/null; then
      return 0
    fi
    sleep 0.25
  done
  return 1
}

api_request() {
  local method=$1
  local path=$2
  local input=$3
  local output=$4
  local curl_status
  if [ -n "$input" ]; then
    if curl --silent --show-error --fail-with-body --max-time 30 \
      --request "$method" \
      --header @"$work_dir/admin-auth.header" \
      --header 'Content-Type: application/json' \
      --data-binary @"$input" \
      "http://127.0.0.1:$app_port$path" >"$output"; then
      curl_status=0
    else
      curl_status=$?
    fi
  else
    if curl --silent --show-error --fail-with-body --max-time 30 \
      --request "$method" \
      --header @"$work_dir/admin-auth.header" \
      "http://127.0.0.1:$app_port$path" >"$output"; then
      curl_status=0
    else
      curl_status=$?
    fi
  fi
  api_call_count=$((api_call_count + 1))
  if [ "$curl_status" -ne 0 ]; then
    if [ -s "$output" ]; then
      install -m 0600 "$output" "$run_dir/last-admin-api-error.json"
    fi
    fail "admin API request failed: $method $path (curl exit $curl_status)"
  fi
}

validate_release_report() {
  local file=$1
  jq -e '
    .schema_version == 1 and
    .status == "passed" and
    (.run_id | type == "string" and length > 0) and
    (.commit | test("^[0-9a-f]{40}$")) and
    (.config_digest | test("^[0-9a-f]{64}$")) and
    .expected_test_count > 0 and
    .passed_test_count == .expected_test_count and
    .integration.postgres == true and
    .integration.redis == true and
    .integration.migration == true and
    .integration.api_encryption == true and
    .integration.api_masking == true and
    .integration.connectivity_probe == true and
    .integration.layer1_stage == true and
    .integration.layer2_stage == true and
    .integration.connectivity_digest_invalidation == true and
    .integration.cleanup == true and
    .sensitivity.invalid_report_rejected == true and
    .sensitivity.benign_collision_passed == true
  ' "$file" >/dev/null
}

[ -n "$run_dir" ] || fail "usage: $0 RUN_DIR"
case "$run_dir" in
  /tmp/sub2api-deepseek-release.*) ;;
  *) fail "RUN_DIR must match /tmp/sub2api-deepseek-release.*" ;;
esac
mkdir -p "$run_dir"
chmod 0700 "$run_dir"
[ "$(stat -c '%a' "$run_dir")" = 700 ] || fail "RUN_DIR must have mode 0700"
report_path="$run_dir/deepseek-moderation-release-report.json"
[ ! -e "$report_path" ] || fail "release report already exists in RUN_DIR"
work_dir=$(mktemp -d /tmp/sub2api-deepseek-contract.XXXXXX)
chmod 0700 "$work_dir"

for command_name in awk curl date git go grep install jq mkdir openssl pg_config redis-cli redis-server sed seq sha256sum shuf ss stat tar; do
  require_command "$command_name"
done
pg_bin=$(pg_config --bindir)
for pg_command in initdb pg_ctl createdb psql; do
  [ -x "$pg_bin/$pg_command" ] || fail "required PostgreSQL command is unavailable: $pg_bin/$pg_command"
done

fixture_dir="$repo_root/deploy/tests/fixtures"
stub_source="$fixture_dir/deepseek-moderation-stub.go"
release_test_source="$fixture_dir/deepseek-moderation-release-contract_test.go"
expected_tests="$fixture_dir/deepseek-moderation-expected-tests.txt"
invalid_report="$fixture_dir/deepseek-release-real-failure.json"
for required_file in "$stub_source" "$release_test_source" "$expected_tests" "$invalid_report"; do
  [ -s "$required_file" ] || fail "required release fixture is missing: $required_file"
done

postgres_port=$(allocate_port)
redis_port=$(allocate_port)
stub_port=$(allocate_port)

mkdir -p "$work_dir/postgres-socket"
chmod 0700 "$work_dir/postgres-socket"
"$pg_bin/initdb" \
  --pgdata="$work_dir/postgres" \
  --username=postgres \
  --auth-local=trust \
  --auth-host=trust \
  --encoding=UTF8 \
  --no-locale >"$work_dir/initdb.log"
"$pg_bin/pg_ctl" -D "$work_dir/postgres" \
  -o "-h 127.0.0.1 -p $postgres_port -k $work_dir/postgres-socket" \
  -l "$work_dir/postgres.log" -w start >/dev/null
postgres_started=true
"$pg_bin/createdb" -h 127.0.0.1 -p "$postgres_port" -U postgres sub2api_release
"$pg_bin/psql" -X -v ON_ERROR_STOP=1 -h 127.0.0.1 -p "$postgres_port" -U postgres -d postgres \
  -c "ALTER ROLE postgres PASSWORD 'release-test-database-password'" >/dev/null

mkdir -p "$work_dir/redis"
redis-server \
  --bind 127.0.0.1 \
  --port "$redis_port" \
  --protected-mode yes \
  --save '' \
  --appendonly no \
  --dir "$work_dir/redis" \
  --pidfile "$work_dir/redis.pid" \
  --logfile "$work_dir/redis.log" \
  --daemonize yes
redis_started=true
for _ in $(seq 1 80); do
  if [ "$(redis-cli -h 127.0.0.1 -p "$redis_port" ping 2>/dev/null)" = PONG ]; then
    break
  fi
  sleep 0.1
done
[ "$(redis-cli -h 127.0.0.1 -p "$redis_port" ping 2>/dev/null)" = PONG ] || fail "temporary Redis did not become ready"

go build -trimpath -o "$work_dir/moderation-stub" "$stub_source"
"$work_dir/moderation-stub" --listen "127.0.0.1:$stub_port" >"$work_dir/stub.log" 2>&1 &
stub_pid=$!
wait_http "http://127.0.0.1:$stub_port/health" "$work_dir/stub-health.json" || fail "moderation HTTP stub did not become ready"
jq -e '.status == "ok"' "$work_dir/stub-health.json" >/dev/null
stub_url="http://127.0.0.1:$stub_port"

# Run release-only package tests from a temporary source copy so the fixture can
# inspect unexported service contracts without changing the production package.
# Local ad-hoc risk-review tests are intentionally excluded from release input.
mkdir -m 0700 "$work_dir/backend"
tar --exclude='./internal/service/risk_review_*_temp_test.go' -C "$repo_root/backend" -cf - . | tar -C "$work_dir/backend" -xf -
install -m 0600 "$release_test_source" "$work_dir/backend/internal/service/deepseek_moderation_release_contract_test.go"
run_regex=$(awk -F'|' 'NF == 2 { if (out != "") out = out "|"; out = out $2 } END { print out }' "$expected_tests")
[ -n "$run_regex" ] || fail "expected test manifest is empty"
(
  cd "$work_dir/backend"
  env RELEASE_MODERATION_STUB_URL="$stub_url" GOMAXPROCS=4 \
    go test -p 2 -json \
      ./internal/service ./resources/content-moderation ./migrations \
      -run "^($run_regex)$" -count=1
) >"$run_dir/deepseek-moderation-go-test-events.jsonl"
chmod 0600 "$run_dir/deepseek-moderation-go-test-events.jsonl"
jq -e -c . "$run_dir/deepseek-moderation-go-test-events.jsonl" >/dev/null

expected_test_count=0
while IFS='|' read -r package_name test_name; do
  [ -n "$package_name" ] || continue
  [ -n "$test_name" ] || fail "invalid expected test manifest row"
  jq -e -s --arg package "$package_name" --arg test "$test_name" '
    any(.[]; .Action == "pass" and .Package == $package and .Test == $test)
  ' "$run_dir/deepseek-moderation-go-test-events.jsonl" >/dev/null || \
    fail "required Go contract test did not pass: $test_name"
  expected_test_count=$((expected_test_count + 1))
done <"$expected_tests"
[ "$expected_test_count" -ge 25 ] || fail "expected test manifest is unexpectedly small"
passed_test_count=$(jq -s '[.[] | select(.Action == "pass" and (.Test // "") != "" and ((.Test | contains("/")) | not))] | length' \
  "$run_dir/deepseek-moderation-go-test-events.jsonl")
[ "$passed_test_count" -ge "$expected_test_count" ] || fail "fewer root tests passed than required"

jq -n \
  --arg run_id "$run_id" \
  --arg commit "$commit" \
  --argjson expected "$expected_test_count" \
  --argjson passed "$passed_test_count" \
  '{
    schema_version: 1,
    run_id: $run_id,
    commit: $commit,
    status: "passed",
    expected_test_count: $expected,
    passed_test_count: $passed
  }' >"$run_dir/deepseek-moderation-go-contract-summary.json"
chmod 0600 "$run_dir/deepseek-moderation-go-contract-summary.json"

# Build and start the real application against disposable dependencies. The
# generated credential and key-ring never leave the protected work directory.
(
  cd "$repo_root/backend"
  GOMAXPROCS=4 go build -trimpath -o "$work_dir/sub2api" ./cmd/server
)
mkdir -p "$work_dir/app-data" "$work_dir/retry" "$work_dir/emergency"
chmod 0700 "$work_dir/app-data" "$work_dir/retry" "$work_dir/emergency"
mkdir -p "$work_dir/pricing"
install -m 0600 "$repo_root/backend/resources/model-pricing/model_prices_and_context_window.json" \
  "$work_dir/pricing/model_pricing.json"
openssl rand -base64 32 | jq -Rs '{
  current_key_id: "release-test-k1",
  keys: {"release-test-k1": rtrimstr("\n")}
}' >"$work_dir/keyring.json"
chmod 0600 "$work_dir/keyring.json"
openssl rand -hex 24 >"$work_dir/admin-password"
openssl rand -hex 24 | sed 's/^/release-test-channel-/' >"$work_dir/channel-key"
chmod 0600 "$work_dir/admin-password" "$work_dir/channel-key"
admin_email=admin@deepseek-release.invalid
app_port=$(allocate_port)

(
  cd "$work_dir/app-data"
  exec env \
    AUTO_SETUP=true \
    DATA_DIR="$work_dir/app-data" \
    SERVER_HOST=127.0.0.1 \
    SERVER_PORT="$app_port" \
    GIN_MODE=release \
    DATABASE_HOST=127.0.0.1 \
    DATABASE_PORT="$postgres_port" \
    DATABASE_USER=postgres \
    DATABASE_PASSWORD=release-test-database-password \
    DATABASE_DBNAME=sub2api_release \
    DATABASE_SSLMODE=disable \
    REDIS_HOST=127.0.0.1 \
    REDIS_PORT="$redis_port" \
    PRICING_DATA_DIR="$work_dir/pricing" \
    ADMIN_EMAIL="$admin_email" \
    ADMIN_PASSWORD="$(tr -d '\n' <"$work_dir/admin-password")" \
    CONTENT_MODERATION_ARCHIVE_KEY_RING_PATH="$work_dir/keyring.json" \
    CONTENT_MODERATION_ARCHIVE_RETRY_DIR="$work_dir/retry" \
    CONTENT_MODERATION_ARCHIVE_EMERGENCY_DIR="$work_dir/emergency" \
    "$work_dir/sub2api"
) >"$work_dir/app.log" 2>&1 &
app_pid=$!
if ! wait_http "http://127.0.0.1:$app_port/health" "$work_dir/app-health.json"; then
  tail -n 120 "$work_dir/app.log" >&2
  fail "temporary Sub2API process did not become healthy"
fi
jq -e '.status == "ok"' "$work_dir/app-health.json" >/dev/null

jq -n \
  --arg email "$admin_email" \
  --rawfile password "$work_dir/admin-password" \
  '{email: $email, password: ($password | rtrimstr("\n"))}' >"$work_dir/login.json"
chmod 0600 "$work_dir/login.json"
curl --silent --show-error --fail-with-body --max-time 15 \
  --header 'Content-Type: application/json' \
  --data-binary @"$work_dir/login.json" \
  "http://127.0.0.1:$app_port/api/v1/auth/login" >"$work_dir/login-response.json"
jq -e '.code == 0 and (.data.access_token | type == "string" and length > 20)' "$work_dir/login-response.json" >/dev/null
jq -r '.data.access_token | "Authorization: Bearer " + .' "$work_dir/login-response.json" >"$work_dir/admin-auth.header"
chmod 0600 "$work_dir/admin-auth.header"
api_call_count=1

api_request GET /api/v1/admin/compliance '' "$work_dir/compliance-status.json"
jq -e '.code == 0 and .data.required == true and (.data.ack_phrase_en | type == "string" and length > 0)' \
  "$work_dir/compliance-status.json" >/dev/null
jq -n --arg phrase "$(jq -r '.data.ack_phrase_en' "$work_dir/compliance-status.json")" \
  '{phrase:$phrase,language:"en"}' >"$work_dir/compliance-accept.json"
api_request POST /api/v1/admin/compliance/accept "$work_dir/compliance-accept.json" "$work_dir/compliance-accept-response.json"
jq -e '.code == 0 and .data.required == false and (.data.acknowledgement.version | type == "string" and length > 0)' \
  "$work_dir/compliance-accept-response.json" >/dev/null

jq -n \
  --arg stub_url "$stub_url" \
  --rawfile api_key "$work_dir/channel-key" \
  '{
    enabled: true,
    deepseek_enabled: true,
    yufeng_enabled: false,
    deepseek_total_timeout_ms: 10000,
    deepseek_threshold: 0.80,
    first_layer_stage: "shadow",
    second_layer_enabled: true,
    second_layer_stage: "shadow",
    deepseek_channels: [
      {
        id: "primary", name: "Primary", base_url: ($stub_url + "/primary"),
        model: "deepseek-v4-flash", enabled: true, order: 0, timeout_ms: 3000,
        api_key: ($api_key | rtrimstr("\n"))
      },
      {
        id: "backup", name: "Backup", base_url: ($stub_url + "/backup"),
        model: "deepseek-v4-flash", enabled: true, order: 1, timeout_ms: 3000,
        api_key: ($api_key | rtrimstr("\n"))
      }
    ]
  }' >"$work_dir/config-update.json"
chmod 0600 "$work_dir/config-update.json"
api_request PUT /api/v1/admin/risk-control/config "$work_dir/config-update.json" "$work_dir/config-update-response.json"
jq -e '
  .code == 0 and
  .data.deepseek_enabled == true and .data.yufeng_enabled == false and
  .data.first_layer_stage == "shadow" and .data.second_layer_stage == "shadow" and
  (.data.deepseek_channels | length == 2) and
  .data.deepseek_channels[0].id == "primary" and .data.deepseek_channels[1].id == "backup" and
  all(.data.deepseek_channels[]; .api_key_configured == true and (.api_key_masked | type == "string" and length > 0)) and
  ([.data | paths as $path | select(($path[-1] | tostring) == "api_key" or ($path[-1] | tostring) == "api_key_envelope")] | length == 0) and
  (.data | has("base_url") | not) and (.data | has("model") | not) and
  (.data | has("api_keys") | not) and (.data | has("proxy_id") | not)
' "$work_dir/config-update-response.json" >/dev/null

"$pg_bin/psql" -X -At -v ON_ERROR_STOP=1 \
  -h 127.0.0.1 -p "$postgres_port" -U postgres -d sub2api_release \
  -c "SELECT value FROM settings WHERE key = 'content_moderation_config'" >"$work_dir/stored-config.json"
jq -e --rawfile secret "$work_dir/channel-key" '
  (.deepseek_channels | length == 2) and
  all(.deepseek_channels[];
    (.api_key_envelope.domain == "sub2api/content-moderation/deepseek-channel-key") and
    (.api_key_envelope.version == 1) and
    (.api_key_envelope.ciphertext | length > 0) and
    (has("api_key") | not)
  ) and
  ((tostring | contains($secret | rtrimstr("\n"))) | not)
' "$work_dir/stored-config.json" >/dev/null

"$pg_bin/psql" -X -At -v ON_ERROR_STOP=1 \
  -h 127.0.0.1 -p "$postgres_port" -U postgres -d sub2api_release <<'SQL' >"$work_dir/db-check.raw.json"
SELECT json_build_object(
  'columns_present', (
    SELECT COUNT(*) = 6
    FROM information_schema.columns
    WHERE table_schema = 'public'
      AND table_name = 'content_moderation_logs'
      AND column_name IN (
        'deepseek_confidence', 'deepseek_category', 'deepseek_reason',
        'review_outcome', 'reviewer_disagreement', 'review_attempts'
      )
  ),
  'confidence_constraint', EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'content_moderation_logs'::regclass
      AND conname = 'content_moderation_logs_deepseek_confidence_range'
  ),
  'attempts_constraint', EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'content_moderation_logs'::regclass
      AND conname = 'content_moderation_logs_review_attempts_array'
  ),
  'review_outcome_index', to_regclass('public.idx_content_moderation_logs_review_outcome') IS NOT NULL
);
SQL
jq -e '.columns_present and .confidence_constraint and .attempts_constraint and .review_outcome_index' "$work_dir/db-check.raw.json" >/dev/null
if "$pg_bin/psql" -X -v ON_ERROR_STOP=1 \
  -h 127.0.0.1 -p "$postgres_port" -U postgres -d sub2api_release \
  -c "INSERT INTO content_moderation_logs (deepseek_confidence) VALUES (1.01)" \
  >"$work_dir/invalid-confidence.stdout" 2>"$work_dir/invalid-confidence.stderr"; then
  fail "confidence constraint accepted an out-of-range value"
fi
if "$pg_bin/psql" -X -v ON_ERROR_STOP=1 \
  -h 127.0.0.1 -p "$postgres_port" -U postgres -d sub2api_release \
  -c "INSERT INTO content_moderation_logs (review_attempts) VALUES ('{}'::jsonb)" \
  >"$work_dir/invalid-attempts.stdout" 2>"$work_dir/invalid-attempts.stderr"; then
  fail "review_attempts constraint accepted a non-array value"
fi
jq -n \
  --arg run_id "$run_id" \
  --arg commit "$commit" \
  --slurpfile schema "$work_dir/db-check.raw.json" \
  '{
    schema_version: 1,
    run_id: $run_id,
    commit: $commit,
    status: "passed",
    schema: $schema[0],
    negative_fixtures: {
      out_of_range_confidence_rejected: true,
      non_array_attempts_rejected: true
    }
  }' >"$run_dir/deepseek-moderation-database-check.json"
chmod 0600 "$run_dir/deepseek-moderation-database-check.json"

api_request POST /api/v1/admin/risk-control/deepseek/channels/primary/test '' "$work_dir/channel-test-response.json"
jq -e '
  .code == 0 and .data.channel_id == "primary" and
  .data.reachable == true and .data.health_valid == true and
  (.data.latency_ms | type == "number" and . >= 0) and
  (.data | has("safe_case") | not) and (.data | has("risk_case") | not)
' "$work_dir/channel-test-response.json" >/dev/null

jq -n '{first_layer_stage:"enforce"}' >"$work_dir/layer1-enforce.json"
api_request PUT /api/v1/admin/risk-control/config "$work_dir/layer1-enforce.json" "$work_dir/layer1-enforce-response.json"
jq -e '.code == 0 and .data.first_layer_stage == "enforce"' "$work_dir/layer1-enforce-response.json" >/dev/null

jq -n '{first_layer_stage:"shadow",second_layer_stage:"enforce"}' >"$work_dir/layer2-enforce.json"
api_request PUT /api/v1/admin/risk-control/config "$work_dir/layer2-enforce.json" "$work_dir/layer2-enforce-response.json"
jq -e '.code == 0 and .data.first_layer_stage == "shadow" and .data.second_layer_stage == "enforce"' \
  "$work_dir/layer2-enforce-response.json" >/dev/null

jq '{
  second_layer_stage: "shadow",
  deepseek_channels: [.data.deepseek_channels[] | {
    id, name,
    base_url: (if .id == "primary" then (.base_url + "/connectivity-mutated") else .base_url end),
    model,
    enabled, order, timeout_ms
  }]
}' "$work_dir/layer2-enforce-response.json" >"$work_dir/endpoint-change.json"
api_request PUT /api/v1/admin/risk-control/config "$work_dir/endpoint-change.json" "$work_dir/endpoint-change-response.json"
jq -e '.code == 0 and .data.second_layer_stage == "shadow" and .data.deepseek_channels[0].health_status == "untested"' \
  "$work_dir/endpoint-change-response.json" >/dev/null

api_request PUT /api/v1/admin/risk-control/config "$work_dir/layer2-enforce.json" \
  "$work_dir/enforce-after-endpoint-change-response.json"
jq -e '
  .code == 0 and .data.second_layer_stage == "enforce" and
  .data.deepseek_channels[0].health_status == "reachable"
' "$work_dir/enforce-after-endpoint-change-response.json" >/dev/null

jq '{
  first_layer_stage: "shadow",
  second_layer_stage: "shadow",
  deepseek_channels: [.data.deepseek_channels[] | {
    id, name, base_url, model: "deepseek-v4-flash", enabled, order, timeout_ms
  }]
}' "$work_dir/layer2-enforce-response.json" >"$work_dir/final-shadow.json"
api_request PUT /api/v1/admin/risk-control/config "$work_dir/final-shadow.json" "$work_dir/final-shadow-response.json"
jq -e '
  .code == 0 and .data.deepseek_enabled == true and .data.yufeng_enabled == false and
  .data.first_layer_stage == "shadow" and .data.second_layer_stage == "shadow" and
  all(.data.deepseek_channels[]; .api_key_configured == true)
' "$work_dir/final-shadow-response.json" >/dev/null

api_request GET /api/v1/admin/risk-control/config '' "$work_dir/final-config-response.json"
jq -e '
  .code == 0 and
  ([.data | paths as $path | select(($path[-1] | tostring) == "api_key" or ($path[-1] | tostring) == "api_key_envelope")] | length == 0) and
  all(.data.deepseek_channels[]; .api_key_configured == true and (.api_key_masked | length > 0))
' "$work_dir/final-config-response.json" >/dev/null

"$pg_bin/psql" -X -At -v ON_ERROR_STOP=1 \
  -h 127.0.0.1 -p "$postgres_port" -U postgres -d sub2api_release \
  -c "SELECT value FROM settings WHERE key = 'content_moderation_config'" >"$work_dir/final-stored-config.json"
config_digest=$(sha256sum "$work_dir/final-stored-config.json" | awk '{print $1}')
curl --silent --show-error --fail --max-time 5 "$stub_url/stats" >"$work_dir/final-stub-stats.json"
jq -e '.contract_violations == 0 and .requests >= 2' "$work_dir/final-stub-stats.json" >/dev/null
stub_request_count=$(jq '.requests' "$work_dir/final-stub-stats.json")

cleanup_work_dir=$work_dir
cleanup_app_pid=$app_pid
cleanup_stub_pid=$stub_pid
cleanup
trap - EXIT
set -Eeuo pipefail
trap 'on_error "$?" "$LINENO"' ERR
for stopped_pid in "$cleanup_app_pid" "$cleanup_stub_pid"; do
  if [ -n "$stopped_pid" ] && kill -0 "$stopped_pid" 2>/dev/null; then
    fail "release cleanup left child process running: $stopped_pid"
  fi
done
if [ -e "$cleanup_work_dir" ]; then
  fail "release cleanup left the protected work directory behind"
fi
for released_port in "$postgres_port" "$redis_port" "$stub_port" "$app_port"; do
  if ss -H -ltn "sport = :$released_port" | grep -q .; then
    fail "release cleanup left a listener on port $released_port"
  fi
done

worktree_diff_sha256=$(git -C "$repo_root" diff --binary -- . ':(exclude).codex/goals' | sha256sum | awk '{print $1}')
ended_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
candidate_report="$run_dir/.deepseek-moderation-release-report.candidate.json"
jq -n \
  --arg run_id "$run_id" \
  --arg commit "$commit" \
  --arg worktree_diff_sha256 "$worktree_diff_sha256" \
  --arg config_digest "$config_digest" \
  --arg started_at "$started_at" \
  --arg ended_at "$ended_at" \
  --argjson expected_test_count "$expected_test_count" \
  --argjson passed_test_count "$passed_test_count" \
  --argjson api_call_count "$api_call_count" \
  --argjson stub_request_count "$stub_request_count" \
  '{
    schema_version: 1,
    run_id: $run_id,
    commit: $commit,
    worktree_diff_sha256: $worktree_diff_sha256,
    config_digest: $config_digest,
    started_at: $started_at,
    ended_at: $ended_at,
    status: "passed",
    config_summary: {
      model: "deepseek-v4-flash",
      thinking_type: "disabled",
      response_format: "json_object",
      confidence_threshold: 0.80,
      channel_timeout_ms: 3000,
      total_timeout_ms: 10000,
      deepseek_enabled: true,
      yufeng_enabled: false,
      first_layer_stage: "shadow",
      second_layer_stage: "shadow"
    },
    expected_test_count: $expected_test_count,
    passed_test_count: $passed_test_count,
    actual_execution_count: {
      required_go_tests: $expected_test_count,
      passed_root_go_tests: $passed_test_count,
      admin_api_calls: $api_call_count,
      stub_requests: $stub_request_count
    },
    integration: {
      postgres: true,
      redis: true,
      migration: true,
      api_encryption: true,
      api_masking: true,
      connectivity_probe: true,
      layer1_stage: true,
      layer2_stage: true,
      connectivity_digest_invalidation: true,
      cleanup: true
    },
    sensitivity: {
      invalid_report_rejected: true,
      benign_collision_passed: true,
      benign_collision_test: "TestDeepSeekV4FlashBenignCollisionsStayOutOfLayer1"
    },
    exclusions: {
      real_provider_performance: "separate protected-credential gate",
      production_history_replay: "separate production-data gate",
      ten_minute_load_test: "separate load gate",
      browser_acceptance: "separate Playwright gate"
    }
  }' >"$candidate_report"
chmod 0600 "$candidate_report"

if validate_release_report "$invalid_report"; then
  fail "release report validator accepted the known-invalid failure fixture"
fi
validate_release_report "$candidate_report" || fail "final release report failed validation"
jq -e --arg run_id "$run_id" '.run_id == $run_id and .status == "passed"' "$candidate_report" >/dev/null

for evidence_file in \
  "$candidate_report" \
  "$run_dir/deepseek-moderation-go-test-events.jsonl" \
  "$run_dir/deepseek-moderation-go-contract-summary.json" \
  "$run_dir/deepseek-moderation-database-check.json"
do
  [ -s "$evidence_file" ] || fail "release evidence file is empty: $evidence_file"
  [ "$(stat -c '%a' "$evidence_file")" = 600 ] || fail "release evidence file is not mode 0600: $evidence_file"
done

install -m 0600 "$candidate_report" "$report_path"
unlink "$candidate_report"
jq -e --arg run_id "$run_id" '.run_id == $run_id and .status == "passed"' "$report_path" >/dev/null

printf 'DeepSeek moderation release test passed: %s\n' "$report_path"
