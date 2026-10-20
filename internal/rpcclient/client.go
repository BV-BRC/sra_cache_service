// Package rpcclient is a small, generic JSONRPC 1.1 client matching the
// wire format internal/jsonrpc.Server actually speaks -- see that
// package's doc comment and PLAN-sra-cache-service-implementation.md's
// "JSONRPC 1.1 wire format" section, which this file implements the
// client side of.
//
// It has no knowledge of SRACache's four methods; internal/sracacheclient
// is the typed layer built on top of it. That split exists so this
// package stays generic enough to reuse for a second Go JSONRPC service,
// should one show up (mirroring internal/jsonrpc's own promotion-candidate
// note).
package rpcclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/BV-BRC/BV-BRC-Go-SDK/v2/version"
)

// Client is a JSONRPC 1.1 client for one service base URL. The zero value
// plus a URL is usable: HTTPClient defaults to http.DefaultClient when nil,
// and Token may be left empty for an unauthenticated caller.
type Client struct {
	// URL is the service's control-plane endpoint, e.g.
	// "http://localhost:7141". Every call POSTs its JSONRPC envelope here
	// directly -- internal/jsonrpc.Server's catch-all "/" mount dispatches
	// a POST to any path other than /ping or /auth_ping, so this need not
	// (but may) include a trailing path segment.
	URL string

	// Token is sent, verbatim and with no "Bearer" prefix, as the
	// Authorization header on every call -- matching the P3AuthToken
	// convention internal/jsonrpc/auth.go's Validator expects
	// (authenticate passes the raw header value straight to
	// Validator.Validate). Left empty, no Authorization header is sent at
	// all, which is what an AuthOptional method requires for an
	// unauthenticated caller and what an AuthRequired method rejects.
	Token string

	// HTTPClient is the underlying HTTP client. nil means
	// http.DefaultClient. Request cancellation/timeouts are expected to
	// come from ctx (http.NewRequestWithContext), not a Timeout set here.
	HTTPClient *http.Client
}

// wireRequest is the JSONRPC 1.1 request envelope this client sends,
// matching internal/jsonrpc's request type field-for-field (it accepts
// either "version" or "jsonrpc" as the version-tag key; this client always
// sends "version", the spelling the generated JS client uses).
type wireRequest struct {
	Params  []any  `json:"params"`
	Method  string `json:"method"`
	Version string `json:"version"`
	ID      string `json:"id"`
}

