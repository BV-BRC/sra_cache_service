// Command p3-sra-precache queues SRA run accessions for precaching on the
// SRACache service (SRACache.start_precache), and optionally waits for
// them to finish (SRACache.precache_status), per
// PLAN-sra-cache-service-implementation.md's Phase 8.
//
// SRACache has no production deployment yet (Phase 10 of that plan) --
// --url / $SRA_CACHE_URL must point at wherever a server is actually
// running; the built-in default is a localhost placeholder for local
// testing only.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	sdkauth "github.com/BV-BRC/BV-BRC-Go-SDK/auth"
	"github.com/BV-BRC/BV-BRC-Go-SDK/version"
	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/rpcclient"
	"github.com/BV-BRC/sra_cache_service/internal/sracacheclient"
)

// cliProduct is this tool's User-Agent product identity, in the same
// "name the product, not the library" spirit as BV-BRC-Go-SDK/version's
// CLIProduct ("bvbrc-cli-go") / AuthProduct ("bvbrc-auth-go") and p3_auth's
// Perl equivalents. p3-sra-precache is neither a p3_cli-mirroring command
// nor a login tool, so it gets a name of its own rather than reusing
// either of those.
const cliProduct = "bvbrc-sra-cache-cli"

// defaultURL is a placeholder control-plane base URL. SRACache has no real
// deployment yet -- see PLAN-sra-cache-service-implementation.md's Phase
// 10 ("Production deployment and live verification"), which has not
// happened as of this writing -- so there is no real host:port to default
// to. 7141 matches this repo's Makefile/SRACache.spec SERVICE_PORT. This
// MUST become a real deployed URL once Phase 10 lands; --url or
// $SRA_CACHE_URL overrides it in the meantime.
const defaultURL = "http://localhost:7141"

func main() {
	version.SetProduct(cliProduct)

	if err := newRootCmd().Execute(); err != nil {
		var ec *exitCodeError
		if errors.As(err, &ec) {
			if ec.err != nil {
				fmt.Fprintln(os.Stderr, "p3-sra-precache:", ec.err)
			}
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "p3-sra-precache:", err)
		os.Exit(1)
	}
}

// exitCodeError carries the process exit code a failure should produce,
// distinguishing a hard RPC/transport failure (code 1) from a call that
// completed successfully but reported at least one accession in a failed
// terminal state (code 2) -- see runPrecache. err may be nil (code 2's
// case: the table has already been printed, there's nothing more to say).
type exitCodeError struct {
	code int
	err  error
}

func (e *exitCodeError) Error() string {
	if e.err != nil {
		return e.err.Error()
	}
	return fmt.Sprintf("exit %d", e.code)
}

func (e *exitCodeError) Unwrap() error { return e.err }

