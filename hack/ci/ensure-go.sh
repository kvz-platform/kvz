#!/bin/sh
# Makes sure a Go toolchain is on PATH (hosted Buildkite Macs may not have one).
# Go downloads the exact version go.mod asks for by itself (GOTOOLCHAIN=auto).
set -eu
if ! command -v go >/dev/null 2>&1; then
  brew install --quiet go
fi
go version
