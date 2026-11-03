BIN ?= bin/kvz
KVZ_VERSION ?= $(shell hack/version.sh)
GOFLAGS ?= -trimpath
K8S_VERSION ?= v1.37.2
KVZ_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VPKG := k8s.io/component-base/version
CPKG := k8s.io/client-go/pkg/version
VFLAGS := -X $(VPKG).gitVersion=$(K8S_VERSION)+kvz -X $(VPKG).gitMajor=1 -X $(VPKG).gitMinor=37 \
	-X $(VPKG).gitCommit=$(KVZ_COMMIT) -X $(VPKG).gitTreeState=clean -X $(VPKG).buildDate=$(BUILD_DATE) \
	-X $(CPKG).gitVersion=$(K8S_VERSION)+kvz -X $(CPKG).gitMajor=1 -X $(CPKG).gitMinor=37 \
	-X $(CPKG).gitCommit=$(KVZ_COMMIT) -X $(CPKG).gitTreeState=clean -X $(CPKG).buildDate=$(BUILD_DATE)
LDFLAGS ?= -s -w $(VFLAGS) -X github.com/GrowlyX/kvz/pkg/version.Release=$(KVZ_VERSION)
INIT := pkg/runtime/microvmrt/kvz-init

.PHONY: build init bench test lint e2e dist version clean
build: init
	CGO_ENABLED=1 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kvz
	codesign --force --entitlements kvz.entitlements -s - $(BIN)

# guest PID 1, embedded into kvz
init:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(INIT) ./cmd/kvz-init

bench: init
	go build $(GOFLAGS) -o bin/kvzbench ./cmd/kvzbench
	go build $(GOFLAGS) -o bin/vmbench ./cmd/vmbench && codesign --force --entitlements kvz.entitlements -s - bin/vmbench

# unit tests and static checks (no VMs needed)
test: init
	CGO_ENABLED=1 go test ./...

lint: init
	@test -z "$$(gofmt -l cmd pkg test)" || (echo "gofmt needed:"; gofmt -l cmd pkg test; exit 1)
	CGO_ENABLED=1 go vet ./...
	@go mod tidy -diff >/dev/null || (echo "go.mod/go.sum not tidy: run go mod tidy"; exit 1)
	@go list -m all >/dev/null # the full module graph must resolve (IDEs run this)

# end-to-end tests: starts a real kvz server on spare ports (needs a Mac with
# Virtualization.framework and the pod kernel)
e2e: build
	KVZ_BIN=$(abspath $(BIN)) go test -tags e2e -count=1 -timeout 30m -v ./test/e2e

# release tarball: bin/kvz + the pod kernel; sign and notarize first for releases
dist: build
	hack/release/package.sh $(KVZ_VERSION)

version:
	@echo $(KVZ_VERSION)

clean:
	rm -rf bin dist $(INIT)
