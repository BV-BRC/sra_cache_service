// Package metadata implements SRACache.get_metadata's field mapping: NCBI
// SRA records (via BV-BRC-Go-SDK/sra) flattened to run-level rows matching
// SRACache.spec's sra_metadata typedef, and matching (for the fields that
// typedef declares) what sra_import/lib/sra_tools.py's
// parse_accession_metadata already produces for the same accessions -- that
// function is this package's compatibility contract, not a fresh design.
package metadata

import (
	"context"
	"strconv"

	"github.com/BV-BRC/BV-BRC-Go-SDK/sra"
)

// SRAMetadata is one output row. Field names and JSON tags match
// SRACache.spec's sra_metadata typedef exactly, so a value of this type can
// be returned as-is in the RPC response.
//
// A row normally describes one run. It can also stand in for an accession
// that produced no run data at all (invalid syntax, or well-formed but
// unknown to NCBI) -- the typedef has no dedicated error/state field (unlike
// precache_status), so that case is signaled via Extra["error"]
// ("invalid_id" or "not_found") with Extra["message"] carrying detail, and
// every typed field left at its zero value. internal/rpcapi/metadata.go
// builds those rows; this package only ever returns rows for accessions it
// actually found data for (see Lookup).
type SRAMetadata struct {
	// Accession is the run's own accession (SRR/DRR/ERR) -- NOT the
	// accession as requested, when that named an experiment or study.
	// This matches sra_tools.py:120 (`rdata['accession'] = rdata['run_id']`),
	// which always reports the run id regardless of what accession_id the
	// caller passed in. For an error row built by internal/rpcapi (see
	// above), there is no run, so Accession there instead echoes the
	// accession as requested -- that's the only sense in which "the
	// accession as requested" ever appears in this field.
	Accession string `json:"accession"`
	// Platform is NCBI's raw PLATFORM tag (e.g. "ILLUMINA", "PACBIO_SMRT"),
	// taken verbatim from sra.Record.Platform -- never canonicalized. See
	// SRACache.spec's sra_metadata doc comment for why: ReadSet.pm and
	// fastq_utils.py already maintain two different, disagreeing
	// platform-name maps, and this deliberately doesn't add a third.
	Platform string `json:"platform"`
	// LibraryLayout is "PAIRED" or "SINGLE" (rarely "", see
	// sra.Record.LibraryLayout's doc comment), taken verbatim from the SDK
	// record.
	LibraryLayout string `json:"library_layout"`
	// NReads reproduces sra_tools.py's exact computation, including its
	// PAIRED->2/else->1 fallback when the run's own Statistics didn't
	// establish a count -- see nReads below.
	NReads int `json:"n_reads"`
	// SizeBytes is the run's size in bytes (sra_tools.py's "size", renamed
	// per the design doc).
	SizeBytes int64 `json:"size_bytes"`
	// Extra carries every other field sra_tools.parse_accession_metadata
	// computes that this package can reproduce from sra.Record/RunRecord
	// (experiment/study accessions and titles, total_bases, total_spots,
	// read_length), stringified, plus (for an error row) "error"/"message".
	// Values only ever come from BV-BRC-Go-SDK/sra's parsed fields --
	// parse_accession_metadata also computes several fields that package
	// does not parse from the XML at all (instrument_model,
	// design_description, library_name/selection/strategy, sample_*,
	// study_<db>_ids, estimated_size); those are not reproduced here. See
	// this package's doc comment.
	Extra map[string]string `json:"extra"`
}

// Client is the subset of *sra.Client this package needs. Declared as an
// interface, rather than depending on *sra.Client concretely, so tests can
// substitute a fake; *sra.Client satisfies it as-is.
type Client interface {
	LookupAll(ctx context.Context, accessions []string) (found map[string][]sra.Record, missing []string, err error)
}

// Lookuper resolves SRA accessions to run-level metadata rows.
type Lookuper struct {
	client Client
}

