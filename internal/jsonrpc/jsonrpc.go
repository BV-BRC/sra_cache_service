// Package jsonrpc implements a JSONRPC 1.1 server matching, on the wire,
// the servers typecomp generates for every Perl BV-BRC service (see
// typecomp/lib/Bio/KBase/KIDL/p3_server_stub.tt in the dev_container
// modules tree). sra-cache-server is the first Go JSONRPC service in that
// codebase -- every existing Go "jsonrpc" reference there is client-side
// -- so this package lives here, private to sra_cache_service, rather
// than in BV-BRC-Go-SDK. If a second Go JSONRPC service shows up, this is
// a promotion candidate; until then it has exactly one caller and no
// reason to be public API.
//
// The wire format (envelope shapes, auth gating, /ping and /auth_ping
// behavior, error-object shape) is reproduced byte-for-byte from
// p3_server_stub.tt plus a captured production error
// (app_service/trc, Workspace/trc) rather than from the design plan's
// paraphrase of it -- see the doc comments on Error, authenticate, and
// handleAuthPing for the specific citations.
package jsonrpc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
)

// Handler implements one JSONRPC method. params is the raw JSON array of
// positional arguments exactly as received on the wire (e.g. `[123,"x"]`
// for a two-argument call); the handler unmarshals it into whatever shape
// its method declares. It returns the method's positional return values
// -- results[i] becomes result[i] in the response envelope's "result"
// array -- or an error, which the dispatcher reports to the caller as a
// -32603 internal error (see errors.go's NewInternalError).
//
// ctx carries the caller's AuthInfo (see auth.go); retrieve it with
// AuthInfoFromContext. It also carries the *http.Request via
// RequestFromContext, for handlers that need ClientIP or other headers.
type Handler func(ctx context.Context, params json.RawMessage) (results []any, err error)

// AuthPolicy controls how Server.dispatch gates a registered method,
// mirroring the three values p3_server_stub.tt's %method_authentication
// table carries per method ("none"/"optional"/"required").
type AuthPolicy int

const (
	// AuthNone: never inspect the Authorization header at all.
	AuthNone AuthPolicy = iota
	// AuthOptional: proceed unauthenticated -- no error -- whether the
	// Authorization header is absent, or present but rejected by the
	// Validator. See authenticate's doc comment in auth.go for why the
	// "present but rejected" half of that is not a bug.
	AuthOptional
	// AuthRequired: a missing header, or a header the Validator rejects,
	// is reported as a -32603 error (see errors.go).
	AuthRequired
)

// methodEntry is what Register stores per method.
type methodEntry struct {
	module  string // everything before the method's *last* '.'; see splitMethod.
	policy  AuthPolicy
	arity   int
	handler Handler
}

// Server is a JSONRPC 1.1 dispatcher and implements http.Handler. Build
// one with NewServer; the zero value is not usable (its methods map is
// nil).
type Server struct {
	validator Validator
	logger    *slog.Logger
	methods   map[string]methodEntry
}

// NewServer builds a Server. validator is consulted for every method
// registered with AuthOptional or AuthRequired -- see auth.go for exactly
// when. Passing a Validator that always fails (e.g. because real
// validation isn't wired up yet) still exercises the real
// AuthRequired/AuthOptional gating logic correctly; only "a valid token
// was presented" becomes unreachable.
//
// logger may be nil, in which case a default text-to-stderr logger is
// used; it is currently used only to log a warning on a failed
// /auth_ping validation, mirroring p3_server_stub.tt's `warn "Token
// validation error ...".
func NewServer(validator Validator, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Server{
		validator: validator,
		logger:    logger,
		methods:   make(map[string]methodEntry),
	}
}

// Register adds an RPC method. method is the full dotted JSONRPC method
// name exactly as clients send it, e.g. "SRACache.get_metadata" --
// dispatch keys on this exact string. The module portion (everything
// before the method's *last* '.', see splitMethod) is extracted at
// registration time so an AuthRequired-but-missing-header error can name
// the right service without a per-request string split.
//
// arity is the number of positional return values the method's spec
// declares; ServeHTTP fills the response's "result" array to exactly
// this length, so a handler returning the wrong number of results is a
// caught, reported -32603 error rather than a malformed wire response.
//
// Register panics if method contains no '.' -- that is a caller bug at
// startup, not a request-time condition, so failing loudly beats silently
// registering an unreachable method.
func (s *Server) Register(method string, policy AuthPolicy, arity int, handler Handler) {
	module, _, ok := splitMethod(method)
	if !ok {
		panic(fmt.Sprintf("jsonrpc: Register(%q): method name must contain a %q separating a module name from a method name", method, "."))
	}
	s.methods[method] = methodEntry{module: module, policy: policy, arity: arity, handler: handler}
}

