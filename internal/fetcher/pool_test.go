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

func openTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	cat, err := catalog.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

// newTestPool wires a Pool with a fake prefetch on a fresh catalog/cache, a
// generous cache budget, and a permissive limiter (so tests aren't
// incidentally exercising AIMD backoff unless they mean to).
func newTestPool(t *testing.T) (*Pool, *catalog.Catalog, *cache.Cache) {
	t.Helper()
	cat := openTestCatalog(t)
	c := cache.New(t.TempDir())
	f := newTestFetcher(t)
	limiter := ratelimit.New(4, 4)
	p := NewPool(cat, c, f, limiter, 1_000_000_000, nil)
	return p, cat, c
}

// waitForState polls the catalog until accession reaches one of wantAny, or
// fails the test after a generous timeout -- Run's poll loop is
// asynchronous, so tests observe it this way rather than sleeping a fixed
// duration.
func waitForState(t *testing.T, cat *catalog.Catalog, accession string, wantAny ...catalog.State) catalog.Entry {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e, ok, err := cat.Get(context.Background(), accession)
		if err != nil {
			t.Fatalf("Get %s: %v", accession, err)
		}
		if ok {
			for _, want := range wantAny {
				if e.State == want {
					return e
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("accession %s did not reach any of %v within the deadline", accession, wantAny)
	return catalog.Entry{}
}

func runPoolInBackground(t *testing.T, p *Pool) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := p.Run(ctx); err != nil {
			t.Errorf("Pool.Run: %v", err)
		}
	}()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Pool.Run did not return within 5s of cancellation")
		}
	}
}

