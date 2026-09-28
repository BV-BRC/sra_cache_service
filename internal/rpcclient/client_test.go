package rpcclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/BV-BRC/sra_cache_service/internal/jsonrpc"
)

// fakeValidator is a minimal test-only jsonrpc.Validator: it accepts
// exactly one token and rejects everything else, including no token at
// all. Used only by TestCallRoundTripsAgainstRealServer, which is the one
// test in this file that stands up a real internal/jsonrpc.Server rather
// than a hand-rolled httptest handler.
type fakeValidator struct{ token string }

func (f fakeValidator) Validate(token string) (string, error) {
	if token == f.token {
		return "test-user", nil
	}
	return "", errors.New("invalid token")
}

func TestCallDecodesSuccessEnvelope(t *testing.T) {
	var gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("server: decoding request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id":"1","result":[[{"accession":"SRR1234567","platform":"ILLUMINA"}]],"version":"1.1"}`))
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Token: "test-token"}
	var rows []struct {
		Accession string `json:"accession"`
		Platform  string `json:"platform"`
	}
	if err := c.Call(context.Background(), "SRACache.get_metadata", []any{[]string{"SRR1234567"}}, &rows); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if len(rows) != 1 || rows[0].Accession != "SRR1234567" || rows[0].Platform != "ILLUMINA" {
		t.Fatalf("rows = %+v", rows)
	}

	if gotAuth != "test-token" {
		t.Errorf("Authorization header = %q, want %q (no Bearer prefix)", gotAuth, "test-token")
	}
	if gotBody["method"] != "SRACache.get_metadata" {
		t.Errorf("method = %v, want SRACache.get_metadata", gotBody["method"])
	}
	if gotBody["version"] != "1.1" {
		t.Errorf("version = %v, want 1.1", gotBody["version"])
	}
	if id, ok := gotBody["id"]; !ok || id == "" {
		t.Errorf("id = %v, want a non-empty id", id)
	}
	params, ok := gotBody["params"].([]any)
	if !ok || len(params) != 1 {
		t.Fatalf("params = %#v, want a one-element array", gotBody["params"])
	}
}

func TestCallOmitsAuthorizationHeaderWhenTokenEmpty(t *testing.T) {
	sawHeader := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawHeader = r.Header["Authorization"]
		w.Write([]byte(`{"id":"1","result":[[]],"version":"1.1"}`))
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL} // Token left empty.
	var rows []any
	if err := c.Call(context.Background(), "SRACache.precache_status", []any{[]string{}}, &rows); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if sawHeader {
		t.Error("Authorization header present, want absent when Token is empty")
	}
}

func TestCallDecodesErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":{"name":"JSONRPCError","code":-32603,"message":"Authentication required for SRACache but no authentication header was passed","error":"Authentication required for SRACache but no authentication header was passed"},"id":"1","version":"1.1"}`))
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL}
	var rows []any
	err := c.Call(context.Background(), "SRACache.start_precache", []any{[]string{"SRR1"}, nil}, &rows)
	if err == nil {
		t.Fatal("Call: expected an error, got nil")
	}
	var rerr *RPCError
	if !errors.As(err, &rerr) {
		t.Fatalf("Call error is %T (%v), want *RPCError", err, err)
	}
	if rerr.Code != -32603 {
		t.Errorf("rerr.Code = %d, want -32603", rerr.Code)
	}
	if rerr.Message != "Authentication required for SRACache but no authentication header was passed" {
		t.Errorf("rerr.Message = %q, want the verbatim auth-required text", rerr.Message)
	}
}

// TestCallUnexpectedNon200WithNoErrorEnvelope covers a transport-level
// surprise (not a JSONRPC error at all) -- e.g. a proxy or crash returning
// plain text with a non-200 status and no {"error":...} body.
func TestCallUnexpectedNon200WithNoErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"id":"1","result":[[]],"version":"1.1"}`))
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL}
	var rows []any
	err := c.Call(context.Background(), "SRACache.precache_status", []any{[]string{}}, &rows)
	if err == nil {
		t.Fatal("Call: expected an error for an unexpected HTTP 502, got nil")
	}
	var rerr *RPCError
	if errors.As(err, &rerr) {
		t.Fatalf("Call error is *RPCError (%v), want a plain transport error since no error envelope was sent", rerr)
	}
}

// TestCallRoundTripsAgainstRealServer is the wire-format acceptance test:
// it exercises this client against a real internal/jsonrpc.Server (not a
// hand-rolled httptest handler), for both an AuthNone and an AuthRequired
// method, so the request/response shapes are validated against the actual
// dispatcher this service runs, not a guessed approximation of it.
func TestCallRoundTripsAgainstRealServer(t *testing.T) {
	rpcSrv := jsonrpc.NewServer(fakeValidator{token: "good-token"}, nil)
	rpcSrv.Register("SRACache.echo", jsonrpc.AuthNone, 1, func(ctx context.Context, params json.RawMessage) ([]any, error) {
		var args []any
		if err := json.Unmarshal(params, &args); err != nil {
			return nil, err
		}
		return []any{args}, nil
	})
	rpcSrv.Register("SRACache.whoami", jsonrpc.AuthRequired, 1, func(ctx context.Context, params json.RawMessage) ([]any, error) {
		info := jsonrpc.AuthInfoFromContext(ctx)
		return []any{info.UserID}, nil
	})

	ts := httptest.NewServer(rpcSrv)
	defer ts.Close()

	t.Run("AuthNone echo", func(t *testing.T) {
		c := &Client{URL: ts.URL}
		var result []any
		if err := c.Call(context.Background(), "SRACache.echo", []any{"a", 2.0}, &result); err != nil {
			t.Fatalf("Call: %v", err)
		}
		if len(result) != 2 || result[0] != "a" || result[1] != 2.0 {
			t.Errorf("result = %#v, want echoed [\"a\", 2]", result)
		}
	})

	t.Run("AuthRequired missing token fails", func(t *testing.T) {
		c := &Client{URL: ts.URL}
		var userID string
		err := c.Call(context.Background(), "SRACache.whoami", []any{}, &userID)
		if err == nil {
			t.Fatal("Call: expected an auth error, got nil")
		}
		var rerr *RPCError
		if !errors.As(err, &rerr) || rerr.Code != -32603 {
			t.Fatalf("Call error = %v, want a -32603 *RPCError", err)
		}
	})

	t.Run("AuthRequired with valid token succeeds", func(t *testing.T) {
		c := &Client{URL: ts.URL, Token: "good-token"}
		var userID string
		if err := c.Call(context.Background(), "SRACache.whoami", []any{}, &userID); err != nil {
			t.Fatalf("Call: %v", err)
		}
		if userID != "test-user" {
			t.Errorf("userID = %q, want %q", userID, "test-user")
		}
	})
}
