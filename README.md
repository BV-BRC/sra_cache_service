# sra_cache_service (SRACache)

A BV-BRC JSONRPC service, implemented in Go, that precaches `.sra` files from
NCBI on request (via `prefetch`), tracks per-accession precache status, serves
SRA run metadata for a set of accessions, and serves the cached `.sra` data
itself to clients over a separate HTTP data plane. It also ships client-side
CLI tools (`p3-sra-precache`, `p3-sra-dump`) that trigger a precache and run
the SRA toolkit dump utilities (`fasterq-dump`/`fastq-dump`) against the
result, as a drop-in for `sra_import`'s `p3-sra`.

This is **Phase 1** of a staged implementation -- only the module skeleton,
config loading, and a bootable binary answering `/ping` exist so far. See
`PLAN-sra-cache-service-implementation.md` in the `dev_container/modules` tree
for the full phased plan, and `PLAN-sra-precache-service.md` in the same tree
for the underlying design (API shapes, cache placement, eviction, AIMD
concurrency control, CLI compatibility with `p3-sra`).

## Building

From within a sourced dev_container environment (`source user-env.sh` at the
`dev_container` root first):

```bash
cd modules/sra_cache_service
make                 # builds bin/sra-cache-server
make test            # go test ./...
make test-race       # go test -race ./...
make vet             # go vet ./...
make compile-typespec  # regenerate the Perl/Python/JS clients in lib/ from SRACache.spec
```

Or directly with the Go toolchain (matching what the Makefile does under the
hood):

```bash
export PATH=/home/olson/P3/go-1.25.6/go/bin:$PATH
CGO_ENABLED=0 go build -buildvcs=false -o bin/sra-cache-server ./cmd/sra-cache-server
```

`CGO_ENABLED=0` and `-buildvcs=false` are required in this tree -- see
`BV-BRC-Go-SDK/Makefile` and the implementation plan's "Cross-cutting
conventions" section.

## Running

```bash
./bin/sra-cache-server --config deploy.cfg --section SRACache
curl localhost:7141/ping   # -> OK
```

`--config`/`$KB_DEPLOYMENT_CONFIG` and `--section`/`$KB_SERVICE_NAME` are read
the same way the Workspace Go download service (`ws-download`) reads them.
Run `./bin/sra-cache-server --help` for the full flag list.

## Cross-repo dependency on BV-BRC-Go-SDK

`go.mod` currently points at `BV-BRC-Go-SDK` via a local `replace` directive
(at `../BV-BRC-Go-SDK-sra-prereqs`, a worktree of the sibling PR branch
`feature/sra-cache-prereqs`), because this service needs SDK changes
(extended `sra.Record`/`LookupAll`, a promoted `p3auth` package) that are not
merged yet. **This `replace` is temporary** and must be removed -- with the
`require` pinned to a real released SDK version -- before any release build
(implementation plan Phase 10).

## Status

Phase 1 only. No JSONRPC dispatcher, no catalog, no fetcher, no data plane
yet -- see `PLAN-sra-cache-service-implementation.md` for what each later
phase adds.
