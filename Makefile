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

# Install location. This target produces a STANDALONE binary — for a shell, a
# script, an `mcp --stdio` client, a benchmark harness — so the only rule is that
# it lands in a directory that is ON $PATH, and that there is exactly ONE copy: a
# second copy elsewhere on $PATH wins the lookup and shadows this one, and the
# symptom is a feature that mysteriously does not exist rather than a version
# mismatch. `doctor` lists every copy it can find, for exactly that reason.
#
# What this does NOT do is feed the Daintree desktop app. Daintree PINS this
# engine as the `vendor/daintree-assistant` submodule and builds it into its own
# `resources/assistant/` (daintree/electron/services/assistant-host/
# resolveAssistantBinary.ts); there is deliberately no PATH fallback there,
# because the host protocol moves in lockstep with the engine and binding to
# whatever copy a user happened to install is how protocol skew happens — the
# failure being an inscrutable protocol rejection, not a missing binary. To
# develop the engine against the app, point DAINTREE_ASSISTANT_BIN at the
# submodule's own build output; `make install` here will not be noticed by it.
#
# We default to the conventional on-PATH directory per platform. Forcing GOBIN
# means `go install` writes ONLY here, never additionally to the default
# $(go env GOPATH)/bin, so we never leave a shadowing second copy.
#
# Override freely:  make install INSTALL_DIR=/some/dir/on/PATH
ifeq ($(OS),Windows_NT)
  # GNU make on Windows. Go names the artifact daintree-assistant.exe; Windows
  # resolves it via PATHEXT, so no suffix handling is needed here. %APPDATA%\npm
  # is on PATH by default, which is the only reason it is the default here.
  # (Background supervision does not exist on Windows at all — see CLAUDE.md.)
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

## install: install ONLY to $(INSTALL_DIR) (an on-PATH dir, for standalone use).
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
