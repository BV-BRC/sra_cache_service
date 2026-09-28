package dataplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeToucher records every accession TouchAccess was called for, so a test
// can assert the "on completion, touch" behavior without a real catalog.
type fakeToucher struct {
	calls []string
}

func (f *fakeToucher) TouchAccess(ctx context.Context, accession string) error {
	f.calls = append(f.calls, accession)
	return nil
}

// writeFixture writes content to <dir>/<accession>.sra and returns the path
// and the content's sha256, for tests that need to assert byte-for-byte
// (not just status-code) correctness.
func writeFixture(t *testing.T, dir, accession string, content []byte) (path string, sum string) {
	t.Helper()
	path = filepath.Join(dir, accession+".sra")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	h := sha256.Sum256(content)
	return path, hex.EncodeToString(h[:])
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newTestHandler(t *testing.T, dir string, signer *Signer, touch AccessToucher) *Handler {
	t.Helper()
	resolve := func(accession string) string {
		return filepath.Join(dir, accession+".sra")
	}
	return NewHandler(signer, resolve, touch, nil)
}

// TestServeHTTPValidRequestServesFullFile is the "assert a full-file sha256
// match" acceptance case, not just a status-code check.
func TestServeHTTPValidRequestServesFullFile(t *testing.T) {
	dir := t.TempDir()
	// Large-ish, non-trivial content so a truncated read would actually be
	// detectable by both length and hash.
	content := make([]byte, 5*1024*1024+37)
	for i := range content {
		content[i] = byte(i * 7 % 251)
	}
	_, wantSum := writeFixture(t, dir, "SRR1234567", content)

	signer := New("test-secret", "http://example.org")
	touch := &fakeToucher{}
	h := newTestHandler(t, dir, signer, touch)

	signedURL, _, err := signer.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("keyFromURL: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sra/"+key+"/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	got := rec.Body.Bytes()
	if len(got) != len(content) {
		t.Fatalf("body length = %d, want %d", len(got), len(content))
	}
	if gotSum := sha256Hex(got); gotSum != wantSum {
		t.Errorf("body sha256 = %s, want %s", gotSum, wantSum)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="SRR1234567.sra"` {
		t.Errorf("Content-Disposition = %q", cd)
	}

	if len(touch.calls) != 1 || touch.calls[0] != "SRR1234567" {
		t.Errorf("TouchAccess calls = %v, want exactly one call for SRR1234567", touch.calls)
	}
}

// TestServeHTTPRangeRequest confirms real HTTP Range support: a partial
// response with the correct Content-Range/Content-Length and exactly the
// requested byte slice.
func TestServeHTTPRangeRequest(t *testing.T) {
	dir := t.TempDir()
	content := make([]byte, 10000)
	for i := range content {
		content[i] = byte(i % 256)
	}
	writeFixture(t, dir, "SRR1234567", content)

	signer := New("test-secret", "http://example.org")
	touch := &fakeToucher{}
	h := newTestHandler(t, dir, signer, touch)

	signedURL, _, err := signer.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("keyFromURL: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sra/"+key+"/SRR1234567.sra", nil)
	req.Header.Set("Range", "bytes=100-199")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206; body length = %d", rec.Code, rec.Body.Len())
	}
	wantContentRange := "bytes 100-199/10000"
	if got := rec.Header().Get("Content-Range"); got != wantContentRange {
		t.Errorf("Content-Range = %q, want %q", got, wantContentRange)
	}
	if got := rec.Header().Get("Content-Length"); got != "100" {
		t.Errorf("Content-Length = %q, want 100", got)
	}
	got := rec.Body.Bytes()
	want := content[100:200]
	if len(got) != len(want) {
		t.Fatalf("body length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("byte %d = %d, want %d", i, got[i], want[i])
		}
	}

	// A fully-drained partial response still counts as a completed access.
	if len(touch.calls) != 1 || touch.calls[0] != "SRR1234567" {
		t.Errorf("TouchAccess calls = %v, want exactly one call for a completed range request", touch.calls)
	}
}

// TestServeHTTPHeadReturnsHeadersNoBody.
func TestServeHTTPHeadReturnsHeadersNoBody(t *testing.T) {
	dir := t.TempDir()
	content := []byte("hello world, this is a fake .sra file")
	writeFixture(t, dir, "SRR1234567", content)

	signer := New("test-secret", "http://example.org")
	h := newTestHandler(t, dir, signer, &fakeToucher{})

	signedURL, _, err := signer.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("keyFromURL: %v", err)
	}

	req := httptest.NewRequest(http.MethodHead, "/sra/"+key+"/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != "37" {
		t.Errorf("Content-Length = %q, want 37", got)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD body length = %d, want 0", rec.Body.Len())
	}
}

// TestServeHTTPTamperedKeyServesNoContent asserts a tampered key gets an
// error status with NO file content leaked -- not just a non-200.
func TestServeHTTPTamperedKeyServesNoContent(t *testing.T) {
	dir := t.TempDir()
	content := []byte("secret .sra bytes that must never leak")
	writeFixture(t, dir, "SRR1234567", content)

	signer := New("test-secret", "http://example.org")
	touch := &fakeToucher{}
	h := newTestHandler(t, dir, signer, touch)

	signedURL, _, err := signer.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("keyFromURL: %v", err)
	}
	tampered := tamperLastChar(key)

	req := httptest.NewRequest(http.MethodGet, "/sra/"+tampered+"/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want a 4xx", rec.Code)
	}
	body := rec.Body.Bytes()
	if containsBytes(body, content) {
		t.Fatalf("response body leaked file content: %q", body)
	}
	if len(touch.calls) != 0 {
		t.Errorf("TouchAccess called on a rejected request: %v", touch.calls)
	}
}

// TestServeHTTPExpiredKeyServesNoContent mirrors the tampered-key case for
// an expired-but-otherwise-valid key.
func TestServeHTTPExpiredKeyServesNoContent(t *testing.T) {
	dir := t.TempDir()
	content := []byte("secret .sra bytes that must never leak")
	writeFixture(t, dir, "SRR1234567", content)

	signer := New("test-secret", "http://example.org")
	h := newTestHandler(t, dir, signer, &fakeToucher{})

	// Mint with a negative TTL so it is already expired by construction.
	signedURL, _, err := signer.Mint("SRR1234567", -time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("keyFromURL: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/sra/"+key+"/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want a 4xx", rec.Code)
	}
	if containsBytes(rec.Body.Bytes(), content) {
		t.Fatalf("response body leaked file content: %q", rec.Body.Bytes())
	}
}

// TestServeHTTPWrongAccessionInPathServesNoContent: a key minted for one
// accession must not authorize fetching a different one via the path.
func TestServeHTTPWrongAccessionInPathServesNoContent(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "SRR1234567", []byte("file A"))
	writeFixture(t, dir, "SRR7654321", []byte("file B, must not leak"))

	signer := New("test-secret", "http://example.org")
	h := newTestHandler(t, dir, signer, &fakeToucher{})

	signedURL, _, err := signer.Mint("SRR1234567", time.Hour)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	key, err := keyFromURL(signedURL)
	if err != nil {
		t.Fatalf("keyFromURL: %v", err)
	}

	// Reuse SRR1234567's key against SRR7654321's path.
	req := httptest.NewRequest(http.MethodGet, "/sra/"+key+"/SRR7654321.sra", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want a 4xx", rec.Code)
	}
	if containsBytes(rec.Body.Bytes(), []byte("file B")) {
		t.Fatalf("response body leaked file content: %q", rec.Body.Bytes())
	}
}

func TestServeHTTPMethodNotAllowed(t *testing.T) {
	dir := t.TempDir()
	signer := New("test-secret", "http://example.org")
	h := newTestHandler(t, dir, signer, &fakeToucher{})

	req := httptest.NewRequest(http.MethodPost, "/sra/whatever/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestServeHTTPMalformedPath(t *testing.T) {
	dir := t.TempDir()
	signer := New("test-secret", "http://example.org")
	h := newTestHandler(t, dir, signer, &fakeToucher{})

	cases := []string{
		"/sra/",
		"/sra/onlykey",
		"/sra/key/notanaccession",
		"/not-sra/key/SRR1234567.sra",
	}
	for _, p := range cases {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code < 400 {
			t.Errorf("path %q: status = %d, want a 4xx", p, rec.Code)
		}
	}
}

func containsBytes(haystack, needle []byte) bool {
	return len(needle) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle []byte) int {
	if len(needle) > len(haystack) {
		return -1
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}
