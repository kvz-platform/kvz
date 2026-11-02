#!/bin/sh
# Writes Formula/kvz.rb for a release into kvz-platform/homebrew-tap and pushes.
# Needs HOMEBREW_TAP_TOKEN: a token with contents:write on the tap repo.
#   usage: update-tap.sh <tag>
set -eu
cd "$(dirname "$0")/../.."
tag=${1:?usage: update-tap.sh <tag>}
sha=$(cut -d' ' -f1 "dist/kvz-${tag}-darwin-arm64.tar.gz.sha256")
: "${HOMEBREW_TAP_TOKEN:?set HOMEBREW_TAP_TOKEN}"

work=$(mktemp -d)
git clone --quiet --depth 1 "https://x-access-token:${HOMEBREW_TAP_TOKEN}@github.com/kvz-platform/homebrew-tap.git" "$work"
mkdir -p "$work/Formula"
sed -e "s/@VERSION@/${tag#v}/g" -e "s/@TAG@/${tag}/g" -e "s/@SHA256@/${sha}/g" \
  packaging/homebrew/kvz.rb.tmpl > "$work/Formula/kvz.rb"
cd "$work"
git add Formula/kvz.rb
git -c user.name="kvz release bot" -c user.email="release-bot@users.noreply.github.com" \
  commit --quiet -m "kvz ${tag}"
git push --quiet origin HEAD
echo "tap updated to ${tag}"
