package catalog

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Catalog {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "catalog.db")
	cat, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })
	return cat
}

// TestCatalogCRUDRoundTrip exercises Upsert/Get/GetMany/TouchAccess/Pin/
// TotalCachedBytes together, per the "Basic Catalog CRUD round-trips" item
// in the phase-5 test list.
func TestCatalogCRUDRoundTrip(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	queued := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cached := queued.Add(time.Hour)
	lastAccess := cached.Add(time.Minute)

	e := Entry{
		Accession:      "SRR00000001",
		State:          StateCached,
		Message:        "ok",
		SizeBytes:      12345,
		QueuedTime:     queued,
		CachedTime:     cached,
		LastAccessTime: lastAccess,
		ErrorCount:     0,
	}
	if err := cat.Upsert(ctx, e); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, ok, err := cat.Get(ctx, e.Accession)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatalf("Get: accession not found after Upsert")
	}
	if got.Accession != e.Accession || got.State != e.State || got.Message != e.Message || got.SizeBytes != e.SizeBytes {
		t.Fatalf("Get: got %+v, want fields matching %+v", got, e)
	}
	if !got.QueuedTime.Equal(e.QueuedTime) {
		t.Errorf("QueuedTime = %v, want %v", got.QueuedTime, e.QueuedTime)
	}
	if !got.CachedTime.Equal(e.CachedTime) {
		t.Errorf("CachedTime = %v, want %v", got.CachedTime, e.CachedTime)
	}
	if !got.LastAccessTime.Equal(e.LastAccessTime) {
		t.Errorf("LastAccessTime = %v, want %v", got.LastAccessTime, e.LastAccessTime)
	}
	if !got.PinnedUntil.IsZero() {
		t.Errorf("PinnedUntil = %v, want zero (unset)", got.PinnedUntil)
	}

	// Get on a missing accession: ok == false, no error.
	_, ok, err = cat.Get(ctx, "SRR99999999")
	if err != nil {
		t.Fatalf("Get on missing accession: unexpected error %v", err)
	}
	if ok {
		t.Fatalf("Get on missing accession: ok = true, want false")
	}

	// A second accession for GetMany / TotalCachedBytes.
	e2 := Entry{Accession: "SRR00000002", State: StateQueued, QueuedTime: queued}
	if err := cat.Upsert(ctx, e2); err != nil {
		t.Fatalf("Upsert e2: %v", err)
	}

	many, err := cat.GetMany(ctx, []string{e.Accession, e2.Accession, "SRR_UNKNOWN"})
	if err != nil {
		t.Fatalf("GetMany: %v", err)
	}
	if len(many) != 2 {
		t.Fatalf("GetMany: got %d entries, want 2 (%v)", len(many), many)
	}
	if _, ok := many["SRR_UNKNOWN"]; ok {
		t.Errorf("GetMany: unknown accession unexpectedly present")
	}

	// GetMany with an empty list must not error and must return an empty map.
	empty, err := cat.GetMany(ctx, nil)
	if err != nil {
		t.Fatalf("GetMany(nil): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("GetMany(nil): got %d entries, want 0", len(empty))
	}

	// TotalCachedBytes only counts state=cached rows (e2 is queued).
	total, err := cat.TotalCachedBytes(ctx)
	if err != nil {
		t.Fatalf("TotalCachedBytes: %v", err)
	}
	if total != e.SizeBytes {
		t.Errorf("TotalCachedBytes = %d, want %d", total, e.SizeBytes)
	}

	// TouchAccess updates last_access_time to (approximately) now.
	before := time.Now()
	if err := cat.TouchAccess(ctx, e.Accession); err != nil {
		t.Fatalf("TouchAccess: %v", err)
	}
	got, _, err = cat.Get(ctx, e.Accession)
	if err != nil {
		t.Fatalf("Get after TouchAccess: %v", err)
	}
	if got.LastAccessTime.Before(before.Add(-time.Second)) {
		t.Errorf("TouchAccess: LastAccessTime %v looks stale (before %v)", got.LastAccessTime, before)
	}

	// Pin sets pinned_until.
	until := time.Now().Add(time.Hour)
	if err := cat.Pin(ctx, e.Accession, until); err != nil {
		t.Fatalf("Pin: %v", err)
	}
	got, _, err = cat.Get(ctx, e.Accession)
	if err != nil {
		t.Fatalf("Get after Pin: %v", err)
	}
	if !got.PinnedUntil.Equal(until) {
		t.Errorf("PinnedUntil = %v, want %v", got.PinnedUntil, until)
	}
}

func TestCatalogSetStateNotFound(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	if err := cat.SetState(ctx, "SRR_NOPE", StateCached, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetState on unknown accession: err = %v, want ErrNotFound", err)
	}
	if err := cat.TouchAccess(ctx, "SRR_NOPE"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TouchAccess on unknown accession: err = %v, want ErrNotFound", err)
	}
	if err := cat.Pin(ctx, "SRR_NOPE", time.Now()); !errors.Is(err, ErrNotFound) {
		t.Errorf("Pin on unknown accession: err = %v, want ErrNotFound", err)
	}
}

