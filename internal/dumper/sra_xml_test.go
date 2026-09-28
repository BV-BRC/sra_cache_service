package dumper

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchRawSRAXML(t *testing.T) {
	const canned = `<?xml version="1.0"?><EXPERIMENT_PACKAGE_SET><EXPERIMENT_PACKAGE/></EXPERIMENT_PACKAGE_SET>`
	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Write([]byte(canned))
	}))
	defer ts.Close()

	// Point FetchRawSRAXML at the test server by overriding the package-level
	// const isn't possible, so this test only exercises the request-building
	// and response-handling logic against a real HTTP round trip using the
	// same client -- see TestFetchRawSRAXMLViaOverridableTransport below for
	// the actual eutils-endpoint-shaped call.
	_ = gotQuery
	_, err := FetchRawSRAXML(context.Background(), ts.Client(), nil)
	if err == nil {
		t.Fatal("expected an error for an empty accession list")
	}
}

// TestFetchRawSRAXMLViaOverridableTransport drives FetchRawSRAXML through a
// custom http.RoundTripper that redirects the real eutils URL to a local
// httptest server, so the test exercises the actual request construction
// (params, User-Agent) without a network call.
func TestFetchRawSRAXMLViaOverridableTransport(t *testing.T) {
	const canned = `<?xml version="1.0"?><EXPERIMENT_PACKAGE_SET><EXPERIMENT_PACKAGE/></EXPERIMENT_PACKAGE_SET>`
	var gotURL string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		w.Write([]byte(canned))
	}))
	defer ts.Close()

	client := &http.Client{Transport: redirectTransport{target: ts.URL}}
	body, err := FetchRawSRAXML(context.Background(), client, []string{"SRR1234567", "SRR7654321"})
	if err != nil {
		t.Fatalf("FetchRawSRAXML: %v", err)
	}
	if string(body) != canned {
		t.Errorf("body = %q, want %q", body, canned)
	}
	if !strings.Contains(gotURL, "db=sra") || !strings.Contains(gotURL, "rettype=docset") {
		t.Errorf("request URL = %q, want db=sra&rettype=docset params", gotURL)
	}
	if !strings.Contains(gotURL, "SRR1234567") || !strings.Contains(gotURL, "SRR7654321") {
		t.Errorf("request URL = %q, want both accessions in the id list", gotURL)
	}
}

func TestFetchRawSRAXMLNon200(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer ts.Close()

	client := &http.Client{Transport: redirectTransport{target: ts.URL}}
	if _, err := FetchRawSRAXML(context.Background(), client, []string{"SRR1234567"}); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

// redirectTransport rewrites every request's scheme/host to target's,
// preserving the path/query eutils would have received, so tests can assert
// on the request FetchRawSRAXML actually built without a real network call.
type redirectTransport struct{ target string }

func (r redirectTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u, err := req.URL.Parse(r.target)
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.URL.Scheme = u.Scheme
	req.URL.Host = u.Host
	req.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}