func newRootCmd() *cobra.Command {
	var (
		urlFlag      string
		wait         bool
		pollInterval time.Duration
		timeout      time.Duration
		debugHTTP    bool
	)

	cmd := &cobra.Command{
		Use:   "p3-sra-precache [flags] <accession>...",
		Short: "Queue SRA run accessions for precaching on the SRACache service",
		Long: `p3-sra-precache queues one or more run-level SRA accessions
(SRR/DRR/ERR) for precaching on the SRACache service via
SRACache.start_precache.

Without --wait, it fires the request and prints each accession's
immediate state (fire-and-forget). With --wait, it polls
SRACache.precache_status until every accession reaches a terminal state
(cached, invalid_id, not_found, or error), prints a summary table, and
exits nonzero if any accession ended in a failed state.

An SRX/SRP accession is rejected as invalid_id -- expand it to its
constituent runs with a get_metadata call first (not something this tool
does for you).`,
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			resolvedURL := urlFlag
			if resolvedURL == "" {
				resolvedURL = os.Getenv("SRA_CACHE_URL")
			}
			if resolvedURL == "" {
				resolvedURL = defaultURL
			}

			// This tool always calls start_precache -- there is no
			// polling-only mode that skips it -- so a token is always
			// required, matching SRACache.spec's `authentication
			// required` on start_precache. (A --wait-only,
			// start_precache-free "just poll" tool would be a different,
			// smaller command; nothing here prevents adding one later.)
			tok := sdkauth.RequireToken()

			httpClient := &http.Client{
				Transport: &debugTransport{enabled: debugHTTP, out: os.Stderr},
			}
			rpc := &rpcclient.Client{URL: resolvedURL, Token: tok.Raw, HTTPClient: httpClient}
			c := sracacheclient.New(rpc)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			_, err := runPrecache(ctx, cmd.OutOrStdout(), c, args, wait, pollInterval, timeout)
			return err
		},
	}

	cmd.Flags().StringVar(&urlFlag, "url", "",
		fmt.Sprintf("SRACache control-plane base URL (default %q; overridable via $SRA_CACHE_URL)", defaultURL))
	cmd.Flags().BoolVar(&wait, "wait", false,
		"poll precache_status until every accession reaches a terminal state, then print a summary table")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 5*time.Second,
		"how often to poll precache_status when --wait is given")
	cmd.Flags().DurationVar(&timeout, "timeout", 0,
		"give up waiting after this long when --wait is given (0 = wait indefinitely)")
	cmd.Flags().BoolVar(&debugHTTP, "debug-http", false,
		"dump request/response headers for a failed or non-200 RPC call (a minimal, local equivalent of BV-BRC-Go-SDK's P3_DEBUG_HTTP -- see this command's package doc)")

	registerVersion(cmd)

	return cmd
}

// client is the subset of *sracacheclient.Client this command needs,
// declared as an interface so tests can inject a fake without standing up
// a real jsonrpc.Server; *sracacheclient.Client satisfies it as-is.
type client interface {
	StartPrecache(ctx context.Context, accessions []string) ([]sracacheclient.PrecacheStatus, error)
	PrecacheStatus(ctx context.Context, accessions []string) ([]sracacheclient.PrecacheStatus, error)
}

// runPrecache is the command's core logic, decoupled from cobra/main so it
// can be unit-tested with a fake client (see main_test.go). It always
// returns the last set of status rows it saw, even on an error return, so
// a caller (a test, or a future caller) can inspect what was known at the
// point of failure.
//
// The returned error, when non-nil, is always an *exitCodeError so main
// can set the right process exit code without re-deriving it.
func runPrecache(ctx context.Context, out io.Writer, c client, accessions []string, wait bool, pollInterval, timeout time.Duration) ([]sracacheclient.PrecacheStatus, error) {
	rows, err := c.StartPrecache(ctx, accessions)
	if err != nil {
		return nil, &exitCodeError{code: 1, err: fmt.Errorf("start_precache: %w", err)}
	}

	if !wait {
		// Fire-and-forget: print what start_precache itself reported and
		// exit 0 regardless of any individual accession's state -- an
		// invalid_id here is a fact about the request, not a failure of
		// the call, and the whole point of skipping --wait is not caring
		// yet how it turns out.
		printTable(out, rows)
		return rows, nil
	}

	final, err := waitForTerminal(ctx, rows, c, pollInterval, timeout)
	printTable(out, final) // print whatever was last observed even on a timeout/context error
	if err != nil {
		return final, &exitCodeError{code: 1, err: err}
	}
	if anyFailed(final) {
		return final, &exitCodeError{code: 2}
	}
	return final, nil
}

