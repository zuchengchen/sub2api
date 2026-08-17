#!/usr/bin/env bash
set -Eeuo pipefail

run_dir=${1:-}
if [ "$#" -gt 0 ]; then
  shift
fi
report_path=
work_dir=
started_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
run_id=$(date -u +%Y%m%dT%H%M%SZ)-$$
repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
if ! commit=$(git -C "$repo_root" rev-parse HEAD 2>/dev/null); then
  commit=unknown
fi
generic_pattern='\bsk-[A-Za-z0-9_-]{24,}\b'

cleanup() {
  trap - ERR
  set +e
  if [ -n "$work_dir" ] && [ -d "$work_dir" ]; then
    case "$work_dir" in
      /tmp/sub2api-deepseek-secret-scan-work.*) rm -rf -- "$work_dir" ;;
    esac
  fi
}
trap cleanup EXIT

fail() {
  local reason=$1
  if [ -n "$report_path" ] && [ ! -e "$report_path" ]; then
    jq -n \
      --arg started_at "$started_at" \
      --arg ended_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
      --arg run_id "$run_id" \
      --arg commit "$commit" \
      --arg reason "$reason" \
      '{
        schema_version:1,status:"failed",run_id:$run_id,commit:$commit,
        started_at:$started_at,ended_at:$ended_at,reason:$reason,
        configuration:{generic_pattern:"deepseek_key_v1",exact_match:true,binary_scan:true},
        target_count:0
      }' \
      >"$report_path"
    chmod 0600 "$report_path"
  fi
  printf 'DeepSeek moderation secret scan failed: %s\n' "$reason" >&2
  exit 1
}

scan_regex() {
  local target=$1
  local output=$2
  local status
  set +e
  rg --hidden --no-ignore -a -l --regexp "$generic_pattern" -- "$target" >"$output" 2>/dev/null
  status=$?
  set -e
  case "$status" in
    0|1) return "$status" ;;
    *) fail "generic scanner could not inspect a target" ;;
  esac
}

scan_exact() {
  local pattern_file=$1
  local target=$2
  local output=$3
  local status
  set +e
  rg --hidden --no-ignore -a -F -l -f "$pattern_file" -- "$target" >"$output" 2>/dev/null
  status=$?
  set -e
  case "$status" in
    0|1) return "$status" ;;
    *) fail "exact scanner could not inspect a target" ;;
  esac
}

[ -n "$run_dir" ] || fail "usage: $0 RUN_DIR TARGET..."
[ "$#" -gt 0 ] || fail "at least one scan target is required"
case "$run_dir" in
  /tmp/sub2api-deepseek-secret-scan.*) ;;
  *) fail "RUN_DIR must match /tmp/sub2api-deepseek-secret-scan.*" ;;
esac
for command_name in chmod date git jq mkdir mktemp readlink rg rm stat wc; do
  command -v "$command_name" >/dev/null 2>&1 || fail "required command is unavailable: $command_name"
done
mkdir -p "$run_dir"
chmod 0700 "$run_dir"
[ "$(stat -c '%a' "$run_dir")" = 700 ] || fail "RUN_DIR must have mode 0700"
report_path="$run_dir/deepseek-moderation-secret-scan-report.json"
[ ! -e "$report_path" ] || fail "secret scan report already exists in RUN_DIR"
work_dir=$(mktemp -d /tmp/sub2api-deepseek-secret-scan-work.XXXXXX)
chmod 0700 "$work_dir"

exact_pattern_file=${SUB2API_DEEPSEEK_SECRET_PATTERN_FILE:-/proc/self/fd/3}
[ -r "$exact_pattern_file" ] || fail "protected exact-key pattern descriptor is unavailable"
pattern_bytes=$(wc -c <"$exact_pattern_file")
[ "$pattern_bytes" -gt 0 ] && [ "$pattern_bytes" -le 16384 ] || fail "protected exact-key pattern size is invalid"

canary='sk-'
canary+='CANARY0123456789abcdefghijklmnop'
printf '%s\n' "$canary" >"$work_dir/canary-target"
printf '%s\n' "$canary" >"$work_dir/canary-pattern"
chmod 0600 "$work_dir/canary-target" "$work_dir/canary-pattern"
if scan_regex "$work_dir/canary-target" "$work_dir/canary-generic-findings"; then
  generic_canary_sensitive=true
else
  generic_canary_sensitive=false
fi
if scan_exact "$work_dir/canary-pattern" "$work_dir/canary-target" "$work_dir/canary-exact-findings"; then
  exact_canary_sensitive=true
else
  exact_canary_sensitive=false
fi
[ "$generic_canary_sensitive" = true ] || fail "generic scanner did not reject the fake key canary"
[ "$exact_canary_sensitive" = true ] || fail "exact scanner did not reject the fake key canary"

target_count=0
generic_findings=0
exact_findings=0
target_manifest="$work_dir/targets.jsonl"
: >"$target_manifest"
chmod 0600 "$target_manifest"
for target in "$@"; do
  [ -e "$target" ] || fail "a scan target does not exist"
  target_count=$((target_count + 1))
  resolved=$(readlink -f -- "$target")
  generic_output="$work_dir/generic-$target_count"
  exact_output="$work_dir/exact-$target_count"
  if scan_regex "$target" "$generic_output"; then
    generic_findings=$((generic_findings + $(wc -l <"$generic_output")))
  fi
  if scan_exact "$exact_pattern_file" "$target" "$exact_output"; then
    exact_findings=$((exact_findings + $(wc -l <"$exact_output")))
  fi
  jq -nc --arg path "$resolved" '{path:$path}' >>"$target_manifest"
done

status=passed
if [ "$generic_findings" -ne 0 ] || [ "$exact_findings" -ne 0 ]; then
  status=failed
fi
jq -s \
  --arg status "$status" \
  --arg run_id "$run_id" \
  --arg commit "$commit" \
  --arg started_at "$started_at" \
  --arg ended_at "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
  --argjson target_count "$target_count" \
  --argjson generic_findings "$generic_findings" \
  --argjson exact_findings "$exact_findings" \
  '{
    schema_version: 1,
    status: $status,
    run_id: $run_id,
    commit: $commit,
    started_at: $started_at,
    ended_at: $ended_at,
    configuration: {generic_pattern:"deepseek_key_v1",exact_match:true,binary_scan:true},
    canary: {generic_rejected: true, exact_rejected: true},
    exact_pattern_source: "protected_descriptor",
    target_count: $target_count,
    targets: .,
    findings: {generic_file_count: $generic_findings, exact_file_count: $exact_findings}
  }' "$target_manifest" >"$report_path"
chmod 0600 "$report_path"

[ "$status" = passed ] || fail "one or more scan targets contain a potential credential"
printf 'DeepSeek moderation secret scan passed: %s\n' "$report_path"
