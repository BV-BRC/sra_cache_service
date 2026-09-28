package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// debugTransport is a minimal, local stand-in for --debug-http: it dumps a
// failed or non-200 request's headers (redacting anything sensitive) to
// out. It is deliberately small -- SRACache is a new internal
// dev_container-deployed service, not something sitting behind Cloudflare
// (the scenario BV-BRC-Go-SDK's internal/httpdiag exists to diagnose), and
// that package is off-limits here anyway (internal to a different Go
// module). See PLAN-sra-cache-service-implementation.md's Phase 8 section:
// --debug-http is explicitly "nice-to-have, not a hard requirement" for
// this phase.
type debugTransport struct {
	enabled bool
	out     io.Writer
}

// sensitiveHeaders mirrors (a small subset of) BV-BRC-Go-SDK/internal/httpdiag's
// redaction list -- Authorization is the one that actually matters here,
// since rpcclient.Client sends the raw P3 token in it with no Bearer
// prefix; Cookie/Set-Cookie/Proxy-Authorization are included on the same
// "never printed" principle even though nothing in this client sets or
// receives them today.
var sensitiveHeaders = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
	"proxy-authorization": true,
}

func (d *debugTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if !d.enabled {
		return resp, err
	}

	if err != nil {
		fmt.Fprintf(d.out, "--debug-http: %s %s: request failed: %v\n", req.Method, req.URL, err)
		dumpHeaders(d.out, "> ", req.Header)
		return resp, err
	}
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(d.out, "--debug-http: %s %s -> %s\n", req.Method, req.URL, resp.Status)
		dumpHeaders(d.out, "> ", req.Header)
		dumpHeaders(d.out, "< ", resp.Header)
	}
	return resp, err
}

func dumpHeaders(out io.Writer, prefix string, h http.Header) {
	for k, v := range h {
		val := strings.Join(v, ",")
		if sensitiveHeaders[strings.ToLower(k)] {
			val = fmt.Sprintf("<redacted, %d bytes>", len(val))
		}
		fmt.Fprintf(out, "%s%s: %s\n", prefix, k, val)
	}
}
