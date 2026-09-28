package dataplane

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// AccessToucher is the subset of *catalog.Catalog the data plane needs: a
// record that accession's bytes were actually delivered, so eviction's LRU
// order (internal/cache/evict.go, ordered by last_access_time) reflects real
// reads rather than only precache-completion time. Declared as an interface,
// mirroring internal/rpcapi's MetadataLookuper/Waker pattern, so a test can
// substitute a fake without a real sqlite catalog.
type AccessToucher interface {
	TouchAccess(ctx context.Context, accession string) error
}

// PathResolver is the subset of *cache.Cache the data plane needs: the local
// filesystem path a cached accession's .sra file lives at. A plain function
// type rather than a one-method interface, since *cache.Cache.Path already
// has exactly this signature.
type PathResolver func(accession string) string

// pathPrefix and pathSuffix bound the URL path this Handler serves:
//
//	GET|HEAD /sra/{key}/{accession}.sra
const pathPrefix = "/sra/"
const pathSuffix = ".sra"

// Handler implements the SRACache data plane: verifying a signed URL's key
// against the accession named in its own path, then streaming that
// accession's cached bytes.
type Handler struct {
	Signer  *Signer
	Resolve PathResolver
	Touch   AccessToucher
	Log     *slog.Logger
}

// NewHandler builds a Handler. log may be nil (defaults to slog.Default()).
func NewHandler(signer *Signer, resolve PathResolver, touch AccessToucher, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{Signer: signer, Resolve: resolve, Touch: touch, Log: log}
}

// ServeHTTP implements http.Handler for GET/HEAD /sra/{key}/{accession}.sra.
// Any other method or an unparseable path is rejected before touching the
// Signer or the filesystem at all.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	key, accession, ok := parsePath(r.URL.Path)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Verify the key BEFORE any file access is attempted, per the
	// implementation plan: "on failure, 4xx with a short body, no file
	// access attempted."
	if err := h.Signer.Verify(accession, key); err != nil {
		status, msg := statusForVerifyError(err)
		h.Log.Warn("data plane: rejected signed URL", "accession", accession, "err", err)
		http.Error(w, msg, status)
		return
	}

	path := h.Resolve(accession)
	f, err := os.Open(path)
	if err != nil {
		// A verified, unexpired URL whose file is gone (evicted out from
		// under it, or never actually published) is a server-side
		// bookkeeping problem, not a client error -- but the safest thing
		// to leak is still "not found", not the underlying path or errno.
		h.Log.Error("data plane: cached file missing for a verified URL", "accession", accession, "path", path, "err", err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		h.Log.Error("data plane: stat failed for a verified URL", "accession", accession, "path", path, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Disposition", `attachment; filename="`+accession+pathSuffix+`"`)

	// http.ServeContent handles Range requests (single and, per the net/http
	// docs, even multipart-range) correctly for an io.ReadSeeker, sets
	// Content-Type via sniffing/extension, and answers HEAD with headers
	// only and no body -- see dataplane_test.go for range/HEAD coverage
	// confirming this rather than assuming it.
	tw := &trackingResponseWriter{ResponseWriter: w, contentLength: -1}
	http.ServeContent(tw, r, accession+pathSuffix, info.ModTime(), f)

	if tw.completedFullBody() && h.Touch != nil {
		// Best-effort: a TouchAccess failure here must not turn a
		// successfully served download into an error response -- the
		// response is already committed. Log and move on.
		if err := h.Touch.TouchAccess(r.Context(), accession); err != nil {
			h.Log.Warn("data plane: touching last_access_time after a served download", "accession", accession, "err", err)
		}
	}
}

// statusForVerifyError maps a Signer.Verify error to an HTTP status and a
// body-safe message. Both ErrExpired and ErrInvalidSignature end up as a
// client-facing 4xx per the plan ("even if both end up as 4xx to the
// client"), but are distinguished (410 vs 403) so a well-behaved client (or
// an operator reading the log) can tell "this link aged out, ask for a new
// one" from "this link is bogus" without parsing prose.
func statusForVerifyError(err error) (int, string) {
	switch {
	case errors.Is(err, ErrExpired):
		return http.StatusGone, "signed URL has expired"
	case errors.Is(err, ErrEmptySecret):
		// A server misconfiguration (Phase 10 enforces this can't happen in
		// production), not the caller's fault -- but still never serve the
		// file.
		return http.StatusInternalServerError, "internal error"
	default:
		return http.StatusForbidden, "invalid signed URL"
	}
}

// parsePath extracts {key} and {accession} from "/sra/{key}/{accession}.sra".
// {key} is opaque (base64url, so no embedded "/"), so a plain split on "/"
// after trimming the fixed prefix/suffix is unambiguous.
func parsePath(p string) (key, accession string, ok bool) {
	if !strings.HasPrefix(p, pathPrefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(p, pathPrefix)
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return "", "", false
	}
	key = rest[:slash]
	tail := rest[slash+1:]
	if key == "" || !strings.HasSuffix(tail, pathSuffix) {
		return "", "", false
	}
	accession = strings.TrimSuffix(tail, pathSuffix)
	if accession == "" || strings.Contains(accession, "/") {
		return "", "", false
	}
	return key, accession, true
}

// trackingResponseWriter wraps http.ResponseWriter to detect whether the
// full response body was actually written -- i.e. whether the client
// received everything, as opposed to aborting the connection partway
// through a large transfer.
//
// net/http gives no direct "the handler's write to the client fully
// succeeded" signal: http.ServeContent returns void, and a client that
// closes its connection mid-copy typically surfaces (if at all) as a
// swallowed write error deep inside the ResponseWriter, not something
// ServeContent reports back to its caller. This wrapper's approximation --
// comparing bytes actually written against the Content-Length ServeContent
// itself set -- is therefore exactly that: an approximation, not a delivery
// receipt. It is accurate for the common cases this matters for (a
// completed GET, a completed ranged GET, a HEAD with no body) and
// conservatively says "not complete" if Content-Length was never set (so a
// response shape this wrapper doesn't understand never gets miscounted as a
// success).
type trackingResponseWriter struct {
	http.ResponseWriter
	status        int
	written       int64
	contentLength int64 // -1 until WriteHeader sets it from the Content-Length header
}

func (w *trackingResponseWriter) WriteHeader(code int) {
	w.status = code
	if cl := w.Header().Get("Content-Length"); cl != "" {
		if n, err := strconv.ParseInt(cl, 10, 64); err == nil {
			w.contentLength = n
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *trackingResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, err := w.ResponseWriter.Write(b)
	w.written += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing (e.g.
// AccessLog's timingResponseWriter, or the server's real ResponseWriter),
// so wrapping this writer never turns a streamed transfer into a buffered
// one.
func (w *trackingResponseWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// completedFullBody reports whether every byte ServeContent declared (via
// Content-Length) was actually handed to the underlying ResponseWriter.
//
//   - HEAD (or any 2xx/3xx response with an explicit zero-length body, e.g.
//     a 304) has contentLength == 0 and written == 0: complete.
//   - A fully-drained GET/ranged-GET has written == contentLength: complete.
//   - A client that disconnects mid-copy leaves written < contentLength:
//     not complete -- this is the case this whole mechanism exists to
//     catch, so a partial read never bumps last_access_time as if the
//     download had actually succeeded.
//   - A response ServeContent answered with an error status before setting
//     Content-Length (contentLength still -1) is conservatively "not
//     complete".
func (w *trackingResponseWriter) completedFullBody() bool {
	if w.status >= 400 {
		return false
	}
	if w.contentLength < 0 {
		return false
	}
	return w.written == w.contentLength
}
