#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

fail() {
  printf 'YuFeng runtime config test failed: %s\n' "$1" >&2
  exit 1
}

assert_exact_line() {
  file=$1
  line=$2
  count=$(grep -Fxc -- "$line" "$file" || true)
  [ "$count" -eq 1 ] || fail "$file must contain exactly once: $line"
}

assert_absent() {
  file=$1
  text=$2
  if grep -Fq -- "$text" "$file"; then
    fail "$file must not contain: $text"
  fi
}

launcher=scripts/content-moderation/run-yufeng-llama.sh
service=deploy/systemd/sub2api-yufeng-xguard.service.example
runbook=docs/runbooks/content-moderation-yufeng.md

assert_exact_line "$launcher" '  --cache-ram 8192 \'
assert_exact_line "$launcher" '  --cache-prompt \'
assert_absent "$launcher" '--cache-ram 0'
assert_absent "$launcher" '--no-cache-prompt'

assert_exact_line "$service" 'MemoryHigh=11G'
assert_exact_line "$service" 'MemoryMax=12G'
assert_absent "$service" 'MemoryHigh=3G'
assert_absent "$service" 'MemoryMax=4G'

grep -Fq -- '--cache-ram 8192 --cache-prompt' "$runbook" || \
  fail 'runbook must document the enabled 8 GiB prompt cache'
grep -Fq -- '`MemoryHigh=11G` and `MemoryMax=12G`' "$runbook" || \
  fail 'runbook must document the YuFeng service memory limits'

printf 'YuFeng runtime config test passed\n'
