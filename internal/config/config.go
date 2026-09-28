// Package config reads the deployment INI shared with the rest of this
// codebase's BV-BRC services, for the sra-cache-server binary.
//
// parseINI is ported from Workspace/go/internal/wsconfig/wsconfig.go
// verbatim, including its "null" -> unset convention, so this service's
// config loading behaves identically to the Workspace Go code's. Full
// validation of the keys later phases add (cache root existence, a
// non-empty download-secret, etc.) is explicitly out of scope here -- see
// PLAN-sra-cache-service-implementation.md, Phase 1 -- this package only
// has to load the [SRACache] section generically.
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/dustin/go-humanize"
)

// DefaultSection is the section name used when neither --section nor
// $KB_SERVICE_NAME is set, matching this service's name in the spec/Makefile.
const DefaultSection = "SRACache"

// Phase 6 defaults. These exist so a minimal (or missing-key) [SRACache]
// section still boots a usable service -- matching Phase 1's stated
// philosophy of being permissive about first-run state (see this package's
// doc comment) -- rather than leaving a zero value that would make the
// fetcher/pool immediately reject every accession as oversized (MaxCacheSize
// == 0) or fail to find `prefetch` at all. Full validation (e.g. refusing to
// start on a nonsensical value) is Phase 10's job, not this one's.
const (
	// DefaultCacheRoot is where the on-disk .sra cache lives if cache-root
	// is unset. A real deployment.cfg is expected to override this to a
	// real local-SSD path (see PLAN-sra-precache-service.md section 2.1 --
	// the cache must never live on the shared NetApp); this default only
	// has to be *somewhere* creatable for local dev/testing.
	DefaultCacheRoot = "/tmp/sra-cache/cache"
	// DefaultCatalogDB is the sqlite catalog file's path if catalog-db is
	// unset.
	DefaultCatalogDB = "/tmp/sra-cache/catalog.sqlite"
	// DefaultMaxCacheSize is the cache size ceiling if max-cache-size is
	// unset or empty -- 100 GiB, an arbitrary but non-zero starting point
	// for local dev/testing; a real deployment.cfg should set this
	// explicitly.
	DefaultMaxCacheSize int64 = 100 * humanize.GByte
	// DefaultPrefetchBin is the prefetch binary's path if prefetch-bin is
	// unset, matching where it actually lives on this site's hosts (it is
	// not on PATH by default).
	DefaultPrefetchBin = "/vol/patric3/cli/ubuntu-runtime/bin/prefetch"
	// DefaultInitialConcurrentPrefetch is the AIMD limiter's starting
	// concurrency if initial-concurrent-prefetch is unset -- "2-4, an
	// unvalidated starting guess" per PLAN-sra-precache-service.md section 4.
	DefaultInitialConcurrentPrefetch = 2
	// DefaultMaxConcurrentPrefetch is the AIMD limiter's hard ceiling if
	// max-concurrent-prefetch is unset.
	DefaultMaxConcurrentPrefetch = 16
)

// Config holds the deployment settings this service reads out of its
// section of the deployment INI. Phase 7/10 add
// data-plane-base-url/download-lifetime/download-secret/ncbi-api-key fields
// here as those pieces get built.
type Config struct {
	// Listen is the control-plane (JSONRPC) listen address, e.g. ":7141".
	Listen string

	// DataListen is the data-plane (plain HTTP) listen address, e.g. ":7142".
	DataListen string

	// LogLevel is one of debug/info/warn/error.
	LogLevel string

	// CacheRoot is the on-disk root of the .sra cache (internal/cache.New's
	// root argument) -- expected to be local SSD, never the shared NetApp
	// (PLAN-sra-precache-service.md section 2.1).
	CacheRoot string

	// CatalogDB is the sqlite catalog file's path (internal/catalog.Open's
	// path argument).
	CatalogDB string

	// MaxCacheSize is the cache size ceiling in bytes, parsed from
	// max-cache-size via humanize.ParseBytes -- so both a bare integer
	// ("500000000000") and a suffixed value ("500G", "1.5TiB") are
	// accepted; there is no suffix convention mandated anywhere in the
	// design docs, and ParseBytes happens to already be a transitive
	// dependency (pulled in by modernc.org/sqlite), so it costs nothing
	// extra to depend on directly.
	MaxCacheSize int64

	// PrefetchBin is the path to the `prefetch` binary the fetcher shells
	// out to.
	PrefetchBin string

	// InitialConcurrentPrefetch is the AIMD limiter's starting concurrency.
	InitialConcurrentPrefetch int

	// MaxConcurrentPrefetch is the AIMD limiter's hard operator-set
	// ceiling-on-the-ceiling (PLAN-sra-precache-service.md section 4) --
	// the adaptive controller can never exceed this, though it may run
	// below it.
	MaxConcurrentPrefetch int

	// Raw holds every key/value pair read from the section, so a later
	// phase can start consuming a new deploy.cfg key without changing
	// Load's signature.
	Raw map[string]string
}

