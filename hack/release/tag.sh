#!/bin/sh
# Creates and pushes the next release tag from the current commit on main.
# Pushing the tag starts the release build in Buildkite.
set -eu
cd "$(dirname "$0")/../.."
tag=$(hack/release/next-version.sh)
git tag -a "$tag" -m "kvz $tag"
git push origin "$tag"
echo "tagged $tag"
