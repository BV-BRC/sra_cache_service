package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

// mustAcquire acquires a slot with a generous timeout so a bug that
// deadlocks Acquire fails the test instead of hanging the suite forever.
func mustAcquire(t *testing.T, l *Limiter) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	release, err := l.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	return release
}

func TestNewClampsInitialAndMax(t *testing.T) {
	l := New(0, 0)
	if got := l.Current(); got != 1 {
		t.Errorf("New(0, 0).Current() = %d, want 1 (clamped)", got)
	}

	l2 := New(5, 2)
	if got := l2.Current(); got != 5 {
		t.Errorf("New(5, 2).Current() = %d, want 5 (max clamped up to initial)", got)
	}
}

// TestAcquireGrantsUpToLimit asserts exactly `limit` concurrent Acquires
// succeed immediately and the next one blocks.
func TestAcquireGrantsUpToLimit(t *testing.T) {
	l := New(2, 8)

	r1 := mustAcquire(t, l)
	r2 := mustAcquire(t, l)

	// A third Acquire must block: use a short-deadline context and expect
	// DeadlineExceeded, not a granted slot.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("Acquire: third call succeeded, want it to block at limit=2")
	}

	r1()
	r2()
}

// TestReleaseUnblocksAWaiter asserts a blocked Acquire is granted as soon as
// a slot is released, without needing the limit to change.
func TestReleaseUnblocksAWaiter(t *testing.T) {
	l := New(1, 4)
	release := mustAcquire(t, l)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		r, err := l.Acquire(ctx)
		if err == nil {
			r()
		}
		done <- err
	}()

	// Give the goroutine time to block, then release.
	time.Sleep(50 * time.Millisecond)
	release()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("blocked Acquire: %v, want nil (should unblock on release)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked Acquire never unblocked after release")
	}
}

// TestAcquireRespectsContextCancellation asserts a canceled context returns
// promptly rather than waiting out some internal poll interval.
func TestAcquireRespectsContextCancellation(t *testing.T) {
	l := New(1, 1)
	release := mustAcquire(t, l) // consume the only slot
	defer release()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := l.Acquire(ctx)
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Acquire: got nil error after context cancellation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not return after context cancellation")
	}
}

// TestAcquirePreCanceledContext asserts an already-canceled context fails
// fast even when a slot is immediately available.
func TestAcquirePreCanceledContext(t *testing.T) {
	l := New(4, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("Acquire with a pre-canceled context: got nil error, want ctx.Err()")
	}
}

// TestReportSuccessWidensAfterWindow asserts the ceiling grows by exactly
// one after successWindow consecutive ReportSuccess calls, not before, and
// never past max.
func TestReportSuccessWidensAfterWindow(t *testing.T) {
	l := New(2, 3, WithSuccessWindow(3))

	for i := 0; i < 2; i++ {
		l.ReportSuccess()
		if got := l.Current(); got != 2 {
			t.Fatalf("after %d ReportSuccess calls: Current() = %d, want unchanged 2", i+1, got)
		}
	}
	l.ReportSuccess() // 3rd -- completes the window
	if got := l.Current(); got != 3 {
		t.Fatalf("after 3 ReportSuccess calls: Current() = %d, want 3 (widened by 1)", got)
	}

	// Already at max: further success windows must not exceed it.
	for i := 0; i < 10; i++ {
		l.ReportSuccess()
	}
	if got := l.Current(); got != 3 {
		t.Fatalf("Current() = %d, want capped at max=3", got)
	}
}

// TestReportThrottleHalvesImmediatelyAndFloorsAtOne asserts backoff is
// immediate (no window) and never drops below 1.
func TestReportThrottleHalvesImmediatelyAndFloorsAtOne(t *testing.T) {
	l := New(8, 16)
	l.ReportThrottle()
	if got := l.Current(); got != 4 {
		t.Fatalf("Current() after one throttle = %d, want 4 (halved from 8)", got)
	}
	l.ReportThrottle()
	if got := l.Current(); got != 2 {
		t.Fatalf("Current() after two throttles = %d, want 2", got)
	}
	l.ReportThrottle()
	if got := l.Current(); got != 1 {
		t.Fatalf("Current() after three throttles = %d, want 1", got)
	}
	l.ReportThrottle()
	if got := l.Current(); got != 1 {
		t.Fatalf("Current() = %d, want floored at 1, not 0", got)
	}
}

// TestReportThrottleDoesNotForceInFlightDown asserts an already-granted
// slot is not revoked by a subsequent ReportThrottle -- only future Acquire
// calls are affected.
func TestReportThrottleDoesNotForceInFlightDown(t *testing.T) {
	l := New(2, 2)
	r1 := mustAcquire(t, l)
	r2 := mustAcquire(t, l)

	l.ReportThrottle() // limit drops to 1, but 2 are already in flight

	// A new Acquire must block (inFlight=2 > limit=1) until a release.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx); err == nil {
		t.Fatal("Acquire granted a third slot immediately after backoff to 1 with 2 already in flight")
	}

	r1()
	r2()
}

// TestReportThrottleResetsSuccessStreak asserts a near-complete success
// window is discarded by an intervening throttle, so the very next success
// does not spuriously widen.
func TestReportThrottleResetsSuccessStreak(t *testing.T) {
	l := New(4, 8, WithSuccessWindow(3))
	l.ReportSuccess()
	l.ReportSuccess() // 2 of 3 -- one more would widen
	l.ReportThrottle()
	before := l.Current()
	l.ReportSuccess() // must count as 1 of a fresh window, not complete an old one
	if got := l.Current(); got != before {
		t.Fatalf("Current() = %d, want unchanged %d (streak must reset on throttle)", got, before)
	}
}

// TestWithBackoffFactorOverride asserts a custom factor is honored, and an
// out-of-range value is ignored rather than accepted silently.
func TestWithBackoffFactorOverride(t *testing.T) {
	l := New(10, 20, WithBackoffFactor(0.25))
	l.ReportThrottle()
	if got := l.Current(); got != 2 {
		t.Fatalf("Current() = %d, want 2 (10 * 0.25)", got)
	}

	l2 := New(10, 20, WithBackoffFactor(1.5)) // out of range, must be ignored
	l2.ReportThrottle()
	if got := l2.Current(); got != 5 {
		t.Fatalf("Current() = %d, want 5 (default 0.5 factor since 1.5 is out of range)", got)
	}
}

// TestConcurrentAcquireReleaseRace exercises Acquire/release/ReportSuccess/
// ReportThrottle concurrently -- this is the test `go test -race` is meant
// to catch a data race in.
func TestConcurrentAcquireReleaseRace(t *testing.T) {
	l := New(3, 6, WithSuccessWindow(2))
	var wg sync.WaitGroup
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				release, err := l.Acquire(ctx)
				if err != nil {
					return
				}
				if (n+j)%7 == 0 {
					l.ReportThrottle()
				} else {
					l.ReportSuccess()
				}
				release()
			}
		}(i)
	}
	wg.Wait()

	if got := l.Current(); got < 1 || got > 6 {
		t.Fatalf("Current() = %d, want within [1, 6]", got)
	}
}