// wireError mirrors internal/jsonrpc.Error's wire shape.
type wireError struct {
	Name    string          `json:"name"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Detail  string          `json:"error"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// wireResponse covers both the success and error envelopes at once: a
// success response has Result set and Error nil; an error response
// (internal/jsonrpc.ErrorEnvelope) has Error set and Result nil/absent.
// Decoding into one struct regardless of which envelope came back avoids
// having to peek at the body twice.
type wireResponse struct {
	ID      json.RawMessage   `json:"id"`
	Result  []json.RawMessage `json:"result"`
	Version string            `json:"version"`
	Error   *wireError        `json:"error"`
}

// RPCError is returned by Call when the server responded with a JSONRPC
// error envelope. Code/Message/Detail mirror internal/jsonrpc.Error's
// Code/Message/Detail (wire key "error") fields exactly, so a caller (or
// this package's own CLI callers) can print the same short/full split the
// Perl side's client stub does.
type RPCError struct {
	Name    string
	Code    int
	Message string
	Detail  string
}

// Error implements the error interface. Detail is included only when it
// says something Message doesn't -- today's server always sets them
// identically (see internal/jsonrpc/errors.go's constructors), but a
// future server-side change adding real detail should show up here without
// a client-side change.
func (e *RPCError) Error() string {
	if e.Detail != "" && e.Detail != e.Message {
		return fmt.Sprintf("jsonrpc error %d: %s: %s", e.Code, e.Message, e.Detail)
	}
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

// Call invokes method with the given positional params, POSTing the exact
// envelope internal/jsonrpc.Server's dispatcher decodes
// (`{"params":[...],"method":"...","version":"1.1","id":"..."}`), and
// decodes the response.
//
// On a JSONRPC error response, Call returns a *RPCError (use errors.As to
// recover it) -- never a bare fmt error -- so a caller can inspect Code and
// print Message/Detail the way the rest of this codebase's clients do.
//
// On success, if result is non-nil, Call unmarshals result[0] -- the first
// (and, for every SRACache method today, only) element of the response's
// "result" array -- into it. This is arity-driven in spirit but hardcoded
// to the first element in practice: all four SRACache funcdefs declare
// exactly one return value (SRACache.spec), so "the method's one return
// value" and "result[0]" are the same thing for every call this client
// makes. A future multi-return method would need a richer result parameter
// here, not a dispatcher change (mirroring internal/jsonrpc.Server's own
// arity-driven wrapping on the server side).
func (c *Client) Call(ctx context.Context, method string, params []any, result any) error {
	if params == nil {
		params = []any{}
	}

	reqBody, err := json.Marshal(wireRequest{
		Params:  params,
		Method:  method,
		Version: "1.1",
		ID:      newID(),
	})
	if err != nil {
		return fmt.Errorf("rpcclient: encoding %s request: %w", method, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("rpcclient: building %s request: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", version.UserAgent())
	if c.Token != "" {
		// No "Bearer " prefix -- see Token's doc comment above.
		httpReq.Header.Set("Authorization", c.Token)
	}

	httpResp, err := c.httpClient().Do(httpReq)
	if err != nil {
		return fmt.Errorf("rpcclient: calling %s: %w", method, err)
	}
	defer httpResp.Body.Close()

	body, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("rpcclient: reading %s response: %w", method, err)
	}

	var wire wireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return fmt.Errorf("rpcclient: decoding %s response (HTTP %d): %w; body=%s",
			method, httpResp.StatusCode, err, truncate(body))
	}

	if wire.Error != nil {
		return &RPCError{
			Name:    wire.Error.Name,
			Code:    wire.Error.Code,
			Message: wire.Error.Message,
			Detail:  wire.Error.Detail,
		}
	}

	// Every JSONRPC error in this codebase is served as HTTP 500 (see
	// internal/jsonrpc/errors.go's writeError doc comment) and every
	// success as 200 -- so a non-200 with no error envelope is a
	// transport-level surprise (a proxy, a crash, a non-JSONRPC endpoint),
	// not a condition any typed caller here should try to interpret.
	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("rpcclient: %s: unexpected HTTP %d with no JSONRPC error envelope; body=%s",
			method, httpResp.StatusCode, truncate(body))
	}

	if result != nil {
		if len(wire.Result) == 0 {
			return fmt.Errorf("rpcclient: %s: response has no result elements", method)
		}
		if err := json.Unmarshal(wire.Result[0], result); err != nil {
			return fmt.Errorf("rpcclient: %s: decoding result[0]: %w", method, err)
		}
	}
	return nil
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

// newID returns a short random hex string to use as a call's correlation
// id -- unique per call, never reused, but otherwise meaningless (neither
// the server nor this client attach any behavior to its value beyond
// echoing it back, per the wire format's own contract). Randomness is used
// rather than an in-process counter so Client stays exactly the three
// fields callers construct it with, with no shared mutable state to guard.
func newID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is essentially unheard-of on any real
		// target this service runs on; fall back to a fixed id rather
		// than failing the call over an identifier nothing downstream
		// actually depends on.
		return "0"
	}
	return hex.EncodeToString(b[:])
}

// truncate caps a response body at 2KiB for inclusion in an error message,
// matching the "≤2 KiB of body" convention BV-BRC-Go-SDK/internal/httpdiag
// uses for the same purpose (this package can't import that -- it's
// internal to a different module -- but nothing stops it from following
// the same convention).
func truncate(b []byte) string {
	const max = 2048
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
