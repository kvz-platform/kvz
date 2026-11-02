BIN ?= bin/kvz
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w
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