// waitForTerminal polls c.PrecacheStatus at pollInterval until every row in
// initial (by accession) has reached a terminal precache_state, or ctx is
// done, or timeout elapses (timeout <= 0 means no deadline beyond ctx
// itself). initial is checked for an already-terminal state before the
// first sleep -- e.g. start_precache's own invalid_id rows, or an
// accession that happened to already be cached -- so a request that needs
// no waiting at all doesn't wait one pollInterval regardless.
func waitForTerminal(ctx context.Context, initial []sracacheclient.PrecacheStatus, c client, pollInterval, timeout time.Duration) ([]sracacheclient.PrecacheStatus, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	rows := initial
	accessions := make([]string, len(rows))
	for i, r := range rows {
		accessions[i] = r.Accession
	}

	for {
		if allTerminal(rows) {
			return rows, nil
		}
		select {
		case <-ctx.Done():
			return rows, fmt.Errorf("waiting for a terminal state: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
		next, err := c.PrecacheStatus(ctx, accessions)
		if err != nil {
			return rows, fmt.Errorf("precache_status: %w", err)
		}
		rows = next
	}
}

// terminalStates are precache_state values (SRACache.spec) that will never
// change on their own -- queued and in_progress are the two non-terminal
// values; an empty state (precache_status's "never requested" sentinel,
// internal/rpcapi/precache.go's fillPrecacheStatuses) is deliberately NOT
// terminal here, since that can only appear for an accession this command
// itself never asked start_precache to enqueue, which shouldn't happen in
// practice but must not be mistaken for "done" if it does.
var terminalStates = map[catalog.State]bool{
	catalog.StateCached:    true,
	catalog.StateInvalidID: true,
	catalog.StateNotFound:  true,
	catalog.StateError:     true,
}

// failedStates is the subset of terminalStates that runPrecache reports as
// a nonzero exit, per this command's --wait contract.
var failedStates = map[catalog.State]bool{
	catalog.StateInvalidID: true,
	catalog.StateNotFound:  true,
	catalog.StateError:     true,
}

func allTerminal(rows []sracacheclient.PrecacheStatus) bool {
	for _, r := range rows {
		if !terminalStates[catalog.State(r.State)] {
			return false
		}
	}
	return true
}

func anyFailed(rows []sracacheclient.PrecacheStatus) bool {
	for _, r := range rows {
		if failedStates[catalog.State(r.State)] {
			return true
		}
	}
	return false
}

// printTable renders rows as a fixed-width table: accession, state, size
// (humanized, "-" until known), and message (set for invalid_id/not_found/
// error -- see SRACache.spec's precache_status doc comment).
func printTable(out io.Writer, rows []sracacheclient.PrecacheStatus) {
	fmt.Fprintf(out, "%-15s %-12s %10s  %s\n", "ACCESSION", "STATE", "SIZE", "MESSAGE")
	for _, r := range rows {
		size := "-"
		if r.SizeBytes > 0 {
			size = humanize.Bytes(uint64(r.SizeBytes))
		}
		fmt.Fprintf(out, "%-15s %-12s %10s  %s\n", r.Accession, r.State, size, r.Message)
	}
}

// versionAnnotation keys the second line of --version output in the
// command's annotations, mirroring BV-BRC-Go-SDK/internal/cliroot's own
// approach (that package is off-limits to import -- it's internal to a
// different module -- so this is a from-scratch, deliberately small
// reimplementation of the same idea, not a copy of its code): putting the
// text in Annotations rather than interpolating it straight into the
// template string means a "{{" in a P3_USER_AGENT override is printed
// literally instead of being parsed as a template action.
const versionAnnotation = "sracache.build"

// registerVersion gives cmd a --version flag that prints, in the same
// two-line style every other p3-*/rast-* tool in this codebase's Go SDK
// uses:
//
//	p3-sra-precache <version>
//	bvbrc-sra-cache-cli/<version> linux/amd64 go1.25.6
//
// The second line is the exact User-Agent this binary sends (reflecting a
// P3_USER_AGENT override, if any) plus the build platform.
func registerVersion(cmd *cobra.Command) {
	cmd.Version = version.Get()
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[versionAnnotation] = fmt.Sprintf("%s %s/%s %s",
		version.UserAgent(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	cmd.SetVersionTemplate(
		`{{.Name}} {{.Version}}` + "\n" +
			`{{index .Annotations "` + versionAnnotation + `"}}` + "\n")
}
