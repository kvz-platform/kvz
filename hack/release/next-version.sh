#!/bin/sh
# Prints the next release version: v<kubernetes version>-<release number>.
# The release number is one more than the highest number on any existing
# release tag, across all Kubernetes versions, so it never repeats or resets.
set -eu
cd "$(dirname "$0")/../.."
git fetch --tags --quiet origin 2>/dev/null || true
k8s=$(sed -n 's/^K8S_VERSION ?= *//p' Makefile)
last=$(git tag --list 'v*.*.*-*' | sed -n 's/^v[0-9]*\.[0-9]*\.[0-9]*-\([0-9][0-9]*\)$/\1/p' | sort -n | tail -1)
echo "${k8s}-$(( ${last:-0} + 1 ))"
