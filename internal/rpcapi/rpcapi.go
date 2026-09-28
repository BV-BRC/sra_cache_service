// Package rpcapi implements the SRACache JSONRPC methods (SRACache.spec),
// wiring internal/jsonrpc's dispatcher to this service's business logic.
package rpcapi

import (
	"context"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/metadata"
)

// MetadataLookuper is the subset of *metadata.Lookuper the get_metadata
// handler needs. Declared as an interface, rather than depending on
// *metadata.Lookuper concretely, so a test can substitute a fake without
// standing up a real sra.Client / HTTP server; *metadata.Lookuper satisfies
// it as-is.
type MetadataLookuper interface {
	Lookup(ctx context.Context, ids []string) (rows []metadata.SRAMetadata, missing []string, err error)
}

// Waker is the subset of *fetcher.Pool that start_precache needs: a
// non-blocking nudge to check for newly queued rows immediately rather than
// waiting out the pool's next poll tick. Declared as an interface (mirroring
// MetadataLookuper above) so a test can substitute a fake without standing
// up a real catalog/cache/fetcher pool; *fetcher.Pool satisfies it as-is.
// rpcapi deliberately does not import internal/fetcher for anything wider
// than this one method -- the pool's own lifecycle (construction, Run) is
// main.go's job, not rpcapi's.
type Waker interface {
	Wake()
}

// Service holds the dependencies SRACache's RPC methods need, and owns
// registering them against a *jsonrpc.Server (see RegisterGetMetadata in
// metadata.go, RegisterStartPrecache/RegisterPrecacheStatus in precache.go).
// Phase 7 will add fields here for the data plane as that piece gets built.
type Service struct {
	Metadata MetadataLookuper

	// Catalog backs start_precache/precache_status (Phase 6). Nil is only
	// valid if neither method is registered against a given Server -- both
	// handlers assume a non-nil Catalog.
	Catalog *catalog.Catalog

	// Pool is woken (see Waker) after start_precache enqueues at least one
	// accession, so a fresh request doesn't sit idle until the pool's next
	// poll tick. May be nil (e.g. in a test that only exercises the
	// catalog-side behavior) -- start_precache skips the wake rather than
	// panicking when it is.
	Pool Waker
}
