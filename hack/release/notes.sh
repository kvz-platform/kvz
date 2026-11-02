#!/bin/sh
# Release notes in Markdown from Conventional Commit subjects since the
# previous release tag.  usage: notes.sh <new tag>
set -eu
cd "$(dirname "$0")/../.."
new=$1
prev=$(git tag --list 'v*.*.*-*' --sort=-v:refname | grep -v "^${new}$" | head -1 || true)
range=${prev:+$prev..}HEAD
k8s=$(sed -n 's/^K8S_VERSION ?= *//p' Makefile)

echo "kvz ${new} embeds Kubernetes ${k8s}."
echo
echo "\`\`\`sh"
echo "brew install kvz-platform/tap/kvz"
echo "\`\`\`"
section() { # title, grep pattern
  lines=$(git log --no-merges --format='%s (%h)' "$range" | grep -E "$2" || true)
  [ -z "$lines" ] && return 0
  printf '\n## %s\n\n' "$1"
  echo "$lines" | sed -E 's/^[a-z]+(\([^)]*\))?!?: /- /'
}
section "Breaking changes" '^[a-z]+(\([^)]*\))?!: '
section "Features" '^feat(\([^)]*\))?: '
section "Fixes" '^fix(\([^)]*\))?: '
section "Performance" '^perf(\([^)]*\))?: '
section "Documentation" '^docs(\([^)]*\))?: '
section "Other changes" '^(refactor|build|ci|chore|test|style|revert)(\([^)]*\))?: '
[ -n "$prev" ] && printf '\n**Full changelog:** https://github.com/kvz-platform/kvz/compare/%s...%s\n' "$prev" "$new"
exit 0
