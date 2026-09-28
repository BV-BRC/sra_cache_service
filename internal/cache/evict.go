package cache

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
)

// ErrTooLargeForCache is returned by EnsureSpace when a single incoming
// file's size exceeds the configured max-cache-size -- no amount of
// eviction can ever make room for it. Per PLAN-sra-precache-service.md §3,
// the caller (Phase 6's fetcher) is responsible for recording this as a
// catalog `error`-state row with an explanatory message; this package only
// has to make the condition cleanly detectable (errors.Is).
var ErrTooLargeForCache = errors.New("cache: incoming size exceeds max-cache-size")

// ErrInsufficientEvictableSpace is returned when evicting every unpinned,
// eviction-eligible cached entry still would not free the requested amount
// -- i.e. the remaining occupied space is held by live pin leases (unexpired
// signed URLs). Evict never removes a pinned entry to reach its target.
var ErrInsufficientEvictableSpace = errors.New("cache: insufficient evictable (unpinned) space")

// Evict deletes cached entries in LRU order (oldest last_access_time first,
// per catalog.Catalog.EvictionCandidates), skipping any entry whose
// pinned_until lease has not yet expired as of now, until at least
// targetFreeBytes has been freed. It stops as soon as that target is met --
// it does not evict more than necessary.
//
// Both the on-disk file (via cache.Remove) and the catalog row (deleted
// outright -- see catalog.Catalog.Delete's doc comment for why there's no
// distinct "evicted" state) are removed for each evicted accession.
//
// Returns ErrInsufficientEvictableSpace if every eligible candidate is
// evicted and targetFreeBytes still hasn't been reached; whatever could be
// freed has still been freed in that case.
func Evict(ctx context.Context, cat *catalog.Catalog, c *Cache, now time.Time, targetFreeBytes int64) error {
	if targetFreeBytes <= 0 {
		return nil
	}

	candidates, err := cat.EvictionCandidates(ctx, now)
	if err != nil {
		return fmt.Errorf("cache: evict: listing candidates: %w", err)
	}

	var freed int64
	for _, e := range candidates {
		if freed >= targetFreeBytes {
			break
		}
		if err := c.Remove(e.Accession); err != nil {
			return fmt.Errorf("cache: evict: removing %s: %w", e.Accession, err)
		}
		if err := cat.Delete(ctx, e.Accession); err != nil {
			return fmt.Errorf("cache: evict: updating catalog for %s: %w", e.Accession, err)
		}
		freed += e.SizeBytes
	}

	if freed < targetFreeBytes {
		return ErrInsufficientEvictableSpace
	}
	return nil
}

// EnsureSpace is the "before publishing a new file" check a Phase-6 fetcher
// calls. If incomingSize alone could never fit under maxCacheSize, it
// returns ErrTooLargeForCache immediately without touching any existing
// cached entry. Otherwise it evicts (LRU order, pin-aware, via Evict) just
// enough currently-cached bytes to bring projected usage (existing cached
// bytes + incomingSize) at or under maxCacheSize.
//
// If eviction cannot free enough unpinned space, EnsureSpace returns
// ErrInsufficientEvictableSpace (propagated from Evict) rather than ever
// evicting a pinned entry.
func EnsureSpace(ctx context.Context, cat *catalog.Catalog, c *Cache, incomingSize, maxCacheSize int64) error {
	if incomingSize > maxCacheSize {
		return ErrTooLargeForCache
	}

	total, err := cat.TotalCachedBytes(ctx)
	if err != nil {
		return fmt.Errorf("cache: ensure space: %w", err)
	}

	projected := total + incomingSize
	if projected <= maxCacheSize {
		return nil
	}

	return Evict(ctx, cat, c, time.Now(), projected-maxCacheSize)
}