// splitMethod splits a JSONRPC method name on its *last* '.', matching
// p3_server_stub.tt's get_method:
//
//	$full_name =~ /^(\S+)\.([^\.]+)$/;
//	my ($package, $method) = ($1, $2);
//
// \S+ is greedy, so the regex engine backtracks from a full match down to
// the last '.' it can still satisfy while leaving a dot-free tail for
// [^\.]+$ -- i.e. group 1 is everything up to the *last* dot, group 2 is
// the final segment. A module name is therefore permitted to contain dots
// itself; only the final segment is ever taken as the method name.
func splitMethod(full string) (module, method string, ok bool) {
	i := strings.LastIndexByte(full, '.')
	if i < 0 {
		return "", "", false
	}
	return full[:i], full[i+1:], true
}

// request is the JSONRPC 1.1 request envelope.
//
// Version and JSONRPC are both accepted spellings of the version-tag
// field -- different real clients in this codebase use each spelling (the
// generated JS client sends "version"; a curl example elsewhere in this
// codebase's docs uses "jsonrpc") -- and neither's *value* is ever
// inspected. Declaring both fields and never reading them back out is
// sufficient to "accept either, ignore its value": encoding/json simply
// leaves whichever key was absent at its zero value.
type request struct {
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	ID      json.RawMessage `json:"id"`
	Version string          `json:"version"`
	JSONRPC string          `json:"jsonrpc"`
}

// SuccessResponse is the JSONRPC 1.1 success envelope. Exported so tests
// (in this package and its caller's) can decode a response into it
// directly.
type SuccessResponse struct {
	ID      json.RawMessage `json:"id"`
	Result  []any           `json:"result"`
	Version string          `json:"version"`
}

// ServeHTTP implements http.Handler. Routes, mirroring every generated
// .psgi file in this codebase (e.g. Workspace/lib/Workspace.psgi):
//
//	mount "/ping"      => $server->ping(...)       (any HTTP method)
//	mount "/auth_ping" => $server->auth_ping(...)   (any HTTP method)
//	mount "/"          => $rpc_handler              (POST only; allow_get => 0)
//
// i.e. the RPC handler is the catch-all mount, not pinned to a specific
// sub-path -- a POST to "/" or to the service's bare base URL both
// dispatch, matching every real deployment in this tree. Anything else
// (a non-POST request to any path other than /ping or /auth_ping) 404s.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/ping":
		Ping(w, r)
		return
	case "/auth_ping":
		s.handleAuthPing(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	s.dispatch(w, r)
}

func (s *Server) dispatch(w http.ResponseWriter, r *http.Request) {
	var req request
	// Decode regardless of Content-Type: the generated JS client posts
	// its JSON body under jQuery's default
	// application/x-www-form-urlencoded content type (it sets
	// processData:false but never overrides contentType), not
	// application/json -- requiring the header would reject every real
	// browser-based caller.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, nil, NewParseError(err))
		return
	}

	entry, ok := s.methods[req.Method]
	if !ok {
		writeError(w, req.ID, NewMethodNotFound(req.Method))
		return
	}

	info, authErr := authenticate(entry.policy, entry.module, r.Header.Get("Authorization"), s.validator)
	if authErr != nil {
		writeError(w, req.ID, authErr)
		return
	}

	ctx := withAuthInfo(r.Context(), info)
	ctx = withRequest(ctx, r)

	results, err := entry.handler(ctx, req.Params)
	if err != nil {
		writeError(w, req.ID, NewInternalError(err))
		return
	}
	if len(results) != entry.arity {
		writeError(w, req.ID, NewInternalError(fmt.Errorf(
			"method %q returned %d result(s), want %d (server bug)",
			req.Method, len(results), entry.arity)))
		return
	}
	if results == nil {
		results = []any{}
	}

	writeJSON(w, http.StatusOK, SuccessResponse{
		ID:      req.ID,
		Result:  results,
		Version: "1.1",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		// Unreachable for the types this package constructs, but fail
		// safely rather than write a half-formed body.
		http.Error(w, "internal error encoding response", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}
