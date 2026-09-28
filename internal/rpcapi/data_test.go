package rpcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/cache"
	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/dataplane"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
)

// fakeSigner is a test-only URLSigner. It records every accession Mint was
// called for (so a test can assert Mint is never reached for a
// not-yet-cached accession), and can be told to fail.
type fakeSigner struct {
	calls []string
	url   string
	err   error
}

func (f *fakeSigner) Mint(accession string, ttl time.Duration) (string, time.Time, error) {
	f.calls = append(f.calls, accession)
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	return f.url, time.Now().Add(ttl), nil
}

type dataLocationRow struct {
	Accession string `json:"accession"`
	State     string `json:"state"`
	URL       string `json:"url"`
	Expires   string `json:"expires"`
}

func newDataServer(cat *catalog.Catalog, waker Waker, signer URLSigner, downloadLifetime time.Duration) *jsonrpc.Server {
	srv := jsonrpc.NewServer(fakeValidator{}, nil)
	svc := &Service{Catalog: cat, Pool: waker, Signer: signer, DownloadLifetime: downloadLifetime}
	svc.RegisterGetSRAData(srv)
	return srv
}

func postGetSRAData(t *testing.T, srv *jsonrpc.Server, idsJSON string, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"params":[` + idsJSON + `],"method":"SRACache.get_sra_data","version":"1.1","id":"1"}`
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func decodeDataRows(t *testing.T, rec *httptest.ResponseRecorder) []dataLocationRow {
	t.Helper()
	var resp jsonrpc.SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v; body = %s", err, rec.Body.String())
	}
	if len(resp.Result) != 1 {
		t.Fatalf("result has %d elements, want 1 (arity); body = %s", len(resp.Result), rec.Body.String())
	}
	raw, err := json.Marshal(resp.Result[0])
	if err != nil {
		t.Fatalf("re-marshaling result[0]: %v", err)
	}
	var rows []dataLocationRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decoding rows: %v; raw = %s", err, raw)
	}
	return rows
}

// TestGetSRADataColdAccessionEnqueuesAndReturnsEmptyURL is the "cold
// accession -> queued/empty url" acceptance case, confirming via the
// catalog (not just the response) that it really was enqueued as a side
// effect, and that Mint was never called for it.
func TestGetSRADataColdAccessionEnqueuesAndReturnsEmptyURL(t *testing.T) {
	cat := openTestCatalog(t)
	waker := &fakeWaker{}
	signer := &fakeSigner{}
	srv := newDataServer(cat, waker, signer, time.Hour)

	rec := postGetSRAData(t, srv, `["SRR1234567"]`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeDataRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	if rows[0].Accession != "SRR1234567" || rows[0].State != string(catalog.StateQueued) {
		t.Errorf("rows[0] = %+v, want SRR1234567/queued", rows[0])
	}
	if rows[0].URL != "" {
		t.Errorf("url = %q, want empty for a not-yet-cached accession", rows[0].URL)
	}

	// Really was enqueued.
	e, ok, err := cat.Get(context.Background(), "SRR1234567")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	if e.State != catalog.StateQueued {
		t.Errorf("catalog state = %q, want queued", e.State)
	}
	if waker.calls != 1 {
		t.Errorf("Wake called %d times, want exactly 1", waker.calls)
	}
	if len(signer.calls) != 0 {
		t.Errorf("Signer.Mint called %v, want never for a cold accession", signer.calls)
	}
}

// TestGetSRADataCachedAccessionReturnsVerifiableURL: a cached accession gets
// a URL that, when independently Verify'd against the same signer, checks
// out -- and the catalog row is pinned to the same expires Mint returned.
func TestGetSRADataCachedAccessionReturnsVerifiableURL(t *testing.T) {
	cat := openTestCatalog(t)
	if err := cat.Upsert(context.Background(), catalog.Entry{
		Accession: "SRR1234567", State: catalog.StateCached, SizeBytes: 42,
		LastAccessTime: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	waker := &fakeWaker{}
	signer := dataplane.New("test-secret", "http://sra-cache.example.org:7142")
	srv := newDataServer(cat, waker, signer, time.Hour)

	rec := postGetSRAData(t, srv, `["SRR1234567"]`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeDataRows(t, rec)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want 1", rows)
	}
	if rows[0].State != string(catalog.StateCached) {
		t.Errorf("state = %q, want cached", rows[0].State)
	}
	if rows[0].URL == "" {
		t.Fatal("url is empty, want a minted signed URL")
	}
	if rows[0].Expires == "" {
		t.Error("expires is empty, want a timestamp")
	}

	// Independently verify the URL against the same signer.
	key, err := urlKey(rows[0].URL)
	if err != nil {
		t.Fatalf("extracting key from url %q: %v", rows[0].URL, err)
	}
	if err := signer.Verify("SRR1234567", key); err != nil {
		t.Errorf("independently verifying the minted url: %v", err)
	}

	// The catalog row must now be pinned until (approximately) the returned
	// expires -- this IS the cache-eviction lease, no separate protocol.
	e, ok, err := cat.Get(context.Background(), "SRR1234567")
	if err != nil || !ok {
		t.Fatalf("Get: ok=%v err=%v", ok, err)
	}
	wantExpires, err := time.Parse(time.RFC3339, rows[0].Expires)
	if err != nil {
		t.Fatalf("parsing expires %q: %v", rows[0].Expires, err)
	}
	// formatTimestamp (precache.go) renders RFC3339, which truncates to
	// whole seconds -- so compare with a tolerance rather than exact
	// equality; the catalog's own PinnedUntil retains full precision.
	if diff := e.PinnedUntil.Sub(wantExpires); diff < -time.Second || diff > time.Second {
		t.Errorf("PinnedUntil = %v, want it within 1s of %v (the minted expires)", e.PinnedUntil, wantExpires)
	}

	// get_sra_data on an already-cached accession must not enqueue/wake.
	if waker.calls != 0 {
		t.Errorf("Wake called %d times, want 0 (already cached, nothing to enqueue)", waker.calls)
	}
}

// TestGetSRADataSRXRejectedAsInvalidID mirrors start_precache's
// expansion-required rule: get_sra_data also requires run-level accessions.
func TestGetSRADataSRXRejectedAsInvalidID(t *testing.T) {
	cat := openTestCatalog(t)
	signer := &fakeSigner{}
	srv := newDataServer(cat, &fakeWaker{}, signer, time.Hour)

	rec := postGetSRAData(t, srv, `["SRX000001"]`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeDataRows(t, rec)
	if len(rows) != 1 || rows[0].State != string(catalog.StateInvalidID) {
		t.Fatalf("rows = %+v, want one invalid_id row", rows)
	}
	if rows[0].URL != "" {
		t.Errorf("url = %q, want empty for invalid_id", rows[0].URL)
	}
	if _, ok, err := cat.Get(context.Background(), "SRX000001"); err != nil || ok {
		t.Errorf("SRX000001 should never have reached the catalog: ok=%v err=%v", ok, err)
	}
	if len(signer.calls) != 0 {
		t.Errorf("Signer.Mint called %v, want never for an invalid_id accession", signer.calls)
	}
}

// TestGetSRADataGarbageTokenStillSucceeds: AuthOptional means an
// unauthenticated call, or one with a garbage token, both succeed.
func TestGetSRADataGarbageTokenStillSucceeds(t *testing.T) {
	cat := openTestCatalog(t)
	srv := newDataServer(cat, &fakeWaker{}, &fakeSigner{}, time.Hour)

	rec := postGetSRAData(t, srv, `["SRR1234567"]`, "garbage-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (AuthOptional + invalid token must still succeed); body = %s", rec.Code, rec.Body.String())
	}
}

// TestGetSRADataPinBlocksEvictionUnderPressure is the single most important
// test in this phase per the implementation plan: a minted, unexpired URL
// (via get_sra_data, not a raw catalog.Pin call) must block eviction of its
// accession even under real cache pressure, exercising Phase 5's
// EnsureSpace/Evict and this phase's Pin call together.
//
// Setup, matching the plan's own recipe exactly: a tiny max-cache-size, two
// real cached accessions (SRR1111111 older, SRR2222222 newer by
// last_access_time -- i.e. SRR1111111 is what plain LRU would evict first),
// get_sra_data mints (and thereby pins) SRR1111111, then a third incoming
// file is simulated via cache.EnsureSpace with a target that would need
// BOTH existing entries evicted to satisfy -- proving the pin, not just LRU
// order, is what saves SRR1111111: evicting SRR2222222 alone (500 bytes)
// cannot meet a 1000-byte target, so without the pin SRR1111111 would also
// have been evicted and the target reached; with the pin, SRR1111111 is
// excluded from candidates entirely and EnsureSpace must report
// ErrInsufficientEvictableSpace instead.
func TestGetSRADataPinBlocksEvictionUnderPressure(t *testing.T) {
	ctx := context.Background()
	cat := openTestCatalog(t)
	cch := cache.New(t.TempDir())
	now := time.Now()

	const size = 500
	seedRealCachedEntry(t, ctx, cat, cch, "SRR1111111", size, now.Add(-2*time.Hour))
	seedRealCachedEntry(t, ctx, cat, cch, "SRR2222222", size, now.Add(-1*time.Hour))

	signer := dataplane.New("test-secret", "http://sra-cache.example.org:7142")
	srv := newDataServer(cat, &fakeWaker{}, signer, time.Hour)

	// Mint (and thereby pin) SRR1111111 via the real get_sra_data RPC path --
	// this is the mechanism under test, not a shortcut through catalog.Pin
	// directly.
	rec := postGetSRAData(t, srv, `["SRR1111111"]`, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	rows := decodeDataRows(t, rec)
	if len(rows) != 1 || rows[0].State != string(catalog.StateCached) || rows[0].URL == "" {
		t.Fatalf("get_sra_data(SRR1111111) = %+v, want a cached row with a url", rows)
	}

	e, ok, err := cat.Get(ctx, "SRR1111111")
	if err != nil || !ok {
		t.Fatalf("Get(SRR1111111): ok=%v err=%v", ok, err)
	}
	if !e.PinnedUntil.After(now) {
		t.Fatalf("SRR1111111.PinnedUntil = %v, want it in the future after get_sra_data minted a URL", e.PinnedUntil)
	}

	// Cache pressure: total cached is 1000 (SRR1111111+SRR2222222),
	// max-cache-size is a tiny 100, and the incoming (third) file is 100 --
	// EnsureSpace needs to free 1000 bytes (projected 1100 down to 100) to
	// fit it. Evicting SRR2222222 alone (500) is not enough to reach that;
	// only evicting SRR1111111 too would meet it -- which is exactly why
	// this must fail rather than silently reach into the pinned entry.
	err = cache.EnsureSpace(ctx, cat, cch, 100, 100)
	if !errors.Is(err, cache.ErrInsufficientEvictableSpace) {
		t.Fatalf("EnsureSpace under pressure: err = %v, want ErrInsufficientEvictableSpace", err)
	}

	// The pinned entry survives, row and file both.
	if _, ok, _ := cat.Get(ctx, "SRR1111111"); !ok {
		t.Error("SRR1111111 (pinned via get_sra_data) must survive eviction pressure")
	}
	if _, err := statCachePath(cch, "SRR1111111"); err != nil {
		t.Errorf("SRR1111111's cached file must still be on disk: %v", err)
	}
	// The unpinned entry was still evicted -- EnsureSpace frees whatever it
	// legitimately can before giving up, per Evict's own contract.
	if _, ok, _ := cat.Get(ctx, "SRR2222222"); ok {
		t.Error("SRR2222222 (unpinned) should have been evicted under pressure")
	}
	if _, err := statCachePath(cch, "SRR2222222"); err == nil {
		t.Error("SRR2222222's cached file should have been removed")
	}
}

// seedRealCachedEntry publishes a real on-disk file for accession and
// records the matching catalog row, mirroring
// internal/cache/evict_test.go's seedCachedEntry -- duplicated here (rather
// than exported from internal/cache) since this test's whole point is to
// drive the pin through the real get_sra_data RPC path rather than reuse
// cache-package test plumbing.
func seedRealCachedEntry(t *testing.T, ctx context.Context, cat *catalog.Catalog, c *cache.Cache, accession string, size int64, lastAccess time.Time) {
	t.Helper()
	content := make([]byte, size)
	if _, _, err := c.Publish(accession, bytes.NewReader(content)); err != nil {
		t.Fatalf("seed Publish %s: %v", accession, err)
	}
	e := catalog.Entry{
		Accession:      accession,
		State:          catalog.StateCached,
		SizeBytes:      size,
		CachedTime:     lastAccess,
		LastAccessTime: lastAccess,
	}
	if err := cat.Upsert(ctx, e); err != nil {
		t.Fatalf("seed Upsert %s: %v", accession, err)
	}
}

func statCachePath(c *cache.Cache, accession string) (os.FileInfo, error) {
	return os.Stat(c.Path(accession))
}

// urlKey extracts the {key} path segment from a URL Mint produced, i.e.
// ".../sra/<key>/<accession>.sra".
func urlKey(u string) (string, error) {
	const marker = "/sra/"
	i := strings.Index(u, marker)
	if i < 0 {
		return "", errors.New("no /sra/ segment in url")
	}
	rest := u[i+len(marker):]
	j := strings.Index(rest, "/")
	if j < 0 {
		return "", errors.New("no trailing /<accession>.sra segment in url")
	}
	return rest[:j], nil
}
