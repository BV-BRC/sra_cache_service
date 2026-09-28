package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/metadata"
	"github.com/BV-BRC/sra_cache_service/internal/sracacheclient"
)

// --- resolveAccessions / resolveOutDir / splitOpts ---

func TestResolveAccessions(t *testing.T) {
	cases := []struct {
		name string
		args []string
		id   string
		want []string
	}{
		{"legacy --id only, no positional args", nil, "SRR1234567", []string{"SRR1234567"}},
		{"positional args only", []string{"SRR1", "SRR2"}, "", []string{"SRR1", "SRR2"}},
		{"both combined", []string{"SRR1"}, "SRR2", []string{"SRR1", "SRR2"}},
		{"neither", nil, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := resolveAccessions(c.args, c.id)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("resolveAccessions(%v, %q) = %v, want %v", c.args, c.id, got, c.want)
			}
		})
	}
}

func TestResolveOutDir(t *testing.T) {
	cases := []struct{ outDir, outAlias, want string }{
		{"/explicit", "/alias", "/explicit"},
		{"", "/alias", "/alias"},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := resolveOutDir(c.outDir, c.outAlias); got != c.want {
			t.Errorf("resolveOutDir(%q,%q) = %q, want %q", c.outDir, c.outAlias, got, c.want)
		}
	}
}

func TestSplitOpts(t *testing.T) {
	if got := splitOpts(""); got != nil {
		t.Errorf("splitOpts(\"\") = %v, want nil", got)
	}
	if got := splitOpts("  "); got != nil {
		t.Errorf("splitOpts(whitespace) = %v, want nil", got)
	}
	got := splitOpts(" --threads  4 ")
	want := []string{"--threads", "4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("splitOpts = %v, want %v", got, want)
	}
}

// --- fakeClient for runDump unit tests ---

type fakeClient struct {
	metaRows  []metadata.SRAMetadata
	metaErr   error
	dataSeq   [][]sracacheclient.SRADataLocation
	dataIdx   int
	dataCalls int
}

func (f *fakeClient) GetMetadata(ctx context.Context, accessions []string) ([]metadata.SRAMetadata, error) {
	return f.metaRows, f.metaErr
}

func (f *fakeClient) GetSRAData(ctx context.Context, accessions []string) ([]sracacheclient.SRADataLocation, error) {
	f.dataCalls++
	if len(f.dataSeq) == 0 {
		return nil, nil
	}
	i := f.dataIdx
	if i >= len(f.dataSeq) {
		i = len(f.dataSeq) - 1
	} else {
		f.dataIdx++
	}
	return f.dataSeq[i], nil
}

func defaultCfg() dumpConfig {
	return dumpConfig{Format: "fastq", PollInterval: time.Millisecond}
}

// --- flag validation ---

func TestRunDumpRequiresAtLeastOneAccession(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runDump(context.Background(), &out, &errOut, &fakeClient{}, http.DefaultClient, nil, defaultCfg())
	assertExitCode(t, err, 1)
}

func TestRunDumpRequiresOutDirUnlessMetaOnly(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg := defaultCfg()
	err := runDump(context.Background(), &out, &errOut, &fakeClient{}, http.DefaultClient, []string{"SRR1234567"}, cfg)
	assertExitCode(t, err, 1)

	// --metaonly must not require an outdir.
	cfg.MetaOnly = true
	fc := &fakeClient{metaRows: []metadata.SRAMetadata{{Accession: "SRR1234567", Platform: "ILLUMINA"}}}
	err = runDump(context.Background(), &out, &errOut, fc, http.DefaultClient, []string{"SRR1234567"}, cfg)
	if err != nil {
		t.Fatalf("runDump with --metaonly and no outdir: %v, want nil", err)
	}
}

func TestRunDumpInvalidFormat(t *testing.T) {
	var out, errOut bytes.Buffer
	cfg := defaultCfg()
	cfg.Format = "bam"
	cfg.OutDir = t.TempDir()
	err := runDump(context.Background(), &out, &errOut, &fakeClient{}, http.DefaultClient, []string{"SRR1234567"}, cfg)
	assertExitCode(t, err, 1)
}

func assertExitCode(t *testing.T, err error, want int) {
	t.Helper()
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != want {
		t.Fatalf("err = %v, want an *exitCodeError with code %d", err, want)
	}
}

// --- --metaonly must never invoke any dump tool ---

