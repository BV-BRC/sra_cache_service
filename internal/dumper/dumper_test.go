package dumper

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/sracacheclient"
)

// --- ResolveStagingDir ---

func TestResolveStagingDir(t *testing.T) {
	t.Run("explicit flag wins", func(t *testing.T) {
		t.Setenv("TMPDIR", "/from-env")
		if got := ResolveStagingDir("/from-flag"); got != "/from-flag" {
			t.Errorf("got %q, want /from-flag", got)
		}
	})
	t.Run("falls back to TMPDIR", func(t *testing.T) {
		t.Setenv("TMPDIR", "/from-env")
		if got := ResolveStagingDir(""); got != "/from-env" {
			t.Errorf("got %q, want /from-env", got)
		}
	})
	t.Run("falls back to /tmp when TMPDIR unset", func(t *testing.T) {
		t.Setenv("TMPDIR", "")
		os.Unsetenv("TMPDIR")
		if got := ResolveStagingDir(""); got != "/tmp" {
			t.Errorf("got %q, want /tmp", got)
		}
	})
}

// --- SelectTool ---

func TestSelectTool(t *testing.T) {
	cases := []struct {
		platform     string
		binOverride  string
		wantBin      string
		wantIsPacBio bool
	}{
		{"PACBIO_SMRT", "", "fastq-dump", true},
		{"ILLUMINA", "", "fasterq-dump", false},
		{"OXFORD_NANOPORE", "", "fasterq-dump", false},
		{"ION_TORRENT", "", "fasterq-dump", false},
		{"", "", "fasterq-dump", false}, // unknown/empty platform defaults to the fasterq-dump branch
		{"PACBIO_SMRT", "/custom/bin", "/custom/bin", true},
		{"ILLUMINA", "/custom/bin", "/custom/bin", false},
	}
	for _, c := range cases {
		t.Run(c.platform+"/"+c.binOverride, func(t *testing.T) {
			got := SelectTool(c.platform, c.binOverride)
			if got.Bin != c.wantBin || got.IsPacBio != c.wantIsPacBio {
				t.Errorf("SelectTool(%q,%q) = %+v, want Bin=%q IsPacBio=%v", c.platform, c.binOverride, got, c.wantBin, c.wantIsPacBio)
			}
		})
	}
}

// --- BuildArgs ---

