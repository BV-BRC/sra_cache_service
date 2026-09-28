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
PRECACHE_CLIENT_BIN = $(BIN_LOCAL)/p3-sra-precache
DUMP_CLIENT_BIN = $(BIN_LOCAL)/p3-sra-dump

# --- Deployment (Phase 3) config ---
# Same TARGET/DEPLOY_TARGET/DEPLOY_RUNTIME convention as Workspace/Makefile:
# TARGET is where we're installing to; DEPLOY_TARGET/DEPLOY_RUNTIME are what
# gets baked *into* the rendered templates (kb_top/kb_runtime), which is
# usually the same as TARGET/KB_RUNTIME but can be overridden separately for
# a build that stages into one place but will run from another.
TARGET ?= /kb/deployment
DEPLOY_TARGET ?= $(TARGET)
DEPLOY_RUNTIME ?= /kb/runtime

ifdef DEPLOYMENT_VAR_DIR
SERVICE_LOGDIR = $(DEPLOYMENT_VAR_DIR)/services/$(SERVICE)
TPAGE_SERVICE_LOGDIR = --define kb_service_log_dir=$(SERVICE_LOGDIR)
endif

ifdef TEMPDIR
TPAGE_TEMPDIR = --define kb_tempdir=$(TEMPDIR)
endif

TPAGE_DEPLOY_ARGS = \
	--define kb_top=$(DEPLOY_TARGET) \
	--define kb_runtime=$(DEPLOY_RUNTIME)

TPAGE_ARGS = \
	--define kb_service_name=$(SERVICE) \
	--define kb_service_port=$(SERVICE_PORT) \
	--define kb_data_service_port=$(DATA_SERVICE_PORT) \
	$(TPAGE_TEMPDIR) \
	$(TPAGE_SERVICE_LOGDIR)

# Phase 1 built only the server binary and proved the JSONRPC client
# generation path. Phase 8 added the first client CLI, p3-sra-precache.
# Phase 9 adds the second, p3-sra-dump (the p3-sra drop-in).

all: $(SERVER_BIN) $(PRECACHE_CLIENT_BIN) $(DUMP_CLIENT_BIN)

$(SERVER_BIN): $(shell find cmd internal -name '*.go') go.mod
	CGO_ENABLED=0 GOFLAGS=$(GOFLAGS) $(GO) build -buildvcs=false -o $(SERVER_BIN) ./cmd/sra-cache-server

$(PRECACHE_CLIENT_BIN): $(shell find cmd internal -name '*.go') go.mod
	CGO_ENABLED=0 GOFLAGS=$(GOFLAGS) $(GO) build -buildvcs=false -o $(PRECACHE_CLIENT_BIN) ./cmd/p3-sra-precache

$(DUMP_CLIENT_BIN): $(shell find cmd internal -name '*.go') go.mod
	CGO_ENABLED=0 GOFLAGS=$(GOFLAGS) $(GO) build -buildvcs=false -o $(DUMP_CLIENT_BIN) ./cmd/p3-sra-dump

build: $(SERVER_BIN) $(PRECACHE_CLIENT_BIN) $(DUMP_CLIENT_BIN)

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
# service/stop_service.tt, service/process.$(SERVICE).tt rendered via tpage,
# following Workspace/Makefile's TPAGE_*/deploy-service/deploy-monit pattern
# (adapted from PSGI/starman to a single static binary -- no wrap_*.sh
# wrapper anywhere in this module's deploy path, unlike the Perl script
# targets in tools/Makefile.common.rules: sra-cache-server and, later, the
# CLI tools are self-contained static binaries that read
# --config/$KB_DEPLOYMENT_CONFIG themselves).
deploy: deploy-client deploy-service

# Phase 8 added the first client CLI, p3-sra-precache, installed straight
# into $(TARGET)/bin like every other dev_container binary. Phase 9 adds
# p3-sra-dump the same way. Deliberately no wrap_*.sh step: these are static
# binaries, not scripts/*.pl|py|sh needing KB_TOP/PERL5LIB/etc injected
# around them (see this Makefile's "Deployment (Phase 3)" comment above).
deploy-client:
	if [ ! -d $(TARGET)/bin ] ; then mkdir -p $(TARGET)/bin ; fi
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=$(GOFLAGS) $(GO) build -buildvcs=false -o $(TARGET)/bin/p3-sra-precache ./cmd/p3-sra-precache
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=$(GOFLAGS) $(GO) build -buildvcs=false -o $(TARGET)/bin/p3-sra-dump ./cmd/p3-sra-dump

deploy-dir:
	if [ ! -d $(TARGET)/services/$(SERVICE) ] ; then mkdir -p $(TARGET)/services/$(SERVICE) ; fi
	if [ ! -d $(TARGET)/services/$(SERVICE)/bin ] ; then mkdir -p $(TARGET)/services/$(SERVICE)/bin ; fi

deploy-service: deploy-dir deploy-monit
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GOFLAGS=$(GOFLAGS) $(GO) build -buildvcs=false -o $(TARGET)/services/$(SERVICE)/bin/sra-cache-server ./cmd/sra-cache-server
	$(TPAGE) $(TPAGE_DEPLOY_ARGS) $(TPAGE_ARGS) service/start_service.tt > $(TARGET)/services/$(SERVICE)/start_service
	chmod +x $(TARGET)/services/$(SERVICE)/start_service
	$(TPAGE) $(TPAGE_DEPLOY_ARGS) $(TPAGE_ARGS) service/stop_service.tt > $(TARGET)/services/$(SERVICE)/stop_service
	chmod +x $(TARGET)/services/$(SERVICE)/stop_service

# deploy-monit depends on deploy-dir directly (Workspace/Makefile's
# equivalent target doesn't, and so only works standalone if deploy-dir
# already ran as a side effect of some earlier `make deploy`) so that
# `make deploy-monit` alone against a brand new TARGET doesn't fail trying
# to write into a services/$(SERVICE)/ directory that doesn't exist yet.
deploy-monit: deploy-dir
	$(TPAGE) $(TPAGE_DEPLOY_ARGS) $(TPAGE_ARGS) service/process.$(SERVICE).tt > $(TARGET)/services/$(SERVICE)/process.$(SERVICE)

.PHONY: all build test test-race vet compile-typespec clean deploy deploy-client deploy-dir deploy-service deploy-monit
