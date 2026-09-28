package dataplane

import (
	"log/slog"
	"net/http"
	"time"
)

// AccessLog wraps next with one structured log line per data-plane request,
// logging time-to-first-byte and total transfer time as two SEPARATE
// fields, not just a combined duration.
//
// This is a direct response to a real production incident recorded in
// Workspace/CLAUDE.md ("The 2026-09-24 download stall"): a multi-day
// download-stall investigation on the Workspace download service burned
// four wrong hypotheses (missing Mongo indexes, replica-set health,
// slow-client backpressure, upstream Shock latency) before the fix landed
// on the very first use of exactly this split --
//
//	shock-fetch client=... status=200 ttfb=0.164 total=30.191 bytes=253425891 ...
//
// -- because a small ttfb with a large total means a slow CLIENT (it is
// draining the response slowly), while a large ttfb means a slow
// DEPENDENCY (here, opening/stat-ing the cache file). Combining the two
// into one "duration" field is exactly what made the Workspace investigation
// take days: the two failure modes are indistinguishable in a single number.
// This service's data plane is architecturally the same shape (an HTTP
// handler streaming a large file to a client of unknown speed over an
// unknown network path), so the same split is built in from day one rather
// than added after a first incident -- see
// Workspace/go/internal/dlservice/accesslog.go, this package's model.
//
// log may be nil (defaults to slog.Default()).
func AccessLog(log *slog.Logger, next http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		lw := &timingResponseWriter{ResponseWriter: w}

		next.ServeHTTP(lw, r)

		done := time.Now()

		// ttfb: time to the first byte (of headers or body -- whichever
		// comes first) written to the client. Unset (never wrote anything)
		// renders as -1, matching Workspace's own round3 convention for
		// "never measured" rather than a misleading 0.
		ttfbSeconds := -1.0
		if !lw.firstByte.IsZero() {
			ttfbSeconds = lw.firstByte.Sub(start).Seconds()
		}
		totalSeconds := done.Sub(start).Seconds()

		attrs := []any{
			"method", r.Method,
			"path", routeFor(r.URL.Path),
			"status", lw.status,
			"ttfb_s", roundMillis(ttfbSeconds),
			"total_s", roundMillis(totalSeconds),
			"bytes", lw.bytes,
			"remote_addr", r.RemoteAddr,
		}
		if rng := r.Header.Get("Range"); rng != "" {
			attrs = append(attrs, "range", rng)
		}
		// A client that goes away mid-transfer is exactly the "slow
		// client"/backpressure failure mode the ttfb/total split exists to
		// distinguish from a slow dependency -- call it out explicitly
		// rather than making a reader infer it from total_s alone.
		if err := r.Context().Err(); err != nil {
			attrs = append(attrs, "client_gone", true)
		}

		log.Info("data plane request", attrs...)
	})
}

// timingResponseWriter wraps http.ResponseWriter to capture the status
// code, the byte count, and the moment of the first write -- the raw
// measurements AccessLog turns into ttfb/total.
type timingResponseWriter struct {
	http.ResponseWriter
	status    int
	bytes     int64
	firstByte time.Time
}

func (w *timingResponseWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		if w.firstByte.IsZero() {
			w.firstByte = time.Now()
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *timingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK // implicit 200 on first write, matching net/http's own behavior
	}
	if w.firstByte.IsZero() {
		w.firstByte = time.Now()
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, so a
// streamed response is not buffered by this wrapper -- relevant here since
// http.ServeContent's whole point for a large .sra file is to stream, not
// buffer.
func (w *timingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// routeFor reduces a data-plane request path to a fixed label for grouping
// in logs, mirroring Workspace's routeOf: the actual path contains the
// signed key, which is a bearer secret and must never be logged (anyone
// holding it can fetch the file for as long as it remains valid).
func routeFor(p string) string {
	if len(p) >= len(pathPrefix) && p[:len(pathPrefix)] == pathPrefix {
		return "/sra"
	}
	return p
}

// roundMillis renders a duration-in-seconds value to millisecond precision,
// matching Workspace's own round3/%.3f convention so the two services' logs
// are directly comparable. -1 (± floating point noise) passes through
// unrounded as -1, matching "never measured".
func roundMillis(seconds float64) float64 {
	if seconds < 0 {
		return -1
	}
	return float64(int64(seconds*1000)) / 1000
}
