package main

import (
	"fmt"
	"io"
	"net/http"
	"strings"
)

// debugTransport is identical to cmd/p3-sra-precache/debug.go's own type,
// duplicated rather than shared for the same reason as this package's
// exitCodeError -- see main.go's doc comment on that type.
type debugTransport struct {
	enabled bool
	out     io.Writer
}

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
