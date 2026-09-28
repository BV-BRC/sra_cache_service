package jsonrpc

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name   string
		xff    string
		xrip   string
		remote string
		want   string
	}{
		{"xff wins, first entry only", "1.2.3.4, 5.6.7.8", "9.9.9.9", "10.0.0.1:1234", "1.2.3.4"},
		{"xrip when no xff", "", "9.9.9.9", "10.0.0.1:1234", "9.9.9.9"},
		{"remote addr when neither header set", "", "", "10.0.0.1:1234", "10.0.0.1:1234"},
		{"xff entries are trimmed", " 1.2.3.4 , 5.6.7.8", "", "10.0.0.1:1234", "1.2.3.4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/", nil)
			req.RemoteAddr = c.remote
			if c.xff != "" {
				req.Header.Set("X-Forwarded-For", c.xff)
			}
			if c.xrip != "" {
				req.Header.Set("X-Real-IP", c.xrip)
			}
			if got := ClientIP(req); got != c.want {
				t.Errorf("ClientIP() = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAuthInfoFromContext_Empty(t *testing.T) {
	info := AuthInfoFromContext(httptest.NewRequest("GET", "/", nil).Context())
	if info.Authenticated {
		t.Errorf("Authenticated = true for a context with no AuthInfo attached, want false")
	}
}

func TestRequestFromContext(t *testing.T) {
	if got := RequestFromContext(httptest.NewRequest("GET", "/", nil).Context()); got != nil {
		t.Errorf("RequestFromContext = %v, want nil outside a dispatched request", got)
	}

	req := httptest.NewRequest("GET", "/", nil)
	ctx := withRequest(req.Context(), req)
	if got := RequestFromContext(ctx); got != req {
		t.Errorf("RequestFromContext did not round-trip the request")
	}
}
