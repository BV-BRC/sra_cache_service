// Package accession implements SRA/ENA/DRA accession syntax validation, for
// SRACache.get_metadata/start_precache/get_sra_data to reject a malformed id
// cheaply -- before ever making a round trip to NCBI.
//
// The prefix set matches sra_import/scripts/p3-sra.py's acceptable_prefixes
// exactly (line 27). p3-sra.py itself validates *only* the prefix
// (str.startswith, no digit or length check at all); the digit-count bound
// here is new, additive tightening on top of that, not a narrowing of
// something callers already relied on being loose -- see Validate's doc
// comment for why the bound is deliberately generous.
package accession

import (
	"fmt"
	"regexp"
)

// Prefixes are the accession prefixes p3-sra.py accepts: SRA's own
// (SRX/SRP/SRR), DDBJ's (DRX/DRP/DRR), and ENA's (ERR/ERX/ERP).
var Prefixes = []string{"SRX", "SRP", "SRR", "DRX", "DRP", "DRR", "ERR", "ERX", "ERP"}

// pattern requires one of Prefixes followed by 6-12 digits. The bound is
// deliberately generous: NCBI has extended accession length over time (SRR
// accessions started at 6 digits and are now commonly 9+), so a tight bound
// risks rejecting a legitimately longer accession NCBI issues later. This
// check exists purely to reject obvious garbage fast, not to police NCBI's
// namespace.
var pattern = regexp.MustCompile(`^(?:SRX|SRP|SRR|DRX|DRP|DRR|ERR|ERX|ERP)[0-9]{6,12}$`)

// Validate reports whether acc has a recognized accession prefix followed
// by a plausible run of digits. Its error message is safe to surface
// directly to a caller (e.g. as a get_metadata result row's
// extra["message"]).
func Validate(acc string) error {
	if !pattern.MatchString(acc) {
		return fmt.Errorf("accession %q: must start with one of %v followed by 6-12 digits", acc, Prefixes)
	}
	return nil
}

// IsRunLevel reports whether acc names a run (SRR/DRR/ERR) as opposed to an
// experiment (SRX/DRX/ERX) or study/project (SRP/DRP/ERP) accession --
// start_precache and get_sra_data require run-level accessions only (see
// PLAN-sra-precache-service.md section 6). This checks only the prefix, not
// the full syntax; call Validate first if digit validity also matters.
func IsRunLevel(acc string) bool {
	if len(acc) < 3 {
		return false
	}
	switch acc[:3] {
	case "SRR", "DRR", "ERR":
		return true
	default:
		return false
	}
}
