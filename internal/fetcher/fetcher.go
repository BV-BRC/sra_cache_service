// Package fetcher shells out to the SRA Toolkit's `prefetch` binary to pull
// one accession's .sra file into a private staging directory, classifies
// the result (classify.go), and exposes a bounded worker pool (pool.go)
// that drains the catalog's queued rows through it.
//
// See PLAN-sra-cache-service-implementation.md, Phase 6, and
// PLAN-sra-precache-service.md sections 3-4.
package fetcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Fetcher wraps the path to the `prefetch` binary and a root directory to
// stage downloads under before they're published into the cache.
type Fetcher struct {
	// PrefetchBin is the path to the prefetch executable (config key
	// prefetch-bin).
	PrefetchBin string
	// StagingDir is the parent of every per-fetch temporary directory Fetch
	// creates. It is created if missing on first use; typically a
	// subdirectory of the cache root so staging and the published cache
	// share a filesystem (irrelevant here, since Fetch never renames out of
	// it -- callers do their own publish -- but keeping it under the same
	// root as the cache keeps operators from needing to reason about a
	// second, unrelated disk).
	StagingDir string
}

// New builds a Fetcher. stagingDir is created (mkdir -p) if it does not
// already exist -- matching the rest of this service's "permissive about
// first-run state" philosophy (see internal/config's doc comment) rather
// than requiring an operator to pre-create it.
func New(prefetchBin, stagingDir string) (*Fetcher, error) {
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return nil, fmt.Errorf("fetcher: creating staging dir %s: %w", stagingDir, err)
	}
	return &Fetcher{PrefetchBin: prefetchBin, StagingDir: stagingDir}, nil
}

// PrefetchError is returned by Fetch when the prefetch subprocess itself
// did not produce a usable file -- either it exited non-zero, or it exited
// 0 but no downloaded file could be found (treated the same as a non-zero
// exit for classification purposes, with ExitCode left at 0 so a caller can
// still tell the two apart if it cares). Output is the captured combined
// stdout+stderr, in full (classify.Classify truncates it for a catalog
// message; PrefetchError keeps the whole thing for logging).
type PrefetchError struct {
	ExitCode int
	Output   string
	Err      error
}

func (e *PrefetchError) Error() string {
	return fmt.Sprintf("prefetch exited %d: %s", e.ExitCode, tail(e.Output, tailBytes))
}

func (e *PrefetchError) Unwrap() error { return e.Err }

// Fetch runs `prefetch --output-directory <staging-subdir> <accession>`
// (flags confirmed against the real v3.1.0 --help and a live successful
// download on 2026-09-28 -- see this package's doc comment on the
// long-vs-short flag choice below) into a fresh, private subdirectory of
// StagingDir, then -- only if prefetch produced a file -- calls consume
// with that file's local path and size while the staging directory still
// exists.
//
// The whole staging subdirectory (and everything prefetch put under it) is
// removed before Fetch returns, regardless of success or failure, so
// callers must do anything they need with the downloaded bytes (typically
// streaming them into cache.Cache.Publish) from inside consume, not after
// Fetch returns. This -- rather than the "return a localPath string"
// signature sketched in the design docs -- is Fetch's chosen shape: it
// keeps the staging directory's lifecycle entirely owned by the code that
// created it, so no caller can forget to clean one up, and no code path
// (including an early return on error) can leak one.
//
// Fetch returns consume's own error unchanged if prefetch succeeded and
// consume ran. If prefetch itself failed (non-zero exit, or exited 0 with
// no discoverable output file), Fetch returns a *PrefetchError and never
// calls consume at all.
//
// --output-directory (not the ambiguous -o, which prefetch's own --help
// binds to two different things -- -o|--order for kart mode and
// -o|--output-file for single-file mode -- while -O is the *documented*
// short form for --output-directory; the long flag name is unambiguous
// either way, so it's what's used here) is the one flag this needs beyond
// the accession itself: no --force (default "no" is correct since the
// staging directory is always empty), no --progress (nothing consumes
// prefetch's progress meter here), matching sra_tools.py's own bare
// `prefetch <run_ids>` invocation.
func (f *Fetcher) Fetch(ctx context.Context, accession string, consume func(path string, size int64) error) error {
	dir, err := os.MkdirTemp(f.StagingDir, sanitizeForTempName(accession)+"-*")
	if err != nil {
		return fmt.Errorf("fetcher: creating staging dir for %s: %w", accession, err)
	}
	defer os.RemoveAll(dir)

	cmd := exec.CommandContext(ctx, f.PrefetchBin, "--output-directory", dir, accession)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	output := out.String()
	if runErr != nil {
		return &PrefetchError{ExitCode: exitCodeOf(runErr), Output: output, Err: runErr}
	}

	path, err := findDownloadedFile(dir, accession)
	if err != nil {
		return &PrefetchError{ExitCode: 0, Output: output, Err: fmt.Errorf("prefetch exited 0 but %w", err)}
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("fetcher: stat %s after successful prefetch: %w", path, err)
	}

	return consume(path, info.Size())
}

