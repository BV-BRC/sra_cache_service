// Command p3-sra-dump is a drop-in replacement for
// sra_import/scripts/p3-sra.py, built on top of the SRACache service
// (SRACache.get_metadata / SRACache.get_sra_data) instead of running
// `prefetch`/`fasterq-dump` against NCBI directly. See
// PLAN-sra-cache-service-implementation.md's Phase 9 and
// PLAN-sra-precache-service.md sections 2.2 and 7/7.1/7.2 for the design
// this implements, and internal/dumper for the actual download/dump-tool
// pipeline.
//
// Unlike p3-sra-precache, this command never requires an auth token:
// SRACache.get_metadata and SRACache.get_sra_data are both `authentication
// optional`, so an anonymous caller can dump any public SRA accession,
// matching p3-sra.py's own total lack of an auth concept. A token is sent
// (raw, no "Bearer" prefix) when one is available from the usual sources
// (see BV-BRC-Go-SDK/auth.GetToken), but its absence is never an error here.
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
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	sdkauth "github.com/BV-BRC/BV-BRC-Go-SDK/auth"
	"github.com/BV-BRC/BV-BRC-Go-SDK/version"
	"github.com/BV-BRC/sra_cache_service/internal/dumper"
	"github.com/BV-BRC/sra_cache_service/internal/metadata"
	"github.com/BV-BRC/sra_cache_service/internal/rpcclient"
	"github.com/BV-BRC/sra_cache_service/internal/sracacheclient"
)

// cliProduct matches p3-sra-precache's own identity -- both are the same
// "SRACache CLI tooling" product from a User-Agent standpoint, not two
// separate products.
const cliProduct = "bvbrc-sra-cache-cli"

// defaultURL mirrors p3-sra-precache/main.go's own placeholder -- see that
// file's doc comment: SRACache has no real deployment yet
// (PLAN-sra-cache-service-implementation.md's Phase 10).
const defaultURL = "http://localhost:7141"

func main() {
	version.SetProduct(cliProduct)

	if err := newRootCmd().Execute(); err != nil {
		var ec *exitCodeError
		if errors.As(err, &ec) {
			if ec.err != nil {
				fmt.Fprintln(os.Stderr, "p3-sra-dump:", ec.err)
			}
			os.Exit(ec.code)
		}
		fmt.Fprintln(os.Stderr, "p3-sra-dump:", err)
		os.Exit(1)
	}
}

// exitCodeError carries the process exit code a failure should produce,
// mirroring cmd/p3-sra-precache/main.go's own type exactly (deliberately
// duplicated rather than shared -- see that file's package doc comment on
// internal/cliroot for the same "cheap to duplicate, nothing to drift"
// reasoning; this type has even less shared behavior than cliroot did).
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

// dumpConfig bundles every flag runDump needs, decoupled from cobra so
// runDump can be unit-tested directly (see main_test.go).
type dumpConfig struct {
	OutDir          string
	StagingDir      string // raw --staging-dir flag value; dumper.ResolveStagingDir applies precedence
	Bin             string
	Format          string
	FasterqDumpOpts string // whitespace-separated passthrough string, split by runDump
	FastqDumpOpts   string
	Gzip            bool
	MetaOnly        bool
	MetadataFile    string
	SRAMetadataFile string
	PollInterval    time.Duration
	Timeout         time.Duration
}

// client is the subset of *sracacheclient.Client this command needs,
// declared as an interface (mirroring p3-sra-precache/main.go's own `client`
// interface) so tests can inject a fake without a real jsonrpc.Server.
// *sracacheclient.Client satisfies it as-is, and it also satisfies
// dumper.DataGetter structurally (same GetSRAData signature), so it can be
// passed directly to dumper.DumpAccession/WaitForCached with no adapter.
type client interface {
	GetMetadata(ctx context.Context, accessions []string) ([]metadata.SRAMetadata, error)
	GetSRAData(ctx context.Context, accessions []string) ([]sracacheclient.SRADataLocation, error)
}

