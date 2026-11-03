#!/bin/sh
# Writes llms-full.txt: kvz's setup and reference docs in one file, for AI
# agents. It is published at https://kvz.systems/llms-full.txt (repo
# kvz-platform/kvz.systems); regenerate it whenever these docs change.
#   usage: hack/llms-full.sh [output]   (default: stdout)
set -eu
cd "$(dirname "$0")/.."
out=${1:-/dev/stdout}
{
  echo "# kvz: full documentation"
  echo
  echo "> Every kvz setup and reference document in one file, generated from"
  echo "> https://github.com/kvz-platform/kvz at commit $(git rev-parse --short HEAD)."
  echo "> The index with links is https://kvz.systems/llms.txt."
  for f in README.md docs/fleet.md docs/getting-started.md docs/operations.md docs/upgrades.md \
           docs/runtimes.md docs/kubernetes-features.md docs/ARCHITECTURE.md SECURITY.md; do
    echo
    echo "---"
    echo
    echo "<!-- source: https://github.com/kvz-platform/kvz/blob/main/$f -->"
    echo
    cat "$f"
  done
} > "$out"
