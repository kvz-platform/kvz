#!/bin/sh
# Prints the kvz version for this checkout.
#   on a release tag:  v1.35.10-12
#   anywhere else:     v1.35.10-dev+<short sha>
# The Kubernetes part always comes from K8S_VERSION in the Makefile.
set -eu
cd "$(dirname "$0")/.."
k8s=$(sed -n 's/^K8S_VERSION ?= *//p' Makefile)
if tag=$(git describe --tags --exact-match --match "${k8s}-*" 2>/dev/null); then
  echo "$tag"
else
  echo "${k8s}-dev+$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
fi
