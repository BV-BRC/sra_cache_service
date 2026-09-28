package rpcapi

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/accession"
	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
)

// precacheStatusWire mirrors SRACache.spec's precache_status typedef
// exactly (field names and JSON shape), so it can be returned as-is in the
// RPC response.
type precacheStatusWire struct {
	Accession  string `json:"accession"`
	State      string `json:"state"`
	Message    string `json:"message"`
	SizeBytes  int64  `json:"size_bytes"`
	QueuedTime string `json:"queued_time"`
	CachedTime string `json:"cached_time"`
}

// precacheOptions mirrors SRACache.spec's precache_options typedef.
// Priority is accepted on the wire -- so a caller that passes it never gets
// an unmarshal error -- but not yet acted on: Phase 6's queue
// (catalog.Catalog.ListByState, a plain FIFO ordered by queued_time) has no
// scheduling-priority concept at all. That is a documented gap, not a
// silent lie: nothing claims priority does anything yet.
type precacheOptions struct {
	Priority int `json:"priority"`
}

// RegisterStartPrecache registers SRACache.start_precache against srv, per
// SRACache.spec:
//
//	funcdef start_precache(list<sra_accession> ids, precache_options options)
//	    returns (list<precache_status> status) authentication required;
//
// AuthRequired matches the spec directly -- unlike get_metadata/
// precache_status, this method has a real side effect (queuing work, and by
// extension consuming prefetch/NCBI capacity), so an anonymous caller must
// not be able to trigger it.
func (s *Service) RegisterStartPrecache(srv *jsonrpc.Server) {
	srv.Register("SRACache.start_precache", jsonrpc.AuthRequired, 1, s.startPrecache)
}

// RegisterPrecacheStatus registers SRACache.precache_status against srv,
// per SRACache.spec:
//
//	funcdef precache_status(list<sra_accession> ids)
//	    returns (list<precache_status> status) authentication optional;
//
// AuthOptional matches the spec: this is a pure read (internal/catalog's
// GetMany, no side effect), so it serves anonymous callers same as
// get_metadata.
func (s *Service) RegisterPrecacheStatus(srv *jsonrpc.Server) {
	srv.Register("SRACache.precache_status", jsonrpc.AuthOptional, 1, s.precacheStatus)
}

// startPrecache implements the handler. Every accession gets its own result
// row in request order, regardless of what happens to any other accession
// in the same batch:
//
//   - not run-level (fails accession.Validate, or is a syntactically valid
//     but non-SRR/DRR/ERR accession) -> state = invalid_id, with the exact
//     "must be expanded via get_metadata first" message the design doc
//     specifies for the latter case -- never sent to the catalog at all.
//   - otherwise -> catalog.Catalog.Enqueue (a documented no-op if the
//     accession is already queued/in_progress/cached -- see Enqueue's own
//     doc comment in catalog.go), then its current status.
//
// The pool is woken at most once per call (not once per accession) if at
// least one accession was actually enqueued, since a single Wake causes the
// pool to drain everything currently queued, not just one row.
func (s *Service) startPrecache(ctx context.Context, params json.RawMessage) ([]any, error) {
	ids, _, err := decodeIDsAndOptions(params)
	if err != nil {
		return nil, fmt.Errorf("start_precache: %w", err)
	}

	rows := make([]precacheStatusWire, len(ids))
	anyEnqueued := false
	for i, id := range ids {
		if verr := validateRunLevel(id); verr != nil {
			rows[i] = precacheStatusWire{Accession: id, State: string(catalog.StateInvalidID), Message: verr.Error()}
			continue
		}
		if err := s.Catalog.Enqueue(ctx, id); err != nil {
			return nil, fmt.Errorf("start_precache: enqueueing %s: %w", id, err)
		}
		anyEnqueued = true
	}
	if anyEnqueued && s.Pool != nil {
		s.Pool.Wake()
	}

	if err := s.fillPrecacheStatuses(ctx, ids, rows); err != nil {
		return nil, fmt.Errorf("start_precache: %w", err)
	}
	return []any{rows}, nil
}

// precacheStatus implements the handler: a pure read via
// catalog.Catalog.GetMany, with the same run-level-only enforcement and
// invalid_id short-circuit as startPrecache -- but never calling Enqueue,
// so polling status can never itself start a precache.
func (s *Service) precacheStatus(ctx context.Context, params json.RawMessage) ([]any, error) {
	ids, err := decodeIDs(params) // shared with get_metadata (metadata.go) -- same single-arg `list<sra_accession> ids` shape.
	if err != nil {
		return nil, fmt.Errorf("precache_status: %w", err)
	}

	rows := make([]precacheStatusWire, len(ids))
	for i, id := range ids {
		if verr := validateRunLevel(id); verr != nil {
			rows[i] = precacheStatusWire{Accession: id, State: string(catalog.StateInvalidID), Message: verr.Error()}
		}
	}

	if err := s.fillPrecacheStatuses(ctx, ids, rows); err != nil {
		return nil, fmt.Errorf("precache_status: %w", err)
	}
	return []any{rows}, nil
}

