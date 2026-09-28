// Package rpcapi implements the SRACache JSONRPC methods (SRACache.spec),
// wiring internal/jsonrpc's dispatcher to this service's business logic.
package rpcapi

import (
	"context"

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

// Service holds the dependencies SRACache's RPC methods need, and owns
// registering them against a *jsonrpc.Server (see RegisterGetMetadata in
// metadata.go). Phase 4 only wires up metadata lookup; later phases (5-7)
// will add fields here for the catalog, cache, and fetcher as those pieces
// get built -- deliberately not guessed at now.
type Service struct {
	Metadata MetadataLookuper
}