func newRootCmd() *cobra.Command {
	var (
		urlFlag         string
		format          string
		fasterqDumpOpts string
		fastqDumpOpts   string
		stagingDir      string
		outDir          string
		bin             string
		idFlag          string
		outAlias        string
		metaOnly        bool
		metadataFile    string
		sraMetadataFile string
		gzipFlag        bool
		pollInterval    time.Duration
		timeout         time.Duration
		debugHTTP       bool
	)

	cmd := &cobra.Command{
		Use:   "p3-sra-dump [flags] <accession>...",
		Short: "Fetch and unpack SRA run data via the SRACache service (p3-sra drop-in)",
		Long: `p3-sra-dump ensures one or more run-level SRA accessions
(SRR/DRR/ERR -- SRX/SRP experiment/study accessions are expanded
automatically via SRACache.get_metadata) are cached by the SRACache service,
downloads each one's .sra bytes, and runs fasterq-dump (or fastq-dump, for
PACBIO_SMRT runs) against them -- a drop-in replacement for
sra_import/scripts/p3-sra.py.

It never runs prefetch locally: the SRACache service does the actual NCBI
fetch, and this tool only streams the already-cached bytes over HTTP.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			accessions := resolveAccessions(args, idFlag)

			resolvedURL := urlFlag
			if resolvedURL == "" {
				resolvedURL = os.Getenv("SRA_CACHE_URL")
			}
			if resolvedURL == "" {
				resolvedURL = defaultURL
			}

			// Deliberately GetToken, not RequireToken: see this file's
			// package doc comment -- both RPC methods this tool calls are
			// AuthOptional, so a missing/absent token is never an error.
			var tokenRaw string
			if tok, terr := sdkauth.GetToken(); terr == nil && tok != nil {
				tokenRaw = tok.Raw
			}

			httpClient := &http.Client{Transport: &debugTransport{enabled: debugHTTP, out: os.Stderr}}
			rpc := &rpcclient.Client{URL: resolvedURL, Token: tokenRaw, HTTPClient: httpClient}
			c := sracacheclient.New(rpc)

			resolvedOutDir := resolveOutDir(outDir, outAlias)

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			cfg := dumpConfig{
				OutDir:          resolvedOutDir,
				StagingDir:      stagingDir,
				Bin:             bin,
				Format:          format,
				FasterqDumpOpts: fasterqDumpOpts,
				FastqDumpOpts:   fastqDumpOpts,
				Gzip:            gzipFlag,
				MetaOnly:        metaOnly,
				MetadataFile:    metadataFile,
				SRAMetadataFile: sraMetadataFile,
				PollInterval:    pollInterval,
				Timeout:         timeout,
			}

			return runDump(ctx, cmd.OutOrStdout(), cmd.ErrOrStderr(), c, httpClient, accessions, cfg)
		},
	}

	cmd.Flags().StringVar(&urlFlag, "url", "",
		fmt.Sprintf("SRACache control-plane base URL (default %q; overridable via $SRA_CACHE_URL)", defaultURL))
	cmd.Flags().StringVar(&format, "format", "fastq",
		`Output format: "fastq" (default) or "fasta". Both fasterq-dump and
fastq-dump support a real --fasta flag (confirmed against their own --help,
not assumed from sra_tools.py, which never uses it) -- this passes it
through; it is not a no-op.`)
	cmd.Flags().StringVar(&fasterqDumpOpts, "fasterq-dump-opts", "",
		"extra options to pass to fasterq-dump, whitespace-separated and passed through opaquely (used for every accession except PACBIO_SMRT ones)")
	cmd.Flags().StringVar(&fastqDumpOpts, "fastq-dump-opts", "",
		"extra options to pass to fastq-dump, whitespace-separated and passed through opaquely (used only for PACBIO_SMRT accessions)")
	cmd.Flags().StringVar(&stagingDir, "staging-dir", "", stagingDirHelp)
	cmd.Flags().StringVar(&outDir, "outdir", "", "output directory for dumped fastq files (required unless --metaonly)")
	cmd.Flags().StringVar(&bin, "bin", "",
		`p3-sra compatibility alias: path override for whichever dump tool
platform selection picks (fasterq-dump or fastq-dump) -- purely a path
override, unlike sra_import/scripts/p3-sra.py's --bin, which only ever
overrode the fasterq-dump path (fastq-dump's PacBio branch was always a
hardcoded literal there). Tool *selection* here always stays automatic,
based on the accession's platform.`)
	cmd.Flags().StringVar(&idFlag, "id", "",
		"p3-sra compatibility alias: a single accession, equivalent to one positional argument (may be combined with positional args)")
	cmd.Flags().StringVar(&outAlias, "out", "", "p3-sra compatibility alias for --outdir")
	cmd.Flags().BoolVar(&metaOnly, "metaonly", false, "skip downloading/dumping entirely; only resolve and write metadata")
	cmd.Flags().StringVar(&metadataFile, "metadata-file", "", "write p3-sra-compatible parsed metadata (JSON) to this file")
	cmd.Flags().StringVar(&sraMetadataFile, "sra-metadata-file", "",
		"fetch the raw NCBI SRA docset XML for the requested accessions directly from eutils and write it to this file (provenance only; nothing in this codebase parses it back)")
	cmd.Flags().BoolVar(&gzipFlag, "gzip", false, "gzip the dumped fastq files after a successful dump")
	cmd.Flags().DurationVar(&pollInterval, "poll-interval", 5*time.Second, "how often to poll get_sra_data while waiting for an accession to be cached")
	cmd.Flags().DurationVar(&timeout, "timeout", 30*time.Minute, "give up waiting for an accession to be cached after this long (0 = wait indefinitely)")
	cmd.Flags().BoolVar(&debugHTTP, "debug-http", false,
		"dump request/response headers for a failed or non-200 RPC call (a minimal, local equivalent of BV-BRC-Go-SDK's P3_DEBUG_HTTP -- see cmd/p3-sra-precache's identical flag)")

	registerVersion(cmd)

	return cmd
}

// stagingDirHelp is PLAN-sra-precache-service.md section 7.1's drafted
// --staging-dir help text, used verbatim: it still reads correctly in this
// command's context (the design doc wrote it specifically for p3-sra-dump).
const stagingDirHelp = `Directory to buffer the .sra file in before unpacking
it. Deleted automatically once the unpack completes. Default: $TMPDIR, or
/tmp if unset.

Pointing this at /dev/shm can speed up large dumps by keeping the
random-access unpack step in memory instead of on disk, but it comes out of
your job's memory allocation, not disk quota -- size your job for ".sra size
+ normal memory use" if you do. Under Slurm, files here are not freed
automatically if the job is killed; they are cleaned up on normal exit only.`

// resolveAccessions combines positional args with the --id compatibility
// alias, per this phase's requirement to support both an existing caller's
// p3-sra-style "--id ONE_ACCESSION" invocation verbatim and the new
// multi-accession positional form. id is appended after any positional args
// (order is otherwise not load-bearing -- every accession gets its own
// independent result row downstream).
func resolveAccessions(args []string, id string) []string {
	if id == "" {
		return args
	}
	if len(args) == 0 {
		return []string{id}
	}
	return append(append([]string{}, args...), id)
}

// resolveOutDir applies --out's compatibility-alias precedence: --outdir
// wins if given, else --out, else empty (runDump then requires --metaonly).
func resolveOutDir(outDir, outAlias string) string {
	if outDir != "" {
		return outDir
	}
	return outAlias
}

// splitOpts splits a whitespace-separated passthrough options string into
// argv-style fields. An empty string yields a nil (not empty-non-nil) slice,
// so BuildArgs' `append(args, extraOpts...)` is a no-op either way.
func splitOpts(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return strings.Fields(s)
}

// runDump is this command's core logic, decoupled from cobra so it can be
// unit-tested with a fake client and fake dump-tool scripts on $PATH (see
// main_test.go). It mirrors sra_import/scripts/p3-sra.py's own top-level
// flow (download_sra_data): resolve metadata first (always), optionally
// fetch the raw SRA XML, then -- unless --metaonly -- dump+gzip each
// accession, finally writing --metadata-file if requested.
//
// Every accession is attempted independently: one accession's get_metadata
// rejection (invalid_id/not_found) or dump failure is reported to stderr and
// makes the overall exit code nonzero, but never stops the rest of the batch
// -- matching the same "one bad accession must never fail the whole call"
// principle SRACache.get_metadata itself follows.
func runDump(ctx context.Context, stdout, stderr io.Writer, c client, httpClient *http.Client, accessions []string, cfg dumpConfig) error {
	if len(accessions) == 0 {
		return &exitCodeError{code: 1, err: fmt.Errorf("at least one accession is required (positional argument or --id)")}
	}
	if cfg.Format != "fastq" && cfg.Format != "fasta" {
		return &exitCodeError{code: 1, err: fmt.Errorf("--format must be \"fastq\" or \"fasta\", got %q", cfg.Format)}
	}
	// Mirrors sra_import/scripts/p3-sra.py:23-24 exactly: an output
	// directory is required unless --metaonly skips the dump step entirely.
	if !cfg.MetaOnly && cfg.OutDir == "" {
		return &exitCodeError{code: 1, err: fmt.Errorf("output directory must be specified (--outdir/--out)")}
	}

	metaRows, err := c.GetMetadata(ctx, accessions)
	if err != nil {
		return &exitCodeError{code: 1, err: fmt.Errorf("get_metadata: %w", err)}
	}

	if cfg.SRAMetadataFile != "" {
		// Best-effort: a provenance-only fetch failing must not abort an
		// otherwise-successful dump (see internal/dumper/sra_xml.go's doc
		// comment on why this is a separate, independent eutils call).
		if xmlBytes, xerr := dumper.FetchRawSRAXML(ctx, httpClient, accessions); xerr != nil {
			fmt.Fprintf(stderr, "p3-sra-dump: warning: could not fetch raw SRA XML: %v\n", xerr)
		} else if werr := os.WriteFile(cfg.SRAMetadataFile, xmlBytes, 0o644); werr != nil {
			fmt.Fprintf(stderr, "p3-sra-dump: warning: could not write %s: %v\n", cfg.SRAMetadataFile, werr)
		}
	}

	filesByAccession := map[string][]string{}
	failed := false

	if !cfg.MetaOnly {
		stagingDir := dumper.ResolveStagingDir(cfg.StagingDir)
		if err := os.MkdirAll(stagingDir, 0o755); err != nil {
			return &exitCodeError{code: 1, err: fmt.Errorf("creating staging dir %s: %w", stagingDir, err)}
		}
		if err := os.MkdirAll(cfg.OutDir, 0o755); err != nil {
			return &exitCodeError{code: 1, err: fmt.Errorf("creating output dir %s: %w", cfg.OutDir, err)}
		}

		opts := dumper.Options{
			StagingDir:      stagingDir,
			OutDir:          cfg.OutDir,
			Bin:             cfg.Bin,
			Fasta:           cfg.Format == "fasta",
			FasterqDumpOpts: splitOpts(cfg.FasterqDumpOpts),
			FastqDumpOpts:   splitOpts(cfg.FastqDumpOpts),
			Gzip:            cfg.Gzip,
			PollInterval:    cfg.PollInterval,
			Timeout:         cfg.Timeout,
		}

		for _, row := range metaRows {
			if row.Extra["error"] != "" {
				fmt.Fprintf(stderr, "p3-sra-dump: skipping %s: %s\n", row.Accession, row.Extra["message"])
				failed = true
				continue
			}
			res, derr := dumper.DumpAccession(ctx, c, httpClient, row.Accession, row.Platform, opts, stdout, stderr)
			if derr != nil {
				fmt.Fprintf(stderr, "p3-sra-dump: %s: %v\n", row.Accession, derr)
				failed = true
				continue
			}
			filesByAccession[res.Accession] = res.Files
		}
	}

	if cfg.MetadataFile != "" {
		records := dumper.BuildMetaRecords(metaRows, filesByAccession)
		if werr := dumper.WriteMetadataFile(cfg.MetadataFile, records); werr != nil {
			return &exitCodeError{code: 1, err: werr}
		}
	}

	if failed {
		return &exitCodeError{code: 1}
	}
	return nil
}

// versionAnnotation/registerVersion mirror cmd/p3-sra-precache/main.go's own
// implementation exactly (see that file's doc comments) -- duplicated for
// the same "cheap, nothing to drift" reason as exitCodeError above.
const versionAnnotation = "sracache.build"

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
