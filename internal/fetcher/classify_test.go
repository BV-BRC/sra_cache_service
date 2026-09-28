package fetcher

import (
	"strings"
	"testing"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/ratelimit"
)

func TestClassifySuccess(t *testing.T) {
	state, msg := Classify(0, "", nil)
	if state != catalog.StateCached {
		t.Errorf("state = %q, want cached", state)
	}
	if msg != "" {
		t.Errorf("message = %q, want empty on success", msg)
	}
}

// TestClassifyFailedToResolve is the "not_found" acceptance case: the exact
// error text captured live from a real prefetch 3.1.0 run against a bogus
// accession (see this package's doc comment on failedToResolvePattern).
func TestClassifyFailedToResolve(t *testing.T) {
	output := `2026-09-28T20:28:26 prefetch.3.1.0 err: name not found while resolving query within virtual file system module - failed to resolve accession 'BOGUSACCESSION123456' - Cannot resolve accession ( 404 ); ncbi_phid=EC812AE4ABACD78100000000000B000B.m_1
2026-09-28T20:28:26 prefetch.3.1.0: Current preference is set to retrieve SRA Normalized Format files with full base quality scores.`

	state, msg := Classify(3, output, nil)
	if state != catalog.StateNotFound {
		t.Fatalf("state = %q, want not_found", state)
	}
	if !strings.Contains(msg, "BOGUSACCESSION123456") {
		t.Errorf("message = %q, want it to retain the captured detail", msg)
	}
}

// TestClassifyThrottleReportsAndRequeues asserts a throttle-shaped output
// calls ReportThrottle on the limiter exactly once and comes back as
// StateQueued (retry), never StateError.
func TestClassifyThrottleReportsAndRequeues(t *testing.T) {
	cases := []string{
		"HTTP request failed: 429 Too Many Requests",
		"server returned 503 Service Unavailable",
		"Retry-After: 30",
		"connection reset by peer",
	}
	for _, output := range cases {
		t.Run(output, func(t *testing.T) {
			l := ratelimit.New(8, 16)
			state, msg := Classify(1, output, l)
			if state != catalog.StateQueued {
				t.Errorf("state = %q, want queued (retry), for output %q", state, output)
			}
			if msg == "" {
				t.Error("message is empty, want detail about the retry")
			}
			if got := l.Current(); got != 4 {
				t.Errorf("limiter.Current() = %d, want 4 (ReportThrottle halved 8)", got)
			}
		})
	}
}

// TestClassifyThrottleNilLimiterDoesNotPanic covers the defensive nil check
// -- a test might reasonably call Classify without a limiter at all.
func TestClassifyThrottleNilLimiterDoesNotPanic(t *testing.T) {
	state, _ := Classify(1, "429 too many requests", nil)
	if state != catalog.StateQueued {
		t.Errorf("state = %q, want queued", state)
	}
}

// TestClassifyOtherFailureIsError asserts anything that matches neither
// pattern is a hard error, carrying a tail of the output as the message.
func TestClassifyOtherFailureIsError(t *testing.T) {
	output := "prefetch.3.1.0 err: some unrelated disk-full failure that isn't a resolve or throttle signal"
	state, msg := Classify(1, output, nil)
	if state != catalog.StateError {
		t.Fatalf("state = %q, want error", state)
	}
	if !strings.Contains(msg, "disk-full") {
		t.Errorf("message = %q, want it to retain the captured detail", msg)
	}
}

// TestClassifyMessageIsTailedNotWholeOutput asserts a large output is
// truncated rather than copied in full into the message.
func TestClassifyMessageIsTailedNotWholeOutput(t *testing.T) {
	big := strings.Repeat("x", tailBytes*3) + "END-OF-OUTPUT"
	_, msg := Classify(1, big, nil)
	if len(msg) >= len(big) {
		t.Errorf("message length %d, want it shorter than the %d-byte input", len(msg), len(big))
	}
	if !strings.Contains(msg, "END-OF-OUTPUT") {
		t.Error("message should retain the tail of the output, including its last line")
	}
}
