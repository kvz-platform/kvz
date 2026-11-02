BIN ?= bin/kvz
GOFLAGS ?= -trimpath
K8S_VERSION ?= v1.35.6
KVZ_COMMIT := $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
VPKG := k8s.io/component-base/version
CPKG := k8s.io/client-go/pkg/version
VFLAGS := -X $(VPKG).gitVersion=$(K8S_VERSION)+kvz -X $(VPKG).gitMajor=1 -X $(VPKG).gitMinor=35 \
	-X $(VPKG).gitCommit=$(KVZ_COMMIT) -X $(VPKG).gitTreeState=clean -X $(VPKG).buildDate=$(BUILD_DATE) \
	-X $(CPKG).gitVersion=$(K8S_VERSION)+kvz -X $(CPKG).gitMajor=1 -X $(CPKG).gitMinor=35 \
	-X $(CPKG).gitCommit=$(KVZ_COMMIT) -X $(CPKG).gitTreeState=clean -X $(CPKG).buildDate=$(BUILD_DATE)
LDFLAGS ?= -s -w $(VFLAGS)
INIT := pkg/runtime/microvmrt/kvz-init

.PHONY: build init bench clean
build: init
	CGO_ENABLED=1 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kvz
	codesign --force --entitlements kvz.entitlements -s - $(BIN)

# guest PID 1, embedded into kvz
init:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(INIT) ./cmd/kvz-init

bench: init
	go build $(GOFLAGS) -o bin/kvzbench ./cmd/kvzbench
	go build $(GOFLAGS) -o bin/vmbench ./cmd/vmbench && codesign --force --entitlements kvz.entitlements -s - bin/vmbench

clean:
	rm -rf bin $(INIT)
