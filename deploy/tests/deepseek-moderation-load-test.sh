#!/usr/bin/env bash
set -Eeuo pipefail

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
run_dir=${1:-}
report_path=
work_dir=
stub_pid=
stub_port=
candidate_report=
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
commit=$(git -C "$repo_root" rev-parse HEAD)

write_failure_report() {
  local reason=$1
  if [ -z "$report_path" ] || [ -e "$report_path" ] || ! command -v jq >/dev/null 2>&1; then
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
      reason: $reason
    }' >"$report_path"
  chmod 0600 "$report_path"
}

fail() {
  local reason=$1
  write_failure_report "$reason"
  printf 'DeepSeek moderation load gate failed: %s\n' "$reason" >&2
  exit 1
}

on_error() {
  local exit_code=$1
  local line=$2
  trap - ERR
  write_failure_report "unexpected command failure at line $line (exit $exit_code)"
  printf 'DeepSeek moderation load gate failed at line %s (exit %s)\n' "$line" "$exit_code" >&2
  exit "$exit_code"
}
trap 'on_error "$?" "$LINENO"' ERR

stop_pid() {
  local pid=${1:-}
  [ -n "$pid" ] || return 0
  kill -0 "$pid" 2>/dev/null || return 0
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 50); do
    if ! kill -0 "$pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  if kill -0 "$pid" 2>/dev/null; then
    kill -KILL "$pid" 2>/dev/null || true
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
  stop_pid "$stub_pid"
  if [ -n "$work_dir" ] && [ -d "$work_dir" ]; then
    case "$work_dir" in
      /tmp/sub2api-deepseek-load-work.*) rm -rf -- "$work_dir" ;;
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

validate_candidate_report() {
  local file=$1
  local allow_short=$2
  jq -e --arg allow_short "$allow_short" '
    .schema_version == 1 and
    .status == "passed" and
    (.commit | test("^[0-9a-f]{40}$")) and
    .configuration.model == "deepseek-v4-flash" and
    .configuration.thinking_type == "disabled" and
    .configuration.response_format == "json_object" and
    .configuration.first_layer_stage == "shadow" and
    .configuration.second_layer_stage == "shadow" and
    .configuration.deepseek_enabled == true and
    .configuration.yufeng_enabled == false and
    .configuration.gomaxprocs == 28 and
    .configuration.target_reviews_per_minute >= 440 and
    .configuration.evidence_mode == "unique_per_request" and
    .configuration.dedupe_probe_reviews == 80 and
    .throughput.minimum_reviews_per_minute == 440 and
    .throughput.completed_reviews_per_minute >= 440 and
    .counts.scheduled > 0 and
    .counts.allowed_synchronously == .counts.scheduled and
    .counts.shadow_submitted == .counts.scheduled and
    .counts.shadow_completed == .counts.scheduled and
    .counts.shadow_in_flight_at_end == 0 and
    .counts.audit_logs == .counts.scheduled and
    .counts.unique_request_ids == .counts.scheduled and
    .counts.expected_unique_evidence == .counts.scheduled and
    .counts.unique_input_hashes == .counts.expected_unique_evidence and
    .counts.production_path_records == .counts.scheduled and
    .counts.model_review_records == .counts.expected_unique_evidence and
    .counts.cache_hit_records == 0 and
    .counts.cache_hits == .counts.cache_hit_records and
    .counts.cache_misses == .counts.expected_unique_evidence and
    .counts.cache_writes == .counts.expected_unique_evidence and
    .counts.cache_errors == 0 and
    .counts.stub_requests == .counts.expected_unique_evidence and
    .counts.stub_channel_calls == .counts.expected_unique_evidence and
    .counts.contract_violations == 0 and
    .counts.dedupe_probe_scheduled == .configuration.dedupe_probe_reviews and
    .counts.dedupe_probe_allowed == .counts.dedupe_probe_scheduled and
    .counts.dedupe_probe_audits == .counts.dedupe_probe_scheduled and
    .counts.dedupe_probe_model_calls == 1 and
    .counts.dedupe_probe_cache_hits == (.counts.dedupe_probe_scheduled - 1) and
    .counts.dedupe_probe_cache_misses == 1 and
    .counts.dedupe_probe_cache_writes == 1 and
    .counts.dedupe_probe_cache_errors == 0 and
    .stub.instance_id_before != "" and
    .stub.instance_id_before == .stub.instance_id_after and
    .stub.started_at_before != "" and
    .stub.started_at_before == .stub.started_at_after and
    .stub.max_active >= 2 and
    .resources.goroutines_end <= .resources.goroutines_limit and
    .resources.heap_end_bytes <= .resources.heap_limit_bytes and
    .resources.rss_end_bytes <= .resources.rss_limit_bytes and
    .resources.rss_tail_spread_bytes <= 16777216 and
    .checks.sustained_dispatch == true and
    .checks.minimum_throughput == true and
    .checks.exact_count == true and
    .checks.exact_deduplication == true and
    .checks.different_evidence_concurrent == true and
    .checks.production_path == true and
    .checks.no_deadlock == true and
    .checks.no_restart == true and
    .checks.contract_valid == true and
    .checks.goroutines_bounded == true and
    .checks.heap_bounded == true and
    .checks.rss_stable == true and
    .checks.gomaxprocs_correct == true and
    (
      (.mode == "production" and .configuration.requested_duration_seconds >= 600 and .checks.production_duration == true) or
      (.mode == "focused" and $allow_short == "true" and .configuration.requested_duration_seconds < 600 and .checks.production_duration == false)
    )
  ' "$file" >/dev/null
}