// TestRunDumpMetaOnlyNeverInvokesDumpTool asserts this directly: no
// fasterq-dump/fastq-dump fakes are placed on $PATH at all, so if runDump's
// --metaonly path ever tried to exec one, the test would fail with an exec
// error (binary not found) rather than silently succeeding. It also asserts
// GetSRAData -- the RPC call that would need to precede any dump -- is never
// called.
func TestRunDumpMetaOnlyNeverInvokesDumpTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // deliberately empty: no dump tool reachable at all
	fc := &fakeClient{metaRows: []metadata.SRAMetadata{
		{Accession: "SRR1234567", Platform: "ILLUMINA", LibraryLayout: "PAIRED", NReads: 2, SizeBytes: 100},
	}}
	cfg := defaultCfg()
	cfg.MetaOnly = true
	cfg.MetadataFile = filepath.Join(t.TempDir(), "meta.json")

	var out, errOut bytes.Buffer
	err := runDump(context.Background(), &out, &errOut, fc, http.DefaultClient, []string{"SRR1234567"}, cfg)
	if err != nil {
		t.Fatalf("runDump: %v", err)
	}
	if fc.dataCalls != 0 {
		t.Errorf("GetSRAData called %d times, want 0 for --metaonly", fc.dataCalls)
	}

	data, rerr := os.ReadFile(cfg.MetadataFile)
	if rerr != nil {
		t.Fatalf("reading metadata file: %v", rerr)
	}
	var records []map[string]any
	if jerr := json.Unmarshal(data, &records); jerr != nil {
		t.Fatalf("decoding metadata file: %v", jerr)
	}
	if len(records) != 1 {
		t.Fatalf("records = %v, want 1", records)
	}
	if _, ok := records[0]["files"]; ok {
		t.Errorf(`record has "files" set under --metaonly, want absent: %v`, records[0])
	}
}

// --- full pipeline (fake dump tool on PATH + httptest data server) ---

const fakeDumpScript = `#!/bin/sh
OUTDIR=""
LAST=""
while [ $# -gt 0 ]; do
  case "$1" in
    --outdir) OUTDIR="$2"; shift 2 ;;
    -t) shift 2 ;;
    *) LAST="$1"; shift ;;
  esac
done
BASE=$(basename "$LAST" .sra)
CODE="${FAKE_DUMP_EXIT_CODE:-0}"
if [ "$CODE" = "0" ]; then
  printf 'fake-fastq-data' > "$OUTDIR/${BASE}_1.fastq"
  printf 'fake-fastq-data' > "$OUTDIR/${BASE}_2.fastq"
fi
exit "$CODE"
`

// writeFakeDumpTool writes fakeDumpScript under name in dir, executable.
func writeFakeDumpTool(t *testing.T, dir, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(fakeDumpScript), 0o755); err != nil {
		t.Fatalf("writing fake %s: %v", name, err)
	}
	return path
}

func TestRunDumpFullPipelineWritesMetadataFile(t *testing.T) {
	dir := t.TempDir()
	writeFakeDumpTool(t, dir, "fasterq-dump")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-sra-bytes"))
	}))
	defer ts.Close()

	fc := &fakeClient{
		metaRows: []metadata.SRAMetadata{{Accession: "SRR1234567", Platform: "ILLUMINA", LibraryLayout: "PAIRED", NReads: 2, SizeBytes: 999}},
		dataSeq: [][]sracacheclient.SRADataLocation{
			{{Accession: "SRR1234567", State: "cached", URL: ts.URL, Expires: "2099-01-01T00:00:00Z"}},
		},
	}
	cfg := defaultCfg()
	cfg.OutDir = t.TempDir()
	cfg.StagingDir = t.TempDir()
	cfg.MetadataFile = filepath.Join(t.TempDir(), "meta.json")

	var out, errOut bytes.Buffer
	if err := runDump(context.Background(), &out, &errOut, fc, ts.Client(), []string{"SRR1234567"}, cfg); err != nil {
		t.Fatalf("runDump: %v (stderr=%s)", err, errOut.String())
	}

	data, rerr := os.ReadFile(cfg.MetadataFile)
	if rerr != nil {
		t.Fatalf("reading metadata file: %v", rerr)
	}
	var records []map[string]any
	if jerr := json.Unmarshal(data, &records); jerr != nil {
		t.Fatalf("decoding metadata file: %v", jerr)
	}
	if len(records) != 1 {
		t.Fatalf("records = %v, want 1", records)
	}
	rec := records[0]
	if rec["accession"] != "SRR1234567" || rec["platform_name"] != "ILLUMINA" || rec["library_layout"] != "PAIRED" {
		t.Errorf("record = %+v", rec)
	}
	files, ok := rec["files"].([]any)
	if !ok || len(files) != 2 {
		t.Errorf(`record["files"] = %v, want a 2-element list`, rec["files"])
	}
}

