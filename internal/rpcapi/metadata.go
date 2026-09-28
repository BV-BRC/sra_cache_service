package rpcapi

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/BV-BRC/sra_cache_service/internal/accession"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
	"github.com/BV-BRC/sra_cache_service/internal/metadata"
)

// RegisterGetMetadata registers SRACache.get_metadata against srv, per
// SRACache.spec:
//
//	funcdef get_metadata(list<sra_accession> ids)
//	    returns (list<sra_metadata> metadata) authentication optional;
//
// AuthOptional here matches the spec directly (get_metadata is one of the
// two methods -- alongside precache_status/get_sra_data -- meant to serve
// anonymous callers as well as authenticated ones).
func (s *Service) RegisterGetMetadata(srv *jsonrpc.Server) {
	srv.Register("SRACache.get_metadata", jsonrpc.AuthOptional, 1, s.getMetadata)
}

// getMetadata implements the handler.
//
// One bad accession in a batch must never fail the whole call: a
// syntactically invalid id is rejected locally (never sent to NCBI at all)
// and reported as its own "invalid_id" row; a syntactically valid id NCBI
// doesn't recognize comes back from internal/metadata as "missing" and is
// reported as its own "not_found" row. The only way the call as a whole
// fails is a genuine NCBI-outage error out of Lookup, or malformed
// request params -- both caller/transport-level problems, not a property of
// any one accession.
func (s *Service) getMetadata(ctx context.Context, params json.RawMessage) ([]any, error) {
	ids, err := decodeIDs(params)
	if err != nil {
		return nil, fmt.Errorf("get_metadata: %w", err)
	}

	rows := make([]metadata.SRAMetadata, 0, len(ids))

	var valid []string
	for _, id := range ids {
		if verr := accession.Validate(id); verr != nil {
			rows = append(rows, invalidRow(id, verr))
			continue
		}
		valid = append(valid, id)
	}

	if len(valid) > 0 {
		found, missing, lerr := s.Metadata.Lookup(ctx, valid)
		if lerr != nil {
			return nil, fmt.Errorf("get_metadata: looking up metadata: %w", lerr)
		}
		rows = append(rows, found...)
		for _, id := range missing {
			rows = append(rows, notFoundRow(id))
		}
	}

	return []any{rows}, nil
}

// decodeIDs unmarshals the JSONRPC positional-params array for a
// single-argument `list<sra_accession> ids` funcdef, i.e. params is a
// one-element array whose element is itself an array of strings:
// `[["SRR1","SRX2"]]`.
func decodeIDs(params json.RawMessage) ([]string, error) {
	var args []json.RawMessage
	if err := json.Unmarshal(params, &args); err != nil {
		return nil, fmt.Errorf("decoding params: %w", err)
	}
	if len(args) < 1 {
		return nil, fmt.Errorf("expected 1 argument (ids), got %d", len(args))
	}
	var ids []string
	if err := json.Unmarshal(args[0], &ids); err != nil {
		return nil, fmt.Errorf("decoding ids: %w", err)
	}
	return ids, nil
}

// invalidRow builds a get_metadata result row for an accession that failed
// local syntax validation -- never sent to NCBI. Accession echoes the
// accession as requested, since there is no run to report.
func invalidRow(id string, cause error) metadata.SRAMetadata {
	return metadata.SRAMetadata{
		Accession: id,
		Extra: map[string]string{
			"error":   "invalid_id",
			"message": cause.Error(),
		},
	}
}

// notFoundRow builds a get_metadata result row for a syntactically valid
// accession NCBI does not recognize (including an SRX/SRP that failed to
// expand on its own -- see internal/metadata.Lookup's doc comment).
func notFoundRow(id string) metadata.SRAMetadata {
	return metadata.SRAMetadata{
		Accession: id,
		Extra: map[string]string{
			"error":   "not_found",
			"message": fmt.Sprintf("NCBI has no accession %s", id),
		},
	}
}