// fillPrecacheStatuses fills rows[i] for every id whose slot fillPrecacheStatuses
// finds still zero-valued (i.e. not already populated with an invalid_id
// placeholder by the caller), via a single catalog.Catalog.GetMany batch
// call -- one query regardless of batch size, rather than one Get per id.
//
// An id with no catalog row at all -- syntactically valid and run-level,
// but never passed to start_precache -- is reported with an empty state and
// an explanatory message rather than an error: "nobody has asked to cache
// this yet" is an ordinary, expected answer for precache_status to give,
// not a failure. The precache_state enum (SRACache.spec) has no dedicated
// value for it, and inventing one would mean changing the design doc, which
// this phase does not do.
func (s *Service) fillPrecacheStatuses(ctx context.Context, ids []string, rows []precacheStatusWire) error {
	var toFetch []string
	for i, id := range ids {
		if rows[i].State == "" {
			toFetch = append(toFetch, id)
		}
	}
	if len(toFetch) == 0 {
		return nil
	}

	entries, err := s.Catalog.GetMany(ctx, toFetch)
	if err != nil {
		return fmt.Errorf("looking up catalog status: %w", err)
	}

	for i, id := range ids {
		if rows[i].State != "" {
			continue // already an invalid_id placeholder
		}
		e, ok := entries[id]
		if !ok {
			rows[i] = precacheStatusWire{Accession: id, Message: "not yet requested: call start_precache first"}
			continue
		}
		rows[i] = wireFromEntry(e)
	}
	return nil
}

// validateRunLevel enforces the run-level-only rule start_precache and
// precache_status share with get_sra_data (Phase 7) per
// PLAN-sra-precache-service.md section 6: "start_precache/get_sra_data
// require run-level accessions only (SRR/DRR/ERR) and reject anything else
// with state = invalid_id and a message explaining why". The message for a
// syntactically valid but non-run-level accession is the design doc's own
// wording, verbatim; a syntactically invalid accession instead carries
// accession.Validate's own message (matching get_metadata's invalidRow in
// metadata.go).
func validateRunLevel(id string) error {
	if err := accession.Validate(id); err != nil {
		return err
	}
	if !accession.IsRunLevel(id) {
		return fmt.Errorf("SRX/SRP accessions must be expanded via get_metadata first")
	}
	return nil
}

// wireFromEntry maps a catalog.Entry to the wire shape.
func wireFromEntry(e catalog.Entry) precacheStatusWire {
	return precacheStatusWire{
		Accession:  e.Accession,
		State:      string(e.State),
		Message:    e.Message,
		SizeBytes:  e.SizeBytes,
		QueuedTime: formatTimestamp(e.QueuedTime),
		CachedTime: formatTimestamp(e.CachedTime),
	}
}

// formatTimestamp renders a catalog timestamp as RFC3339, or "" for the
// zero value (catalog.Entry's documented "unset" sentinel -- see catalog.go
// on Entry). SRACache.spec's `timestamp` typedef is a bare string with no
// wire format mandated beyond that, so RFC3339 (Go's usual default) is used
// here; "" avoids printing a misleading 1970-01-01 for cached_time before
// an accession has actually been cached, matching the typedef's own doc
// comment ("unset until cached").
func formatTimestamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// decodeIDsAndOptions unmarshals the JSONRPC positional-params array for
// start_precache's two-argument funcdef:
// `(list<sra_accession> ids, precache_options options)`, i.e.
// `[["SRR1","SRR2"], {"priority": 5}]`. options is optional on the wire --
// absent, null, or simply a short params array all decode to the zero
// precacheOptions rather than an error, since every field in it is
// currently unused anyway (see precacheOptions' doc comment).
func decodeIDsAndOptions(params json.RawMessage) ([]string, precacheOptions, error) {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil {
		return nil, precacheOptions{}, fmt.Errorf("decoding params: %w", err)
	}
	if len(args) < 1 {
		return nil, precacheOptions{}, fmt.Errorf("expected at least 1 argument (ids), got %d", len(args))
	}
	var ids []string
	if err := json.Unmarshal(args[0], &ids); err != nil {
		return nil, precacheOptions{}, fmt.Errorf("decoding ids: %w", err)
	}

	var opts precacheOptions
	if len(args) >= 2 && len(args[1]) > 0 && string(args[1]) != "null" {
		if err := json.Unmarshal(args[1], &opts); err != nil {
			return nil, precacheOptions{}, fmt.Errorf("decoding options: %w", err)
		}
	}
	return ids, opts, nil
}
