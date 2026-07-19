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

# State directory hard-reset target (db-reset). Mirrors internal/config's
# resolution: honour the DAINTREE_ASSISTANT_STATE_DIR override (used verbatim
# there) and otherwise fall back to the flat root ~/.daintree/assistant-cli.
# $(strip) collapses a whitespace-only override to empty so $(or) still picks
# the default instead of passing garbage to rm -rf. Immediate (:=) so the path
# is fixed and can be echoed before the recipe runs. We reset the whole root,
# not a per-project subdir — replicating config's project-slug logic here would
# duplicate Go, and the issue's intent is a clean slate.
STATE_DIR := $(or $(strip $(DAINTREE_ASSISTANT_STATE_DIR)),$(HOME)/.daintree/assistant-cli)

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

.PHONY: build install test test-race test-pty bench-local vet fmt generate run clean db-reset

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

## test-pty: run the real PTY render harness (allocates a pseudoterminal; not in the default suite).
test-pty:
	go test -count=1 -v -tags pty -run TestPTY ./internal/e2e/...

## bench-local: deterministic, offline hot-path benchmarks with allocation counts.
bench-local:
	go test -p 1 -run '^$$' -bench . -benchmem \
		./internal/storage ./internal/backend ./internal/agent ./internal/daemon ./internal/ui/markdown

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

## db-reset: hard-reset the assistant SQLite state dir (respects DAINTREE_ASSISTANT_STATE_DIR).
# The schema is a single clean baseline (one schemaUserVersion, not a chain), so a
# schema change is handled by wiping and rebuilding rather than a migration chain.
# Honours the state-dir override; falls back to ~/.daintree/assistant-cli. The
# empty-string guard is a safety net so a misconfigured STATE_DIR never expands
# into a bare `rm -rf`. Idempotent: rm -rf on a missing dir exits 0. Safe to run
# while the assistant is open — POSIX unlink keeps the live session's open file
# descriptors valid until it exits; the next launch creates a fresh state.db.
# The `--` stops a STATE_DIR that begins with `-` from being read as a flag.
db-reset:
	@if [ -z "$(STATE_DIR)" ]; then echo "db-reset: STATE_DIR is empty, refusing to rm -rf" >&2; exit 1; fi
	@echo "db-reset: removing $(STATE_DIR)"
	rm -rf -- "$(STATE_DIR)"
