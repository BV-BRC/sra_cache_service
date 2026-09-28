package fetcher

import (
	"fmt"
	"regexp"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/ratelimit"
)

// failedToResolvePattern is ported verbatim (as a match, not just an idea)
// from sra_import/lib/sra_tools.py:368
// (`re.search("failed to resolve", edata, re.MULTILINE)`), which is the
// proven signal for "well-formed accession, NCBI doesn't have it". Confirmed
// live against the real prefetch 3.1.0 binary (2026-09-28):
//
//	$ prefetch --output-directory ./out BOGUSACCESSION123456
//	2026-09-28T20:28:26 prefetch.3.1.0 err: name not found while resolving
//	  query within virtual file system module - failed to resolve accession
//	  'BOGUSACCESSION123456' - Cannot resolve accession ( 404 );
//	  ncbi_phid=...
//	$ echo $?
//	3
//
// i.e. exit code 3, and the exact substring "failed to resolve" is present.
// re.MULTILINE has no effect on a plain substring search with no ^/$
// anchors, so this is behavior-identical to sra_tools.py's check despite
// carrying the flag for documentation parity.
var failedToResolvePattern = regexp.MustCompile(`(?m)failed to resolve`)

// throttlePattern matches text that suggests NCBI (or a mirror it
// redirected to) is rate-limiting or resetting connections under load,
// per PLAN-sra-cache-service-implementation.md's Phase 6 instructions:
// "429/503/Retry-After/reset patterns you find by actually testing or by
// reading prefetch's real error text if you can trigger a comparable case".
//
// Caveat, stated plainly rather than left implicit: this pattern is NOT
// verified against a real throttling response -- triggering one from NCBI
// on demand isn't practical (it requires actually being rate-limited), and
// none of the prefetch invocations made while building this package hit
// one. It is assembled from the design doc's own list of signals
// (429/503/Retry-After/connection reset) plus the standard "too many
// requests" HTTP 429 reason phrase. Revisit this pattern the first time a
// real throttle is observed in production logs (Phase 10's live
// verification is the place that will happen) -- until then it is a
// best-effort guess, not a confirmed contract, unlike failedToResolvePattern
// above.
var throttlePattern = regexp.MustCompile(`(?i)\b(429|503)\b|retry-after|too many requests|connection reset`)

// tailBytes bounds how much of a failed command's captured output is kept
// as a catalog message -- enough to be useful in a `precache_status` call
// or a log line, not enough to dump megabytes of prefetch chatter into a
// sqlite TEXT column.
const tailBytes = 2048

// tail returns the last n bytes of s, prefixed with an ellipsis marker if
// anything was actually cut off.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-n:]
}

// Classify maps a completed `prefetch` invocation's exit code and captured
// combined stdout+stderr to a precache_state -- using the exact enum from
// PLAN-sra-precache-service.md section 6 / SRACache.spec's precache_state
// typedef (catalog.State mirrors it 1:1: queued/in_progress/cached/
// invalid_id/not_found/error) -- plus a human-readable message.
//
// exitCode == 0 is success: catalog.StateCached (callers that already know
// the fetch succeeded, i.e. the pool after a nil Fetch error, don't need to
// call this at all; it's included for completeness and for classify_test.go
// to exercise the full contract in one place).
//
// A throttling signal is NOT reported as catalog.StateError: the design doc
// is explicit that this is "a transient condition the pool should retry,
// not a hard error" (Phase 6 instructions). Since the precache_state enum
// has no dedicated "throttled, will retry" value, this reuses
// catalog.StateQueued -- which is exactly what the pool does with it: the
// accession goes back into the queue rather than being reported to a caller
// as a failure. As its one side effect on this path (and only this path),
// Classify calls limiter.ReportThrottle() itself, so every call site that
// detects a throttle -- there is only one, here -- backs the limiter off the
// same way, rather than leaving that up to each caller to remember.
func Classify(exitCode int, output string, limiter *ratelimit.Limiter) (catalog.State, string) {
	if exitCode == 0 {
		return catalog.StateCached, ""
	}

	if failedToResolvePattern.MatchString(output) {
		return catalog.StateNotFound, fmt.Sprintf(
			"NCBI has no such accession (prefetch exited %d): %s", exitCode, tail(output, tailBytes))
	}

	if throttlePattern.MatchString(output) {
		if limiter != nil {
			limiter.ReportThrottle()
		}
		return catalog.StateQueued, fmt.Sprintf(
			"transient failure (possible rate limiting), requeued for retry (prefetch exited %d): %s",
			exitCode, tail(output, tailBytes))
	}

	return catalog.StateError, fmt.Sprintf("prefetch exited %d: %s", exitCode, tail(output, tailBytes))
}