func TestCatalogSetStateCachedStampsTime(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	if err := cat.Enqueue(ctx, "SRR1"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := cat.SetState(ctx, "SRR1", StateInProgress, ""); err != nil {
		t.Fatalf("SetState in_progress: %v", err)
	}

	before := time.Now()
	if err := cat.SetState(ctx, "SRR1", StateCached, "done"); err != nil {
		t.Fatalf("SetState cached: %v", err)
	}
	e, ok, err := cat.Get(ctx, "SRR1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.State != StateCached {
		t.Errorf("State = %q, want cached", e.State)
	}
	if e.CachedTime.Before(before.Add(-time.Second)) {
		t.Errorf("CachedTime %v not stamped near %v", e.CachedTime, before)
	}
}

func TestCatalogSetStateErrorIncrementsErrorCount(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	if err := cat.Enqueue(ctx, "SRR1"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if err := cat.SetState(ctx, "SRR1", StateError, "boom"); err != nil {
			t.Fatalf("SetState error (%d): %v", i, err)
		}
		e, _, err := cat.Get(ctx, "SRR1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if e.ErrorCount != i {
			t.Errorf("ErrorCount = %d, want %d", e.ErrorCount, i)
		}
	}
}

// TestEnqueueDoesNotClobber covers the Enqueue contract: it must not reset
// an in_progress or cached row back to queued.
func TestEnqueueDoesNotClobber(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	if err := cat.Enqueue(ctx, "SRR1"); err != nil {
		t.Fatalf("Enqueue (first): %v", err)
	}
	if err := cat.SetState(ctx, "SRR1", StateCached, "ok"); err != nil {
		t.Fatalf("SetState cached: %v", err)
	}

	// Re-enqueuing an already-cached accession must be a no-op.
	if err := cat.Enqueue(ctx, "SRR1"); err != nil {
		t.Fatalf("Enqueue (second): %v", err)
	}
	e, ok, err := cat.Get(ctx, "SRR1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.State != StateCached {
		t.Errorf("State after re-Enqueue = %q, want cached (Enqueue must not clobber)", e.State)
	}

	// Same for in_progress.
	if err := cat.Enqueue(ctx, "SRR2"); err != nil {
		t.Fatalf("Enqueue SRR2: %v", err)
	}
	if err := cat.SetState(ctx, "SRR2", StateInProgress, ""); err != nil {
		t.Fatalf("SetState in_progress: %v", err)
	}
	if err := cat.Enqueue(ctx, "SRR2"); err != nil {
		t.Fatalf("Enqueue SRR2 (second): %v", err)
	}
	e2, _, err := cat.Get(ctx, "SRR2")
	if err != nil {
		t.Fatalf("Get SRR2: %v", err)
	}
	if e2.State != StateInProgress {
		t.Errorf("State after re-Enqueue = %q, want in_progress", e2.State)
	}
}

// TestEvictionCandidatesLRUOrder asserts, by name, that EvictionCandidates
// orders cached rows oldest-last_access_time-first, and excludes both
// non-cached rows and rows with an unexpired pin.
func TestEvictionCandidatesLRUOrder(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)
	now := time.Now()

	entries := []Entry{
		{Accession: "OLDEST", State: StateCached, SizeBytes: 1, LastAccessTime: now.Add(-3 * time.Hour)},
		{Accession: "MIDDLE", State: StateCached, SizeBytes: 1, LastAccessTime: now.Add(-2 * time.Hour)},
		{Accession: "NEWEST", State: StateCached, SizeBytes: 1, LastAccessTime: now.Add(-1 * time.Hour)},
		// Not cached: must never show up as a candidate.
		{Accession: "QUEUED", State: StateQueued, SizeBytes: 1, LastAccessTime: now.Add(-10 * time.Hour)},
		// Cached but pinned in the future: excluded.
		{Accession: "PINNED_FUTURE", State: StateCached, SizeBytes: 1, LastAccessTime: now.Add(-10 * time.Hour), PinnedUntil: now.Add(time.Hour)},
		// Cached and pinned in the past: included (expired pin).
		{Accession: "PINNED_EXPIRED", State: StateCached, SizeBytes: 1, LastAccessTime: now.Add(-30 * time.Minute), PinnedUntil: now.Add(-time.Minute)},
	}
	for _, e := range entries {
		if err := cat.Upsert(ctx, e); err != nil {
			t.Fatalf("Upsert %s: %v", e.Accession, err)
		}
	}

	got, err := cat.EvictionCandidates(ctx, now)
	if err != nil {
		t.Fatalf("EvictionCandidates: %v", err)
	}

	var order []string
	for _, e := range got {
		order = append(order, e.Accession)
	}
	want := []string{"OLDEST", "MIDDLE", "NEWEST", "PINNED_EXPIRED"}
	if len(order) != len(want) {
		t.Fatalf("EvictionCandidates order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("EvictionCandidates[%d] = %q, want %q (full order %v)", i, order[i], want[i], order)
		}
	}
}

