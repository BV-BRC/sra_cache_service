package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"

	"github.com/BV-BRC/BV-BRC-Go-SDK/sra"
)

// fixture returns the contents of a testdata/*.xml file.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return data
}

// serve starts an httptest server that always answers with the named
// fixture (the real efetch response does depend on the requested id= list,
// but every test here only ever queries accessions the chosen fixture can
// answer, so a fixed response is sufficient -- the same simplification
// BV-BRC-Go-SDK/sra/sra_test.go's own "serve" helper makes), and returns a
// Lookuper wrapping a real *sra.Client pointed at it. Using the real client
// (not a fake) means these tests exercise the genuine XML-parsing path
// through to this package's field mapping, not just the mapping logic in
// isolation.
func serve(t *testing.T, fixtureName string) *Lookuper {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(fixture(t, fixtureName))
	}))
	t.Cleanup(srv.Close)
	client := sra.New(sra.WithBaseURL(srv.URL), sra.WithHTTPClient(srv.Client()))
	return New(client)
}

func rowByAccession(rows []SRAMetadata, acc string) (SRAMetadata, bool) {
	for _, r := range rows {
		if r.Accession == acc {
			return r, true
		}
	}
	return SRAMetadata{}, false
}

// A mixed run/experiment/study id list where everything resolves: a bare
// run accession, an experiment accession belonging to a two-experiment
// study, and the study accession itself (which must expand to both of that
// study's runs).
func TestLookupMixedRunExperimentStudy(t *testing.T) {
	l := serve(t, "mixed.xml")

	rows, missing, err := l.Lookup(context.Background(), []string{
		"SRR70000001", "SRX70000002", "SRP70000000",
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(missing) != 0 {
		t.Fatalf("missing = %v, want none", missing)
	}

	// SRR70000001: a direct run-level request, SINGLE layout, no
	// Statistics block -> NReads falls back to 1.
	run1, ok := rowByAccession(rows, "SRR70000001")
	if !ok {
		t.Fatal("SRR70000001 not found in rows")
	}
	if run1.Platform != "ILLUMINA" {
		t.Errorf("SRR70000001 Platform = %q, want ILLUMINA", run1.Platform)
	}
	if run1.LibraryLayout != "SINGLE" {
		t.Errorf("SRR70000001 LibraryLayout = %q, want SINGLE", run1.LibraryLayout)
	}
	if run1.NReads != 1 {
		t.Errorf("SRR70000001 NReads = %d, want 1 (fallback for a non-PAIRED layout)", run1.NReads)
	}
	if run1.SizeBytes != 90000 {
		t.Errorf("SRR70000001 SizeBytes = %d, want 90000", run1.SizeBytes)
	}
	if run1.Extra["requested_accession"] != "SRR70000001" {
		t.Errorf("SRR70000001 requested_accession = %q, want SRR70000001", run1.Extra["requested_accession"])
	}

	// SRX70000002: an experiment-level request naming one run in the
	// study; must resolve to exactly that run, PAIRED layout, no
	// Statistics -> NReads falls back to 2.
	var fromExperiment []SRAMetadata
	for _, r := range rows {
		if r.Extra["requested_accession"] == "SRX70000002" {
			fromExperiment = append(fromExperiment, r)
		}
	}
	if len(fromExperiment) != 1 || fromExperiment[0].Accession != "SRR70000002" {
		t.Fatalf("rows for SRX70000002 = %+v, want exactly [SRR70000002]", fromExperiment)
	}
	if fromExperiment[0].NReads != 2 {
		t.Errorf("SRR70000002 (via SRX70000002) NReads = %d, want 2 (PAIRED fallback)", fromExperiment[0].NReads)
	}

	// SRP70000000: the study accession must expand to BOTH runs in the
	// study (SRR70000002 and SRR70000003), each still reporting the study
	// as their requested_accession for that group of rows.
	var fromStudy []string
	for _, r := range rows {
		if r.Extra["requested_accession"] == "SRP70000000" {
			fromStudy = append(fromStudy, r.Accession)
		}
	}
	sort.Strings(fromStudy)
	want := []string{"SRR70000002", "SRR70000003"}
	if len(fromStudy) != len(want) || fromStudy[0] != want[0] || fromStudy[1] != want[1] {
		t.Errorf("rows for SRP70000000 = %v, want %v", fromStudy, want)
	}

	run3, ok := rowByAccession(rows, "SRR70000003")
	if !ok {
		t.Fatal("SRR70000003 not found in rows")
	}
	if run3.Extra["study_accession"] != "SRP70000000" {
		t.Errorf("SRR70000003 study_accession = %q, want SRP70000000", run3.Extra["study_accession"])
	}
	if run3.Extra["study_title"] != "Study spanning two experiment packages" {
		t.Errorf("SRR70000003 study_title = %q", run3.Extra["study_title"])
	}
}

// A syntactically valid id that's simply unknown to NCBI comes back as
// "missing", not an error and not a zero-value row.
func TestLookupGarbageIDIsMissing(t *testing.T) {
	l := serve(t, "mixed.xml")

	rows, missing, err := l.Lookup(context.Background(), []string{
		"SRR70000001", "SRR99999999",
	})
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if len(missing) != 1 || missing[0] != "SRR99999999" {
		t.Errorf("missing = %v, want [SRR99999999]", missing)
	}
	if _, ok := rowByAccession(rows, "SRR70000001"); !ok {
		t.Error("SRR70000001 should still be found")
	}
	for _, r := range rows {
		if r.Accession == "SRR99999999" {
			t.Errorf("SRR99999999 should not appear as a row, got %+v", r)
		}
	}
}

// The real-world SRP-batch-expansion quirk (see BV-BRC-Go-SDK/sra's
// LookupAll doc comment): NCBI's efetch does not resolve a bare SRP id as
// an id= value on its own. When the SRP is batched with an SRR that
// resolves but belongs to a DIFFERENT, unrelated study (so nothing in the
// response NCBI actually returns carries a matching study accession
// either), the SRP must come back as missing -- not as an error, and
// without taking the resolvable SRR down with it.
func TestLookupSRPThatFailsToExpandAlongsideAResolvingSRR(t *testing.T) {
	l := serve(t, "srp_batch_quirk.xml")

	rows, missing, err := l.Lookup(context.Background(), []string{
		"SRP80000000", // never appears anywhere in the fixture
		"SRR80000001", // resolves, but belongs to SRP80000099, not SRP80000000
	})
	if err != nil {
		t.Fatalf("Lookup returned an error for an SRP that failed to expand: %v", err)
	}
	if len(missing) != 1 || missing[0] != "SRP80000000" {
		t.Fatalf("missing = %v, want [SRP80000000]", missing)
	}
	row, ok := rowByAccession(rows, "SRR80000001")
	if !ok {
		t.Fatal("SRR80000001 should have resolved despite the SRP's failed expansion")
	}
	if row.Platform != "ILLUMINA" || row.LibraryLayout != "SINGLE" {
		t.Errorf("SRR80000001 row = %+v, unexpected fields", row)
	}
}

func TestNReadsFallback(t *testing.T) {
	cases := []struct {
		layout string
		stated int
		want   int
	}{
		{"PAIRED", 0, 2},
		{"SINGLE", 0, 1},
		{"", 0, 1},
		{"PAIRED", 4, 4}, // a real Statistics-derived count always wins
		{"SINGLE", 3, 3},
	}
	for _, c := range cases {
		if got := nReads(c.layout, c.stated); got != c.want {
			t.Errorf("nReads(%q, %d) = %d, want %d", c.layout, c.stated, got, c.want)
		}
	}
}