// TestRunDumpBinOverride confirms --bin's path override actually reaches the
// dump-tool invocation: a uniquely-named fake binary (not literally named
// fasterq-dump) is the ONLY dump tool on $PATH, and cfg.Bin points straight
// at it.
func TestRunDumpBinOverride(t *testing.T) {
	dir := t.TempDir()
	customBin := writeFakeDumpTool(t, dir, "my-custom-fasterq-dump")
	// Prepend (not replace) $PATH -- the fake script itself shells out to
	// ordinary utilities (basename, printf); a scorched-earth empty $PATH
	// would break those, not just hide a real fasterq-dump. The test
	// environment has no real "fasterq-dump" on PATH to begin with, so
	// success here already proves --bin (an absolute path, resolved with no
	// PATH search at all) was what actually ran.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-sra-bytes"))
	}))
	defer ts.Close()

	fc := &fakeClient{
		metaRows: []metadata.SRAMetadata{{Accession: "SRR1234567", Platform: "ILLUMINA"}},
		dataSeq: [][]sracacheclient.SRADataLocation{
			{{Accession: "SRR1234567", State: "cached", URL: ts.URL, Expires: "2099-01-01T00:00:00Z"}},
		},
	}
	cfg := defaultCfg()
	cfg.OutDir = t.TempDir()
	cfg.StagingDir = t.TempDir()
	cfg.Bin = customBin

	var out, errOut bytes.Buffer
	if err := runDump(context.Background(), &out, &errOut, fc, ts.Client(), []string{"SRR1234567"}, cfg); err != nil {
		t.Fatalf("runDump: %v (stderr=%s)", err, errOut.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.OutDir, "SRR1234567_1.fastq")); err != nil {
		t.Errorf("expected fastq output from the --bin-overridden tool: %v", err)
	}
}

// TestRunDumpErrorRowSkippedButOthersProceed: a batch with one
// invalid/not-found accession and one good one -- the bad one is skipped
// (not attempted) and the overall exit code is nonzero, but the good one
// still gets dumped and appears in the metadata file.
func TestRunDumpErrorRowSkippedButOthersProceed(t *testing.T) {
	dir := t.TempDir()
	writeFakeDumpTool(t, dir, "fasterq-dump")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-sra-bytes"))
	}))
	defer ts.Close()

	fc := &fakeClient{
		metaRows: []metadata.SRAMetadata{
			{Accession: "SRX000001", Extra: map[string]string{"error": "invalid_id", "message": "bad accession"}},
			{Accession: "SRR1234567", Platform: "ILLUMINA"},
		},
		dataSeq: [][]sracacheclient.SRADataLocation{
			{{Accession: "SRR1234567", State: "cached", URL: ts.URL, Expires: "2099-01-01T00:00:00Z"}},
		},
	}
	cfg := defaultCfg()
	cfg.OutDir = t.TempDir()
	cfg.StagingDir = t.TempDir()
	cfg.MetadataFile = filepath.Join(t.TempDir(), "meta.json")

	var out, errOut bytes.Buffer
	err := runDump(context.Background(), &out, &errOut, fc, ts.Client(), []string{"SRX000001", "SRR1234567"}, cfg)
	assertExitCode(t, err, 1)
	if !strings.Contains(errOut.String(), "SRX000001") {
		t.Errorf("stderr = %q, want it to mention the skipped SRX000001", errOut.String())
	}
	if _, statErr := os.Stat(filepath.Join(cfg.OutDir, "SRR1234567_1.fastq")); statErr != nil {
		t.Errorf("the good accession should still have been dumped: %v", statErr)
	}
}

// TestRunDumpSRAMetadataFileBestEffort confirms a failed raw-XML fetch is a
// warning, not a fatal error -- the rest of the run (here, --metaonly with a
// metadata-file) still succeeds.
func TestRunDumpSRAMetadataFileBestEffort(t *testing.T) {
	fc := &fakeClient{metaRows: []metadata.SRAMetadata{{Accession: "SRR1234567", Platform: "ILLUMINA"}}}
	cfg := defaultCfg()
	cfg.MetaOnly = true
	cfg.SRAMetadataFile = filepath.Join(t.TempDir(), "raw.xml")

	failingClient := &http.Client{Transport: alwaysFailTransport{}}
	var out, errOut bytes.Buffer
	err := runDump(context.Background(), &out, &errOut, fc, failingClient, []string{"SRR1234567"}, cfg)
	if err != nil {
		t.Fatalf("runDump: %v, want nil (a failed --sra-metadata-file fetch must not be fatal)", err)
	}
	if !strings.Contains(errOut.String(), "warning") {
		t.Errorf("stderr = %q, want a warning about the failed XML fetch", errOut.String())
	}
	if _, statErr := os.Stat(cfg.SRAMetadataFile); statErr == nil {
		t.Error("raw.xml should not exist: the fetch was made to fail")
	}
}

type alwaysFailTransport struct{}

func (alwaysFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("simulated network failure")
}

// --- --version / cobra wiring sanity ---

func TestNewRootCmdRegistersExpectedFlags(t *testing.T) {
	cmd := newRootCmd()
	for _, name := range []string{
		"url", "format", "fasterq-dump-opts", "fastq-dump-opts", "staging-dir",
		"outdir", "bin", "id", "out", "metaonly", "metadata-file",
		"sra-metadata-file", "gzip", "poll-interval", "timeout", "debug-http",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing expected flag --%s", name)
		}
	}
}
