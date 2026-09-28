// Package rpcapi implements the SRACache JSONRPC methods (SRACache.spec),
// wiring internal/jsonrpc's dispatcher to this service's business logic.
package rpcapi

import (
	"context"
	"time"

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

// URLSigner is the subset of *dataplane.Signer the get_sra_data handler
// needs: minting a short-lived signed URL for an already-cached accession.
// Declared as an interface (mirroring MetadataLookuper/Waker above) so a
// test can substitute a fake without a real HMAC secret or data-plane base
// URL; *dataplane.Signer satisfies it as-is. rpcapi does not otherwise
// import internal/dataplane -- Verify and the HTTP handler are that
// package's own concern, not this one's.
type URLSigner interface {
	Mint(accession string, ttl time.Duration) (url string, expires time.Time, err error)
}

// Service holds the dependencies SRACache's RPC methods need, and owns
// registering them against a *jsonrpc.Server (see RegisterGetMetadata in
// metadata.go, RegisterStartPrecache/RegisterPrecacheStatus in precache.go,
// RegisterGetSRAData in data.go).
type Service struct {
	Metadata MetadataLookuper

	// Catalog backs start_precache/precache_status/get_sra_data (Phases 6-7).
	// Nil is only valid if none of those methods is registered against a
	// given Server -- all three handlers assume a non-nil Catalog.
	Catalog *catalog.Catalog

	// Pool is woken (see Waker) after start_precache or get_sra_data
	// enqueues at least one accession, so a fresh request doesn't sit idle
	// until the pool's next poll tick. May be nil (e.g. in a test that only
	// exercises the catalog-side behavior) -- both handlers skip the wake
	// rather than panicking when it is.
	Pool Waker

	// Signer mints get_sra_data's signed download URLs (Phase 7). Nil is
	// only valid if get_sra_data is not registered against a given Server.
	Signer URLSigner

	// DownloadLifetime is the ttl get_sra_data passes to Signer.Mint --
	// config.Config.DownloadLifetime, threaded through by main.go. Per
	// PLAN-sra-precache-service.md section 3, this is also the
	// cache-eviction pin lease for the minted accession: get_sra_data calls
	// catalog.Catalog.Pin with the same expires Mint returns.
	DownloadLifetime time.Duration
}