// Load reads path and extracts the given section. An empty section means
// DefaultSection. Keys absent from the section come back as the zero value
// ("") -- callers apply their own defaults (e.g. from flags) on top, the
// same division of labor ws-download uses with wsconfig.Load -- EXCEPT for
// the Phase 6 fields above, which get the package-level defaults applied
// right here, because (unlike Listen/DataListen/LogLevel) there is no flag
// layer above this for them to fall back to; main.go has nothing else to
// default them from.
//
// A present-but-unparseable value (e.g. max-cache-size = banana) is a hard
// error -- Load must never silently coerce a bad value to zero.
func Load(path, section string) (*Config, error) {
	if section == "" {
		section = DefaultSection
	}

	raw, err := parseINI(path)
	if err != nil {
		return nil, err
	}
	vals := raw[section]
	if vals == nil {
		return nil, fmt.Errorf("config: no [%s] section in %s", section, path)
	}

	cfg := &Config{
		Listen:      vals["listen"],
		DataListen:  vals["data-listen"],
		LogLevel:    vals["log-level"],
		CacheRoot:   vals["cache-root"],
		CatalogDB:   vals["catalog-db"],
		PrefetchBin: vals["prefetch-bin"],
		Raw:         vals,
	}
	if cfg.CacheRoot == "" {
		cfg.CacheRoot = DefaultCacheRoot
	}
	if cfg.CatalogDB == "" {
		cfg.CatalogDB = DefaultCatalogDB
	}
	if cfg.PrefetchBin == "" {
		cfg.PrefetchBin = DefaultPrefetchBin
	}

	if v := vals["max-cache-size"]; v != "" {
		sz, err := humanize.ParseBytes(v)
		if err != nil {
			return nil, fmt.Errorf("config: %s: max-cache-size %q: %w", path, v, err)
		}
		cfg.MaxCacheSize = int64(sz)
	} else {
		cfg.MaxCacheSize = DefaultMaxCacheSize
	}

	cfg.InitialConcurrentPrefetch, err = intOrDefault(vals, "initial-concurrent-prefetch", DefaultInitialConcurrentPrefetch)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}
	cfg.MaxConcurrentPrefetch, err = intOrDefault(vals, "max-concurrent-prefetch", DefaultMaxConcurrentPrefetch)
	if err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	return cfg, nil
}

// intOrDefault parses vals[key] as a decimal integer, returning def if the
// key is absent/empty, or an error (never a silent zero) if it is present
// but not a valid integer.
func intOrDefault(vals map[string]string, key string, def int) (int, error) {
	v := vals[key]
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s %q: %w", key, v, err)
	}
	return n, nil
}

// parseINI reads a Config::Simple-style INI: [section] headers, key = value
// pairs, and ';' or '#' comments. It applies two Perl-side conventions: the
// literal string "null" means unset, and an empty value means unset.
//
// Ported verbatim (behavior-for-behavior) from
// Workspace/go/internal/wsconfig/wsconfig.go's parseINI.
func parseINI(path string) (map[string]map[string]string, error) {
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	defer fh.Close()

	out := map[string]map[string]string{}
	section := ""

	sc := bufio.NewScanner(fh)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			if out[section] == nil {
				out[section] = map[string]string{}
			}
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("config: %s:%d: expected 'key = value', got %q", path, lineNo, line)
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if val == "null" {
			// Perl maps the literal "null" to undef; deploy.cfg relies on
			// this convention for keys that are intentionally unset.
			val = ""
		}
		if section == "" {
			return nil, fmt.Errorf("config: %s:%d: key %q outside any section", path, lineNo, key)
		}
		out[section][key] = val
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("config: reading %s: %w", path, err)
	}
	return out, nil
}