// exitCodeOf extracts a process exit code from a subprocess error, or -1 if
// the process never got far enough to have one (e.g. the binary itself
// could not be found or executed).
func exitCodeOf(err error) int {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// findDownloadedFile locates the file prefetch wrote for accession under
// dir. Confirmed live (2026-09-28, prefetch 3.1.0): a successful
// `--output-directory dir accession` download lands at
// dir/accession/accession.sra. This globs a little wider than that exact
// name (accession*) and picks the largest non-lock, non-temp match rather
// than hardcoding ".sra", since NCBI's own SRA Lite vs. normalized-format
// preference (mentioned in prefetch's own banner text) could plausibly
// change the extension without changing this package's contract.
func findDownloadedFile(dir, accession string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(dir, accession, accession+"*"))
	if err != nil {
		return "", fmt.Errorf("globbing for downloaded file: %w", err)
	}

	var best string
	var bestSize int64
	for _, m := range matches {
		info, err := os.Stat(m)
		if err != nil || info.IsDir() {
			continue
		}
		base := filepath.Base(m)
		if strings.HasSuffix(base, ".lock") || strings.Contains(base, ".tmp") || strings.Contains(base, ".vdbcache") {
			continue
		}
		if info.Size() > bestSize {
			best = m
			bestSize = info.Size()
		}
	}
	if best == "" {
		return "", fmt.Errorf("no downloaded file found under %s", filepath.Join(dir, accession))
	}
	return best, nil
}

// sanitizeForTempName strips characters os.MkdirTemp's pattern argument
// can't safely carry (it treats "*" specially and a path separator would
// escape the intended parent directory) out of accession before using it as
// a directory-name prefix. Accession syntax (internal/accession.Validate)
// already rules out anything but letters and digits reaching here in
// production; this is defense in depth, not a validation layer.
func sanitizeForTempName(accession string) string {
	return strings.NewReplacer("*", "_", string(filepath.Separator), "_").Replace(accession)
}

// Probe runs `prefetch --version` and checks whether the running user has
// SRA Toolkit's first-run configuration file
// (~/.ncbi/user-settings.mkfg) -- a headless service account commonly
// lacks it (the toolkit normally writes it interactively on first use), and
// its absence can make prefetch prompt or behave unexpectedly rather than
// failing with a clear error. Per the design doc, this logs a loud warning
// rather than failing the probe (and never stops the service from
// starting) -- operators need to see it, but the eventual failure mode (if
// any) is better diagnosed from prefetch's own per-fetch error output than
// guessed at here.
func (f *Fetcher) Probe(ctx context.Context, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}

	cmd := exec.CommandContext(ctx, f.PrefetchBin, "--version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("fetcher: probing %q --version: %w (output: %s)", f.PrefetchBin, err, tail(string(out), tailBytes))
	}
	log.Info("prefetch probe succeeded", "bin", f.PrefetchBin, "output", strings.TrimSpace(string(out)))

	home, herr := os.UserHomeDir()
	if herr != nil {
		log.Warn("could not determine home directory to check for SRA Toolkit's user-settings.mkfg", "err", herr)
		return nil
	}

	settingsPath := filepath.Join(home, ".ncbi", "user-settings.mkfg")
	if _, statErr := os.Stat(settingsPath); statErr != nil {
		log.Warn("SRA Toolkit has no user-settings.mkfg for this account -- a headless service account commonly lacks the interactive first-run config; prefetch may prompt or misbehave until one is provisioned out of band (e.g. `vdb-config --interactive` once as this user, or copying a working config)",
			"path", settingsPath, "err", statErr)
		return nil
	}
	log.Info("SRA Toolkit user-settings.mkfg found", "path", settingsPath)
	return nil
}
