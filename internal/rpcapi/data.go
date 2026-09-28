package rpcapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
)

// dataLocationWire mirrors SRACache.spec's sra_data_location typedef
// exactly -- field names, and just those four fields. Note there is
// deliberately NO message field here, unlike precacheStatusWire: the design
// doc's prose (PLAN-sra-precache-service.md section 6) says an invalid_id
// rejection carries "a message explaining why", but the sra_data_location
// typedef it defines two paragraphs later has no message field at all, and
// SRACache.spec (already committed, transcribed from that same section) is
// the frozen contract this phase implements against -- so an invalid_id row
// here reports accession/state only, same as every other row shape.
type dataLocationWire struct {
	Accession string `json:"accession"`
	State     string `json:"state"`
	URL       string `json:"url"`
	Expires   string `json:"expires"`
}

// RegisterGetSRAData registers SRACache.get_sra_data against srv, per
// SRACache.spec:
//
//	funcdef get_sra_data(list<sra_accession> ids)
//	    returns (list<sra_data_location> locations) authentication optional;
//
// AuthOptional matches the spec: get_sra_data serves anonymous callers, same
// as get_metadata/precache_status -- an unauthenticated caller can still
// read (and implicitly trigger precaching of) public SRA data.
func (s *Service) RegisterGetSRAData(srv *jsonrpc.Server) {
	srv.Register("SRACache.get_sra_data", jsonrpc.AuthOptional, 1, s.getSRAData)
}

// getSRAData implements the handler.
//
// Every accession gets its own result row in request order:
//
//   - not run-level -> state = invalid_id, url/expires empty. Never reaches
//     the catalog (same validateRunLevel gate precache.go's handlers use).
//   - run-level and not yet cached (no catalog row at all, or one in
//     queued/in_progress/error) -> catalog.Catalog.Enqueue as a side effect
//     (a documented no-op if a row already exists -- see Enqueue's doc
//     comment in catalog.go, and note this means a prior `error` row is
//     reported back as-is, not silently retried; that matches
//     start_precache's own behavior on the same accession), current state,
//     url/expires empty. This is the design doc's explicit "get_sra_data
//     implicitly starts precaching" behavior (SRACache.spec's doc comment
//     on the funcdef).
//   - run-level and cached -> mint a signed URL via Signer.Mint (ttl =
//     s.DownloadLifetime), Pin the catalog row until the returned expires
//     (the cache-eviction lease -- PLAN-sra-precache-service.md section 3;
//     there is no separate refcount protocol), and return
//     state=cached/url/expires.
//
// The pool is woken at most once per call, mirroring startPrecache in
// precache.go, if at least one accession was passed to Enqueue.
func (s *Service) getSRAData(ctx context.Context, params json.RawMessage) ([]any, error) {
	ids, err := decodeIDs(params) // shared with get_metadata/precache_status -- same single-arg `list<sra_accession> ids` shape.
	if err != nil {
		return nil, fmt.Errorf("get_sra_data: %w", err)
	}

	rows := make([]dataLocationWire, len(ids))

	var toCheck []string
	for i, id := range ids {
		if verr := validateRunLevel(id); verr != nil {
			rows[i] = dataLocationWire{Accession: id, State: string(catalog.StateInvalidID)}
			continue
		}
		toCheck = append(toCheck, id)
	}
	if len(toCheck) == 0 {
		return []any{rows}, nil
	}

	entries, err := s.Catalog.GetMany(ctx, toCheck)
	if err != nil {
		return nil, fmt.Errorf("get_sra_data: looking up catalog status: %w", err)
	}

	anyEnqueued := false
	for i, id := range ids {
		if rows[i].State != "" {
			continue // already an invalid_id placeholder
		}

		if e, ok := entries[id]; ok && e.State == catalog.StateCached {
			url, expires, merr := s.Signer.Mint(id, s.DownloadLifetime)
			if merr != nil {
				return nil, fmt.Errorf("get_sra_data: minting url for %s: %w", id, merr)
			}
			// The minted expires IS the eviction pin lease -- see this
			// file's doc comment and catalog.Catalog.Pin's own.
			if perr := s.Catalog.Pin(ctx, id, expires); perr != nil {
				return nil, fmt.Errorf("get_sra_data: pinning %s: %w", id, perr)
			}
			rows[i] = dataLocationWire{
				Accession: id,
				State:     string(catalog.StateCached),
				URL:       url,
				Expires:   formatTimestamp(expires),
			}
			continue
		}

		// Not cached (absent from the catalog entirely, or present in
		// queued/in_progress/error/not_found) -- enqueue as a side effect
		// and report the current state with no url.
		if err := s.Catalog.Enqueue(ctx, id); err != nil {
			return nil, fmt.Errorf("get_sra_data: enqueueing %s: %w", id, err)
		}
		anyEnqueued = true

		state := catalog.StateQueued
		if e, ok := entries[id]; ok {
			state = e.State
		}
		rows[i] = dataLocationWire{Accession: id, State: string(state)}
	}

	if anyEnqueued && s.Pool != nil {
		s.Pool.Wake()
	}

	return []any{rows}, nil
}
