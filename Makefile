# Makefile for daintree-assistant (Go).
# Single static binary; pure-Go SQLite (no CGO). Requires Go >= 1.25.8.

BINARY    := daintree-assistant
PKG       := ./cmd/daintree-assistant
BIN_DIR   := bin
BIN       := $(BIN_DIR)/$(BINARY)

# Version injected into main.version via ldflags. Defaults to the git describe,
# falling back to "dev" outside a tagged checkout.
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X main.version=$(VERSION)

# Reproducible builds: -trimpath strips local filesystem paths from the binary.
GOFLAGS   := -trimpath

.DEFAULT_GOAL := build

.PHONY: build install test test-race vet fmt generate run clean

## build: compile the binary into ./bin with version + trimpath.
build:
	@mkdir -p $(BIN_DIR)
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install: install to $(go env GOBIN) (or $(go env GOPATH)/bin) with the same flags.
install:
	go install $(GOFLAGS) -ldflags "$(LDFLAGS)" $(PKG)

## test: run the full test suite (no network).
test:
	go test ./...

## test-race: run the suite under the race detector.
test-race:
	go test -race ./...

## vet: run go vet across all packages.
vet:
	go vet ./...

## fmt: format all Go sources in place.
fmt:
	gofmt -w .

## generate: run go generate (e.g. splash frames) across all packages.
generate:
	go generate ./...

## run: build and launch the interactive cockpit.
run: build
	$(BIN)

## clean: remove build artifacts.
clean:
	rm -rf $(BIN_DIR)