// New builds a Lookuper wrapping client -- typically the result of
// sra.New(), constructed the same way the SDK's own tests do.
func New(client Client) *Lookuper {
	return &Lookuper{client: client}
}

// Lookup resolves every accession in ids -- run, experiment, or study -- to
// run-level metadata rows, flattening a multi-run experiment or study
// exactly like sra_tools.py's parse_accession_metadata: one output row per
// run found, across every matching sra.Record.
//
// missing lists the input accessions LookupAll could not match to any
// returned experiment package at all. This is also how the real-world
// SRP-batch-expansion quirk is reported (see BV-BRC-Go-SDK/sra's LookupAll
// doc comment and sra_test.go's TestLookupAllLiveEutils): efetch does not
// resolve a bare SRP id as an id= value on its own, so unless the same
// batch also carries a resolvable SRR/SRX from that study, an SRP comes
// back here as missing -- not as an error, exactly like an unrecognized SRR
// would. Callers must not treat that case as a crash-worthy failure.
//
// A non-nil error means NCBI itself could not be reached (see
// sra.Client.Lookup's doc comment) -- a different, much rarer condition
// than "accession not found" that callers should report distinctly.
func (l *Lookuper) Lookup(ctx context.Context, ids []string) (rows []SRAMetadata, missing []string, err error) {
	all, missing, err := l.client.LookupAll(ctx, ids)
	if err != nil {
		return nil, nil, err
	}

	for _, id := range ids {
		recs, ok := all[id]
		if !ok {
			continue // reported in missing already
		}
		for _, rec := range recs {
			for _, run := range rec.Runs {
				rows = append(rows, buildRow(id, rec, run))
			}
		}
	}
	return rows, missing, nil
}

// buildRow maps one (requested accession, matching Record, one of its Runs)
// triple to an output row.
func buildRow(requestedID string, rec sra.Record, run sra.RunRecord) SRAMetadata {
	return SRAMetadata{
		Accession:     run.Accession,
		Platform:      rec.Platform,
		LibraryLayout: rec.LibraryLayout,
		NReads:        nReads(rec.LibraryLayout, run.NReads),
		SizeBytes:     run.SizeBytes,
		Extra:         buildExtra(requestedID, rec, run),
	}
}

// nReads reproduces sra_tools.py:165-170's fallback exactly: when the run's
// own Statistics block didn't establish a read count (statedNReads == 0 --
// sra.RunRecord.NReads's doc comment: "Zero means unknown, not zero
// reads"), Python assumes 2 reads for a paired library and 1 otherwise
// (`2 if library_layout == 'PAIRED' else 1`). This is one of the five typed
// fields the real p3-sra.py comparison checks, so the fallback has to match
// exactly, not just the common case where Statistics is present.
func nReads(libraryLayout string, statedNReads int) int {
	if statedNReads > 0 {
		return statedNReads
	}
	if libraryLayout == "PAIRED" {
		return 2
	}
	return 1
}

// buildExtra collects the subset of parse_accession_metadata's other
// computed fields that BV-BRC-Go-SDK/sra actually parses out of the XML.
// requestedID is carried through as "requested_accession" so a caller that
// batched an experiment/study id can tell which request produced a given
// run row -- not present in sra_tools.py's output, but cheap provenance
// nothing in the design doc's compatibility contract forbids adding.
func buildExtra(requestedID string, rec sra.Record, run sra.RunRecord) map[string]string {
	extra := map[string]string{
		"requested_accession":  requestedID,
		"experiment_accession": rec.ExperimentAccession,
		"study_accession":      rec.StudyAccession,
		"experiment_title":     rec.ExperimentTitle,
		"study_title":          rec.StudyTitle,
		"total_bases":          strconv.FormatInt(run.TotalBases, 10),
		"total_spots":          strconv.FormatInt(run.TotalSpots, 10),
	}
	if run.ReadLength != 0 {
		extra["read_length"] = strconv.FormatFloat(run.ReadLength, 'g', -1, 64)
	}
	return extra
}
