package fetcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
)

// fakePrefetchScript is the test-double `prefetch` binary the design doc
// calls for: it writes a canned file to the requested output directory and
// exits with a canned code / prints canned stderr, controlled entirely by
// environment variables, so the classify/publish/error paths can be
// exercised with no network and no real SRA Toolkit.
//
//	FAKE_PREFETCH_EXIT_CODE   exit code to return (default "0")
//	FAKE_PREFETCH_STDERR      text to print to stderr before exiting
//	FAKE_PREFETCH_CONTENT     content of the downloaded file on success
//	                          (default "fake-sra-data")
//
// It also answers --version (used by Fetcher.Probe's tests), independent of
// the above.
const fakePrefetchScript = `#!/bin/sh
set -e
OUTDIR=""
ACCESSION=""
while [ $# -gt 0 ]; do
  case "$1" in
    --version)
      echo "prefetch : 3.1.0-fake"
      exit 0
      ;;
    --output-directory)
      OUTDIR="$2"
      shift 2
      ;;
    *)
      ACCESSION="$1"
      shift
      ;;
  esac
done

if [ -n "$FAKE_PREFETCH_STDERR" ]; then
  echo "$FAKE_PREFETCH_STDERR" >&2
fi

CODE="${FAKE_PREFETCH_EXIT_CODE:-0}"
if [ "$CODE" = "0" ]; then
  mkdir -p "$OUTDIR/$ACCESSION"
  printf '%s' "${FAKE_PREFETCH_CONTENT:-fake-sra-data}" > "$OUTDIR/$ACCESSION/$ACCESSION.sra"
fi
exit "$CODE"
`

func writeFakePrefetch(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prefetch")
	if err := os.WriteFile(path, []byte(fakePrefetchScript), 0o755); err != nil {
		t.Fatalf("writing fake prefetch script: %v", err)
	}
	return path
}