func TestBuildArgsFasterqDump(t *testing.T) {
	tool := Tool{Bin: "fasterq-dump", IsPacBio: false}
	got := BuildArgs(tool, "/staging", "/out", "/staging/SRR123.sra", false, nil)
	want := []string{"-t", "/staging", "--outdir", "/out", "--split-files", "-f", "/staging/SRR123.sra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestBuildArgsFastqDumpPacBio(t *testing.T) {
	tool := Tool{Bin: "fastq-dump", IsPacBio: true}
	got := BuildArgs(tool, "/staging", "/out", "/staging/SRR123.sra", false, nil)
	want := []string{"--outdir", "/out", "/staging/SRR123.sra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"-t", "--split-files", "-f"} {
		for _, a := range got {
			if a == bad {
				t.Errorf("PacBio args %v must not contain %q", got, bad)
			}
		}
	}
}

func TestBuildArgsFastaAndExtraOptsOrdering(t *testing.T) {
	tool := Tool{Bin: "fasterq-dump", IsPacBio: false}
	got := BuildArgs(tool, "/staging", "/out", "/path.sra", true, []string{"--threads", "4"})
	want := []string{"-t", "/staging", "--outdir", "/out", "--split-files", "-f", "--fasta", "--threads", "4", "/path.sra"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- fake dump-tool scripts on PATH (same trick Phase 6 used for prefetch) ---

// writeFakeDumpTool writes a shell script named name that: on success (exit
// code 0, the default), writes a canned "<accession>_1.fastq" (and, unless
// noSplit, "<accession>_2.fastq") file under whatever --outdir it was given,
// containing FAKE_DUMP_CONTENT (default "fake-fastq-data"); it exits
// FAKE_DUMP_EXIT_CODE (default 0) and prints FAKE_DUMP_STDERR to stderr if
// set. The accession is taken as the script's last positional argument,
// matching BuildArgs' own convention (a local .sra path in this dumper's
// case, but the script only cares about the basename).
func writeFakeDumpTool(t *testing.T, dir, name string, noSplit bool) string {
	t.Helper()
	splitBlock := `printf '%s' "${FAKE_DUMP_CONTENT:-fake-fastq-data}" > "$OUTDIR/${BASE}_1.fastq"
  printf '%s' "${FAKE_DUMP_CONTENT:-fake-fastq-data}" > "$OUTDIR/${BASE}_2.fastq"`
	if noSplit {
		splitBlock = `printf '%s' "${FAKE_DUMP_CONTENT:-fake-fastq-data}" > "$OUTDIR/${BASE}.fastq"`
	}
	script := `#!/bin/sh
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

if [ -n "$FAKE_DUMP_STDERR" ]; then
  echo "$FAKE_DUMP_STDERR" >&2
fi

CODE="${FAKE_DUMP_EXIT_CODE:-0}"
if [ "$CODE" = "0" ]; then
  ` + splitBlock + `
fi
exit "$CODE"
`
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake %s: %v", name, err)
	}
	return path
}

// withFakeDumpToolsOnPath creates fasterq-dump and fastq-dump fakes in a
// fresh directory and prepends it to $PATH for the duration of the test.
func withFakeDumpToolsOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	writeFakeDumpTool(t, dir, "fasterq-dump", false)
	writeFakeDumpTool(t, dir, "fastq-dump", true)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// --- RunDumpTool ---

func TestRunDumpToolSuccess(t *testing.T) {
	withFakeDumpToolsOnPath(t)
	outDir := t.TempDir()
	tool := SelectTool("ILLUMINA", "")
	args := BuildArgs(tool, t.TempDir(), outDir, filepath.Join(t.TempDir(), "SRR1234567.sra"), false, nil)
	var stdout, stderr strings.Builder
	if err := RunDumpTool(context.Background(), tool, args, &stdout, &stderr); err != nil {
		t.Fatalf("RunDumpTool: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "SRR1234567_1.fastq")); err != nil {
		t.Errorf("expected _1.fastq to be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "SRR1234567_2.fastq")); err != nil {
		t.Errorf("expected _2.fastq to be written: %v", err)
	}
}

func TestRunDumpToolPropagatesFailure(t *testing.T) {
	withFakeDumpToolsOnPath(t)
	t.Setenv("FAKE_DUMP_EXIT_CODE", "3")
	t.Setenv("FAKE_DUMP_STDERR", "boom")
	outDir := t.TempDir()
	tool := SelectTool("ILLUMINA", "")
	args := BuildArgs(tool, t.TempDir(), outDir, filepath.Join(t.TempDir(), "SRR1234567.sra"), false, nil)
	var stdout, stderr strings.Builder
	err := RunDumpTool(context.Background(), tool, args, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected an error from a nonzero exit")
	}
	if !strings.Contains(stderr.String(), "boom") {
		t.Errorf("stderr = %q, want it to contain the fake tool's stderr", stderr.String())
	}
	if _, statErr := os.Stat(filepath.Join(outDir, "SRR1234567_1.fastq")); statErr == nil {
		t.Error("no fastq file should have been written on a simulated failure")
	}
}

// --- GzipFastqFiles ---

func TestGzipFastqFiles(t *testing.T) {
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not on PATH")
	}
	outDir := t.TempDir()
	for _, name := range []string{"a.fastq", "b.fastq"} {
		if err := os.WriteFile(filepath.Join(outDir, name), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := GzipFastqFiles(outDir); err != nil {
		t.Fatalf("GzipFastqFiles: %v", err)
	}
	for _, name := range []string{"a.fastq.gz", "b.fastq.gz"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
	for _, name := range []string{"a.fastq", "b.fastq"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err == nil {
			t.Errorf("expected original %s to be removed by gzip", name)
		}
	}
}

// TestGzipFastqFilesNoFilesIsANoOp is the direct proof that this function
// does NOT reproduce sra_tools.py's stdin-blocking bug when there is nothing
// to gzip (see GzipFastqFiles' doc comment) -- it must return promptly with
// no error, not hang.
func TestGzipFastqFilesNoFilesIsANoOp(t *testing.T) {
	outDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- GzipFastqFiles(outDir) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("GzipFastqFiles on an empty dir: %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GzipFastqFiles hung on an empty directory (the sra_tools.py bug this must not reproduce)")
	}
}

// --- DownloadFile ---

func TestDownloadFile(t *testing.T) {
	const content = "hello-sra-bytes"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(content))
	}))
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "out.sra")
	n, err := DownloadFile(context.Background(), nil, ts.URL, dest)
	if err != nil {
		t.Fatalf("DownloadFile: %v", err)
	}
	if n != int64(len(content)) {
		t.Errorf("n = %d, want %d", n, len(content))
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("reading downloaded file: %v", err)
	}
	if string(got) != content {
		t.Errorf("content = %q, want %q", got, content)
	}
}

