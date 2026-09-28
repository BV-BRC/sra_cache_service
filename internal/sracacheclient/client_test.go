package sracacheclient

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
	"github.com/BV-BRC/sra_cache_service/internal/metadata"
	"github.com/BV-BRC/sra_cache_service/internal/rpcapi"
	"github.com/BV-BRC/sra_cache_service/internal/rpcclient"
)

// fakeValidator accepts exactly one token, matching the fake-Validator
// approach the implementation plan directs for anything that touches
// start_precache: the real dev binary's NoopValidator (main.go) rejects
// every token unconditionally, since the real p3auth-backed validator is
// still a follow-up, so there is currently no way to exercise a
// *successful* start_precache call against any actually-running instance
// of this service. This fake stands in for that, in a local test server
// only.
type fakeValidator struct{ token string }

func (f fakeValidator) Validate(token string) (string, error) {
	if token == f.token {
		return "test-user", nil
	}
	return "", errors.New("invalid token")
}

// fakeMetadataLookuper is a realistic fake of rpcapi.MetadataLookuper: it
// answers "SRR0000000" as not found (mirroring what internal/metadata.Lookup
// reports for a syntactically valid accession LookupAll's underlying eutils
// call didn't match -- getMetadata (metadata.go) only ever calls Lookup with
// accessions that already passed accession.Validate, so the fake's sentinel
// has to be a well-formed run-level accession too, not an arbitrary string)
// and every other accession with one canned run-level row -- enough to
// exercise get_metadata's field mapping without a real NCBI round trip.
type fakeMetadataLookuper struct{}

func (fakeMetadataLookuper) Lookup(ctx context.Context, ids []string) ([]metadata.SRAMetadata, []string, error) {
	var rows []metadata.SRAMetadata
	var missing []string
	for _, id := range ids {
		if id == "SRR0000000" {
			missing = append(missing, id)
			continue
		}
		rows = append(rows, metadata.SRAMetadata{
			Accession:     id,
			Platform:      "ILLUMINA",
			LibraryLayout: "PAIRED",
			NReads:        2,
			SizeBytes:     123456,
			Extra:         map[string]string{"study_accession": "SRPTEST"},
		})
	}
	return rows, missing, nil
}

// fakeSigner is a realistic fake of rpcapi.URLSigner -- deterministic, so
// tests can assert on the exact minted URL without a real HMAC secret.
type fakeSigner struct{}

func (fakeSigner) Mint(accession string, ttl time.Duration) (string, time.Time, error) {
	return "http://data.example.org/sra/" + accession + ".sra", time.Now().Add(ttl), nil
}

// fakeWaker is rpcapi.Waker's minimal fake -- this package's tests don't
// care whether the pool was woken, only that the RPC call itself completed
// correctly.
type fakeWaker struct{}

func (fakeWaker) Wake() {}

// newTestServer wires a real internal/jsonrpc.Server to real
// internal/rpcapi handlers -- all four methods -- backed by a real
// (temp-file) catalog.Catalog and the fakes above for the pieces that
// would otherwise need a real NCBI/prefetch/HMAC-secret dependency. This
// is deliberately not a hand-rolled fake RPC layer: it is the actual
// dispatcher and the actual handler code this client talks to in
// production, so a decoding bug in either this package or internal/rpcapi
// would show up here.
func newTestServer(t *testing.T, validToken string) (*httptest.Server, *catalog.Catalog) {
	t.Helper()
	cat, err := catalog.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	rpcSrv := jsonrpc.NewServer(fakeValidator{token: validToken}, nil)
	svc := &rpcapi.Service{
		Metadata:         fakeMetadataLookuper{},
		Catalog:          cat,
		Pool:             fakeWaker{},
		Signer:           fakeSigner{},
		DownloadLifetime: time.Hour,
	}
	svc.RegisterGetMetadata(rpcSrv)
	svc.RegisterStartPrecache(rpcSrv)
	svc.RegisterPrecacheStatus(rpcSrv)
	svc.RegisterGetSRAData(rpcSrv)

	ts := httptest.NewServer(rpcSrv)
	t.Cleanup(ts.Close)
	return ts, cat
}

