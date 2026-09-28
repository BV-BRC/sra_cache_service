package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeValidator is a test-only Validator. err, if non-nil, is returned for
// every call (userID is ignored in that case); otherwise every call
// succeeds with userID. calls counts invocations so tests can assert a
// Validator was (or, notably, was NOT) consulted -- e.g. AuthRequired
// with a missing header must short-circuit before ever calling Validate,
// matching p3_server_stub.tt's `if (!$token && $method_auth eq
// 'required')` guard, which fires before the ->validate call below it.
type fakeValidator struct {
	userID string
	err    error
	calls  int
}

func (f *fakeValidator) Validate(token string) (string, error) {
	f.calls++
	if f.err != nil {
		return "", f.err
	}
	return f.userID, nil
}

// echoHandler is the test-only "SRACache.echo"-style method the golden
// wire-format tests register directly -- never added to any production
// registration path. It unmarshals params (the JSONRPC positional-args
// array) back into []any and returns that array as its single result, so
// a round trip is easy to assert on, and it optionally records the
// AuthInfo the dispatcher attached to its context so a test can inspect
// authentication outcomes a wire response alone can't reveal (e.g.
// Authenticated == false for a rejected-but-optional token still returns
// a normal 200 success envelope with no distinguishing feature except
// what the handler itself observed).
func echoHandler(capture *AuthInfo) Handler {
	return func(ctx context.Context, params json.RawMessage) ([]any, error) {
		if capture != nil {
			*capture = AuthInfoFromContext(ctx)
		}
		var args []any
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return []any{args}, nil
	}
}

func newTestServer(v Validator) *Server {
	return NewServer(v, nil)
}

func postJSON(t *testing.T, srv *Server, body string, formEncodedContentType bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
	if formEncodedContentType {
		// This is the exact shape the real generated JS client sends: a
		// raw JSON body under jQuery's default form-encoded
		// Content-Type, not application/json.
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestGoldenWireFormat_VersionSpelling(t *testing.T) {
	srv := newTestServer(&fakeValidator{})
	srv.Register("SRACache.echo", AuthNone, 1, echoHandler(nil))

	body := `{"params":["a","b"],"method":"SRACache.echo","version":"1.1","id":"12345"}`
	rec := postJSON(t, srv, body, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v; body = %s", err, rec.Body.String())
	}
	if string(resp.ID) != `"12345"` {
		t.Errorf("id = %s, want %q", resp.ID, `"12345"`)
	}
	if resp.Version != "1.1" {
		t.Errorf("version = %q, want %q", resp.Version, "1.1")
	}
	if len(resp.Result) != 1 {
		t.Fatalf("result has %d elements, want 1 (arity)", len(resp.Result))
	}
	got, ok := resp.Result[0].([]any)
	if !ok || len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("result[0] = %#v, want echoed [\"a\",\"b\"]", resp.Result[0])
	}
}

func TestGoldenWireFormat_JSONRPCSpelling(t *testing.T) {
	srv := newTestServer(&fakeValidator{})
	srv.Register("SRACache.echo", AuthNone, 1, echoHandler(nil))

	// Same request, but with "jsonrpc":"1.1" in place of "version":"1.1" --
	// must succeed identically.
	body := `{"params":["a","b"],"method":"SRACache.echo","jsonrpc":"1.1","id":"12345"}`
	rec := postJSON(t, srv, body, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	var resp SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if resp.Version != "1.1" {
		t.Errorf("version = %q, want %q (response always says version, regardless of the request's spelling)", resp.Version, "1.1")
	}
	if len(resp.Result) != 1 {
		t.Fatalf("result has %d elements, want 1", len(resp.Result))
	}
}

func TestIgnoresContentType(t *testing.T) {
	// The generated JS client's Content-Type header lies (form-encoded,
	// not JSON) -- the dispatcher must decode the body regardless.
	srv := newTestServer(&fakeValidator{})
	srv.Register("SRACache.echo", AuthNone, 1, echoHandler(nil))

	body := `{"params":[1],"method":"SRACache.echo","version":"1.1","id":1}`
	rec := postJSON(t, srv, body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAuthRequired_MissingHeader(t *testing.T) {
	v := &fakeValidator{userID: "should-not-matter"}
	srv := newTestServer(v)
	srv.Register("SRACache.needs_auth", AuthRequired, 1, echoHandler(nil))

	body := `{"params":[],"method":"SRACache.needs_auth","version":"1.1","id":"1"}`
	rec := postJSON(t, srv, body, true)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope: %v; body = %s", err, rec.Body.String())
	}
	const wantMsg = "Authentication required for SRACache but no authentication header was passed"
	if env.ErrorObj.Message != wantMsg {
		t.Errorf("error.message = %q, want verbatim %q", env.ErrorObj.Message, wantMsg)
	}
	if env.ErrorObj.Name != "JSONRPCError" {
		t.Errorf("error.name = %q, want %q", env.ErrorObj.Name, "JSONRPCError")
	}
	if env.ErrorObj.Code != -32603 {
		t.Errorf("error.code = %d, want -32603", env.ErrorObj.Code)
	}
	if env.Version != "1.1" {
		t.Errorf("version = %q, want %q", env.Version, "1.1")
	}
	if v.calls != 0 {
		t.Errorf("Validate called %d times, want 0 -- a missing header on a required method must short-circuit before validation, matching p3_server_stub.tt's `if (!$token && $method_auth eq 'required')` guard", v.calls)
	}
}

func TestAuthRequired_InvalidToken(t *testing.T) {
	v := &fakeValidator{err: errors.New("token expired")}
	srv := newTestServer(v)
	srv.Register("SRACache.needs_auth", AuthRequired, 1, echoHandler(nil))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"params":[],"method":"SRACache.needs_auth","version":"1.1","id":"1"}`))
	req.Header.Set("Authorization", "garbage-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding error envelope: %v", err)
	}
	const wantMsg = "Token validation failed: token expired"
	if env.ErrorObj.Message != wantMsg {
		t.Errorf("error.message = %q, want verbatim %q", env.ErrorObj.Message, wantMsg)
	}
	if env.ErrorObj.Code != -32603 {
		t.Errorf("error.code = %d, want -32603", env.ErrorObj.Code)
	}
}

func TestAuthOptional_NoHeader(t *testing.T) {
	v := &fakeValidator{userID: "someone"}
	srv := newTestServer(v)
	var captured AuthInfo
	srv.Register("SRACache.maybe_auth", AuthOptional, 1, echoHandler(&captured))

	body := `{"params":[],"method":"SRACache.maybe_auth","version":"1.1","id":"1"}`
	rec := postJSON(t, srv, body, true)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if captured.Authenticated {
		t.Errorf("Authenticated = true, want false for no header")
	}
}

func TestAuthOptional_GarbageTokenProceedsUnauthenticated(t *testing.T) {
	// The asymmetric case: AuthOptional + a present-but-rejected token
	// must still succeed, unauthenticated -- verified against
	// p3_server_stub.tt's `if ($method_auth eq 'required' && !$valid)`
	// guard, which only ever raises for 'required'.
	v := &fakeValidator{err: errors.New("garbage token rejected")}
	srv := newTestServer(v)
	var captured AuthInfo
	srv.Register("SRACache.maybe_auth", AuthOptional, 1, echoHandler(&captured))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"params":[],"method":"SRACache.maybe_auth","version":"1.1","id":"1"}`))
	req.Header.Set("Authorization", "garbage-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (Optional+invalid must proceed, not error); body = %s", rec.Code, rec.Body.String())
	}
	if captured.Authenticated {
		t.Errorf("Authenticated = true, want false for a rejected token")
	}
	if v.calls != 1 {
		t.Errorf("Validate called %d times, want 1", v.calls)
	}
}