func TestDownloadFileNon200IsError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer ts.Close()

	dest := filepath.Join(t.TempDir(), "out.sra")
	if _, err := DownloadFile(context.Background(), nil, ts.URL, dest); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

// --- WaitForCached ---

type fakeDataGetter struct {
	seq   [][]sracacheclient.SRADataLocation
	idx   int
	calls int
	err   error
}

func (f *fakeDataGetter) GetSRAData(ctx context.Context, accessions []string) ([]sracacheclient.SRADataLocation, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	if len(f.seq) == 0 {
		return nil, nil
	}
	i := f.idx
	if i >= len(f.seq) {
		i = len(f.seq) - 1
	} else {
		f.idx++
	}
	return f.seq[i], nil
}

func TestWaitForCachedPollsToTerminal(t *testing.T) {
	fd := &fakeDataGetter{seq: [][]sracacheclient.SRADataLocation{
		{{Accession: "SRR1", State: "queued"}},
		{{Accession: "SRR1", State: "in_progress"}},
		{{Accession: "SRR1", State: "cached", URL: "http://example.org/sra/x/SRR1.sra", Expires: "2099-01-01T00:00:00Z"}},
	}}
	loc, err := WaitForCached(context.Background(), fd, "SRR1", time.Millisecond, 0)
	if err != nil {
		t.Fatalf("WaitForCached: %v", err)
	}
	if loc.State != "cached" || loc.URL == "" {
		t.Errorf("loc = %+v, want a cached row with a URL", loc)
	}
	if fd.calls != 3 {
		t.Errorf("calls = %d, want 3", fd.calls)
	}
}

func TestWaitForCachedFailureStatesReturnImmediately(t *testing.T) {
	for _, state := range []string{"invalid_id", "not_found", "error"} {
		t.Run(state, func(t *testing.T) {
			fd := &fakeDataGetter{seq: [][]sracacheclient.SRADataLocation{{{Accession: "SRR1", State: state}}}}
			_, err := WaitForCached(context.Background(), fd, "SRR1", time.Hour, 0)
			if err == nil {
				t.Fatalf("WaitForCached with state %q: expected an error", state)
			}
			if fd.calls != 1 {
				t.Errorf("calls = %d, want 1 (must not poll a terminal-failure state)", fd.calls)
			}
		})
	}
}

