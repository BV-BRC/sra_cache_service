// Package dumper implements the client-side half of p3-sra-dump: given a
// run-level accession already known to be (or about to become) cached by
// SRACache, download its .sra bytes over the data plane and run the SRA
// Toolkit's fasterq-dump/fastq-dump against them -- the compatibility
// contract sra_import/lib/sra_tools.py already establishes for p3-sra.
//
// See PLAN-sra-cache-service-implementation.md, Phase 9, and
// PLAN-sra-precache-service.md sections 2.2 and 7/7.1/7.2 for the design
// this implements.
//
// # Why the final dump-tool argument is a local path, not a bare accession
//
// sra_tools.py's own invocation (download_fastq_files, lines 294-300) passes
// the bare run_id as the tool's last positional argument, relying on a
// separate, already-completed `prefetch <run_id>` call (lines 277-282, run
// just before) having populated the SRA Toolkit's own local repository
// (typically ~/ncbi/public/sra/<run_id>/<run_id>.sra) -- fasterq-dump then
// resolves the bare accession through VDB configuration to find that file.
//
// p3-sra-dump never runs `prefetch` locally at all -- by design, precaching
// happens on the SRACache service, and this tool's only job is to pull the
// already-cached bytes over HTTP (get_sra_data's signed URL) into its own
// staging directory. There is no local toolkit repository for a bare
// accession to resolve against. Both fasterq-dump and fastq-dump's own
// --help documents a path as an equally valid first form ("fasterq-dump
// <path> [options]"), so BuildArgs passes the downloaded file's local path
// in the position sra_tools.py fills with a bare run_id -- same flag
// structure, the one substitution this tool's different download mechanism
// makes unavoidable.
package dumper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BV-BRC/sra_cache_service/internal/catalog"
	"github.com/BV-BRC/sra_cache_service/internal/sracacheclient"
)

// ResolveStagingDir implements standard Unix temp-file precedence for the
// directory p3-sra-dump buffers a downloaded .sra file in, and also hands to
// fasterq-dump as its own `-t`/`--temp` scratch directory (this dumper
// deliberately reuses one directory for both "ephemeral local scratch space"
// roles rather than inventing a second, separately configured one --
// PLAN-sra-precache-service.md section 2.2 only ever describes one staging
// directory): flag (if non-empty) wins, else $TMPDIR, else "/tmp" -- matching
// sra_import/lib/sra_tools.py:294-299's existing fallback for `-t` exactly
// (that code has always done the $TMPDIR-or-/tmp fallback; it simply never
// had a CLI override before --staging-dir added one).
func ResolveStagingDir(flag string) string {
	if flag != "" {
		return flag
	}
	if t := os.Getenv("TMPDIR"); t != "" {
		return t
	}
	return "/tmp"
}

// Tool is the dump-tool binary selected for one accession's platform, plus
// enough of the selection outcome (IsPacBio) for BuildArgs to reproduce
// sra_tools.py's branch exactly.
type Tool struct {
	// Bin is the executable to invoke: "fasterq-dump" or "fastq-dump" by
	// default (resolved via $PATH -- exec.Command's normal behavior --
	// matching sra_tools.py's own defaults), or binOverride
	// (cmd/p3-sra-dump's --bin / -p3-sra's --bin compatibility alias) when
	// one was given.
	Bin string
	// IsPacBio is true when this run's platform was PACBIO_SMRT.
	IsPacBio bool
}

// SelectTool reproduces sra_tools.py:294's platform branch exactly:
// PACBIO_SMRT goes to fastq-dump (no --split-files), everything else
// (ILLUMINA, OXFORD_NANOPORE, ION_TORRENT, "", ...) goes to fasterq-dump
// with --split-files. platform must be the raw NCBI PLATFORM tag from a
// prior SRACache.get_metadata call (metadata.SRAMetadata.Platform) -- taken
// from the SERVICE's response, never re-derived locally, per the
// implementation plan's explicit instruction.
//
// binOverride is cmd/p3-sra-dump's --bin flag: purely a path override for
// whichever tool platform selects (see this package's doc comment on --bin's
// resolved meaning here, which is deliberately simpler than
// sra_import/scripts/p3-sra.py's --bin -- that one only ever overrode the
// fasterq-dump path, never fastq-dump's hardcoded-literal PacBio branch).
// Empty means "use the default name for whichever tool was selected".
func SelectTool(platform, binOverride string) Tool {
	if platform == "PACBIO_SMRT" {
		bin := "fastq-dump"
		if binOverride != "" {
			bin = binOverride
		}
		return Tool{Bin: bin, IsPacBio: true}
	}
	bin := "fasterq-dump"
	if binOverride != "" {
		bin = binOverride
	}
	return Tool{Bin: bin, IsPacBio: false}
}

