BIN ?= bin/kvz
GOFLAGS ?= -trimpath
LDFLAGS ?= -s -w

.PHONY: build bench clean
build:
	CGO_ENABLED=1 go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/kvz
	codesign --force --entitlements kvz.entitlements -s - $(BIN)

bench:
	go build $(GOFLAGS) -o bin/kvzbench ./cmd/kvzbench

clean:
	rm -rf bin
