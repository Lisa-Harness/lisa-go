#!/usr/bin/env bash
# Runs the Python ↔ Go conformance suite against a canonical Lisa checkout.
#
# Usage:
#   scripts/conformance.sh [LISA_DIR]
#
# LISA_DIR defaults to $LISA_DIR, then ../lisa. The checkout's uv workspace is
# synced from its lockfile (skip with LISA_SKIP_SYNC=1 when it is already
# provisioned). Every parity test is required: a skip fails the run, so a
# misconfigured environment can never pass as green.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
lisa_dir="${1:-${LISA_DIR:-$repo_root/../lisa}}"
lisa_dir="$(cd "$lisa_dir" && pwd)"
cd "$repo_root"

if [[ "${LISA_SKIP_SYNC:-}" != "1" ]]; then
  (cd "$lisa_dir" && uv sync --frozen)
fi

python="$lisa_dir/.venv/bin/python"
if [[ ! -x "$python" ]]; then
  python="$lisa_dir/.venv/Scripts/python.exe"
fi
if [[ ! -x "$python" ]]; then
  echo "conformance: no Python environment under $lisa_dir/.venv" >&2
  exit 1
fi

echo "conformance: canonical lisa $(git -C "$lisa_dir" rev-parse HEAD 2>/dev/null || echo unknown) at $lisa_dir"
echo "conformance: lisa-go $(git rev-parse HEAD)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

# require_no_skips runs go test verbosely and fails if any test skipped.
require_no_skips() {
  local log="$work/go-test.log"
  local status=0
  go test -count=1 -v "$@" >"$log" 2>&1 || status=$?
  cat "$log"
  if grep -q -- '--- SKIP' "$log"; then
    echo "conformance: a required test was skipped" >&2
    return 1
  fi
  return "$status"
}

status=0

echo "::group::memory store crosscheck"
"$python" connector/memory/py_probe.py "$work/memory.json"
LISA_MEMORY_CROSSCHECK_FILE="$work/memory.json" \
  require_no_skips ./connector/memory/ -run '^TestGoMemoryCrossCheck$' || status=1
echo "::endgroup::"

echo "::group::stdio server conformance"
LISA_CONFORMANCE_PYTHON="$python" LISA_CONFORMANCE_REQUIRED=1 \
  require_no_skips ./conformance/ || status=1
echo "::endgroup::"

exit "$status"
