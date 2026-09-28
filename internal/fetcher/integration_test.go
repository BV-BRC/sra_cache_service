package fetcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/cache"
	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/ratelimit"
)

// TestIntegrationRealPrefetchEndToEnd runs one real `prefetch` against a
// small public SRR through the full Pool -> catalog -> cache path (network
// included), then confirms the cached file's on-disk size matches the
// catalog's recorded size_bytes.
//
// Gated behind BVBRC_TEST_INTEGRATION=1, matching the existing SDK
// convention (BV-BRC-Go-SDK/p3_test.go, cited by
// PLAN-sra-cache-service-implementation.md's cross-cutting conventions) --
// no test in the default suite touches the network or shells out to the
// real SRA Toolkit.
//
// SRR1553609 was picked by checking eutils runinfo for run size (51 MB, one
// of the smaller candidates checked) -- not an exhaustive search for the
// single smallest accession that exists anywhere in SRA, per this phase's
// instructions.
func TestIntegrationRealPrefetchEndToEnd(t *testing.T) {
	if os.Getenv("BVBRC_TEST_INTEGRATION") == "" {
		t.Skip("Skipping live prefetch integration test (set BVBRC_TEST_INTEGRATION=1 to run)")
	}

	prefetchBin := os.Getenv("P3_PREFETCH_BIN")
	if prefetchBin == "" {
		prefetchBin = "/vol/patric3/cli/ubuntu-runtime/bin/prefetch"
	}
	if _, err := os.Stat(prefetchBin); err != nil {
		t.Skipf("real prefetch binary not found at %s (override with P3_PREFETCH_BIN): %v", prefetchBin, err)
	}

	const accession = "SRR1553609"

	cat, err := catalog.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	defer cat.Close()

	c := cache.New(t.TempDir())
	f, err := New(prefetchBin, t.TempDir())
	if err != nil {
		t.Fatalf("fetcher.New: %v", err)
	}
	limiter := ratelimit.New(2, 4)
	pool := NewPool(cat, c, f, limiter, 5_000_000_000 /* 5 GB, comfortably larger than this run */, nil)

	if err := cat.Enqueue(context.Background(), accession); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	runCtx, cancelRun := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelRun()
	runDone := make(chan error, 1)
	go func() { runDone <- pool.Run(runCtx) }()

	var final catalog.Entry
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		e, ok, gerr := cat.Get(context.Background(), accession)
		if gerr != nil {
			t.Fatalf("Get: %v", gerr)
		}
		if ok && (e.State == catalog.StateCached || e.State == catalog.StateError || e.State == catalog.StateNotFound) {
			final = e
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	cancelRun()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("pool.Run did not stop within 10s of its context being cancelled")
	}

	if final.State != catalog.StateCached {
		t.Fatalf("accession %s ended in state %q (message: %q), want cached", accession, final.State, final.Message)
	}
	if final.SizeBytes <= 0 {
		t.Errorf("catalog SizeBytes = %d, want > 0", final.SizeBytes)
	}

	path := c.Path(accession)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat cached file %s: %v", path, err)
	}
	if info.Size() != final.SizeBytes {
		t.Errorf("cached file size %d does not match catalog SizeBytes %d", info.Size(), final.SizeBytes)
	}
}
