package fetcher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/cache"
	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/ratelimit"
)

// pollInterval is how often Pool.Run re-scans the catalog for queued rows
// when nothing has explicitly called Wake. It also doubles as the natural
// pacing for retrying an accession Classify sent back to StateQueued (a
// throttle signal, not a hard error -- see classify.go) rather than needing
// a second, dedicated retry-backoff timer: a throttled accession simply
// waits for the next poll (or the one after, since ReportThrottle also
// lowers the limiter's ceiling in the same call).
const pollInterval = 2 * time.Second

// batchSize bounds how many queued rows one poll asks the catalog for, so a
// large backlog is drained incrementally rather than forcing one huge query
// (and one huge in-flight dedupe pass) per poll.
const batchSize = 50

// Pool is a bounded worker pool that drains catalog.StateQueued rows
// through a Fetcher, publishes successful downloads into a Cache, and
// reports outcomes back to both the catalog and an AIMD ratelimit.Limiter.
// Concurrency is bounded by the Limiter, not by the pool itself -- the pool
// will happily have many goroutines in flight waiting on Limiter.Acquire;
// the limiter is what actually caps how many run `prefetch` at once.
//
// See PLAN-sra-cache-service-implementation.md, Phase 6, and
// PLAN-sra-precache-service.md sections 3-4.
type Pool struct {
	Catalog      *catalog.Catalog
	Cache        *cache.Cache
	Fetcher      *Fetcher
	Limiter      *ratelimit.Limiter
	MaxCacheSize int64
	Logger       *slog.Logger

	mu       sync.Mutex
	inFlight map[string]bool
	wake     chan struct{}
	wg       sync.WaitGroup
}

// NewPool builds a Pool. logger may be nil (defaults to slog.Default()).
func NewPool(cat *catalog.Catalog, c *cache.Cache, f *Fetcher, limiter *ratelimit.Limiter, maxCacheSize int64, logger *slog.Logger) *Pool {
	if logger == nil {
		logger = slog.Default()
	}
	return &Pool{
		Catalog:      cat,
		Cache:        c,
		Fetcher:      f,
		Limiter:      limiter,
		MaxCacheSize: maxCacheSize,
		Logger:       logger,
		inFlight:     make(map[string]bool),
		wake:         make(chan struct{}, 1),
	}
}

// Wake nudges Run to check for newly queued rows immediately instead of
// waiting up to pollInterval -- e.g. the start_precache RPC handler calls
// this right after catalog.Enqueue so a fresh request doesn't sit idle for
// up to pollInterval before anything picks it up. Safe to call from any
// goroutine, non-blocking, and coalesces multiple pending wakeups into one.
func (p *Pool) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Run is the pool's main loop: it first recovers anything stuck
// catalog.StateInProgress from a prior crash (catalog.Catalog.Requeue), then
// repeatedly drains catalog.StateQueued rows until ctx is done.
//
// Run is meant to be started as its own goroutine by main.go, using the
// same shutdown context (signal.NotifyContext) that already drives the two
// HTTP servers' Shutdown calls -- main.go adds this goroutine to the same
// sync.WaitGroup it already waits on, rather than inventing a second,
// parallel shutdown mechanism. Cancelling ctx both stops Run's poll loop and
// -- via exec.CommandContext inside Fetcher.Fetch -- kills any `prefetch`
// subprocess currently running, so Run returns promptly rather than waiting
// out a long-running fetch. Any accession that was in flight at that point
// is left in_progress; Requeue recovers it on the next startup, the same as
// after a crash.
//
// Run returns nil on a normal shutdown (ctx cancellation). It returns a
// non-nil error only if the initial Requeue call fails, since that means
// the catalog itself is unusable and continuing would just mean silently
// never processing anything.
func (p *Pool) Run(ctx context.Context) error {
	if err := p.Catalog.Requeue(ctx); err != nil {
		return fmt.Errorf("fetcher: pool startup requeue: %w", err)
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		p.drainOnce(ctx)

		select {
		case <-ctx.Done():
			p.wg.Wait() // let in-flight fetches unwind (bounded by ctx cancellation killing their subprocess)
			return nil
		case <-ticker.C:
		case <-p.wake:
		}
	}
}

// drainOnce lists up to batchSize catalog.StateQueued rows and launches a
// goroutine (dedupe-guarded by inFlight) for each one not already being
// processed. If a full batch came back and at least one entry was newly
// launched, it immediately asks for another batch -- there may be more
// queued rows behind this one -- rather than waiting for the next poll
// tick. It stops as soon as a batch comes back short (queue looks drained)
// or, on a full batch, nothing new was launched (every returned row is
// already in flight -- asking again immediately would just spin against the
// catalog until one of them transitions out of StateQueued).
func (p *Pool) drainOnce(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		entries, err := p.Catalog.ListByState(ctx, catalog.StateQueued, batchSize)
		if err != nil {
			p.Logger.Error("fetcher: listing queued accessions", "err", err)
			return
		}

		launched := 0
		for _, e := range entries {
			if !p.markInFlight(e.Accession) {
				continue
			}
			launched++
			p.wg.Add(1)
			go func(accession string) {
				defer p.wg.Done()
				defer p.clearInFlight(accession)
				p.process(ctx, accession)
			}(e.Accession)
		}

		if len(entries) < batchSize || launched == 0 {
			return
		}
	}
}

func (p *Pool) markInFlight(accession string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.inFlight[accession] {
		return false
	}
	p.inFlight[accession] = true
	return true
}

func (p *Pool) clearInFlight(accession string) {
	p.mu.Lock()
	delete(p.inFlight, accession)
	p.mu.Unlock()
}

