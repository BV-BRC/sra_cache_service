package rpcapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
	"github.com/BV-BRC/sra_cache_service/internal/metadata"
)

// fakeLookuper is a test-only MetadataLookuper. It records the ids it was
// actually called with, so a test can assert that an id rejected by
// internal/accession never reached it.
type fakeLookuper struct {
	rows       []metadata.SRAMetadata
	missing    []string
	err        error
	calledWith []string
}

func (f *fakeLookuper) Lookup(ctx context.Context, ids []string) ([]metadata.SRAMetadata, []string, error) {
	f.calledWith = ids
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.rows, f.missing, nil
}

// fakeValidator is a minimal jsonrpc.Validator; get_metadata is
// AuthOptional so none of these tests need it to ever succeed.
type fakeValidator struct{}

func (fakeValidator) Validate(token string) (string, error) {
	return "", errors.New("no real validation configured for this test")
}

// wireRow mirrors the JSON shape of SRACache.spec's sra_metadata typedef,
// for decoding the dispatcher's real HTTP response body -- this test goes
// through the actual wire format, not just the Go function.
type wireRow struct {
	Accession     string            `json:"accession"`
	Platform      string            `json:"platform"`
	LibraryLayout string            `json:"library_layout"`
	NReads        int               `json:"n_reads"`
	SizeBytes     int64             `json:"size_bytes"`
	Extra         map[string]string `json:"extra"`
}

func newServer(lookuper MetadataLookuper) *jsonrpc.Server {
	srv := jsonrpc.NewServer(fakeValidator{}, nil)
	svc := &Service{Metadata: lookuper}
	svc.RegisterGetMetadata(srv)
	return srv
}

func postGetMetadata(t *testing.T, srv *jsonrpc.Server, ids []string, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	idsJSON, err := json.Marshal(ids)
	if err != nil {
		t.Fatalf("marshaling ids: %v", err)
	}
	body := `{"params":[` + string(idsJSON) + `],"method":"SRACache.get_metadata","version":"1.1","id":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// A real JSONRPC round trip: a batch with one valid, resolving accession
// and one syntactically invalid one, posted with no Authorization header at
// all (get_metadata is AuthOptional -- this must still succeed).
func TestDispatchGetMetadata_MixedValidAndInvalid(t *testing.T) {
	fl := &fakeLookuper{
		rows: []metadata.SRAMetadata{
			{
				Accession:     "SRR40145022",
				Platform:      "ILLUMINA",
				LibraryLayout: "PAIRED",
				NReads:        2,
				SizeBytes:     12345,
				Extra:         map[string]string{"requested_accession": "SRR40145022"},
			},
		},
	}
	srv := newServer(fl)

	rec := postGetMetadata(t, srv, []string{"SRR40145022", "NOT-AN-ACCESSION"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp jsonrpc.SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v; body = %s", err, rec.Body.String())
	}
	if len(resp.Result) != 1 {
		t.Fatalf("result has %d elements, want 1 (arity)", len(resp.Result))
	}

	// resp.Result[0] decodes (via json.RawMessage round trip through `any`)
	// as []any of map[string]any when unmarshaled generically; re-marshal
	// and decode into the typed wire shape for convenient assertions.
	raw, err := json.Marshal(resp.Result[0])
	if err != nil {
		t.Fatalf("re-marshaling result[0]: %v", err)
	}
	var rows []wireRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decoding rows: %v; raw = %s", err, raw)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want 2", rows)
	}

	// The invalid accession must never have reached the lookuper.
	if len(fl.calledWith) != 1 || fl.calledWith[0] != "SRR40145022" {
		t.Errorf("lookuper called with %v, want exactly [SRR40145022] (the invalid id must be filtered out before Lookup)", fl.calledWith)
	}

	var okRow, badRow *wireRow
	for i := range rows {
		switch rows[i].Accession {
		case "SRR40145022":
			okRow = &rows[i]
		case "NOT-AN-ACCESSION":
			badRow = &rows[i]
		}
	}
	if okRow == nil {
		t.Fatal("no row for SRR40145022")
	}
	if okRow.Platform != "ILLUMINA" || okRow.LibraryLayout != "PAIRED" || okRow.NReads != 2 || okRow.SizeBytes != 12345 {
		t.Errorf("SRR40145022 row = %+v, unexpected fields", okRow)
	}

	if badRow == nil {
		t.Fatal("no row for NOT-AN-ACCESSION")
	}
	if badRow.Extra["error"] != "invalid_id" {
		t.Errorf("NOT-AN-ACCESSION extra[error] = %q, want invalid_id", badRow.Extra["error"])
	}
	if badRow.Extra["message"] == "" {
		t.Error("NOT-AN-ACCESSION extra[message] is empty, want a validation error")
	}
}

// A syntactically valid but NCBI-unknown accession comes back as its own
// not_found row, without failing accessions found in the same batch.
func TestDispatchGetMetadata_NotFound(t *testing.T) {
	fl := &fakeLookuper{
		rows:    nil,
		missing: []string{"SRR99999999"},
	}
	srv := newServer(fl)

	rec := postGetMetadata(t, srv, []string{"SRR99999999"}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp jsonrpc.SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	raw, _ := json.Marshal(resp.Result[0])
	var rows []wireRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decoding rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Accession != "SRR99999999" {
		t.Fatalf("rows = %+v, want one row for SRR99999999", rows)
	}
	if rows[0].Extra["error"] != "not_found" {
		t.Errorf("extra[error] = %q, want not_found", rows[0].Extra["error"])
	}
}

// A genuine NCBI-outage error from the lookuper must fail the whole RPC
// call (-32603), distinct from a per-accession invalid/not_found row.
func TestDispatchGetMetadata_LookupErrorFailsTheCall(t *testing.T) {
	fl := &fakeLookuper{err: errors.New("contacting NCBI: connection refused")}
	srv := newServer(fl)

	rec := postGetMetadata(t, srv, []string{"SRR40145022"}, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	var env jsonrpc.ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope: %v; body = %s", err, rec.Body.String())
	}
	if env.ErrorObj.Code != -32603 {
		t.Errorf("error.code = %d, want -32603", env.ErrorObj.Code)
	}
	if !strings.Contains(env.ErrorObj.Message, "connection refused") {
		t.Errorf("error.message = %q, want it to mention the underlying cause", env.ErrorObj.Message)
	}
}

// An Authorization header is accepted but irrelevant for this
// AuthOptional method -- a garbage token must not turn into an error.
func TestDispatchGetMetadata_GarbageTokenStillSucceeds(t *testing.T) {
	fl := &fakeLookuper{rows: []metadata.SRAMetadata{
		{Accession: "SRR40145022", Platform: "ILLUMINA"},
	}}
	srv := newServer(fl)

	rec := postGetMetadata(t, srv, []string{"SRR40145022"}, "garbage-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (AuthOptional + invalid token must still succeed); body = %s", rec.Code, rec.Body.String())
	}
}