func TestWaitForCachedTimeout(t *testing.T) {
	fd := &fakeDataGetter{seq: [][]sracacheclient.SRADataLocation{{{Accession: "SRR1", State: "queued"}}}}
	_, err := WaitForCached(context.Background(), fd, "SRR1", 5*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// --- DumpAccession (full pipeline) ---

func newCachedDataGetter(url string) *fakeDataGetter {
	return &fakeDataGetter{seq: [][]sracacheclient.SRADataLocation{
		{{Accession: "SRR1234567", State: "cached", URL: url, Expires: "2099-01-01T00:00:00Z"}},
	}}
}

func TestDumpAccessionSuccessCleansUpStagedFile(t *testing.T) {
	withFakeDumpToolsOnPath(t)
	const sraContent = "fake-sra-bytes"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sraContent))
	}))
	defer ts.Close()

	stagingDir := t.TempDir()
	outDir := t.TempDir()
	fd := newCachedDataGetter(ts.URL)

	opts := Options{StagingDir: stagingDir, OutDir: outDir, PollInterval: time.Millisecond}
	var stdout, stderr strings.Builder
	res, err := DumpAccession(context.Background(), fd, ts.Client(), "SRR1234567", "ILLUMINA", opts, &stdout, &stderr)
	if err != nil {
		t.Fatalf("DumpAccession: %v", err)
	}
	if len(res.Files) != 2 {
		t.Errorf("Files = %v, want 2 entries", res.Files)
	}

	stagedPath := filepath.Join(stagingDir, "SRR1234567.sra")
	if _, err := os.Stat(stagedPath); !os.IsNotExist(err) {
		t.Errorf("staged file %s should have been cleaned up on success, stat err = %v", stagedPath, err)
	}
}

// TestDumpAccessionFailureLeavesStagedFile is the direct proof of the
// documented non-goal: a mid-dump failure (the fake tool exiting nonzero)
// must NOT clean up the staged .sra.
func TestDumpAccessionFailureLeavesStagedFile(t *testing.T) {
	withFakeDumpToolsOnPath(t)
	t.Setenv("FAKE_DUMP_EXIT_CODE", "1")
	const sraContent = "fake-sra-bytes"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(sraContent))
	}))
	defer ts.Close()

	stagingDir := t.TempDir()
	outDir := t.TempDir()
	fd := newCachedDataGetter(ts.URL)

	opts := Options{StagingDir: stagingDir, OutDir: outDir, PollInterval: time.Millisecond}
	var stdout, stderr strings.Builder
	_, err := DumpAccession(context.Background(), fd, ts.Client(), "SRR1234567", "ILLUMINA", opts, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected DumpAccession to fail when the dump tool exits nonzero")
	}

	stagedPath := filepath.Join(stagingDir, "SRR1234567.sra")
	if _, statErr := os.Stat(stagedPath); statErr != nil {
		t.Errorf("staged file %s must survive a mid-dump failure, stat err = %v", stagedPath, statErr)
	}
}

func TestDumpAccessionPacBioSelectsFastqDumpNoSplit(t *testing.T) {
	withFakeDumpToolsOnPath(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-sra-bytes"))
	}))
	defer ts.Close()

	stagingDir := t.TempDir()
	outDir := t.TempDir()
	fd := newCachedDataGetter(ts.URL)

	opts := Options{StagingDir: stagingDir, OutDir: outDir, PollInterval: time.Millisecond}
	var stdout, stderr strings.Builder
	res, err := DumpAccession(context.Background(), fd, ts.Client(), "SRR1234567", "PACBIO_SMRT", opts, &stdout, &stderr)
	if err != nil {
		t.Fatalf("DumpAccession: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0] != "SRR1234567.fastq" {
		t.Errorf("Files = %v, want exactly [SRR1234567.fastq] (no split for PacBio)", res.Files)
	}
}

func TestDumpAccessionGzip(t *testing.T) {
	if _, err := exec.LookPath("gzip"); err != nil {
		t.Skip("gzip not on PATH")
	}
	withFakeDumpToolsOnPath(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fake-sra-bytes"))
	}))
	defer ts.Close()

	stagingDir := t.TempDir()
	outDir := t.TempDir()
	fd := newCachedDataGetter(ts.URL)

	opts := Options{StagingDir: stagingDir, OutDir: outDir, PollInterval: time.Millisecond, Gzip: true}
	var stdout, stderr strings.Builder
	res, err := DumpAccession(context.Background(), fd, ts.Client(), "SRR1234567", "ILLUMINA", opts, &stdout, &stderr)
	if err != nil {
		t.Fatalf("DumpAccession: %v", err)
	}
	for _, f := range res.Files {
		if !strings.HasSuffix(f, ".fastq.gz") {
			t.Errorf("file %q was not gzipped", f)
		}
	}
}
