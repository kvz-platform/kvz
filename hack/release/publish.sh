#!/bin/sh
# Creates the GitHub release for a tag and uploads dist/*.  Needs GITHUB_TOKEN.
#   usage: publish.sh <tag>
set -eu
cd "$(dirname "$0")/../.."
tag=${1:?usage: publish.sh <tag>}
notes=$(mktemp)
hack/release/notes.sh "$tag" > "$notes"
gh release create "$tag" dist/kvz-"$tag"-darwin-arm64.tar.gz dist/kvz-"$tag"-darwin-arm64.tar.gz.sha256 \
  --repo kvz-platform/kvz --title "kvz $tag" --notes-file "$notes" --verify-tag
