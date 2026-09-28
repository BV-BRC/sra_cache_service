package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
)

func TestHandlePing(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()

	handlePing(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "OK\n" {
		t.Fatalf("body = %q, want %q", got, "OK\n")
	}
}

func TestHandlePingHead(t *testing.T) {
	req := httptest.NewRequest(http.MethodHead, "/ping", nil)
	rec := httptest.NewRecorder()

	handlePing(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "" {
		t.Fatalf("body = %q, want empty for HEAD", got)
	}
}

func TestHandlePingRejectsOtherMethods(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/ping", nil)
	rec := httptest.NewRecorder()

	handlePing(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
}

// TestNoopValidatorRejectsEverything pins down the one behavior the rest
// of this file's control-plane tests rely on: until a real p3auth
// Validator is wired up (see main's TODO at rpcServer's construction
// site), every token is rejected.
func TestNoopValidatorRejectsEverything(t *testing.T) {
	if _, err := (NoopValidator{}).Validate("anything"); err == nil {
		t.Fatal("NoopValidator.Validate: got nil error, want non-nil (no real validation is wired up yet)")
	}
}

// TestControlPlaneAnswersPing and TestControlPlaneAnswersAuthPing build
// the control-plane handler the same way main() does -- a
// jsonrpc.Server wrapping NoopValidator, with no RPC methods registered
// (Phase 2 registers none; Phases 4/6/7 do) -- and confirm it now answers
// both /ping and /auth_ping, not just /ping as in Phase 1. This is the
// end-to-end confirmation that main's wiring change (mainMux+handlePing
// -> jsonrpc.Server) actually took effect, independent of
// internal/jsonrpc's own unit tests.
func newControlPlaneHandler() http.Handler {
	return jsonrpc.NewServer(NoopValidator{}, nil)
}

func TestControlPlaneAnswersPing(t *testing.T) {
	h := newControlPlaneHandler()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "OK\n" {
		t.Fatalf("body = %q, want %q", got, "OK\n")
	}
}

func TestControlPlaneAnswersAuthPing(t *testing.T) {
	h := newControlPlaneHandler()

	// No Authorization header -> 401, matching NoopValidator being
	// irrelevant here (the real stub never even calls the validator
	// when the header is absent).
	req := httptest.NewRequest(http.MethodGet, "/auth_ping", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-header: status = %d, want 401", rec.Code)
	}
	if got := rec.Body.String(); got != "Authentication required" {
		t.Fatalf("no-header: body = %q, want %q", got, "Authentication required")
	}

	// A token is now present but NoopValidator rejects everything -> 403.
	req = httptest.NewRequest(http.MethodGet, "/auth_ping", nil)
	req.Header.Set("Authorization", "any-token-at-all")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("with-token: status = %d, want 403 (NoopValidator rejects every token)", rec.Code)
	}
	if got := rec.Body.String(); got != "Authentication failed" {
		t.Fatalf("with-token: body = %q, want %q", got, "Authentication failed")
	}
}

func TestSectionOrDefault(t *testing.T) {
	if got := sectionOrDefault(""); got != "SRACache" {
		t.Errorf("sectionOrDefault(\"\") = %q, want SRACache", got)
	}
	if got := sectionOrDefault("Custom"); got != "Custom" {
		t.Errorf("sectionOrDefault(\"Custom\") = %q, want Custom", got)
	}
}
