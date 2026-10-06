#!/usr/bin/env bash
set -euo pipefail

# Sync the framework-neutral MeshX token artifact into Kubo's embedded portal.
# Usage: hack/sync-design-system.sh /path/to/meshx-design-system

source_dir="${1:-../meshx-design-system}"
target_dir="internal/portal/assets"

if [[ ! -f "$source_dir/build/css/embedded.css" ]]; then
  echo "missing $source_dir/build/css/embedded.css; run npm ci && npm run build:tokens in meshx-design-system" >&2
  exit 1
fi

mkdir -p "$target_dir/fonts"
cp "$source_dir/build/css/embedded.css" "$target_dir/meshx-design-system.css"
cp "$source_dir"/public/fonts/*.woff2 "$target_dir/fonts/"

python3 - "$target_dir/meshx-design-system.css" <<'PY'
from pathlib import Path
import sys

path = Path(sys.argv[1])
css = path.read_text()
css = css.replace("../../public/fonts/", "/assets/fonts/")
banner = (
    "/* Vendored from meshxdata/meshx-design-system.\n"
    "   Regenerate with hack/sync-design-system.sh; do not edit directly. */\n"
)
path.write_text(banner + css)
PY

echo "Synced MeshX design tokens and fonts into $target_dir"
