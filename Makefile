# Makefile for daintree-assistant (Go).
# Single static binary; pure-Go SQLite (no CGO). Requires Go >= 1.25.13.

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

# Install location. Daintree's host does NOT hardcode a path — it locates the
# binary by a shell PATH lookup (`which` on Unix, `where` on Windows), with the
# DAINTREE_CLI_PATH_PREPEND env var taking precedence and an npm-global-prefix
# shim as the last-resort fallback (daintree/electron/services/
# CliAvailabilityService.ts). So the rule is simply: the binary must sit in a
# directory that is ON $PATH, and there must be exactly ONE copy — a second copy
# elsewhere on $PATH can win the lookup and shadow this one.
#
# We default to the first PATH dir Daintree already resolves to per platform.
# Forcing GOBIN means `go install` writes ONLY here, never the default
# $(go env GOPATH)/bin, so we never leave a shadowing second copy.
#
# Override freely:  make install INSTALL_DIR=/some/dir/on/PATH
ifeq ($(OS),Windows_NT)
  # GNU make on Windows. Go names the artifact daintree-assistant.exe; Windows
  # resolves it via PATHEXT, so no suffix handling is needed here. %APPDATA%\npm
  # is on PATH by default and is also where the host's npm-prefix fallback looks.
  INSTALL_DIR ?= $(APPDATA)\npm
else
  UNAME_S := $(shell uname -s)
  ifeq ($(UNAME_S),Darwin)
    # Apple Silicon → Homebrew /opt/homebrew/bin; Intel → /usr/local/bin.
    ifeq ($(shell uname -m),arm64)
      INSTALL_DIR ?= /opt/homebrew/bin
    else
      INSTALL_DIR ?= /usr/local/bin
    endif
  else
    # Linux / other Unix: /usr/local/bin is the conventional on-PATH location.
    INSTALL_DIR ?= /usr/local/bin
  endif
endif

.DEFAULT_GOAL := build

.PHONY: build install test test-race bench-local vet fmt generate run clean db-reset

## build: compile the binary into ./bin with version + trimpath.
build:
	@mkdir -p $(BIN_DIR)
	go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

## install: install ONLY to $(INSTALL_DIR) (where Daintree's host expects it).
install:
	GOBIN=$(INSTALL_DIR) go install $(GOFLAGS) -ldflags "$(LDFLAGS)" $(PKG)

## test: run the full test suite (no network).
test:
	go test ./...

## test-race: run the suite under the race detector.
test-race:
	go test -race ./...

## bench-local: deterministic, offline hot-path benchmarks with allocation counts.
bench-local:
	go test -p 1 -run '^$$' -bench . -benchmem \
		./internal/storage ./internal/backend ./internal/agent ./internal/daemon

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

## db-reset: reset this project's local state.
#
# DELEGATES to the CLI. It used to be `rm -rf "$(STATE_DIR)"` straight from the shell,
# which was wrong in ways make cannot see:
#
#   - it unlinked owner.lock while a live process still held a flock on that INODE, so
#     the next process created a different file, acquired it trivially, and the
#     single-owner invariant protecting the database was gone with no error anywhere;
#   - it left a running assistant writing to an unlinked database;
#   - it removed the daemon's socket and lock while the daemon was still alive;
#   - it duplicated internal/config's state-path resolution in shell, so the two could
#     disagree about which directory to destroy.
#
# `reset project-state` stops the daemon, ACQUIRES the owner lease (and refuses if
# something else holds it), backs up to a timestamped directory, and removes only this
# project's state. --yes because a make target has nobody to prompt.
#
# Use `daintree-assistant reset all-data` for the nuclear option.
db-reset: build
	$(BIN) reset project-state --yes
