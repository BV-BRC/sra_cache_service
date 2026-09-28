package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTestINI(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "deploy.cfg")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("writing test ini: %v", err)
	}
	return path
}

func TestLoadBasics(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141
data-listen = :7142
log-level = debug
`)

	cfg, err := Load(path, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":7141" {
		t.Errorf("Listen = %q, want :7141", cfg.Listen)
	}
	if cfg.DataListen != ":7142" {
		t.Errorf("DataListen = %q, want :7142", cfg.DataListen)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
}

func TestLoadExplicitSection(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141

[SRACacheAlt]
listen = :9999
`)

	cfg, err := Load(path, "SRACacheAlt")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9999" {
		t.Errorf("Listen = %q, want :9999", cfg.Listen)
	}
}

func TestLoadNullConvention(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141
download-secret = null
`)

	cfg, err := Load(path, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Raw["download-secret"]; got != "" {
		t.Errorf("Raw[download-secret] = %q, want empty (the \"null\" convention)", got)
	}
}

func TestLoadMissingSection(t *testing.T) {
	path := writeTestINI(t, `
[SomeOtherService]
listen = :1
`)

	if _, err := Load(path, ""); err == nil {
		t.Fatal("Load: expected an error for a missing [SRACache] section, got nil")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "does-not-exist.cfg"), ""); err == nil {
		t.Fatal("Load: expected an error for a missing file, got nil")
	}
}

// TestLoadPhase6DefaultsApplyWhenKeysAbsent asserts the "permissive about
// first-run state" philosophy: a minimal [SRACache] section with none of the
// Phase 6 keys set still gets a fully usable Config, not zero values that
// would make the fetcher/pool reject everything as oversized or unable to
// find prefetch.
func TestLoadPhase6DefaultsApplyWhenKeysAbsent(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141
`)

	cfg, err := Load(path, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CacheRoot != DefaultCacheRoot {
		t.Errorf("CacheRoot = %q, want default %q", cfg.CacheRoot, DefaultCacheRoot)
	}
	if cfg.CatalogDB != DefaultCatalogDB {
		t.Errorf("CatalogDB = %q, want default %q", cfg.CatalogDB, DefaultCatalogDB)
	}
	if cfg.MaxCacheSize != DefaultMaxCacheSize {
		t.Errorf("MaxCacheSize = %d, want default %d", cfg.MaxCacheSize, DefaultMaxCacheSize)
	}
	if cfg.PrefetchBin != DefaultPrefetchBin {
		t.Errorf("PrefetchBin = %q, want default %q", cfg.PrefetchBin, DefaultPrefetchBin)
	}
	if cfg.InitialConcurrentPrefetch != DefaultInitialConcurrentPrefetch {
		t.Errorf("InitialConcurrentPrefetch = %d, want default %d", cfg.InitialConcurrentPrefetch, DefaultInitialConcurrentPrefetch)
	}
	if cfg.MaxConcurrentPrefetch != DefaultMaxConcurrentPrefetch {
		t.Errorf("MaxConcurrentPrefetch = %d, want default %d", cfg.MaxConcurrentPrefetch, DefaultMaxConcurrentPrefetch)
	}
}

// TestLoadPhase6ExplicitValues asserts explicit values override the
// defaults, including a suffixed max-cache-size.
func TestLoadPhase6ExplicitValues(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141
cache-root = /data/sra-cache
catalog-db = /data/sra-cache/catalog.sqlite
max-cache-size = 2TiB
prefetch-bin = /usr/local/bin/prefetch
initial-concurrent-prefetch = 3
max-concurrent-prefetch = 32
`)

	cfg, err := Load(path, "")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CacheRoot != "/data/sra-cache" {
		t.Errorf("CacheRoot = %q", cfg.CacheRoot)
	}
	if cfg.CatalogDB != "/data/sra-cache/catalog.sqlite" {
		t.Errorf("CatalogDB = %q", cfg.CatalogDB)
	}
	wantSize := int64(2) * 1024 * 1024 * 1024 * 1024
	if cfg.MaxCacheSize != wantSize {
		t.Errorf("MaxCacheSize = %d, want %d (2TiB)", cfg.MaxCacheSize, wantSize)
	}
	if cfg.PrefetchBin != "/usr/local/bin/prefetch" {
		t.Errorf("PrefetchBin = %q", cfg.PrefetchBin)
	}
	if cfg.InitialConcurrentPrefetch != 3 {
		t.Errorf("InitialConcurrentPrefetch = %d, want 3", cfg.InitialConcurrentPrefetch)
	}
	if cfg.MaxConcurrentPrefetch != 32 {
		t.Errorf("MaxConcurrentPrefetch = %d, want 32", cfg.MaxConcurrentPrefetch)
	}
}

// TestLoadMaxCacheSizeUnparseableFailsClearly is the "don't silently zero
// it" requirement: a present-but-garbage max-cache-size must be a load
// error, not a quietly-zeroed Config.
func TestLoadMaxCacheSizeUnparseableFailsClearly(t *testing.T) {
	path := writeTestINI(t, `
[SRACache]
listen = :7141
max-cache-size = banana
`)

	if _, err := Load(path, ""); err == nil {
		t.Fatal("Load: expected an error for max-cache-size = banana, got nil")
	}
}

// TestLoadConcurrencyUnparseableFailsClearly covers the same requirement for
// the two integer AIMD keys.
func TestLoadConcurrencyUnparseableFailsClearly(t *testing.T) {
	for _, key := range []string{"initial-concurrent-prefetch", "max-concurrent-prefetch"} {
		t.Run(key, func(t *testing.T) {
			path := writeTestINI(t, "[SRACache]\nlisten = :7141\n"+key+" = many\n")
			if _, err := Load(path, ""); err == nil {
				t.Fatalf("Load: expected an error for %s = many, got nil", key)
			}
		})
	}
}