// process runs one accession end to end: mark in_progress, acquire an AIMD
// slot, fetch, and -- inside the fetch's consume callback, i.e. while the
// staged file still exists, and only once its real size is known --
// EnsureSpace then Publish. See EnsureSpace's own doc comment
// (internal/cache/evict.go) for why the space check happens here rather
// than before the fetch: it is explicitly "the 'before publishing a new
// file' check", and the incoming size isn't known until the fetch completes.
func (p *Pool) process(ctx context.Context, accession string) {
	if err := p.Catalog.SetState(ctx, accession, catalog.StateInProgress, ""); err != nil {
		p.Logger.Error("fetcher: marking in_progress", "accession", accession, "err", err)
		return
	}

	release, err := p.Limiter.Acquire(ctx)
	if err != nil {
		// ctx is done (shutdown, most likely) -- there is no fetch to
		// retry from here; put the row back so the next Run (after a
		// restart, via Requeue) or a later poll in a still-live process
		// picks it up again, rather than leaving it stuck in_progress
		// with nothing that will ever revisit it in this process.
		p.requeueAfterInterruption(accession, "waiting for a concurrency slot")
		return
	}
	defer release()

	var publishedSize int64
	fetchErr := p.Fetcher.Fetch(ctx, accession, func(path string, size int64) error {
		return p.publish(ctx, accession, path, size, &publishedSize)
	})

	p.finish(ctx, accession, publishedSize, fetchErr)
}

// publish is Fetch's consume callback: it enforces the cache size budget
// (evicting LRU entries if needed) and then streams the staged file into
// the cache. Its return value becomes Fetch's own returned error, which
// finish inspects to decide the catalog state -- see finish's doc comment
// for how cache.ErrTooLargeForCache / cache.ErrInsufficientEvictableSpace
// are distinguished from a fetch/classify failure there.
func (p *Pool) publish(ctx context.Context, accession, path string, size int64, publishedSize *int64) error {
	if err := cache.EnsureSpace(ctx, p.Catalog, p.Cache, size, p.MaxCacheSize); err != nil {
		return err
	}

	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening staged file: %w", err)
	}
	defer f.Close()

	_, n, err := p.Cache.Publish(accession, f)
	if err != nil {
		return fmt.Errorf("publishing to cache: %w", err)
	}
	*publishedSize = n
	return nil
}

// finish records the outcome of one process() call to the catalog (and, for
// a throttle or a genuine success, to the Limiter).
//
//   - fetchErr == nil: the file is already published (publish ran to
//     completion inside Fetch's consume callback) -- record size_bytes,
//     flip to StateCached, and report the clean completion to the limiter
//     (the AIMD "additive increase" signal).
//   - cache.ErrTooLargeForCache / cache.ErrInsufficientEvictableSpace: a
//     cache-capacity problem, not a network/NCBI problem -- StateError with
//     an explanatory message, and the limiter is left alone (this has
//     nothing to do with NCBI's throttling behavior).
//   - a *PrefetchError: handed to classify.Classify, which both returns the
//     right terminal (or, for a throttle, StateQueued-for-retry) state and,
//     for a throttle, calls Limiter.ReportThrottle() itself.
//   - anything else while ctx is already done: shutdown raced the fetch --
//     requeue rather than recording a spurious error.
//   - anything else: an unexpected failure (e.g. failing to open the staged
//     file) -- StateError with the raw error text.
func (p *Pool) finish(ctx context.Context, accession string, publishedSize int64, fetchErr error) {
	if fetchErr == nil {
		if err := p.Catalog.SetSizeBytes(ctx, accession, publishedSize); err != nil {
			p.Logger.Error("fetcher: recording size_bytes", "accession", accession, "err", err)
		}
		if err := p.Catalog.SetState(ctx, accession, catalog.StateCached, ""); err != nil {
			p.Logger.Error("fetcher: marking cached", "accession", accession, "err", err)
		}
		p.Limiter.ReportSuccess()
		return
	}

	if errors.Is(fetchErr, cache.ErrTooLargeForCache) {
		p.setTerminalState(accession, catalog.StateError, fmt.Sprintf(
			"accession exceeds max-cache-size and cannot be cached: %s", fetchErr))
		return
	}
	if errors.Is(fetchErr, cache.ErrInsufficientEvictableSpace) {
		p.setTerminalState(accession, catalog.StateError, fmt.Sprintf(
			"could not free enough cache space (remaining space is held by pinned entries): %s", fetchErr))
		return
	}

	var perr *PrefetchError
	if errors.As(fetchErr, &perr) {
		state, msg := Classify(perr.ExitCode, perr.Output, p.Limiter)
		p.setTerminalState(accession, state, msg)
		return
	}

	if ctx.Err() != nil {
		p.requeueAfterInterruption(accession, "shutdown during fetch")
		return
	}
	p.setTerminalState(accession, catalog.StateError, fetchErr.Error())
}

// setTerminalState writes state/message using context.Background() rather
// than the (possibly already-cancelled) ctx passed to process/finish -- a
// shutdown in progress must not prevent recording why an accession failed;
// only requeueAfterInterruption's "leave it for later" case skips writing at
// all.
func (p *Pool) setTerminalState(accession string, state catalog.State, message string) {
	if err := p.Catalog.SetState(context.Background(), accession, state, message); err != nil {
		p.Logger.Error("fetcher: recording terminal state", "accession", accession, "state", state, "err", err)
	}
}

func (p *Pool) requeueAfterInterruption(accession, reason string) {
	p.Logger.Warn("fetcher: interrupted, requeuing", "accession", accession, "reason", reason)
	if err := p.Catalog.SetState(context.Background(), accession, catalog.StateQueued, ""); err != nil {
		p.Logger.Error("fetcher: requeuing after interruption", "accession", accession, "err", err)
	}
}
