package main

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
	"github.com/BV-BRC/sra_cache_service/internal/metadata"
	"github.com/BV-BRC/sra_cache_service/internal/rpcapi"
	"github.com/BV-BRC/sra_cache_service/internal/rpcclient"
	"github.com/BV-BRC/sra_cache_service/internal/sracacheclient"
)

// fakeClient is a hand-rolled client (not a real jsonrpc.Server) for
// exercising runPrecache/waitForTerminal's polling logic deterministically
// and fast, with no HTTP involved at all. statusSeq[i] is returned by the
// i-th PrecacheStatus call; calls past the end of statusSeq repeat the last
// entry, so a test can under-specify how many polls it takes and still get
// a stable final state.
type fakeClient struct {
	startRows []sracacheclient.PrecacheStatus
	startErr  error

	statusSeq   [][]sracacheclient.PrecacheStatus
	statusIdx   int
	statusCalls int
	statusErr   error
}

func (f *fakeClient) StartPrecache(ctx context.Context, accessions []string) ([]sracacheclient.PrecacheStatus, error) {
	return f.startRows, f.startErr
}

func (f *fakeClient) PrecacheStatus(ctx context.Context, accessions []string) ([]sracacheclient.PrecacheStatus, error) {
	f.statusCalls++
	if f.statusErr != nil {
		return nil, f.statusErr
	}
	if len(f.statusSeq) == 0 {
		return nil, nil
	}
	idx := f.statusIdx
	if idx >= len(f.statusSeq) {
		idx = len(f.statusSeq) - 1
	} else {
		f.statusIdx++
	}
	return f.statusSeq[idx], nil
}

