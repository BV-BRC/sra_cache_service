module github.com/BV-BRC/sra_cache_service

go 1.24.0

require (
	// Imported for sra (internal/metadata), auth and version (the two CLIs,
	// internal/dumper, internal/rpcclient). Not yet for p3auth -- that is
	// what the NoopValidator TODO in cmd/sra-cache-server/main.go is about;
	// the package is available here, just not wired up.
	//
	// This was a local-path replace against a worktree until 2026-10-20,
	// because the two prerequisite branches were unmerged and because no
	// release tag could be resolved from Go at all: the module was declared
	// without a /v2 suffix while every tag was v2.x, so the proxy rejected
	// them. Both are fixed -- the prerequisites merged (BV-BRC-Go-SDK#18,
	// #19) and the module was renamed (#17) -- so this is now an ordinary
	// pin to the first release that can be named.
	github.com/BV-BRC/BV-BRC-Go-SDK/v2 v2.0.17
	github.com/spf13/pflag v1.0.9
	// Not yet imported by any package -- Phase 5's internal/catalog is the
	// first real user. Pulled in now so go.sum is settled ahead of that
	// phase, per the implementation plan.
	modernc.org/sqlite v1.45.0
)

// Phase 6: internal/config uses humanize.ParseBytes for max-cache-size.
// Already a transitive dependency of modernc.org/sqlite (indirect below
// would otherwise cover it), promoted to direct now that a package in this
// module imports it itself.
require github.com/dustin/go-humanize v1.0.1

require github.com/spf13/cobra v1.10.2

require (
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sys v0.40.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
