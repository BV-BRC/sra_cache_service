TOP_DIR = ../..
include $(TOP_DIR)/tools/Makefile.common

SERVICE = SRACache
SERVICE_PORT = 7141
DATA_SERVICE_PORT = 7142

SERVICE_URL = https://p3.theseed.org/services/$(SERVICE)
SERVER_SPEC = $(SERVICE).spec

GO ?= go
GOFLAGS ?=

BIN_LOCAL = bin
SERVER_BIN = $(BIN_LOCAL)/sra-cache-server

# Phase 1: build only the server binary and prove the JSONRPC client
# generation path. Deployment (Phase 3) and the CLI tools (Phases 8/9) are
# later phases -- the deploy-* targets below are intentionally stubs.

all: $(SERVER_BIN)

$(SERVER_BIN): $(shell find cmd internal -name '*.go') go.mod
	CGO_ENABLED=0 GOFLAGS=$(GOFLAGS) $(GO) build -buildvcs=false -o $(SERVER_BIN) ./cmd/sra-cache-server

build: $(SERVER_BIN)

test:
	CGO_ENABLED=0 $(GO) test -buildvcs=false ./...

test-race:
	# -race requires cgo (unlike the CGO_ENABLED=0 production build), so this
	# target deliberately does not set CGO_ENABLED=0 -- it needs a C toolchain
	# on the host, same as any other Go project's race-detector run.
	$(GO) test -buildvcs=false -race ./...

vet:
	$(GO) vet ./...

# Client-only: no --impl/--psgi/--service, since the server is hand-written
# Go, not generated. compile_typespec.pl unconditionally writes a
# <Service>Impl.{pm,py} and <Service>Server.{pm,py} pair regardless of those
# flags (verified directly against this spec, 2026-09) -- the two -rm lines
# below delete them, following the pattern (if not the letter -- that one
# references an undefined SERVER_MODULE var) of Workspace/Makefile:102-104.
compile-typespec: Makefile
	mkdir -p lib
	compile_typespec \
		--patric \
		--client $(SERVICE)Client \
		--py $(SERVICE)Client \
		--js $(SERVICE)Client \
		--url $(SERVICE_URL) \
		$(SERVER_SPEC) lib
	-rm -f lib/$(SERVICE)Impl.pm lib/$(SERVICE)Impl.py
	-rm -f lib/$(SERVICE)Server.pm lib/$(SERVICE)Server.py

clean:
	rm -rf $(BIN_LOCAL)
	rm -f lib/$(SERVICE)Client.pm lib/$(SERVICE)Client.py lib/$(SERVICE)Client.js

# --- Deployment (Phase 3) ---
# monit-based deploy of the static Go binary: service/start_service.tt,
# service/stop_service.tt, service/process.$(SERVICE).tt, plus the
# deploy/deploy-client/deploy-service/deploy-monit Makefile targets, follow
# Workspace/Makefile's TPAGE_* pattern adapted from PSGI/starman to a single
# static binary. Not built yet -- see
# PLAN-sra-cache-service-implementation.md, Phase 3.
deploy:
	@echo "deploy: not implemented until Phase 3 -- see PLAN-sra-cache-service-implementation.md"

deploy-client:
	@echo "deploy-client: not implemented until Phase 3 -- see PLAN-sra-cache-service-implementation.md"

deploy-service:
	@echo "deploy-service: not implemented until Phase 3 -- see PLAN-sra-cache-service-implementation.md"

deploy-monit:
	@echo "deploy-monit: not implemented until Phase 3 -- see PLAN-sra-cache-service-implementation.md"

.PHONY: all build test test-race vet compile-typespec clean deploy deploy-client deploy-service deploy-monit
