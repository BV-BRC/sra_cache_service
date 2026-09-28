package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
)

// seedCachedEntry both publishes a real on-disk file of the given size and
// records the matching catalog row, so eviction tests exercise the same
// "file + row disappear together" path Evict actually performs.
func seedCachedEntry(t *testing.T, ctx context.Context, cat *catalog.Catalog, c *Cache, accession string, size int64, lastAccess time.Time, pinnedUntil time.Time) {
	t.Helper()
	if _, _, err := c.Publish(accession, strings.NewReader(strings.Repeat("x", int(size)))); err != nil {
		t.Fatalf("seed Publish %s: %v", accession, err)
	}
	e := catalog.Entry{
		Accession:      accession,
		State:          catalog.StateCached,
		SizeBytes:      size,
		CachedTime:     lastAccess,
		LastAccessTime: lastAccess,
		PinnedUntil:    pinnedUntil,
	}
	if err := cat.Upsert(ctx, e); err != nil {
		t.Fatalf("seed Upsert %s: %v", accession, err)
	}
}

func openTestCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	ctx := context.Background()
	cat, err := catalog.Open(ctx, filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected %s to exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Errorf("expected %s to be gone, but it still exists", path)
	}
}

// TestEvictLRUOrder asserts, by name, that Evict removes cached entries
// oldest-last-accessed-first, and stops once the target is met rather than
// over-evicting.
func TestEvictLRUOrder(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	c := New(t.TempDir())
	now := time.Now()

	// Four 100-byte entries, oldest to newest.
	seedCachedEntry(t, ctx, cat, c, "OLDEST", 100, now.Add(-4*time.Hour), time.Time{})
	seedCachedEntry(t, ctx, cat, c, "OLDER", 100, now.Add(-3*time.Hour), time.Time{})
	seedCachedEntry(t, ctx, cat, c, "NEWER", 100, now.Add(-2*time.Hour), time.Time{})
	seedCachedEntry(t, ctx, cat, c, "NEWEST", 100, now.Add(-1*time.Hour), time.Time{})

	// Ask for 150 bytes: must evict OLDEST (100) then OLDER (100) = 200 >=
	// 150, stopping there -- NEWER and NEWEST must survive.
	if err := Evict(ctx, cat, c, now, 150); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	for _, acc := range []string{"OLDEST", "OLDER"} {
		if _, ok, err := cat.Get(ctx, acc); err != nil || ok {
			t.Errorf("%s: expected evicted (row gone), ok=%v err=%v", acc, ok, err)
		}
		mustNotExist(t, c.Path(acc))
	}
	for _, acc := range []string{"NEWER", "NEWEST"} {
		if _, ok, err := cat.Get(ctx, acc); err != nil || !ok {
			t.Errorf("%s: expected to survive, ok=%v err=%v", acc, ok, err)
		}
		mustExist(t, c.Path(acc))
	}

	total, err := cat.TotalCachedBytes(ctx)
	if err != nil {
		t.Fatalf("TotalCachedBytes: %v", err)
	}
	if total != 200 {
		t.Errorf("TotalCachedBytes after Evict = %d, want 200 (NEWER+NEWEST only)", total)
	}
}

// TestEvictDoesNotOverEvict checks the "frees at least the requested space
// and stops there" requirement explicitly: when one candidate would have
// sufficed, Evict must not also remove the next-oldest one.
func TestEvictDoesNotOverEvict(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	c := New(t.TempDir())
	now := time.Now()

	seedCachedEntry(t, ctx, cat, c, "OLDEST", 500, now.Add(-3*time.Hour), time.Time{})
	seedCachedEntry(t, ctx, cat, c, "MIDDLE", 500, now.Add(-2*time.Hour), time.Time{})
	seedCachedEntry(t, ctx, cat, c, "NEWEST", 500, now.Add(-1*time.Hour), time.Time{})

	// Only 300 bytes needed: OLDEST alone (500) covers it.
	if err := Evict(ctx, cat, c, now, 300); err != nil {
		t.Fatalf("Evict: %v", err)
	}

	if _, ok, _ := cat.Get(ctx, "OLDEST"); ok {
		t.Errorf("OLDEST should have been evicted")
	}
	for _, acc := range []string{"MIDDLE", "NEWEST"} {
		if _, ok, _ := cat.Get(ctx, acc); !ok {
			t.Errorf("%s should NOT have been evicted (one candidate sufficed)", acc)
		}
		mustExist(t, c.Path(acc))
	}
}