func TestGetMetadata(t *testing.T) {
	ts, _ := newTestServer(t, "test-token")
	c := New(&rpcclient.Client{URL: ts.URL})

	rows, err := c.GetMetadata(context.Background(), []string{"SRR1234567", "SRR0000000"})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if rows[0].Accession != "SRR1234567" || rows[0].Platform != "ILLUMINA" || rows[0].LibraryLayout != "PAIRED" || rows[0].NReads != 2 || rows[0].SizeBytes != 123456 {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[0].Extra["study_accession"] != "SRPTEST" {
		t.Errorf("rows[0].Extra = %+v, want study_accession=SRPTEST", rows[0].Extra)
	}
	if rows[1].Accession != "SRR0000000" || rows[1].Extra["error"] != "not_found" {
		t.Errorf("rows[1] = %+v, want a not_found error row", rows[1])
	}
}

func TestStartPrecacheRequiresAuth(t *testing.T) {
	ts, cat := newTestServer(t, "test-token")
	c := New(&rpcclient.Client{URL: ts.URL}) // no token

	_, err := c.StartPrecache(context.Background(), []string{"SRR1234567"})
	if err == nil {
		t.Fatal("StartPrecache: expected an auth error with no token, got nil")
	}
	var rerr *rpcclient.RPCError
	if !errors.As(err, &rerr) {
		t.Fatalf("StartPrecache error = %T (%v), want *rpcclient.RPCError", err, err)
	}
	if rerr.Code != -32603 {
		t.Errorf("rerr.Code = %d, want -32603", rerr.Code)
	}

	if _, ok, err := cat.Get(context.Background(), "SRR1234567"); err != nil || ok {
		t.Errorf("SRR1234567 should never have reached the catalog: ok=%v err=%v", ok, err)
	}
}

func TestStartPrecacheWithValidTokenEnqueues(t *testing.T) {
	ts, cat := newTestServer(t, "test-token")
	c := New(&rpcclient.Client{URL: ts.URL, Token: "test-token"})

	rows, err := c.StartPrecache(context.Background(), []string{"SRR1234567", "SRX000001"})
	if err != nil {
		t.Fatalf("StartPrecache: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if rows[0].Accession != "SRR1234567" || rows[0].State != string(catalog.StateQueued) {
		t.Errorf("rows[0] = %+v, want SRR1234567/queued", rows[0])
	}
	if rows[1].Accession != "SRX000001" || rows[1].State != string(catalog.StateInvalidID) {
		t.Errorf("rows[1] = %+v, want SRX000001/invalid_id", rows[1])
	}

	e, ok, err := cat.Get(context.Background(), "SRR1234567")
	if err != nil || !ok || e.State != catalog.StateQueued {
		t.Errorf("catalog entry = %+v ok=%v err=%v, want queued", e, ok, err)
	}
}

func TestStartPrecacheGarbageTokenFails(t *testing.T) {
	ts, _ := newTestServer(t, "test-token")
	c := New(&rpcclient.Client{URL: ts.URL, Token: "wrong-token"})

	_, err := c.StartPrecache(context.Background(), []string{"SRR1234567"})
	if err == nil {
		t.Fatal("StartPrecache: expected an error with a rejected token, got nil")
	}
}

func TestPrecacheStatusSucceedsWithoutAuth(t *testing.T) {
	ts, cat := newTestServer(t, "test-token")
	if err := cat.Upsert(context.Background(), catalog.Entry{
		Accession: "SRR1234567", State: catalog.StateCached, SizeBytes: 42,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c := New(&rpcclient.Client{URL: ts.URL}) // no token

	rows, err := c.PrecacheStatus(context.Background(), []string{"SRR1234567", "SRR9999999"})
	if err != nil {
		t.Fatalf("PrecacheStatus: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}
	if rows[0].Accession != "SRR1234567" || rows[0].State != string(catalog.StateCached) || rows[0].SizeBytes != 42 {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[1].Accession != "SRR9999999" || rows[1].State != "" {
		t.Errorf("rows[1] = %+v, want an empty-state 'never requested' row", rows[1])
	}
}

func TestGetSRADataCached(t *testing.T) {
	ts, cat := newTestServer(t, "test-token")
	if err := cat.Upsert(context.Background(), catalog.Entry{
		Accession: "SRR1234567", State: catalog.StateCached, SizeBytes: 42,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	c := New(&rpcclient.Client{URL: ts.URL}) // AuthOptional, no token needed

	rows, err := c.GetSRAData(context.Background(), []string{"SRR1234567"})
	if err != nil {
		t.Fatalf("GetSRAData: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	if rows[0].State != string(catalog.StateCached) {
		t.Errorf("state = %q, want cached", rows[0].State)
	}
	if rows[0].URL != "http://data.example.org/sra/SRR1234567.sra" {
		t.Errorf("url = %q, want the fakeSigner's minted URL", rows[0].URL)
	}
	if rows[0].Expires == "" {
		t.Error("expires is empty, want the minted lease timestamp")
	}

	// Getting a signed URL must also pin the catalog row (the eviction
	// lease -- see getSRAData's doc comment in data.go).
	e, ok, err := cat.Get(context.Background(), "SRR1234567")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.PinnedUntil.IsZero() {
		t.Error("PinnedUntil is zero, want it set to the minted expiry")
	}
}

func TestGetSRADataNotYetCachedEnqueuesAndReturnsList(t *testing.T) {
	ts, cat := newTestServer(t, "test-token")
	c := New(&rpcclient.Client{URL: ts.URL})

	// This exercises the "takes and returns a list" shape directly: two
	// accessions in one call, neither previously known to the catalog.
	rows, err := c.GetSRAData(context.Background(), []string{"SRR1111111", "SRR2222222"})
	if err != nil {
		t.Fatalf("GetSRAData: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2 (one per requested accession)", rows)
	}
	for i, r := range rows {
		if r.URL != "" {
			t.Errorf("rows[%d].URL = %q, want empty (not cached yet)", i, r.URL)
		}
		if r.State != string(catalog.StateQueued) {
			t.Errorf("rows[%d].State = %q, want queued", i, r.State)
		}
	}

	if _, ok, err := cat.Get(context.Background(), "SRR1111111"); err != nil || !ok {
		t.Errorf("SRR1111111 should have been enqueued as a side effect: ok=%v err=%v", ok, err)
	}
}
