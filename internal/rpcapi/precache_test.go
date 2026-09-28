package rpcapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
)

// fakeWaker records how many times Wake was called, so a test can assert
// start_precache wakes the pool exactly when (and how often) it should.
type fakeWaker struct {
	calls int
}

func (f *fakeWaker) Wake() { f.calls++ }

// fakeSucceedingValidator always authenticates successfully -- unlike
// metadata_test.go's fakeValidator (which always fails, since get_metadata
// is AuthOptional and none of its tests need a token to actually validate).
// start_precache is AuthRequired, so exercising its non-auth-error behavior
// needs a validator that can actually succeed.
type fakeSucceedingValidator struct{}

func (fakeSucceedingValidator) Validate(token string) (string, error) {
	return "test-user", nil
}

func openTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

func newPrecacheServer(cat *catalog.Catalog, waker Waker, validator jsonrpc.Validator) *jsonrpc.Server {
	srv := jsonrpc.NewServer(validator, nil)
	svc := &Service{Catalog: cat, Pool: waker}
	svc.RegisterStartPrecache(srv)
	svc.RegisterPrecacheStatus(srv)
	return srv
}

type precacheWireRow struct {
	Accession  string `json:"accession"`
	State      string `json:"state"`
	Message    string `json:"message"`
	SizeBytes  int64  `json:"size_bytes"`
	QueuedTime string `json:"queued_time"`
	CachedTime string `json:"cached_time"`
}

func postRPC(t *testing.T, srv *jsonrpc.Server, method string, paramsJSON string, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"params":` + paramsJSON + `,"method":"` + method + `","version":"1.1","id":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decodeRows(t *testing.T, rec *httptest.ResponseRecorder) []precacheWireRow {
	t.Helper()
	var resp jsonrpc.SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v; body = %s", err, rec.Body.String())
	}
	if len(resp.Result) != 1 {
		t.Fatalf("result has %d elements, want 1 (arity); body = %s", len(resp.Result), rec.Body.String())
	}
	raw, err := json.Marshal(resp.Result[0])
	if err != nil {
		t.Fatalf("re-marshaling result[0]: %v", err)
	}
	var rows []precacheWireRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decoding rows: %v; raw = %s", err, raw)
	}
	return rows
}

// TestStartPrecacheEnqueuesAndWakesPool is the "start_precache -> queued"
// acceptance case: a run-level accession is enqueued, comes back as
// state=queued, and the pool is woken.
func TestStartPrecacheEnqueuesAndWakesPool(t *testing.T) {
	cat := openTestCatalog(t)
	waker := &fakeWaker{}
	srv := newPrecacheServer(cat, waker, fakeSucceedingValidator{})

	rec := postRPC(t, srv, "SRACache.start_precache", `[["SRR1234567"], null]`, "some-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0].Accession != "SRR1234567" {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].State != string(catalog.StateQueued) {
		t.Errorf("state = %q, want queued", rows[0].State)
	}
	if waker.calls != 1 {
		t.Errorf("Wake called %d times, want exactly 1", waker.calls)
	}

	e, ok, err := cat.Get(context.Background(), "SRR1234567")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.State != catalog.StateQueued {
		t.Errorf("catalog state = %q, want queued", e.State)
	}
}

// TestStartPrecacheSXPRejectedAsInvalidID is the design doc's explicit
// expansion-required case: an SRX/SRP id never reaches the catalog at all.
func TestStartPrecacheSRXRejectedAsInvalidID(t *testing.T) {
	cat := openTestCatalog(t)
	waker := &fakeWaker{}
	srv := newPrecacheServer(cat, waker, fakeSucceedingValidator{})

	rec := postRPC(t, srv, "SRACache.start_precache", `[["SRX000001"], null]`, "some-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].State != string(catalog.StateInvalidID) {
		t.Errorf("state = %q, want invalid_id", rows[0].State)
	}
	if !strings.Contains(rows[0].Message, "must be expanded via get_metadata first") {
		t.Errorf("message = %q, want the expansion-required wording", rows[0].Message)
	}

	// Never touched the catalog.
	if _, ok, err := cat.Get(context.Background(), "SRX000001"); err != nil || ok {
		t.Errorf("SRX000001 should never have reached the catalog: ok=%v err=%v", ok, err)
	}
	if waker.calls != 0 {
		t.Errorf("Wake called %d times, want 0 (nothing valid was enqueued)", waker.calls)
	}
}