// TestEvictPinnedSurvivesPressure asserts a pinned (pinned_until in the
// future) entry is never evicted, even when it is the oldest candidate and
// eviction pressure would otherwise reach it.
func TestEvictPinnedSurvivesPressure(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	c := New(t.TempDir())
	now := time.Now()

	// PINNED is the oldest (most eligible by LRU) but leased into the future.
	seedCachedEntry(t, ctx, cat, c, "PINNED", 500, now.Add(-10*time.Hour), now.Add(time.Hour))
	seedCachedEntry(t, ctx, cat, c, "UNPINNED", 500, now.Add(-1*time.Hour), time.Time{})

	// Ask for more than UNPINNED alone can supply -- if PINNED were
	// eviction-eligible this would succeed by consuming it too.
	err := Evict(ctx, cat, c, now, 600)
	if !errors.Is(err, ErrInsufficientEvictableSpace) {
		t.Fatalf("Evict with only a pinned candidate left: err = %v, want ErrInsufficientEvictableSpace", err)
	}

	if _, ok, _ := cat.Get(ctx, "PINNED"); !ok {
		t.Errorf("PINNED must survive eviction pressure while its lease is unexpired")
	}
	mustExist(t, c.Path("PINNED"))
	// UNPINNED should still have been evicted (the max that could be freed).
	if _, ok, _ := cat.Get(ctx, "UNPINNED"); ok {
		t.Errorf("UNPINNED should have been evicted as the only eligible candidate")
	}
}

// TestEvictExpiredPinIsEvictable asserts a pin whose pinned_until is in the
// past is treated as unpinned.
func TestEvictExpiredPinIsEvictable(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	c := New(t.TempDir())
	now := time.Now()

	seedCachedEntry(t, ctx, cat, c, "EXPIRED_PIN", 500, now.Add(-10*time.Hour), now.Add(-time.Minute))

	if err := Evict(ctx, cat, c, now, 100); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if _, ok, _ := cat.Get(ctx, "EXPIRED_PIN"); ok {
		t.Errorf("entry with an expired pin should have been evictable")
	}
	mustNotExist(t, c.Path("EXPIRED_PIN"))
}

// TestEnsureSpaceOversizeRejection asserts an incoming size larger than
// maxCacheSize is rejected via ErrTooLargeForCache, and touches nothing
// already cached.
func TestEnsureSpaceOversizeRejection(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	c := New(t.TempDir())
	now := time.Now()

	seedCachedEntry(t, ctx, cat, c, "EXISTING", 100, now.Add(-time.Hour), time.Time{})

	err := EnsureSpace(ctx, cat, c, 1000 /* incoming */, 500 /* maxCacheSize */)
	if !errors.Is(err, ErrTooLargeForCache) {
		t.Fatalf("EnsureSpace: err = %v, want ErrTooLargeForCache", err)
	}

	// Existing cached entry must be completely untouched.
	if _, ok, gerr := cat.Get(ctx, "EXISTING"); gerr != nil || !ok {
		t.Errorf("EXISTING disturbed by oversize rejection: ok=%v err=%v", ok, gerr)
	}
	mustExist(t, c.Path("EXISTING"))
	total, err := cat.TotalCachedBytes(ctx)
	if err != nil {
		t.Fatalf("TotalCachedBytes: %v", err)
	}
	if total != 100 {
		t.Errorf("TotalCachedBytes = %d, want unchanged 100", total)
	}
}

// TestEnsureSpaceFreesEnoughAndNoMore covers EnsureSpace's normal path: it
// evicts the minimum needed to fit the incoming file under maxCacheSize.
func TestEnsureSpaceFreesEnoughAndNoMore(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	c := New(t.TempDir())
	now := time.Now()

	seedCachedEntry(t, ctx, cat, c, "OLDEST", 400, now.Add(-3*time.Hour), time.Time{})
	seedCachedEntry(t, ctx, cat, c, "MIDDLE", 400, now.Add(-2*time.Hour), time.Time{})
	seedCachedEntry(t, ctx, cat, c, "NEWEST", 400, now.Add(-1*time.Hour), time.Time{})
	// Total cached = 1200. maxCacheSize = 1500. Incoming = 400 -> projected
	// 1600, 100 over budget -> evicting OLDEST (400) is more than enough and
	// is the only one that should go.
	if err := EnsureSpace(ctx, cat, c, 400, 1500); err != nil {
		t.Fatalf("EnsureSpace: %v", err)
	}

	if _, ok, _ := cat.Get(ctx, "OLDEST"); ok {
		t.Errorf("OLDEST should have been evicted to make room")
	}
	for _, acc := range []string{"MIDDLE", "NEWEST"} {
		if _, ok, _ := cat.Get(ctx, acc); !ok {
			t.Errorf("%s should not have been evicted", acc)
		}
	}
}

// TestEnsureSpaceNoEvictionNeeded checks the no-op path: plenty of headroom
// already, nothing should be evicted.
func TestEnsureSpaceNoEvictionNeeded(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	c := New(t.TempDir())
	now := time.Now()

	seedCachedEntry(t, ctx, cat, c, "ONLY", 100, now.Add(-time.Hour), time.Time{})

	if err := EnsureSpace(ctx, cat, c, 100, 10_000); err != nil {
		t.Fatalf("EnsureSpace: %v", err)
	}
	if _, ok, _ := cat.Get(ctx, "ONLY"); !ok {
		t.Errorf("ONLY should not have been evicted when there is plenty of headroom")
	}
}
