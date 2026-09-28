package dumper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/BV-BRC/sra_cache_service/internal/metadata"
)

func TestGlobDumpedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"SRR1234567_1.fastq", "SRR1234567_2.fastq", "other.txt", "SRR7654321_1.fastq"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := GlobDumpedFiles(dir, "SRR1234567")
	if err != nil {
		t.Fatalf("GlobDumpedFiles: %v", err)
	}
	want := []string{"SRR1234567_1.fastq", "SRR1234567_2.fastq"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestBuildMetaRecordsLoadBearingFields confirms every field
// PLAN-sra-precache-service.md section 7.2 identified as read by a real
// p3-sra --metadata-file consumer is present under its ORIGINAL sra_tools.py
// key name (not this service's own get_metadata renaming).
func TestBuildMetaRecordsLoadBearingFields(t *testing.T) {
	rows := []metadata.SRAMetadata{
		{
			Accession:     "SRR1234567",
			Platform:      "ILLUMINA",
			LibraryLayout: "PAIRED",
			NReads:        2,
			SizeBytes:     123456,
			Extra:         map[string]string{"study_accession": "SRP000001"},
		},
	}
	files := map[string][]string{"SRR1234567": {"SRR1234567_1.fastq", "SRR1234567_2.fastq"}}

	recs := BuildMetaRecords(rows, files)
	if len(recs) != 1 {
		t.Fatalf("len(recs) = %d, want 1", len(recs))
	}
	rec := recs[0]

	checks := map[string]any{
		"accession":      "SRR1234567",
		"platform_name":  "ILLUMINA",
		"library_layout": "PAIRED",
		"n_reads":        2,
		"size":           int64(123456),
		"run_id":         "SRR1234567",
	}
	for k, want := range checks {
		got, ok := rec[k]
		if !ok {
			t.Errorf("record missing key %q", k)
			continue
		}
		if got != want {
			t.Errorf("record[%q] = %v (%T), want %v (%T)", k, got, got, want, want)
		}
	}

	filesVal, ok := rec["files"].([]string)
	if !ok || len(filesVal) != 2 {
		t.Errorf(`record["files"] = %v, want a 2-element []string`, rec["files"])
	}

	if rec["study_accession"] != "SRP000001" {
		t.Errorf(`record["study_accession"] = %v, want "SRP000001" (Extra must be merged through)`, rec["study_accession"])
	}
}

// TestBuildMetaRecordsMetaOnlyOmitsFiles confirms "files" is absent (not
// present-but-empty) when no post-dump listing exists for an accession --
// matching sra_tools.py's own --metaonly behavior of never setting "files"
// at all in that mode.
func TestBuildMetaRecordsMetaOnlyOmitsFiles(t *testing.T) {
	rows := []metadata.SRAMetadata{{Accession: "SRR1234567", Platform: "ILLUMINA"}}
	recs := BuildMetaRecords(rows, nil)
	if _, ok := recs[0]["files"]; ok {
		t.Errorf(`record has a "files" key with no post-dump listing supplied, want it absent`)
	}
}

// TestBuildMetaRecordsErrorRowSurvives confirms a get_metadata rejection row
// (Extra["error"]/["message"] set, everything else zero) still produces a
// record -- not silently dropped -- so a --metadata-file reader can see why
// an accession has no data.
func TestBuildMetaRecordsErrorRowSurvives(t *testing.T) {
	rows := []metadata.SRAMetadata{{
		Accession: "SRX000001",
		Extra:     map[string]string{"error": "invalid_id", "message": "bad accession"},
	}}
	recs := BuildMetaRecords(rows, nil)
	if len(recs) != 1 {
		t.Fatalf("len(recs) = %d, want 1", len(recs))
	}
	if recs[0]["error"] != "invalid_id" || recs[0]["message"] != "bad accession" {
		t.Errorf("record = %+v, want error/message carried through", recs[0])
	}
}

func TestWriteMetadataFile(t *testing.T) {
	recs := []map[string]any{{"accession": "SRR1234567", "n_reads": 2}}
	path := filepath.Join(t.TempDir(), "meta.json")
	if err := WriteMetadataFile(path, recs); err != nil {
		t.Fatalf("WriteMetadataFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written file: %v", err)
	}
	var got []map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("decoding written JSON: %v", err)
	}
	if len(got) != 1 || got[0]["accession"] != "SRR1234567" {
		t.Errorf("got = %+v", got)
	}
}
