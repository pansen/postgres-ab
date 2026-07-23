# pgdev — Go automation for the snapshottable-PostgreSQL Apple machine.
#
# The binary runs *inside* the Linux container machine (linux/arm64), invoked by
# scripts/pg-dev-local. `make build` cross-compiles from macOS and drops the
# binary onto the home-mount at bin/pgdevd, where the in-machine script sees it
# at the same repo path. No Go toolchain is needed inside the machine.

BIN        := pgdevd
BIN_DIR    := bin
OUT        := $(BIN_DIR)/$(BIN)
HOST_OUT   := $(BIN_DIR)/pgdev
PKG        := ./...
# The machine is Apple silicon; build for it by default. Override for other hosts.
GOOS       ?= linux
GOARCH     ?= arm64
# Stamp both binaries from the same git state so the deploy version handshake
# (host pgdev ↔ in-machine pgdevd via GET /v1/version) matches.
VERSION    ?= $(shell git -C .. describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS    := -s -w -X main.version=$(VERSION)
# Code-signing for the host CLI. macOS Local Network Privacy (Tahoe+) keys the
# forwarder LaunchAgent's grant to the binary's SIGNING IDENTITY, and we re-sign
# on every host build so a plain `go build` can't drop it.
#
#   SIGN_CERT — the `codesign --sign` identity. Default "-" is ad-hoc: it has no
#     stable identity, so macOS falls back to the cdhash, which changes every
#     build → each rebuild re-prompts for Local Network AND adds a duplicate row
#     to the list. Set it to a stable keychain code-signing identity (a one-time
#     self-signed cert is enough for local dev — see README "macOS Security") and
#     the once-granted permission survives every rebuild, no re-prompt:
#       make build SIGN_CERT="PGDev Signing"      (or set it in .env)
#   SIGN_ID   — the stable --identifier, so the Local Network entry shows a fixed
#     name (me.pansen.pgdev) regardless of which cert signs it.
SIGN_ID    := me.pansen.pgdev
SIGN_CERT  ?= -

.DEFAULT_GOAL := build

## build: build both binaries — the in-machine daemon and the host CLI
.PHONY: build
build: daemon host

## daemon: cross-compile the in-machine binary to bin/pgdevd (linux/arm64)
.PHONY: daemon
daemon:
	@mkdir -p $(BIN_DIR)
	@GOOS=$(GOOS) GOARCH=$(GOARCH) CGO_ENABLED=0 \
		go build -trimpath -ldflags '$(LDFLAGS)' -o $(OUT) ./cmd/pgdevd
	@echo "built $(OUT) ($(GOOS)/$(GOARCH), $(VERSION))"

## host: build the host (macOS) CLI to bin/pgdev
.PHONY: host
host:
	@mkdir -p $(BIN_DIR)
	@go build -trimpath -ldflags '$(LDFLAGS)' -o $(HOST_OUT) ./cmd/pgdev
	@if command -v codesign >/dev/null 2>&1; then \
		codesign --force --sign "$(SIGN_CERT)" --identifier $(SIGN_ID) $(HOST_OUT) || exit 1; \
		echo "signed $(HOST_OUT) ($(SIGN_ID), cert=$(SIGN_CERT))"; \
	fi
	@echo "built $(HOST_OUT) (host, $(VERSION))"

## build-host: build for the host OS/arch (handy for local smoke runs)
.PHONY: build-host
build-host:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags '$(LDFLAGS)' -o $(OUT)-host ./cmd/pgdevd

## test: run unit tests (host) — task engine + store transaction logic
.PHONY: test
test:
	go test $(PKG)

## race: unit tests under the race detector
.PHONY: race
race:
	go test -race $(PKG)

## cover: unit tests with a coverage summary
.PHONY: cover
cover:
	go test -coverprofile=coverage.out $(PKG)
	go tool cover -func=coverage.out | tail -n1

## integration: real XFS reflink tests (Linux only; needs root + losetup + mkfs.xfs)
.PHONY: integration
integration:
	go test -tags=integration -run XFS ./internal/store/...

## vet: go vet
.PHONY: vet
vet:
	go vet $(PKG)

## lint: golangci-lint if present, else vet
.PHONY: lint
lint:
	@if command -v golangci-lint >/dev/null; then golangci-lint run; else $(MAKE) vet; fi

## fmt: format and tidy
.PHONY: fmt
fmt:
	gofmt -w .
	go mod tidy

## check: everything CI runs
.PHONY: check
check: fmt vet test build

## clean: remove build + coverage artifacts
.PHONY: clean
clean:
	rm -rf $(BIN_DIR) coverage.out

## help: list targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/^## //'
