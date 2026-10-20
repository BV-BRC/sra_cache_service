package metadata

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BV-BRC/BV-BRC-Go-SDK/v2/sra"
)

// TestAcceptanceAgainstRealP3Sra is the "real acceptance test" called out in
// PLAN-sra-cache-service-implementation.md's Phase 4: for real accessions,
// diff this package's Lookup output against what
// sra_import/scripts/p3-sra.py --metaonly --metadata-file actually produces
// for the SAME accessions, over the real network, both hitting real NCBI
// eutils. This is what actually protects the downstream p3-sra metadata
// readers the design doc identified -- a canned-fixture test cannot catch a
// real behavioral drift between this package's mapping and
// sra_tools.parse_accession_metadata's.
//
// Gated behind BVBRC_TEST_INTEGRATION=1, matching the existing SDK
// convention (BV-BRC-Go-SDK/sra/sra_test.go's TestLookupAllLiveEutils).
//
// Running p3-sra.py needs a Python with lxml + requests, which the system
// python3 on this host does NOT have -- only the dev_container's runtime
// python3 (/vol/patric3/cli/ubuntu-runtime/bin/python3, confirmed to carry
// lxml 4.9.3 and requests 2.31.0) does. P3_SRA_PYTHON/P3_SRA_SCRIPT/
// P3_SRA_LIB let a different host override all three paths; absent either
// the interpreter or the script, the test skips with a clear message
// instead of failing, per the plan's "say so clearly" instruction.
func TestAcceptanceAgainstRealP3Sra(t *testing.T) {
	if os.Getenv("BVBRC_TEST_INTEGRATION") == "" {
		t.Skip("Skipping live acceptance test (set BVBRC_TEST_INTEGRATION=1 to run)")
	}

	pythonBin := os.Getenv("P3_SRA_PYTHON")
	if pythonBin == "" {
		pythonBin = "/vol/patric3/cli/ubuntu-runtime/bin/python3"
	}
	scriptPath := os.Getenv("P3_SRA_SCRIPT")
	if scriptPath == "" {
		scriptPath = "../../../sra_import/scripts/p3-sra.py"
	}
	libPath := os.Getenv("P3_SRA_LIB")
	if libPath == "" {
		libPath = "../../../sra_import/lib"
	}

	if _, err := os.Stat(pythonBin); err != nil {
		t.Skipf("p3-sra.py's python interpreter not found at %s (needs lxml+requests; override with P3_SRA_PYTHON): %v", pythonBin, err)
	}
	if _, err := os.Stat(scriptPath); err != nil {
		t.Skipf("p3-sra.py not found at %s (override with P3_SRA_SCRIPT): %v", scriptPath, err)
	}
	if _, err := os.Stat(libPath); err != nil {
		t.Skipf("sra_tools.py's directory not found at %s (override with P3_SRA_LIB): %v", libPath, err)
	}

	l := New(sra.New())
	py := pyRunner{python: pythonBin, script: scriptPath, lib: libPath}

	// One Illumina run, queried directly.
	t.Run("SRR40145022_run", func(t *testing.T) {
		compareOneAccession(t, py, l, "SRR40145022")
	})
	// One SRX experiment (the same run's enclosing experiment) -- must
	// expand to the same single run.
	t.Run("SRX34779904_experiment", func(t *testing.T) {
		compareOneAccession(t, py, l, "SRX34779904")
	})
	// One SRP study, queried ALONE. This is the documented real-world
	// finding (see this package's Lookup doc comment and
	// BV-BRC-Go-SDK/sra's LookupAll doc comment): NCBI's efetch does not
	// resolve a bare SRP id as an id= value on its own, so p3-sra.py's own
	// single-id query gets nothing back either -- confirmed live 2026-09:
	// `p3-sra.py --id SRP393881 --metaonly` writes an empty `[]`. The
	// parity this asserts is that both tools fail to expand it the same
	// way, not that either successfully lists the study's runs.
	t.Run("SRP393881_study_known_quirk", func(t *testing.T) {
		pyRows := py.run(t, "SRP393881")
		if len(pyRows) != 0 {
			t.Fatalf("p3-sra.py returned %d rows for a bare SRP393881 query; the known-quirk assumption this test documents no longer holds -- NCBI or p3-sra.py's behavior may have changed", len(pyRows))
		}
		rows, missing, err := l.Lookup(context.Background(), []string{"SRP393881"})
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		if len(rows) != 0 {
			t.Errorf("Lookup returned %d rows for a bare SRP393881 query, want 0 (matching p3-sra.py's own inability to expand it alone)", len(rows))
		}
		if len(missing) != 1 || missing[0] != "SRP393881" {
			t.Errorf("missing = %v, want [SRP393881]", missing)
		}
	})
}