[ -n "$run_dir" ] || fail "usage: $0 RUN_DIR"
case "$run_dir" in
  /tmp/sub2api-deepseek-load.*) ;;
  *) fail "RUN_DIR must match /tmp/sub2api-deepseek-load.*" ;;
esac
mkdir -p "$run_dir"
chmod 0700 "$run_dir"
[ "$(stat -c '%a' "$run_dir")" = 700 ] || fail "RUN_DIR must have mode 0700"
report_path="$run_dir/deepseek-moderation-load-report.json"
[ ! -e "$report_path" ] || fail "load report already exists in RUN_DIR"
work_dir=$(mktemp -d /tmp/sub2api-deepseek-load-work.XXXXXX)
chmod 0700 "$work_dir"

for command_name in chmod curl date git go grep install jq kill mkdir mktemp mv rm seq shuf ss stat tar; do
  require_command "$command_name"
done

fixture_dir="$repo_root/deploy/tests/fixtures"
stub_source="$fixture_dir/deepseek-moderation-stub.go"
load_test_source="$fixture_dir/deepseek-moderation-load-gate_test.go"
for required_file in "$stub_source" "$load_test_source"; do
  [ -s "$required_file" ] || fail "required load fixture is missing: $required_file"
done

load_duration=${SUB2API_DEEPSEEK_LOAD_DURATION:-10m}
load_rate=${SUB2API_DEEPSEEK_LOAD_RATE_PER_MINUTE:-480}
allow_short=${SUB2API_DEEPSEEK_LOAD_ALLOW_SHORT:-false}
drain_timeout=${SUB2API_DEEPSEEK_LOAD_DRAIN_TIMEOUT:-2m}
stub_delay_ms=${SUB2API_DEEPSEEK_LOAD_STUB_DELAY_MS:-250}
case "$allow_short" in
  true|false) ;;
  *) fail "SUB2API_DEEPSEEK_LOAD_ALLOW_SHORT must be true or false" ;;
esac
case "$load_rate" in
  ''|*[!0-9]*) fail "SUB2API_DEEPSEEK_LOAD_RATE_PER_MINUTE must be an integer" ;;
esac
case "$stub_delay_ms" in
  ''|*[!0-9]*) fail "SUB2API_DEEPSEEK_LOAD_STUB_DELAY_MS must be an integer" ;;
esac
[ "$load_rate" -ge 440 ] && [ "$load_rate" -le 5000 ] || fail "review rate must be between 440 and 5000 per minute"
[ "$stub_delay_ms" -ge 250 ] && [ "$stub_delay_ms" -le 2500 ] || fail "stub delay must be between 250ms and 2500ms"

stub_port=$(allocate_port)
GOMAXPROCS=28 go build -o "$work_dir/deepseek-moderation-stub" "$stub_source"
chmod 0700 "$work_dir/deepseek-moderation-stub"
"$work_dir/deepseek-moderation-stub" -listen "127.0.0.1:$stub_port" >"$work_dir/stub.log" 2>&1 &
stub_pid=$!
printf '%s\n' "$stub_pid" >"$work_dir/stub.pid"
chmod 0600 "$work_dir/stub.pid" "$work_dir/stub.log"
wait_http "http://127.0.0.1:$stub_port/health" "$work_dir/stub-health.json" || fail "moderation stub did not become healthy"

jq -n --argjson delay_ms "$stub_delay_ms" '{channel:"load", mode:"safe", delay_ms:$delay_ms}' >"$work_dir/stub-control.json"
chmod 0600 "$work_dir/stub-control.json"
curl --silent --show-error --fail-with-body --max-time 5 \
  --header 'Content-Type: application/json' \
  --data-binary @"$work_dir/stub-control.json" \
  "http://127.0.0.1:$stub_port/control" >"$work_dir/stub-control-response.json"
chmod 0600 "$work_dir/stub-control-response.json"

