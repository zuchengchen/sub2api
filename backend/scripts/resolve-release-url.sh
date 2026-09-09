#!/bin/sh
set -eu

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
BACKEND_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"
REPO_DIR="$(CDPATH= cd -- "$BACKEND_DIR/.." && pwd)"
ORIGIN_RELEASE_BASE="https://github.com/zuchengchen/sub2api/releases/tag"

# Only tagged czc-* checkouts represent an immutable customized release page.
# Source / untagged builds print nothing so the admin UI does not claim an
# older Wei-Shaw GitHub release as "this build".
if command -v git >/dev/null 2>&1; then
  TAG="$(git -C "$REPO_DIR" describe --tags --exact-match --match 'czc-*' 2>/dev/null || true)"
  if [ -n "$TAG" ]; then
    printf '%s/%s\n' "$ORIGIN_RELEASE_BASE" "$TAG"
    exit 0
  fi
fi

printf '\n'
