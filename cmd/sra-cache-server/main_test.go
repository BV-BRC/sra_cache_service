package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestSectionOrDefault(t *testing.T) {
	if got := sectionOrDefault(""); got != "SRACache" {
		t.Errorf("sectionOrDefault(\"\") = %q, want SRACache", got)
	}
	if got := sectionOrDefault("Custom"); got != "Custom" {
		t.Errorf("sectionOrDefault(\"Custom\") = %q, want Custom", got)
	}
}