// TestStartPrecacheMixedBatchPreservesOrderAndWakesOnce covers a batch with
// both a valid and an invalid id: request order is preserved, and Wake
// fires exactly once (not once per enqueued accession).
func TestStartPrecacheMixedBatchPreservesOrderAndWakesOnce(t *testing.T) {
	cat := openTestCatalog(t)
	waker := &fakeWaker{}
	srv := newPrecacheServer(cat, waker, fakeSucceedingValidator{})

	rec := postRPC(t, srv, "SRACache.start_precache", `[["SRP000001", "SRR1111111", "SRR2222222"], null]`, "some-token")
	rows := decodeRows(t, rec)
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3", rows)
	}
	if rows[0].Accession != "SRP000001" || rows[0].State != string(catalog.StateInvalidID) {
		t.Errorf("rows[0] = %+v, want SRP000001/invalid_id", rows[0])
	}
	if rows[1].Accession != "SRR1111111" || rows[1].State != string(catalog.StateQueued) {
		t.Errorf("rows[1] = %+v, want SRR1111111/queued", rows[1])
	}
	if rows[2].Accession != "SRR2222222" || rows[2].State != string(catalog.StateQueued) {
		t.Errorf("rows[2] = %+v, want SRR2222222/queued", rows[2])
	}
	if waker.calls != 1 {
		t.Errorf("Wake called %d times, want exactly 1 for the whole batch", waker.calls)
	}
}

// TestStartPrecacheMissingAuthorizationFails is the real dispatcher-level
// auth check -- not a bypassed unit call -- for start_precache's
// AuthRequired policy.
func TestStartPrecacheMissingAuthorizationFails(t *testing.T) {
	cat := openTestCatalog(t)
	srv := newPrecacheServer(cat, &fakeWaker{}, fakeValidator{})

	rec := postRPC(t, srv, "SRACache.start_precache", `[["SRR1234567"], null]`, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (the real -32603 auth error); body = %s", rec.Code, rec.Body.String())
	}
	var env jsonrpc.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope: %v; body = %s", err, rec.Body.String())
	}
	if env.ErrorObj.Code != -32603 {
		t.Errorf("error.code = %d, want -32603", env.ErrorObj.Code)
	}
	if !strings.Contains(env.ErrorObj.Message, "Authentication required for SRACache") {
		t.Errorf("error.message = %q, want the Authentication-required wording", env.ErrorObj.Message)
	}

	// Nothing should have been enqueued.
	if _, ok, err := cat.Get(context.Background(), "SRR1234567"); err != nil || ok {
		t.Errorf("SRR1234567 should never have reached the catalog: ok=%v err=%v", ok, err)
	}
}

