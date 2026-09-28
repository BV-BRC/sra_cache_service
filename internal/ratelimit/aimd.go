// Package ratelimit implements the AIMD (additive-increase,
// multiplicative-decrease) prefetch concurrency limiter described in
// PLAN-sra-precache-service.md section 4: the safe number of concurrent
// `prefetch` downloads against NCBI isn't knowable in advance (NCBI's SRA
// Data Locator may serve a given accession's bytes from its own servers or
// from an AWS/GCP mirror, so there isn't even one true constant to measure),
// so the limiter starts conservative and adapts to whatever throttling it
// actually observes -- the same idea TCP congestion control uses for an
// analogous problem.
//
// See PLAN-sra-cache-service-implementation.md, Phase 6.
package ratelimit

import (
	"context"
	"sync"
)

// Tuning constants. Both are deliberately unvalidated starting points --
// per the design doc, AIMD's whole point is that the real operating point is
// discovered at runtime by observing NCBI's actual behavior, not
// pre-measured in a benchmark. They're documented here (not just "picked")
// so Phase 10's live-verification pass has a stated rationale to revise
// against, and are overridable per-Limiter via WithSuccessWindow /
// WithBackoffFactor (config-overridable, per the design doc) without a
// package-level global.
const (
	// DefaultSuccessWindow is how many consecutive clean completions
	// (ReportSuccess calls with no intervening ReportThrottle) it takes
	// before the ceiling widens by one step. Chosen as a round number
	// balancing two failure modes: too small (e.g. 1-2) makes the limiter
	// probe past NCBI's real ceiling almost immediately after any brief
	// lull in throttling, causing it to oscillate; too large (e.g. 100+)
	// wastes a lot of achievable throughput climbing back after a single
	// backoff. 20 is a middle-ground guess, not a measured value.
	DefaultSuccessWindow = 20

	// DefaultBackoffFactor is the multiplicative factor applied to the
	// current ceiling on ReportThrottle, floored at 1. 0.5 (halving)
	// mirrors both the design doc's own worked example ("multiplicatively
	// backs off (e.g. halves)") and TCP's classic multiplicative-decrease
	// factor -- a well-understood, aggressive-enough response to a real
	// congestion signal that it should clear a transient throttle quickly,
	// at the cost of giving up more headroom than a gentler factor would.
	DefaultBackoffFactor = 0.5
)

// Option configures a Limiter at construction time.
type Option func(*Limiter)

// WithSuccessWindow overrides DefaultSuccessWindow.
func WithSuccessWindow(n int) Option {
	return func(l *Limiter) {
		if n > 0 {
			l.successWindow = n
		}
	}
}

// WithBackoffFactor overrides DefaultBackoffFactor. Values outside (0, 1)
// are ignored (0 or negative would never allow recovery upward; >= 1
// wouldn't back off at all).
func WithBackoffFactor(f float64) Option {
	return func(l *Limiter) {
		if f > 0 && f < 1 {
			l.backoffFactor = f
		}
	}
}

// Limiter is an AIMD-adaptive concurrency gate. The zero value is not
// usable; construct with New.
//
// Deliberately NOT persisted across restarts: PLAN-sra-cache-service-implementation.md's
// Phase 6 section calls this out explicitly -- a persisted ceiling would
// carry a stale optimistic number across exactly the events most likely to
// have invalidated it (a config change, a network change, or a restart
// caused by NCBI-side trouble in the first place). Every New() call starts
// fresh at initial; the ramp back up costs a little time and buys
// correctness. There is no Save/Load pair on this type on purpose.
type Limiter struct {
	mu       sync.Mutex
	max      int // hard ceiling; ReportSuccess can never widen past this
	limit    int // current concurrency ceiling (the AIMD "cwnd")
	inFlight int

	successWindow int
	backoffFactor float64
	successStreak int

	// waitCh is closed (and replaced) under mu whenever state changes in a
	// way that might unblock an Acquire waiter -- a slot freed by release,
	// or the ceiling widening. A blocked Acquire selects on the channel it
	// observed and retries once it's closed, avoiding a busy-poll loop
	// without needing a sync.Cond (whose Wait doesn't compose with
	// context cancellation).
	waitCh chan struct{}
}

// New builds a Limiter starting at concurrency initial, never exceeding
// max. initial is clamped to at least 1; max is clamped to at least
// initial (a limiter that could never grant even its own starting
// concurrency would be a caller bug, not a runtime condition to handle
// silently).
func New(initial, max int, opts ...Option) *Limiter {
	if initial < 1 {
		initial = 1
	}
	if max < initial {
		max = initial
	}
	l := &Limiter{
		max:           max,
		limit:         initial,
		successWindow: DefaultSuccessWindow,
		backoffFactor: DefaultBackoffFactor,
		waitCh:        make(chan struct{}),
	}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

// Current returns the limiter's live concurrency ceiling, exposed as a
// metric per the design doc ("the live value should be exposed as a
// metric").
func (l *Limiter) Current() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.limit
}

// notifyLocked wakes every Acquire currently blocked on the previous
// waitCh. Callers must hold l.mu.
func (l *Limiter) notifyLocked() {
	close(l.waitCh)
	l.waitCh = make(chan struct{})
}

// Acquire blocks until a concurrency slot is available or ctx is done,
// whichever comes first. On success it returns a release func the caller
// must call exactly once (subsequent calls are no-ops) to give the slot
// back; on failure it returns ctx.Err() and no release func.
func (l *Limiter) Acquire(ctx context.Context) (release func(), err error) {
	for {
		l.mu.Lock()
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return nil, err
		}
		if l.inFlight < l.limit {
			l.inFlight++
			l.mu.Unlock()
			return l.releaseFunc(), nil
		}
		wait := l.waitCh
		l.mu.Unlock()

		select {
		case <-wait:
			// State changed (a release or a widened ceiling) -- loop and
			// recheck rather than assuming a slot is ours; another
			// waiter may have taken it first.
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (l *Limiter) releaseFunc() func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.inFlight--
			l.notifyLocked()
			l.mu.Unlock()
		})
	}
}

// ReportSuccess records one more clean completion (a fetch that finished
// without a throttling signal). After successWindow consecutive clean
// completions since the last widen or backoff, the ceiling grows by one
// step, bounded by max -- the "additive increase" half of AIMD.
func (l *Limiter) ReportSuccess() {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.limit >= l.max {
		// Already at the hard ceiling; nothing to count toward.
		l.successStreak = 0
		return
	}

	l.successStreak++
	if l.successStreak >= l.successWindow {
		l.limit++
		l.successStreak = 0
		l.notifyLocked() // a wider ceiling may free a slot for a waiter
	}
}

// ReportThrottle immediately (not waiting for a window) multiplies the
// current ceiling by backoffFactor, floored at 1, and resets the success
// streak so a near-complete widen doesn't fire on stale progress right
// after a real throttling signal -- the "multiplicative decrease" half of
// AIMD.
//
// This only ever lowers limit; it never forces an already-in-flight fetch
// to stop, so inFlight may transiently exceed the new limit until enough
// releases bring it back down. No waiter needs waking: a smaller ceiling
// can only make Acquire block more, never less.
func (l *Limiter) ReportThrottle() {
	l.mu.Lock()
	defer l.mu.Unlock()

	next := int(float64(l.limit) * l.backoffFactor)
	if next < 1 {
		next = 1
	}
	l.limit = next
	l.successStreak = 0
}