func TestIDRoundTrip_String(t *testing.T) {
	srv := newTestServer(&fakeValidator{})
	srv.Register("SRACache.echo", AuthNone, 1, echoHandler(nil))

	rec := postJSON(t, srv, `{"params":[],"method":"SRACache.echo","version":"1.1","id":"abc-123"}`, true)
	var resp SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if string(resp.ID) != `"abc-123"` {
		t.Errorf("id = %s, want %q", resp.ID, `"abc-123"`)
	}
}

func TestIDRoundTrip_Numeric(t *testing.T) {
	srv := newTestServer(&fakeValidator{})
	srv.Register("SRACache.echo", AuthNone, 1, echoHandler(nil))

	rec := postJSON(t, srv, `{"params":[],"method":"SRACache.echo","version":"1.1","id":42}`, true)
	var resp SuccessResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if string(resp.ID) != "42" {
		t.Errorf("id = %s, want 42 (unquoted, numeric)", resp.ID)
	}
}

func TestUnknownMethod(t *testing.T) {
	srv := newTestServer(&fakeValidator{})
	rec := postJSON(t, srv, `{"params":[],"method":"SRACache.no_such_method","version":"1.1","id":"1"}`, true)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if env.ErrorObj.Code != -32601 {
		t.Errorf("error.code = %d, want -32601", env.ErrorObj.Code)
	}
}

func TestGetNotDispatchedToRPC(t *testing.T) {
	srv := newTestServer(&fakeValidator{})
	srv.Register("SRACache.echo", AuthNone, 1, echoHandler(nil))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a non-POST, non-ping/auth_ping request", rec.Code)
	}
}

func TestSplitMethod(t *testing.T) {
	cases := []struct {
		in         string
		wantModule string
		wantMethod string
		wantOK     bool
	}{
		{"SRACache.get_metadata", "SRACache", "get_metadata", true},
		{"a.b.c", "a.b", "c", true}, // module names may themselves contain dots.
		{"no-dot-at-all", "", "", false},
	}
	for _, c := range cases {
		module, method, ok := splitMethod(c.in)
		if ok != c.wantOK || module != c.wantModule || method != c.wantMethod {
			t.Errorf("splitMethod(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, module, method, ok, c.wantModule, c.wantMethod, c.wantOK)
		}
	}
}