// BuildArgs returns the argv (excluding argv[0], the binary itself) for
// invoking tool against localSraPath (see this package's doc comment for why
// that's a local path rather than a bare accession).
//
//   - PacBio: `--outdir <outDir> [--fasta] [extraOpts...] <localSraPath>` --
//     no `-t`/`--temp` (fastq-dump has no such flag) and no `--split-files`,
//     matching sra_tools.py:295 exactly.
//   - otherwise: `-t <stagingDir> --outdir <outDir> --split-files -f [--fasta]
//     [extraOpts...] <localSraPath>`, matching sra_tools.py:300 exactly.
//
// fasta appends `--fasta` (both binaries support it per their own --help;
// see cmd/p3-sra-dump's --format flag doc for why this is real tool support,
// not a design-doc placeholder). extraOpts is the caller's
// --fasterq-dump-opts/--fastq-dump-opts passthrough, inserted before the
// final positional argument so it can still override anything set above it
// (both tools accept their own flags in any order; only the trailing
// path/accession argument is positionally significant).
func BuildArgs(tool Tool, stagingDir, outDir, localSraPath string, fasta bool, extraOpts []string) []string {
	var args []string
	if tool.IsPacBio {
		args = append(args, "--outdir", outDir)
	} else {
		args = append(args, "-t", stagingDir, "--outdir", outDir, "--split-files", "-f")
	}
	if fasta {
		args = append(args, "--fasta")
	}
	args = append(args, extraOpts...)
	args = append(args, localSraPath)
	return args
}

// RunDumpTool execs tool with args, connecting its combined stdout/stderr to
// the given writers. Broken out as its own function (rather than inlined in
// DumpAccession) so tests can drive it directly against a fake
// fasterq-dump/fastq-dump shell script on $PATH -- the same testability move
// Phase 6 used for `prefetch` (internal/fetcher/fetcher_test.go).
func RunDumpTool(ctx context.Context, tool Tool, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, tool.Bin, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("running %s %s: %w", tool.Bin, strings.Join(args, " "), err)
	}
	return nil
}

// GzipFastqFiles gzips every "*.fastq" file directly under outDir, matching
// sra_import/lib/sra_tools.py:310-315's own post-dump gzip step.
//
// Deliberate fix, not a silent behavior change: sra_tools.py's guard there is
// `if fq_files.count > 0`, which references (but never calls) list.count --
// a bound method object, which is always truthy in Python regardless of
// whether the list is empty. So the Python original actually runs a bare
// `gzip` with zero file arguments whenever a dump produced no .fastq files at
// all, which makes gzip read from stdin and block forever. This function
// instead treats "nothing to gzip" as a true no-op, which is what the
// original code's own intent obviously was (this mirrors the design doc's
// own precedent of noting sra_tools.py's stage-then-delete gap as "a
// deliberate improvement, not something to match" -- PLAN-sra-precache-service.md
// section 7.2).
func GzipFastqFiles(outDir string) error {
	matches, err := filepath.Glob(filepath.Join(outDir, "*.fastq"))
	if err != nil {
		return fmt.Errorf("dumper: globbing fastq files in %s: %w", outDir, err)
	}
	if len(matches) == 0 {
		return nil
	}
	cmd := exec.Command("gzip", matches...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("dumper: gzip failed: %w (%s)", err, out.String())
	}
	return nil
}

// DownloadFile GETs url and writes its body verbatim to destPath, returning
// the number of bytes written. httpClient may be nil (defaults to
// http.DefaultClient). A non-200 response is an error; the (possibly large)
// body is not buffered in memory first -- it streams straight to destPath via
// io.Copy.
func DownloadFile(ctx context.Context, httpClient *http.Client, url, destPath string) (int64, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, fmt.Errorf("dumper: building download request: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("dumper: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return 0, fmt.Errorf("dumper: downloading %s: HTTP %d: %s", url, resp.StatusCode, string(body))
	}

	f, err := os.Create(destPath)
	if err != nil {
		return 0, fmt.Errorf("dumper: creating %s: %w", destPath, err)
	}
	defer f.Close()

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return n, fmt.Errorf("dumper: writing %s: %w", destPath, err)
	}
	return n, nil
}

// DataGetter is the subset of *sracacheclient.Client DumpAccession/
// WaitForCached need. Declared as an interface so tests can inject a fake
// without a real jsonrpc.Server; *sracacheclient.Client satisfies it as-is.
type DataGetter interface {
	GetSRAData(ctx context.Context, accessions []string) ([]sracacheclient.SRADataLocation, error)
}

