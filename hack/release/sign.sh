#!/bin/sh
# Signs bin/kvz for release, with the virtualization entitlement.
#
#   APPLE_SIGNING_IDENTITY  "Developer ID Application: <name> (<team id>)" in a
#                           keychain the agent can read. If unset, the binary is
#                           ad-hoc signed, which works for Virtualization.framework
#                           on the user's Mac.
#   KVZ_NOTARIZE=1          also notarize with Apple (off for now). Needs a
#                           Developer ID identity and APPLE_API_KEY_PATH,
#                           APPLE_API_KEY_ID, APPLE_API_ISSUER.
set -eu
cd "$(dirname "$0")/../.."
bin=${1:-bin/kvz}

if [ -n "${APPLE_SIGNING_IDENTITY:-}" ]; then
  codesign --force --timestamp --options runtime \
    --entitlements kvz.entitlements --sign "$APPLE_SIGNING_IDENTITY" "$bin"
  echo "signed with $APPLE_SIGNING_IDENTITY"
else
  codesign --force --entitlements kvz.entitlements --sign - "$bin"
  echo "ad-hoc signed (no APPLE_SIGNING_IDENTITY)"
fi
codesign --verify --strict --verbose=2 "$bin"

if [ "${KVZ_NOTARIZE:-0}" = 1 ]; then
  : "${APPLE_SIGNING_IDENTITY:?notarization needs a Developer ID identity}"
  : "${APPLE_API_KEY_PATH:?set APPLE_API_KEY_PATH}" "${APPLE_API_KEY_ID:?}" "${APPLE_API_ISSUER:?}"
  zip=$(mktemp -d)/kvz.zip
  ditto -c -k --keepParent "$bin" "$zip"
  xcrun notarytool submit "$zip" --wait \
    --key "$APPLE_API_KEY_PATH" --key-id "$APPLE_API_KEY_ID" --issuer "$APPLE_API_ISSUER"
  # A bare binary can't be stapled; Gatekeeper checks the notarization online.
else
  echo "notarization skipped (set KVZ_NOTARIZE=1 to enable)"
fi
