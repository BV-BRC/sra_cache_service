module github.com/BV-BRC/sra_cache_service

go 1.24.0

// TEMPORARY: BV-BRC-Go-SDK's sra-cache prerequisites (extended sra.Record /
// LookupAll, promoted p3auth) live on an unmerged branch. Point at the local
// worktree so development here isn't blocked on that PR landing. This
// replace MUST be removed, and the require below pinned to a real released
// SDK version, before any Phase 10 release build -- see
// PLAN-sra-cache-service-implementation.md, "Cross-cutting conventions".
replace github.com/BV-BRC/BV-BRC-Go-SDK => ../BV-BRC-Go-SDK-sra-prereqs

require (
	// Not yet imported by any package in this phase -- see the replace
	// comment above. Phase 4's internal/metadata will import
	// BV-BRC-Go-SDK/sra directly.
	github.com/BV-BRC/BV-BRC-Go-SDK v0.0.0-20260902193530-3defe848dae6
	github.com/spf13/pflag v1.0.5
	// Not yet imported by any package -- Phase 5's internal/catalog is the
	// first real user. Pulled in now so go.sum is settled ahead of that
	// phase, per the implementation plan.
	modernc.org/sqlite v1.45.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546 // indirect
	golang.org/x/sys v0.40.0 // indirect
	modernc.org/libc v1.67.6 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.11.0 // indirect
)