// TestRunPrecacheFireAndForget covers the default (no --wait) path: prints
// start_precache's immediate rows and always returns nil (exit 0),
// regardless of what those rows say.
func TestRunPrecacheFireAndForget(t *testing.T) {
	fc := &fakeClient{startRows: []sracacheclient.PrecacheStatus{
		{Accession: "SRR1234567", State: "queued"},
		{Accession: "SRX000001", State: "invalid_id", Message: "must be expanded via get_metadata first"},
	}}
	var out bytes.Buffer
	rows, err := runPrecache(context.Background(), &out, fc, []string{"SRR1234567", "SRX000001"}, false, 0, 0)
	if err != nil {
		t.Fatalf("runPrecache: %v, want nil (fire-and-forget always exits 0)", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if fc.statusCalls != 0 {
		t.Errorf("PrecacheStatus called %d times, want 0 (no --wait)", fc.statusCalls)
	}
	got := out.String()
	if !strings.Contains(got, "SRR1234567") || !strings.Contains(got, "queued") {
		t.Errorf("output = %q, want it to mention SRR1234567/queued", got)
	}
	if !strings.Contains(got, "invalid_id") {
		t.Errorf("output = %q, want it to mention invalid_id", got)
	}
}

// TestRunPrecacheStartPrecacheErrorExitsOne confirms a hard RPC failure
// (not a per-accession state) maps to exit code 1.
func TestRunPrecacheStartPrecacheErrorExitsOne(t *testing.T) {
	fc := &fakeClient{startErr: errors.New("connection refused")}
	var out bytes.Buffer
	_, err := runPrecache(context.Background(), &out, fc, []string{"SRR1234567"}, false, 0, 0)
	if err == nil {
		t.Fatal("runPrecache: expected an error")
	}
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 1 {
		t.Fatalf("err = %v, want an *exitCodeError with code 1", err)
	}
}

// TestRunPrecacheWaitPollsUntilTerminal is the polling acceptance case: the
// fake client transitions queued -> in_progress -> cached across
// successive PrecacheStatus calls, and runPrecache must poll until it sees
// the terminal one, then exit 0.
func TestRunPrecacheWaitPollsUntilTerminal(t *testing.T) {
	fc := &fakeClient{
		startRows: []sracacheclient.PrecacheStatus{{Accession: "SRR1234567", State: "queued"}},
		statusSeq: [][]sracacheclient.PrecacheStatus{
			{{Accession: "SRR1234567", State: "queued"}},
			{{Accession: "SRR1234567", State: "in_progress"}},
			{{Accession: "SRR1234567", State: "cached", SizeBytes: 123456789}},
		},
	}
	var out bytes.Buffer
	rows, err := runPrecache(context.Background(), &out, fc, []string{"SRR1234567"}, true, time.Millisecond, 0)
	if err != nil {
		t.Fatalf("runPrecache: %v", err)
	}
	if len(rows) != 1 || rows[0].State != "cached" {
		t.Fatalf("rows = %+v, want a single cached row", rows)
	}
	if fc.statusCalls != 3 {
		t.Errorf("PrecacheStatus called %d times, want 3 (queued, in_progress, cached)", fc.statusCalls)
	}
	got := out.String()
	if !strings.Contains(got, "cached") || !strings.Contains(got, "SRR1234567") {
		t.Errorf("output = %q, want the final cached row", got)
	}
}

// TestRunPrecacheWaitAlreadyTerminalSkipsPolling covers the case
// waitForTerminal's doc comment calls out explicitly: an accession that is
// already terminal in start_precache's own response (invalid_id) must not
// cost even one poll.
func TestRunPrecacheWaitAlreadyTerminalSkipsPolling(t *testing.T) {
	fc := &fakeClient{startRows: []sracacheclient.PrecacheStatus{
		{Accession: "SRX000001", State: "invalid_id", Message: "must be expanded via get_metadata first"},
	}}
	var out bytes.Buffer
	_, err := runPrecache(context.Background(), &out, fc, []string{"SRX000001"}, true, time.Hour, 0)
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("err = %v, want an *exitCodeError with code 2 (invalid_id is a failed terminal state)", err)
	}
	if fc.statusCalls != 0 {
		t.Errorf("PrecacheStatus called %d times, want 0 (already terminal from start_precache)", fc.statusCalls)
	}
}

// TestRunPrecacheWaitExitCodesByTerminalState table-tests each of the
// terminal states' exit-code contract: cached succeeds, the other three
// fail (exit 2).
func TestRunPrecacheWaitExitCodesByTerminalState(t *testing.T) {
	cases := []struct {
		state    string
		wantFail bool
	}{
		{"cached", false},
		{"invalid_id", true},
		{"not_found", true},
		{"error", true},
	}
	for _, c := range cases {
		t.Run(c.state, func(t *testing.T) {
			fc := &fakeClient{startRows: []sracacheclient.PrecacheStatus{{Accession: "SRR1234567", State: c.state}}}
			var out bytes.Buffer
			_, err := runPrecache(context.Background(), &out, fc, []string{"SRR1234567"}, true, time.Hour, 0)
			if c.wantFail {
				var ec *exitCodeError
				if !errors.As(err, &ec) || ec.code != 2 {
					t.Fatalf("err = %v, want an *exitCodeError with code 2", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want nil for a cached terminal state", err)
			}
		})
	}
}

// TestRunPrecacheWaitTimeoutExitsOne confirms a --timeout that elapses
// before every accession reaches a terminal state is a hard failure (code
// 1, not 2 -- this is "we don't know", not "it failed").
func TestRunPrecacheWaitTimeoutExitsOne(t *testing.T) {
	fc := &fakeClient{
		startRows: []sracacheclient.PrecacheStatus{{Accession: "SRR1234567", State: "queued"}},
		statusSeq: [][]sracacheclient.PrecacheStatus{
			{{Accession: "SRR1234567", State: "queued"}},
		}, // never advances past queued
	}
	var out bytes.Buffer
	_, err := runPrecache(context.Background(), &out, fc, []string{"SRR1234567"}, true, 5*time.Millisecond, 20*time.Millisecond)
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 1 {
		t.Fatalf("err = %v, want an *exitCodeError with code 1 (timeout)", err)
	}
}

// TestRunPrecacheWaitContextCanceled confirms a canceled context (e.g. the
// process receiving SIGINT/SIGTERM in the real main) stops the poll loop
// with a code-1 error rather than looping forever.
func TestRunPrecacheWaitContextCanceled(t *testing.T) {
	fc := &fakeClient{
		startRows: []sracacheclient.PrecacheStatus{{Accession: "SRR1234567", State: "queued"}},
		statusSeq: [][]sracacheclient.PrecacheStatus{
			{{Accession: "SRR1234567", State: "queued"}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	var out bytes.Buffer
	_, err := runPrecache(ctx, &out, fc, []string{"SRR1234567"}, true, 5*time.Millisecond, 0)
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 1 {
		t.Fatalf("err = %v, want an *exitCodeError with code 1", err)
	}
}

// --- Integration: a real internal/jsonrpc.Server, not a fake ---

// fakeValidator accepts exactly one token -- the fake-Validator approach
// the implementation plan directs, since the real dev binary's
// NoopValidator (cmd/sra-cache-server/main.go) rejects every token
// unconditionally, and there is no real deployment to test against yet.
type fakeValidator struct{ token string }

func (f fakeValidator) Validate(token string) (string, error) {
	if token == f.token {
		return "test-user", nil
	}
	return "", errors.New("invalid token")
}

type fakeMetadataLookuper struct{}

func (fakeMetadataLookuper) Lookup(ctx context.Context, ids []string) ([]metadata.SRAMetadata, []string, error) {
	return nil, ids, nil
}

type fakeWaker struct{}

func (fakeWaker) Wake() {}

// newIntegrationServer wires a real internal/jsonrpc.Server to the real
// internal/rpcapi handlers, backed by a real temp-file catalog -- the same
// approach internal/sracacheclient's own tests use -- so this file's
// integration test exercises the actual dispatcher and handler code this
// CLI talks to in production, not a stand-in for it.
func newIntegrationServer(t *testing.T, validToken string) *httptest.Server {
	t.Helper()
	cat, err := catalog.Open(context.Background(), filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatalf("catalog.Open: %v", err)
	}
	t.Cleanup(func() { cat.Close() })

	rpcSrv := jsonrpc.NewServer(fakeValidator{token: validToken}, nil)
	svc := &rpcapi.Service{
		Metadata: fakeMetadataLookuper{},
		Catalog:  cat,
		Pool:     fakeWaker{},
	}
	svc.RegisterStartPrecache(rpcSrv)
	svc.RegisterPrecacheStatus(rpcSrv)

	ts := httptest.NewServer(rpcSrv)
	t.Cleanup(ts.Close)
	return ts
}

// TestIntegrationMissingTokenFails is the real dispatcher-level auth check
// for start_precache's AuthRequired policy, run through this command's own
// client wiring (rpcclient.Client -> sracacheclient.Client -> runPrecache),
// not a bypassed unit call.
func TestIntegrationMissingTokenFails(t *testing.T) {
	ts := newIntegrationServer(t, "test-token")
	c := sracacheclient.New(&rpcclient.Client{URL: ts.URL}) // no token

	var out bytes.Buffer
	_, err := runPrecache(context.Background(), &out, c, []string{"SRR1234567"}, false, 0, 0)
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 1 {
		t.Fatalf("err = %v, want an *exitCodeError with code 1 (missing Authorization)", err)
	}
}

// TestIntegrationWaitReachesInvalidIDImmediately is a real "--wait against
// a live server" end-to-end case that doesn't require a running prefetch
// pipeline: an SRX accession is rejected as invalid_id synchronously by
// start_precache itself (internal/rpcapi/precache.go's validateRunLevel),
// so the wait loop completes on its very first check.
func TestIntegrationWaitReachesInvalidIDImmediately(t *testing.T) {
	ts := newIntegrationServer(t, "test-token")
	c := sracacheclient.New(&rpcclient.Client{URL: ts.URL, Token: "test-token"})

	var out bytes.Buffer
	_, err := runPrecache(context.Background(), &out, c, []string{"SRX000001"}, true, time.Hour, 0)
	var ec *exitCodeError
	if !errors.As(err, &ec) || ec.code != 2 {
		t.Fatalf("err = %v, want an *exitCodeError with code 2", err)
	}
	got := out.String()
	if !strings.Contains(got, "SRX000001") || !strings.Contains(got, "invalid_id") {
		t.Errorf("output = %q, want the invalid_id row", got)
	}
}

// TestIntegrationStartPrecacheThenQueuedStatus confirms the full
// authenticated round trip -- start_precache enqueues against the real
// catalog, and a subsequent precache_status (no token needed -- it's
// AuthOptional) sees the same row.
func TestIntegrationStartPrecacheThenQueuedStatus(t *testing.T) {
	ts := newIntegrationServer(t, "test-token")
	authed := sracacheclient.New(&rpcclient.Client{URL: ts.URL, Token: "test-token"})
	anon := sracacheclient.New(&rpcclient.Client{URL: ts.URL})

	rows, err := authed.StartPrecache(context.Background(), []string{"SRR1234567"})
	if err != nil {
		t.Fatalf("StartPrecache: %v", err)
	}
	if len(rows) != 1 || rows[0].State != "queued" {
		t.Fatalf("rows = %+v, want queued", rows)
	}

	rows, err = anon.PrecacheStatus(context.Background(), []string{"SRR1234567"})
	if err != nil {
		t.Fatalf("PrecacheStatus: %v", err)
	}
	if len(rows) != 1 || rows[0].State != "queued" {
		t.Fatalf("rows = %+v, want queued (unauthenticated poll of the same accession)", rows)
	}
}