// TestRequeueRestartRecovery asserts Requeue moves only in_progress rows to
// queued, leaving queued/cached/error rows untouched.
func TestRequeueRestartRecovery(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	seed := []Entry{
		{Accession: "STUCK1", State: StateInProgress},
		{Accession: "STUCK2", State: StateInProgress},
		{Accession: "ALREADY_QUEUED", State: StateQueued},
		{Accession: "DONE", State: StateCached},
		{Accession: "BROKEN", State: StateError, Message: "boom"},
	}
	for _, e := range seed {
		if err := cat.Upsert(ctx, e); err != nil {
			t.Fatalf("Upsert %s: %v", e.Accession, err)
		}
	}

	if err := cat.Requeue(ctx); err != nil {
		t.Fatalf("Requeue: %v", err)
	}

	wantState := map[string]State{
		"STUCK1":         StateQueued,
		"STUCK2":         StateQueued,
		"ALREADY_QUEUED": StateQueued,
		"DONE":           StateCached,
		"BROKEN":         StateError,
	}
	for acc, want := range wantState {
		e, ok, err := cat.Get(ctx, acc)
		if err != nil || !ok {
			t.Fatalf("Get %s: ok=%v err=%v", acc, ok, err)
		}
		if e.State != want {
			t.Errorf("%s: State = %q, want %q", acc, e.State, want)
		}
	}
}

// TestListByStateFIFOOrderAndFilter asserts ListByState returns only rows
// in the requested state, oldest queued_time first, and respects limit.
func TestListByStateFIFOOrderAndFilter(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	seed := []Entry{
		{Accession: "SRR3", State: StateQueued, QueuedTime: base.Add(3 * time.Minute)},
		{Accession: "SRR1", State: StateQueued, QueuedTime: base.Add(1 * time.Minute)},
		{Accession: "SRR2", State: StateQueued, QueuedTime: base.Add(2 * time.Minute)},
		{Accession: "CACHED1", State: StateCached, QueuedTime: base},
	}
	for _, e := range seed {
		if err := cat.Upsert(ctx, e); err != nil {
			t.Fatalf("Upsert %s: %v", e.Accession, err)
		}
	}

	got, err := cat.ListByState(ctx, StateQueued, 10)
	if err != nil {
		t.Fatalf("ListByState: %v", err)
	}
	var order []string
	for _, e := range got {
		order = append(order, e.Accession)
	}
	want := []string{"SRR1", "SRR2", "SRR3"}
	if len(order) != len(want) {
		t.Fatalf("ListByState order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("ListByState[%d] = %q, want %q (full order %v)", i, order[i], want[i], order)
		}
	}

	limited, err := cat.ListByState(ctx, StateQueued, 2)
	if err != nil {
		t.Fatalf("ListByState (limit 2): %v", err)
	}
	if len(limited) != 2 || limited[0].Accession != "SRR1" || limited[1].Accession != "SRR2" {
		t.Fatalf("ListByState (limit 2) = %v, want [SRR1 SRR2]", limited)
	}

	none, err := cat.ListByState(ctx, StateQueued, 0)
	if err != nil {
		t.Fatalf("ListByState (limit 0): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("ListByState (limit 0) = %v, want empty", none)
	}
}

func TestSetSizeBytes(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	if err := cat.Enqueue(ctx, "SRR1"); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := cat.SetSizeBytes(ctx, "SRR1", 123456); err != nil {
		t.Fatalf("SetSizeBytes: %v", err)
	}
	e, ok, err := cat.Get(ctx, "SRR1")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.SizeBytes != 123456 {
		t.Errorf("SizeBytes = %d, want 123456", e.SizeBytes)
	}

	if err := cat.SetSizeBytes(ctx, "SRR_NOPE", 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetSizeBytes on unknown accession: err = %v, want ErrNotFound", err)
	}
}

func TestCatalogDelete(t *testing.T) {
	ctx := context.Background()
	cat := openTest(t)

	if err := cat.Upsert(ctx, Entry{Accession: "SRR1", State: StateCached, SizeBytes: 10}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := cat.Delete(ctx, "SRR1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, err := cat.Get(ctx, "SRR1")
	if err != nil {
		t.Fatalf("Get after Delete: %v", err)
	}
	if ok {
		t.Errorf("Get after Delete: still present")
	}
	// Deleting an already-absent row is not an error.
	if err := cat.Delete(ctx, "SRR1"); err != nil {
		t.Errorf("Delete (already gone): unexpected error %v", err)
	}
}
