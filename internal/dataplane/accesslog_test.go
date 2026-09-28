package dataplane

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newJSONTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, nil))
}

// TestAccessLogRecordsTTFBAndTotalSeparately is the core requirement: ttfb
// and total must be two distinct, separately named fields (not folded into
// one "duration"), because that split is what the Workspace CLAUDE.md
// incident says actually diagnosed a real production stall.
func TestAccessLogRecordsTTFBAndTotalSeparately(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONTestLogger(&buf)

	handler := AccessLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		time.Sleep(5 * time.Millisecond)
		w.Write([]byte("some bytes"))
	}))

	req := httptest.NewRequest(http.MethodGet, "/sra/somekey/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decoding log line: %v; line = %s", err, buf.String())
	}

	if _, ok := entry["ttfb_s"]; !ok {
		t.Errorf("log entry missing ttfb_s field: %v", entry)
	}
	if _, ok := entry["total_s"]; !ok {
		t.Errorf("log entry missing total_s field: %v", entry)
	}
	// They must be independently present and distinguishable, i.e. not the
	// same key emitted twice or a single combined "duration" field.
	if entry["ttfb_s"] == nil || entry["total_s"] == nil {
		t.Fatalf("ttfb_s/total_s must both have values: %v", entry)
	}
	ttfb, _ := entry["ttfb_s"].(float64)
	total, _ := entry["total_s"].(float64)
	if total < ttfb {
		t.Errorf("total_s (%v) < ttfb_s (%v), want total >= ttfb", total, ttfb)
	}
	if status, _ := entry["status"].(float64); status != 200 {
		t.Errorf("status = %v, want 200", entry["status"])
	}
	if bytesField, _ := entry["bytes"].(float64); bytesField != 10 {
		t.Errorf("bytes = %v, want 10", entry["bytes"])
	}
}

// TestAccessLogNeverLogsTheSignedKey: the route field must not carry the
// actual signed key from the path (it's a bearer secret).
func TestAccessLogNeverLogsTheSignedKey(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONTestLogger(&buf)

	handler := AccessLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	const secretKey = "super-secret-signed-url-key-material"
	req := httptest.NewRequest(http.MethodGet, "/sra/"+secretKey+"/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if strings.Contains(buf.String(), secretKey) {
		t.Errorf("log line leaked the signed key: %s", buf.String())
	}
}

// TestAccessLogNeverMeasuredTTFBIsMinusOne: a handler that never writes
// anything at all (e.g. it panicked upstream and recovered elsewhere, or in
// practice a request whose handler returned with no output) should not
// report a fabricated ttfb.
func TestAccessLogHandlesNoWrite(t *testing.T) {
	var buf bytes.Buffer
	log := newJSONTestLogger(&buf)

	handler := AccessLog(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately writes nothing at all.
	}))

	req := httptest.NewRequest(http.MethodGet, "/sra/key/SRR1234567.sra", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("decoding log line: %v", err)
	}
	if ttfb, _ := entry["ttfb_s"].(float64); ttfb != -1 {
		t.Errorf("ttfb_s = %v, want -1 for a handler that never wrote anything", entry["ttfb_s"])
	}
}
