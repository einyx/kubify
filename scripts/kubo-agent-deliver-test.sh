#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

bash -n "$script_dir/kubo-agent-deliver"

if KUBO_REPOSITORY=https://example.com/not-github.git \
  "$script_dir/kubo-agent-deliver" --smoke 2>/dev/null; then
  echo "expected non-GitHub repository validation to fail" >&2
  exit 1
fi

echo "kubo-agent-deliver validation tests passed"
