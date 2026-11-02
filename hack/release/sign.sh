#!/bin/sh
# Signs bin/kvz with a Developer ID certificate (hardened runtime plus the
# virtualization entitlement) and notarizes it with Apple.
#
# Needs, usually from Buildkite secrets:
#   APPLE_SIGNING_IDENTITY  "Developer ID Application: <name> (<team id>)",
#                           in a keychain the agent can read
#   APPLE_API_KEY_PATH      App Store Connect API key (.p8) for notarytool
#   APPLE_API_KEY_ID        its key ID
#   APPLE_API_ISSUER        its issuer ID
set -eu
cd "$(dirname "$0")/../.."
bin=${1:-bin/kvz}
: "${APPLE_SIGNING_IDENTITY:?set APPLE_SIGNING_IDENTITY}"

codesign --force --timestamp --options runtime \
  --entitlements kvz.entitlements --sign "$APPLE_SIGNING_IDENTITY" "$bin"
codesign --verify --strict --verbose=2 "$bin"

if [ -n "${APPLE_API_KEY_PATH:-}" ]; then
  zip=$(mktemp -d)/kvz.zip
  ditto -c -k --keepParent "$bin" "$zip"
  xcrun notarytool submit "$zip" --wait \
    --key "$APPLE_API_KEY_PATH" --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_ISSUER"
  # A bare binary can't be stapled; Gatekeeper checks the notarization online.
else
  echo "APPLE_API_KEY_PATH not set: signed but not notarized" >&2
fi