// TestStartPrecacheGarbageTokenFails: AuthRequired means even a garbage
// (Validate-rejected) token is not enough -- distinct from AuthOptional's
// behavior on get_metadata/precache_status.
func TestStartPrecacheGarbageTokenFails(t *testing.T) {
	cat := openTestCatalog(t)
	srv := newPrecacheServer(cat, &fakeWaker{}, fakeValidator{})

	rec := postRPC(t, srv, "SRACache.start_precache", `[["SRR1234567"], null]`, "garbage-token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
}

// TestPrecacheStatusSucceedsWithoutAuthorization is the "precache_status
// without a token succeeds" acceptance case.
func TestPrecacheStatusSucceedsWithoutAuthorization(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.Upsert(context.Background(), catalog.Entry{
		Accession: "SRR1234567", State: catalog.StateCached, SizeBytes: 42,
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	srv := newPrecacheServer(cat, &fakeWaker{}, fakeValidator{})

	rec := postRPC(t, srv, "SRACache.precache_status", `[["SRR1234567"]]`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (AuthOptional, no header); body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0].State != string(catalog.StateCached) || rows[0].SizeBytes != 42 {
		t.Fatalf("rows = %+v", rows)
	}
}

// TestPrecacheStatusNeverEnqueues is the "pure read" contract: polling
// status for an accession nobody has start_precache'd must not create a
// catalog row, and must not wake the pool.
func TestPrecacheStatusNeverEnqueues(t *testing.T) {
	cat := openTestCatalog(t)
	waker := &fakeWaker{}
	srv := newPrecacheServer(cat, waker, fakeValidator{})

	rec := postRPC(t, srv, "SRACache.precache_status", `[["SRR9999999"]]`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0].Accession != "SRR9999999" || rows[0].State != "" {
		t.Fatalf("rows = %+v, want one empty-state row for an unregistered accession", rows)
	}
	if rows[0].Message == "" {
		t.Error("message should explain the accession was never requested")
	}
	if _, ok, err := cat.Get(context.Background(), "SRR9999999"); err != nil || ok {
		t.Errorf("precache_status must never create a catalog row: ok=%v err=%v", ok, err)
	}
	if waker.calls != 0 {
		t.Errorf("Wake called %d times, want 0 (precache_status must never wake the pool)", waker.calls)
	}
}

// TestPrecacheStatusInvalidIDAndGarbageTokenStillSucceeds combines the
// invalid_id short-circuit with an AuthOptional garbage token, mirroring
// TestDispatchGetMetadata_GarbageTokenStillSucceeds in metadata_test.go.
func TestPrecacheStatusInvalidIDAndGarbageTokenStillSucceeds(t *testing.T) {
	cat := openTestCatalog(t)
	srv := newPrecacheServer(cat, &fakeWaker{}, fakeValidator{})

	rec := postRPC(t, srv, "SRACache.precache_status", `[["NOT-AN-ACCESSION"]]`, "garbage-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeRows(t, rec)
	if len(rows) != 1 || rows[0].State != string(catalog.StateInvalidID) {
		t.Fatalf("rows = %+v, want one invalid_id row", rows)
	}
}

// TestEnqueueErrorFailsTheWholeCall asserts a genuine catalog failure (not
// a per-accession condition) fails start_precache as a whole, the same
// shape as TestDispatchGetMetadata_LookupErrorFailsTheCall.
func TestEnqueueErrorFailsTheWholeCall(t *testing.T) {
	cat := openTestCatalog(t)
	cat.Close() // force every subsequent catalog call to fail
	srv := newPrecacheServer(cat, &fakeWaker{}, fakeSucceedingValidator{})

	rec := postRPC(t, srv, "SRACache.start_precache", `[["SRR1234567"], null]`, "some-token")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	var env jsonrpc.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	if env.ErrorObj.Code != -32603 {
		t.Errorf("error.code = %d, want -32603", env.ErrorObj.Code)
	}
}

// TestValidateRunLevelTable is a direct table test of the shared helper.
func TestValidateRunLevelTable(t *testing.T) {
	cases := []struct {
		id      string
		wantErr bool
		wantMsg string
	}{
		{"SRR1234567", false, ""},
		{"DRR1234567", false, ""},
		{"ERR1234567", false, ""},
		{"SRX1234567", true, "must be expanded via get_metadata first"},
		{"SRP1234567", true, "must be expanded via get_metadata first"},
		{"NOT-AN-ACCESSION", true, ""},
	}
	for _, c := range cases {
		t.Run(c.id, func(t *testing.T) {
			err := validateRunLevel(c.id)
			if (err != nil) != c.wantErr {
				t.Fatalf("validateRunLevel(%q) error = %v, wantErr %v", c.id, err, c.wantErr)
			}
			if c.wantMsg != "" && (err == nil || !strings.Contains(err.Error(), c.wantMsg)) {
				t.Errorf("validateRunLevel(%q) = %v, want it to contain %q", c.id, err, c.wantMsg)
			}
		})
	}
}