// compareOneAccession runs both tools against acc and asserts the fields
// SRACache.spec's sra_metadata typedef declares agree row for row (matched
// by run accession), allowing the platform_name->platform,
// size->size_bytes renames the design doc specifies.
func compareOneAccession(t *testing.T, py pyRunner, l *Lookuper, acc string) {
	t.Helper()

	pyRows := py.run(t, acc)
	if len(pyRows) == 0 {
		t.Fatalf("p3-sra.py returned no rows for %s", acc)
	}
	pyByAccession := make(map[string]map[string]any, len(pyRows))
	for _, r := range pyRows {
		runID, _ := r["accession"].(string)
		if runID == "" {
			t.Fatalf("p3-sra.py row missing 'accession': %+v", r)
		}
		pyByAccession[runID] = r
	}

	goRows, missing, err := l.Lookup(context.Background(), []string{acc})
	if err != nil {
		t.Fatalf("Lookup(%s): %v", acc, err)
	}
	if len(missing) != 0 {
		t.Fatalf("Lookup(%s): missing = %v, want none", acc, missing)
	}
	if len(goRows) != len(pyRows) {
		t.Fatalf("Lookup(%s) returned %d rows, p3-sra.py returned %d", acc, len(goRows), len(pyRows))
	}

	for _, goRow := range goRows {
		pyRow, ok := pyByAccession[goRow.Accession]
		if !ok {
			t.Errorf("Go row for run %s has no matching p3-sra.py row", goRow.Accession)
			continue
		}

		if wantPlatform, _ := pyRow["platform_name"].(string); wantPlatform != goRow.Platform {
			t.Errorf("%s: Platform = %q, p3-sra.py platform_name = %q", goRow.Accession, goRow.Platform, wantPlatform)
		}
		if wantLayout, _ := pyRow["library_layout"].(string); wantLayout != goRow.LibraryLayout {
			t.Errorf("%s: LibraryLayout = %q, p3-sra.py library_layout = %q", goRow.Accession, goRow.LibraryLayout, wantLayout)
		}
		if wantNReads, ok := numberField(pyRow, "n_reads"); ok && int(wantNReads) != goRow.NReads {
			t.Errorf("%s: NReads = %d, p3-sra.py n_reads = %v", goRow.Accession, goRow.NReads, wantNReads)
		}
		if wantSize, ok := numberField(pyRow, "size"); ok && int64(wantSize) != goRow.SizeBytes {
			t.Errorf("%s: SizeBytes = %d, p3-sra.py size = %v", goRow.Accession, goRow.SizeBytes, wantSize)
		}
	}
}

// numberField reads a JSON-decoded numeric field (always float64 after
// encoding/json decodes into map[string]any) out of row.
func numberField(row map[string]any, key string) (float64, bool) {
	v, ok := row[key]
	if !ok {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

// pyRunner shells out to the real p3-sra.py for one accession at a time
// (matching its actual CLI, which only ever takes a single --id) and
// returns its parsed --metadata-file output.
type pyRunner struct {
	python string
	script string
	lib    string
}

func (p pyRunner) run(t *testing.T, acc string) []map[string]any {
	t.Helper()

	dir := t.TempDir()
	metaFile := filepath.Join(dir, acc+".json")

	cmd := exec.Command(p.python, p.script,
		"--id", acc,
		"--metaonly",
		"--metadata-file", metaFile,
	)
	cmd.Env = append(os.Environ(), "PYTHONPATH="+p.lib)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running p3-sra.py --id %s: %v\noutput:\n%s", acc, err, out)
	}

	data, err := os.ReadFile(metaFile)
	if err != nil {
		t.Fatalf("reading p3-sra.py's metadata file for %s: %v\np3-sra.py output:\n%s", acc, err, out)
	}

	var rows []map[string]any
	if err := json.Unmarshal(data, &rows); err != nil {
		t.Fatalf("decoding p3-sra.py's metadata JSON for %s: %v\ncontent:\n%s", acc, err, data)
	}
	return rows
}
