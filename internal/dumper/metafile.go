package dumper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BV-BRC/sra_cache_service/internal/metadata"
)

// GlobDumpedFiles reproduces sra_import/lib/sra_tools.py:332-333 exactly:
// glob "<outDir>/*<accession>*", basenames only. Must be called AFTER the
// dump tool has produced its output -- "files" is a post-dump artifact of how
// the toolkit actually split/named its output, it cannot come from
// SRACache.get_metadata (PLAN-sra-precache-service.md section 6 says this
// explicitly). Results are sorted for deterministic output; Go's
// filepath.Glob does not guarantee an order and sra_tools.py's own
// glob.glob/os.path.basename pipeline doesn't either, so sorting here is a
// (harmless) improvement over relying on filesystem enumeration order.
func GlobDumpedFiles(outDir, accession string) ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(outDir, "*"+accession+"*"))
	if err != nil {
		return nil, fmt.Errorf("dumper: globbing dumped files for %s: %w", accession, err)
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		names = append(names, filepath.Base(m))
	}
	sort.Strings(names)
	return names, nil
}

// BuildMetaRecords maps a SRACache.get_metadata response (rows, one per run)
// plus the post-dump file listing (files, keyed by run accession -- see
// GlobDumpedFiles) to p3-sra-compatible metadata records: a JSON array of
// flat per-run objects, matching sra_import/lib/sra_tools.py's run_meta shape
// (parse_accession_metadata:214-226) closely enough that an existing
// --metadata-file consumer sees every field PLAN-sra-precache-service.md
// section 7.2 identified as load-bearing, under its ORIGINAL sra_tools.py key
// name -- not this service's own (different) key names:
//
//	accession        (same name both sides)
//	platform_name    (sra_tools.py's name; SRACache.get_metadata calls this
//	                  "platform" -- see metadata.SRAMetadata's own doc comment
//	                  on why the service renamed it)
//	library_layout   (same name both sides)
//	n_reads          (same name both sides)
//	size             (sra_tools.py's name; SRACache.get_metadata calls this
//	                  "size_bytes")
//	files            (same name both sides; present only when files supplies
//	                  an entry for that accession -- absent for --metaonly,
//	                  matching sra_tools.py's own behavior of never setting
//	                  "files" in that mode)
//
// Every other field this service's get_metadata computed (SRAMetadata.Extra:
// requested_accession, experiment_accession/title, study_accession/title,
// total_bases, total_spots, read_length) is merged in verbatim under those
// names, plus "run_id" (always equal to accession, matching sra_tools.py's
// own rdata['run_id']==rdata['accession'] invariant) for cheap extra
// compatibility. None of this second group has a confirmed real-world reader
// (section 7.2's ten-call-site survey), so there is no compatibility
// requirement to reproduce sra_tools.py's own differently-spelled counterparts
// (exp_id, study_id, design_description, library_name/selection/strategy,
// sample_*, estimated_size) -- carrying them through under the service's own
// names is a superset, not a narrowing.
//
// A row get_metadata built for a rejected accession (SRAMetadata.Extra
// carrying "error"/"message" -- see internal/rpcapi/metadata.go's
// invalidRow/notFoundRow) is passed through the same way: its typed fields
// are all zero and "error"/"message" land in the output object via the Extra
// merge, so a caller scanning the metadata file can still see why an
// accession has no data rather than the row silently vanishing.
func BuildMetaRecords(rows []metadata.SRAMetadata, files map[string][]string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		rec := map[string]any{}
		for k, v := range r.Extra {
			rec[k] = v
		}
		rec["run_id"] = r.Accession
		rec["accession"] = r.Accession
		rec["platform_name"] = r.Platform
		rec["library_layout"] = r.LibraryLayout
		rec["n_reads"] = r.NReads
		rec["size"] = r.SizeBytes
		if fs, ok := files[r.Accession]; ok {
			rec["files"] = fs
		}
		out = append(out, rec)
	}
	return out
}

// WriteMetadataFile marshals records as indented JSON to path, matching
// sra_import/lib/sra_tools.py:339-340's `json.dump(metadata, fp, indent=2)`
// shape: a bare JSON array at the top level, 2-space indent.
func WriteMetadataFile(path string, records []map[string]any) error {
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("dumper: encoding metadata file: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("dumper: writing metadata file %s: %w", path, err)
	}
	return nil
}
