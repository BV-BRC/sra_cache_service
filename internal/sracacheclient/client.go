// Package sracacheclient is the typed Go client for SRACache's four
// JSONRPC methods, built on top of internal/rpcclient's generic envelope
// handling. Request/response shapes here are transcribed from
// internal/rpcapi's actual handler code (metadata.go/precache.go/data.go)
// -- not from SRACache.spec's or the implementation plan's prose -- since
// those are what the dispatcher this client talks to actually sends and
// expects on the wire; see each type's doc comment for the specific
// handler file/type it mirrors.
package sracacheclient

import (
	"context"

	"github.com/BV-BRC/sra_cache_service/internal/metadata"
	"github.com/BV-BRC/sra_cache_service/internal/rpcclient"
)

// PrecacheStatus mirrors internal/rpcapi/precache.go's unexported
// precacheStatusWire type field-for-field (which in turn mirrors
// SRACache.spec's precache_status typedef) -- both start_precache and
// precache_status return a list of these. precacheStatusWire is
// unexported, so this type is a deliberate, field-matching duplicate
// rather than an import; keep the two in sync if precache.go's shape ever
// changes.
type PrecacheStatus struct {
	Accession  string `json:"accession"`
	State      string `json:"state"`
	Message    string `json:"message"`
	SizeBytes  int64  `json:"size_bytes"`
	QueuedTime string `json:"queued_time"`
	CachedTime string `json:"cached_time"`
}

// SRADataLocation mirrors internal/rpcapi/data.go's unexported
// dataLocationWire type field-for-field (SRACache.spec's
// sra_data_location typedef) -- note there is deliberately no Message
// field here, unlike PrecacheStatus; see dataLocationWire's own doc
// comment on why (the typedef simply doesn't have one).
type SRADataLocation struct {
	Accession string `json:"accession"`
	State     string `json:"state"`
	URL       string `json:"url"`
	Expires   string `json:"expires"`
}

// Client is the typed SRACache client, wrapping an *rpcclient.Client.
type Client struct {
	rpc *rpcclient.Client
}

// New wraps rpc, an already-configured generic JSONRPC client (URL/Token/
// HTTPClient), as a typed SRACache client.
func New(rpc *rpcclient.Client) *Client {
	return &Client{rpc: rpc}
}

// GetMetadata calls SRACache.get_metadata, per internal/rpcapi/metadata.go:
//
//	funcdef get_metadata(list<sra_accession> ids)
//	    returns (list<sra_metadata> metadata) authentication optional;
//
// One row is returned per input accession that resolves to at least one
// run (an experiment/study id can expand to several); a syntactically
// invalid or NCBI-unknown accession still gets its own row, with
// Extra["error"] set to "invalid_id" or "not_found" rather than causing
// GetMetadata to return an error (see getMetadata's own doc comment in
// metadata.go) -- an error return here means the call itself failed
// (a genuine NCBI-outage error, or a transport/decode problem), not that
// any one accession was rejected.
func (c *Client) GetMetadata(ctx context.Context, accessions []string) ([]metadata.SRAMetadata, error) {
	var rows []metadata.SRAMetadata
	if err := c.rpc.Call(ctx, "SRACache.get_metadata", []any{accessions}, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// StartPrecache calls SRACache.start_precache, per
// internal/rpcapi/precache.go:
//
//	funcdef start_precache(list<sra_accession> ids, precache_options options)
//	    returns (list<precache_status> status) authentication required;
//
// The second positional argument (precache_options) is sent as JSON null;
// internal/rpcapi/precache.go's decodeIDsAndOptions tolerates that (and an
// altogether absent second element) since every field precache_options
// declares is currently unused server-side -- see precacheOptions' own doc
// comment there. This method requires an authenticated Client (see
// rpcclient.Client.Token) -- an empty token gets the same -32603
// "Authentication required" *rpcclient.RPCError precache_status's
// AuthOptional policy would never produce for the same caller.
func (c *Client) StartPrecache(ctx context.Context, accessions []string) ([]PrecacheStatus, error) {
	var rows []PrecacheStatus
	if err := c.rpc.Call(ctx, "SRACache.start_precache", []any{accessions, nil}, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// PrecacheStatus calls SRACache.precache_status, per
// internal/rpcapi/precache.go:
//
//	funcdef precache_status(list<sra_accession> ids)
//	    returns (list<precache_status> status) authentication optional;
//
// A pure read: unlike StartPrecache, this never enqueues anything, and
// succeeds with no Token set.
func (c *Client) PrecacheStatus(ctx context.Context, accessions []string) ([]PrecacheStatus, error) {
	var rows []PrecacheStatus
	if err := c.rpc.Call(ctx, "SRACache.precache_status", []any{accessions}, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// GetSRAData calls SRACache.get_sra_data, per internal/rpcapi/data.go:
//
//	funcdef get_sra_data(list<sra_accession> ids)
//	    returns (list<sra_data_location> locations) authentication optional;
//
// Despite this package's other three methods sharing a batch shape, this
// one is easy to get wrong by assumption: get_sra_data ALSO takes and
// returns a list, one row per requested accession in order -- it is not a
// single-accession call, even though a typical CLI use case (dump one
// accession) only ever passes one. internal/rpcapi/data.go's decodeIDs
// call confirms the list shape directly (shared with
// get_metadata/precache_status, not a special single-argument path).
//
// A not-yet-cached accession is enqueued as a side effect (see getSRAData's
// doc comment in data.go) and returned with an empty URL; a cached one
// comes back with a signed, short-lived URL and its pin-lease Expires.
func (c *Client) GetSRAData(ctx context.Context, accessions []string) ([]SRADataLocation, error) {
	var rows []SRADataLocation
	if err := c.rpc.Call(ctx, "SRACache.get_sra_data", []any{accessions}, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