// TestPoolSuccessPublishesAndRecordsSize is the "successful fetch" case
// from the phase-6 test list: cached state, correct size in both the
// catalog and on disk.
func TestPoolSuccessPublishesAndRecordsSize(t *testing.T) {
	p, cat, c := newTestPool(t)
	stop := runPoolInBackground(t, p)
	defer stop()

	t.Setenv("FAKE_PREFETCH_CONTENT", "0123456789")
	if err := cat.Enqueue(context.Background(), "SRR1"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	p.Wake()

	e := waitForState(t, cat, "SRR1", catalog.StateCached, catalog.StateError)
	if e.State != catalog.StateCached {
		t.Fatalf("SRR1 state = %q, want cached (message: %s)", e.State, e.Message)
	}
	if e.SizeBytes != 10 {
		t.Errorf("SRR1 SizeBytes = %d, want 10", e.SizeBytes)
	}
	data, err := os.ReadFile(c.Path("SRR1"))
	if err != nil {
		t.Fatalf("reading cached file: %v", err)
	}
	if string(data) != "0123456789" {
		t.Errorf("cached file content = %q", data)
	}
}

// TestPoolNotFoundState covers the "failed to resolve" -> not_found path
// end to end through the pool.
func TestPoolNotFoundState(t *testing.T) {
	p, cat, _ := newTestPool(t)
	stop := runPoolInBackground(t, p)
	defer stop()

	t.Setenv("FAKE_PREFETCH_EXIT_CODE", "3")
	t.Setenv("FAKE_PREFETCH_STDERR", "err: failed to resolve accession 'SRRBOGUS' - Cannot resolve accession ( 404 )")
	if err := cat.Enqueue(context.Background(), "SRRBOGUS"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	p.Wake()

	e := waitForState(t, cat, "SRRBOGUS", catalog.StateNotFound, catalog.StateCached, catalog.StateError)
	if e.State != catalog.StateNotFound {
		t.Fatalf("SRRBOGUS state = %q, want not_found (message: %s)", e.State, e.Message)
	}
}

// TestPoolErrorState covers a nonzero exit with an unrelated stderr message.
func TestPoolErrorState(t *testing.T) {
	p, cat, _ := newTestPool(t)
	stop := runPoolInBackground(t, p)
	defer stop()

	t.Setenv("FAKE_PREFETCH_EXIT_CODE", "1")
	t.Setenv("FAKE_PREFETCH_STDERR", "some unrelated disk-full failure")
	if err := cat.Enqueue(context.Background(), "SRR2"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	p.Wake()

	e := waitForState(t, cat, "SRR2", catalog.StateError, catalog.StateCached, catalog.StateNotFound)
	if e.State != catalog.StateError {
		t.Fatalf("SRR2 state = %q, want error (message: %s)", e.State, e.Message)
	}
	if e.ErrorCount != 1 {
		t.Errorf("SRR2 ErrorCount = %d, want 1", e.ErrorCount)
	}
}

// TestPoolThrottleRequeuesAndBacksOffLimiter asserts a throttle-shaped
// failure does NOT land as StateError: it goes back to StateQueued (and
// stays retryable), while the limiter's ceiling drops.
func TestPoolThrottleRequeuesAndBacksOffLimiter(t *testing.T) {
	p, cat, _ := newTestPool(t)
	// Give the limiter a distinctive starting point so a halving is easy to
	// assert on unambiguously.
	p.Limiter = ratelimit.New(8, 16)

	t.Setenv("FAKE_PREFETCH_EXIT_CODE", "1")
	t.Setenv("FAKE_PREFETCH_STDERR", "429 Too Many Requests")
	if err := cat.Enqueue(context.Background(), "SRR3"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := runPoolInBackground(t, p)
	defer stop()
	p.Wake()

	// It will keep cycling queued -> in_progress -> queued forever (the
	// fake prefetch always fails the same way) -- just confirm it reaches
	// queued at least once and the limiter backed off, then stop before it
	// can flake on a race between polls.
	deadline := time.Now().Add(5 * time.Second)
	sawQueuedAgain := false
	for time.Now().Before(deadline) {
		if p.Limiter.Current() < 8 {
			sawQueuedAgain = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !sawQueuedAgain {
		t.Fatal("limiter never backed off below its starting ceiling of 8")
	}

	e, ok, err := cat.Get(context.Background(), "SRR3")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.State != catalog.StateQueued && e.State != catalog.StateInProgress {
		t.Errorf("SRR3 state = %q, want queued or in_progress (never a terminal error for a throttle signal)", e.State)
	}
}

// TestPoolDedupesInFlightAccession is the "a second start_precache for the
// same id while it's downloading should not spawn a second fetch" case,
// exercised directly against the same markInFlight/clearInFlight guard
// drainOnce uses before launching a fetch goroutine: a second mark attempt
// for an accession already in flight must be rejected, and clearing it must
// make it available again.
func TestPoolDedupesInFlightAccession(t *testing.T) {
	p, _, _ := newTestPool(t)

	if !p.markInFlight("SRR9") {
		t.Fatal("first markInFlight(SRR9) should succeed")
	}
	if p.markInFlight("SRR9") {
		t.Fatal("second markInFlight(SRR9) should be rejected while still in flight")
	}
	p.clearInFlight("SRR9")
	if !p.markInFlight("SRR9") {
		t.Fatal("markInFlight(SRR9) should succeed again once cleared")
	}
	p.clearInFlight("SRR9")
}

// TestPoolDrainOnceSkipsAccessionAlreadyInFlight is the same guarantee
// exercised through drainOnce itself: an accession pre-marked in flight
// must not be (re-)launched even though it has a catalog.StateQueued row.
func TestPoolDrainOnceSkipsAccessionAlreadyInFlight(t *testing.T) {
	p, cat, _ := newTestPool(t)
	if err := cat.Enqueue(context.Background(), "SRR10"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	p.markInFlight("SRR10")

	p.drainOnce(context.Background())

	// drainOnce must not have transitioned it to in_progress -- process()
	// was never launched for it.
	e, ok, err := cat.Get(context.Background(), "SRR10")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.State != catalog.StateQueued {
		t.Errorf("SRR10 state = %q, want unchanged queued (already in flight, must not be launched again)", e.State)
	}
}

// TestPoolRequeuesStuckInProgressOnStartup is the Requeue acceptance case:
// a row left in_progress (simulating a prior crash) is queued again by
// Run's startup call, and the poll loop then picks it up and finishes it.
func TestPoolRequeuesStuckInProgressOnStartup(t *testing.T) {
	p, cat, _ := newTestPool(t)
	if err := cat.Upsert(context.Background(), catalog.Entry{Accession: "SRR4", State: catalog.StateInProgress}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	stop := runPoolInBackground(t, p)
	defer stop()

	e := waitForState(t, cat, "SRR4", catalog.StateCached, catalog.StateError)
	if e.State != catalog.StateCached {
		t.Fatalf("SRR4 (recovered from stuck in_progress) state = %q, want cached", e.State)
	}
}

// TestPoolOversizeAccessionIsError asserts EnsureSpace's
// ErrTooLargeForCache surfaces as a catalog error, not a cached file.
func TestPoolOversizeAccessionIsError(t *testing.T) {
	p, cat, c := newTestPool(t)
	p.MaxCacheSize = 5 // smaller than the fake download below

	t.Setenv("FAKE_PREFETCH_CONTENT", "this content is way more than five bytes")
	if err := cat.Enqueue(context.Background(), "SRR5"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	stop := runPoolInBackground(t, p)
	defer stop()
	p.Wake()

	e := waitForState(t, cat, "SRR5", catalog.StateError, catalog.StateCached)
	if e.State != catalog.StateError {
		t.Fatalf("SRR5 state = %q, want error (oversize)", e.State)
	}
	if _, err := os.Stat(c.Path("SRR5")); !os.IsNotExist(err) {
		t.Error("an oversize accession must never be published to the cache")
	}
}

// TestPoolStopsOnContextCancellation asserts Run returns promptly once its
// context is cancelled, even mid-poll.
func TestPoolStopsOnContextCancellation(t *testing.T) {
	p, _, _ := newTestPool(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil on a clean shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return within 5s of context cancellation")
	}
}

// TestPoolRunFailsIfRequeueFails asserts Run reports a non-nil error when
// the catalog itself is unusable (here: already closed).
func TestPoolRunFailsIfRequeueFails(t *testing.T) {
	p, cat, _ := newTestPool(t)
	cat.Close()

	if err := p.Run(context.Background()); err == nil {
		t.Fatal("Run: got nil error against a closed catalog, want non-nil")
	}
}