func newTestFetcher(t *testing.T) *Fetcher {
	t.Helper()
	f, err := New(writeFakePrefetch(t), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return f
}

// TestFetchSuccessConsumesFileThenCleansUp is the "successful fetch" path
// from the phase-6 test list: consume sees the right content/size while the
// file exists, and the entire staging directory -- not just the file -- is
// gone once Fetch returns.
func TestFetchSuccessConsumesFileThenCleansUp(t *testing.T) {
	f := newTestFetcher(t)
	t.Setenv("FAKE_PREFETCH_CONTENT", "hello-world-sra-bytes")

	var gotPath string
	var gotSize int64
	err := f.Fetch(context.Background(), "SRR000001", func(path string, size int64) error {
		gotPath, gotSize = path, size
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("reading staged file inside consume: %v", rerr)
		}
		if string(data) != "hello-world-sra-bytes" {
			t.Errorf("staged file content = %q", data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if gotSize != int64(len("hello-world-sra-bytes")) {
		t.Errorf("size passed to consume = %d, want %d", gotSize, len("hello-world-sra-bytes"))
	}
	stagingRoot := filepath.Dir(filepath.Dir(gotPath))
	if _, statErr := os.Stat(stagingRoot); !os.IsNotExist(statErr) {
		t.Errorf("staging directory %s still exists after Fetch returned (err=%v)", stagingRoot, statErr)
	}
}

// TestFetchConsumeErrorPropagates asserts an error from consume (e.g. a
// failed cache.Publish) comes back from Fetch unchanged, and the staging
// directory is still cleaned up regardless.
func TestFetchConsumeErrorPropagates(t *testing.T) {
	f := newTestFetcher(t)
	wantErr := errors.New("publish failed")

	var gotPath string
	err := f.Fetch(context.Background(), "SRR000001", func(path string, size int64) error {
		gotPath = path
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Fetch error = %v, want %v", err, wantErr)
	}
	if _, statErr := os.Stat(filepath.Dir(filepath.Dir(gotPath))); !os.IsNotExist(statErr) {
		t.Error("staging directory should still be cleaned up when consume errors")
	}
}

// TestFetchFailedToResolveClassifiesAsNotFound is the "failed to resolve"
// stderr acceptance case from the phase-6 test list: consume must never be
// called, and Classify on the resulting *PrefetchError must say not_found.
func TestFetchFailedToResolveClassifiesAsNotFound(t *testing.T) {
	f := newTestFetcher(t)
	t.Setenv("FAKE_PREFETCH_EXIT_CODE", "3")
	t.Setenv("FAKE_PREFETCH_STDERR", "err: failed to resolve accession 'BOGUS' - Cannot resolve accession ( 404 )")

	called := false
	err := f.Fetch(context.Background(), "BOGUS", func(path string, size int64) error {
		called = true
		return nil
	})
	if called {
		t.Error("consume must not be called when prefetch fails")
	}

	var perr *PrefetchError
	if !errors.As(err, &perr) {
		t.Fatalf("Fetch error = %v (%T), want *PrefetchError", err, err)
	}
	if perr.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", perr.ExitCode)
	}
	state, msg := Classify(perr.ExitCode, perr.Output, nil)
	if state != catalog.StateNotFound {
		t.Errorf("Classify(perr) = %q, want not_found (msg=%q)", state, msg)
	}
}

// TestFetchOtherFailureClassifiesAsError is the "nonzero exit with other
// stderr" acceptance case: neither the not_found nor the throttle pattern
// matches, so it's a hard error carrying a message.
func TestFetchOtherFailureClassifiesAsError(t *testing.T) {
	f := newTestFetcher(t)
	t.Setenv("FAKE_PREFETCH_EXIT_CODE", "1")
	t.Setenv("FAKE_PREFETCH_STDERR", "some unrelated disk-full failure")

	err := f.Fetch(context.Background(), "SRR1", func(path string, size int64) error { return nil })
	var perr *PrefetchError
	if !errors.As(err, &perr) {
		t.Fatalf("Fetch error = %v, want *PrefetchError", err)
	}
	state, msg := Classify(perr.ExitCode, perr.Output, nil)
	if state != catalog.StateError {
		t.Errorf("Classify(perr) = %q, want error", state)
	}
	if !strings.Contains(msg, "disk-full") {
		t.Errorf("message = %q, want it to mention the captured stderr", msg)
	}
}

// TestFetchExitZeroButNoFileIsStillAnError guards findDownloadedFile's
// contract: a fake prefetch that exits 0 without writing anything must not
// be treated as success.
func TestFetchExitZeroButNoFileIsStillAnError(t *testing.T) {
	// A script that always exits 0 but never writes a file (unlike
	// fakePrefetchScript, which always writes one on exit 0) -- a distinct
	// minimal script rather than another env knob, since this is a shape
	// fakePrefetchScript's own contract doesn't produce.
	path := filepath.Join(t.TempDir(), "prefetch")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := New(path, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	called := false
	err = f.Fetch(context.Background(), "SRR1", func(string, int64) error { called = true; return nil })
	if called {
		t.Error("consume must not be called when no file was produced")
	}
	var perr *PrefetchError
	if !errors.As(err, &perr) {
		t.Fatalf("Fetch error = %v, want *PrefetchError", err)
	}
}

// TestProbeSucceedsAndWarnsWhenSettingsMissing asserts a missing
// ~/.ncbi/user-settings.mkfg is a logged warning, never a hard failure --
// the service must still start.
func TestProbeSucceedsAndWarnsWhenSettingsMissing(t *testing.T) {
	f := newTestFetcher(t)
	t.Setenv("HOME", t.TempDir())
	if err := f.Probe(context.Background(), nil); err != nil {
		t.Fatalf("Probe: %v, want nil (a missing settings file is a warning, not a failure)", err)
	}
}

// TestProbeFindsSettingsFileWhenPresent exercises the found-it path too.
func TestProbeFindsSettingsFileWhenPresent(t *testing.T) {
	f := newTestFetcher(t)
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ncbi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".ncbi", "user-settings.mkfg"), []byte("# fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := f.Probe(context.Background(), nil); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

// TestProbeFailsWhenPrefetchBinIsUnusable asserts a genuinely broken
// prefetch-bin path (not just a missing settings file) is reported as an
// error from Probe.
func TestProbeFailsWhenPrefetchBinIsUnusable(t *testing.T) {
	f, err := New(filepath.Join(t.TempDir(), "does-not-exist"), t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := f.Probe(context.Background(), nil); err == nil {
		t.Fatal("Probe: got nil error for a nonexistent prefetch binary, want non-nil")
	}
}