// WaitForCached polls SRACache.get_sra_data (via data) for accession until it
// reports catalog.StateCached, one of the other terminal-but-failed states
// (invalid_id/not_found/error), ctx is done, or timeout elapses (timeout <= 0
// means no deadline beyond ctx itself). Each call to get_sra_data implicitly
// enqueues the accession if it wasn't already queued/cached -- see
// internal/rpcapi/data.go's getSRAData doc comment -- so this alone is enough
// to trigger precaching; the caller never needs to call start_precache
// itself.
func WaitForCached(ctx context.Context, data DataGetter, accession string, pollInterval, timeout time.Duration) (sracacheclient.SRADataLocation, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if pollInterval <= 0 {
		pollInterval = 5 * time.Second
	}

	for {
		rows, err := data.GetSRAData(ctx, []string{accession})
		if err != nil {
			return sracacheclient.SRADataLocation{}, fmt.Errorf("get_sra_data(%s): %w", accession, err)
		}
		if len(rows) != 1 {
			return sracacheclient.SRADataLocation{}, fmt.Errorf("get_sra_data(%s): expected 1 row, got %d", accession, len(rows))
		}
		loc := rows[0]
		switch catalog.State(loc.State) {
		case catalog.StateCached:
			return loc, nil
		case catalog.StateInvalidID, catalog.StateNotFound, catalog.StateError:
			return loc, fmt.Errorf("accession %s: %s", accession, loc.State)
		}

		select {
		case <-ctx.Done():
			return loc, fmt.Errorf("timed out waiting for %s to be cached (last state %q): %w", accession, loc.State, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Options bundles the per-run knobs DumpAccession needs, resolved by the
// caller (cmd/p3-sra-dump/main.go) from cobra flags before use -- StagingDir
// is expected to already be the result of ResolveStagingDir, not a raw flag
// value.
type Options struct {
	StagingDir      string
	OutDir          string
	Bin             string // --bin override; empty = default per platform
	Fasta           bool   // --format fasta
	FasterqDumpOpts []string
	FastqDumpOpts   []string
	Gzip            bool
	PollInterval    time.Duration
	Timeout         time.Duration
}

// Result is DumpAccession's per-accession outcome.
type Result struct {
	Accession string
	// Files is the post-dump file listing -- basenames of everything under
	// OutDir matching "*<accession>*" -- see GlobDumpedFiles. Always
	// populated (DumpAccession is never called for --metaonly).
	Files []string
}

// DumpAccession is the full client-side dump pipeline for one run-level
// accession: wait for SRACache to report it cached (enqueueing it as a side
// effect if needed), download its .sra bytes into o.StagingDir, run the
// platform-selected dump tool against the download, optionally gzip the
// fastq output, and report the files it produced.
//
// platform is the raw NCBI PLATFORM tag from a prior get_metadata call (see
// SelectTool's doc comment). stdout/stderr receive the dump tool's own
// combined output for the caller to relay/log. httpClient may be nil.
//
// # Cleanup -- success only, defer-based, no signal handling
//
// The staged .sra file is removed via a `defer` registered ONLY after
// RunDumpTool has already returned successfully -- so a failure inside the
// dump tool itself leaves the staged file in place (useful for a retry or
// for post-mortem inspection; also exactly what this phase's tests assert:
// a simulated mid-dump failure must NOT clean up). This function installs no
// signal handler of its own; a process killed with SIGKILL mid-dump leaves
// the staged file behind regardless of how far execution got, and even a
// SIGTERM-driven context cancellation (cmd/p3-sra-dump wires one up, mirroring
// p3-sra-precache) only ever aborts the in-flight download/exec -- it never
// triggers cleanup on its own. This is the concrete mechanism behind the
// design doc's stated non-goal (PLAN-sra-precache-service.md section 7.1):
// best-effort cleanup on normal successful exit, no promise beyond that.
func DumpAccession(ctx context.Context, data DataGetter, httpClient *http.Client, accession, platform string, o Options, stdout, stderr io.Writer) (Result, error) {
	loc, err := WaitForCached(ctx, data, accession, o.PollInterval, o.Timeout)
	if err != nil {
		return Result{}, err
	}

	stagedPath := filepath.Join(o.StagingDir, accession+".sra")
	if _, err := DownloadFile(ctx, httpClient, loc.URL, stagedPath); err != nil {
		return Result{}, fmt.Errorf("downloading %s: %w", accession, err)
	}

	tool := SelectTool(platform, o.Bin)
	opts := o.FasterqDumpOpts
	if tool.IsPacBio {
		opts = o.FastqDumpOpts
	}
	args := BuildArgs(tool, o.StagingDir, o.OutDir, stagedPath, o.Fasta, opts)

	if err := RunDumpTool(ctx, tool, args, stdout, stderr); err != nil {
		// Deliberately no cleanup here -- see the doc comment above.
		return Result{}, fmt.Errorf("dumping %s: %w", accession, err)
	}
	defer func() {
		if rerr := os.Remove(stagedPath); rerr != nil && !os.IsNotExist(rerr) {
			fmt.Fprintf(stderr, "p3-sra-dump: warning: could not remove staged file %s: %v\n", stagedPath, rerr)
		}
	}()

	if o.Gzip {
		if err := GzipFastqFiles(o.OutDir); err != nil {
			return Result{}, fmt.Errorf("gzipping %s output: %w", accession, err)
		}
	}

	files, err := GlobDumpedFiles(o.OutDir, accession)
	if err != nil {
		return Result{}, err
	}
	return Result{Accession: accession, Files: files}, nil
}