mkdir -m 0700 "$work_dir/backend"
tar --exclude='./internal/service/risk_review_*_temp_test.go' -C "$repo_root/backend" -cf - . | tar -C "$work_dir/backend" -xf -
install -m 0600 "$load_test_source" "$work_dir/backend/internal/service/deepseek_moderation_load_gate_test.go"
candidate_report="$work_dir/deepseek-moderation-load-result.json"
go_test_log="$work_dir/go-test.log"

if (
  cd "$work_dir/backend"
  GOMAXPROCS=28 \
  SUB2API_DEEPSEEK_LOAD_STUB_URL="http://127.0.0.1:$stub_port" \
  SUB2API_DEEPSEEK_LOAD_REPORT="$candidate_report" \
  SUB2API_DEEPSEEK_LOAD_COMMIT="$commit" \
  SUB2API_DEEPSEEK_LOAD_DURATION="$load_duration" \
  SUB2API_DEEPSEEK_LOAD_RATE_PER_MINUTE="$load_rate" \
  SUB2API_DEEPSEEK_LOAD_ALLOW_SHORT="$allow_short" \
  SUB2API_DEEPSEEK_LOAD_DRAIN_TIMEOUT="$drain_timeout" \
    go test -p 2 -count=1 -timeout 20m ./internal/service -run '^TestDeepSeekModerationSustainedLoadGate$'
) >"$go_test_log" 2>&1; then
  go_test_status=0
else
  go_test_status=$?
fi
chmod 0600 "$go_test_log"
install -m 0600 "$go_test_log" "$run_dir/deepseek-moderation-load-go-test.log"
if [ "$go_test_status" -ne 0 ]; then
  if [ -s "$candidate_report" ]; then
    install -m 0600 "$candidate_report" "$run_dir/deepseek-moderation-load-failed-result.json"
  fi
  fail "load Go test failed (exit $go_test_status)"
fi
[ -s "$candidate_report" ] || fail "load Go test did not produce a result report"
chmod 0600 "$candidate_report"
validate_candidate_report "$candidate_report" "$allow_short" || fail "load result report failed strict validation"
kill -0 "$stub_pid" 2>/dev/null || fail "moderation stub exited or restarted during the load run"

curl --silent --show-error --fail --max-time 10 \
  "http://127.0.0.1:$stub_port/stats" >"$work_dir/final-stub-stats.json"
chmod 0600 "$work_dir/final-stub-stats.json"
jq -e --slurpfile stats "$work_dir/final-stub-stats.json" '
  .stub.instance_id_after == $stats[0].instance_id and
  .stub.started_at_after == $stats[0].started_at and
  $stats[0].active == 0 and
  $stats[0].contract_violations == 0
' "$candidate_report" >/dev/null || fail "stub identity or final state changed after the load run"

candidate_copy="$run_dir/.deepseek-moderation-load-candidate.json"
install -m 0600 "$candidate_report" "$candidate_copy"
stub_pid_before_cleanup=$stub_pid
stub_port_before_cleanup=$stub_port
work_dir_before_cleanup=$work_dir
cleanup
set -e
trap 'on_error "$?" "$LINENO"' ERR
stub_pid=

if kill -0 "$stub_pid_before_cleanup" 2>/dev/null; then
  fail "moderation stub process survived cleanup"
fi
if ss -H -ltn "sport = :$stub_port_before_cleanup" | grep -q .; then
  fail "moderation stub port remained open after cleanup"
fi
[ ! -d "$work_dir_before_cleanup" ] || fail "isolated load work directory survived cleanup"

final_tmp="$run_dir/.deepseek-moderation-load-report.tmp"
jq \
  --arg run_id "$run_id" \
  --arg shell_started_at "$started_at" \
  --arg shell_ended_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson stub_pid "$stub_pid_before_cleanup" \
  '. + {
    run_id: $run_id,
    shell_started_at: $shell_started_at,
    shell_ended_at: $shell_ended_at,
    cleanup: {
      verified: true,
      stub_process_stopped: true,
      stub_port_closed: true,
      work_directory_removed: true
    },
    stub: (.stub + {pid: $stub_pid})
  }' "$candidate_copy" >"$final_tmp"
chmod 0600 "$final_tmp"
mv "$final_tmp" "$report_path"
rm -f -- "$candidate_copy"

jq -e '
  .status == "passed" and
  .cleanup.verified == true and
  .cleanup.stub_process_stopped == true and
  .cleanup.stub_port_closed == true and
  .cleanup.work_directory_removed == true
' "$report_path" >/dev/null || fail "final report did not retain cleanup proof"

printf 'DeepSeek moderation load gate passed (%s mode): %s\n' "$(jq -r .mode "$report_path")" "$report_path"
