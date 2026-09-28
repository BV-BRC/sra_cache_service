package jsonrpc

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPing(t *testing.T) {
	srv := NewServer(&fakeValidator{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "OK\n" {
		t.Fatalf("body = %q, want %q", got, "OK\n")
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
}

func TestPingHead(t *testing.T) {
	srv := NewServer(&fakeValidator{}, nil)
	req := httptest.NewRequest(http.MethodHead, "/ping", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty for HEAD", got)
	}
}

func TestAuthPing_NoHeader(t *testing.T) {
	srv := NewServer(&fakeValidator{}, nil)
	req := httptest.NewRequest(http.MethodGet, "/auth_ping", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got := rec.Body.String(); got != "Authentication required" {
		t.Fatalf("body = %q, want %q (no trailing newline)", got, "Authentication required")
	}
	if got := rec.Header().Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want unset (Perl sends [] headers for this branch)", got)
	}
}

func TestAuthPing_InvalidToken(t *testing.T) {
	srv := NewServer(&fakeValidator{err: errors.New("expired")}, nil)
	req := httptest.NewRequest(http.MethodGet, "/auth_ping", nil)
	req.Header.Set("Authorization", "garbage")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := rec.Body.String(); got != "Authentication failed" {
		t.Fatalf("body = %q, want %q (no trailing newline)", got, "Authentication failed")
	}
}

func TestAuthPing_ValidToken(t *testing.T) {
	srv := NewServer(&fakeValidator{userID: "olson@patricbrc.org"}, nil)
	req := httptest.NewRequest(http.MethodGet, "/auth_ping", nil)
	req.Header.Set("Authorization", "a-real-looking-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got, want := rec.Body.String(), "OK olson@patricbrc.org\n"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
}
