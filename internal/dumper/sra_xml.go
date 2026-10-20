package dumper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/BV-BRC/BV-BRC-Go-SDK/v2/version"
)

// eutilsEfetchURL is the same endpoint sra_import/lib/sra_tools.py's
// get_accession_metadata and BV-BRC-Go-SDK/sra.DefaultBaseURL both use.
const eutilsEfetchURL = "https://eutils.ncbi.nlm.nih.gov/entrez/eutils/efetch.fcgi"

// FetchRawSRAXML fetches the raw NCBI efetch docset XML for accessions,
// verbatim, for cmd/p3-sra-dump's --sra-metadata-file flag.
//
// Per PLAN-sra-cache-service-implementation.md's Phase 9 decision:
// SRACache.get_metadata's sra_metadata typedef does not carry the XML it was
// parsed from (nothing that consumes p3-sra's --sra-metadata-file output
// today parses it back -- it is write-only provenance, per
// PLAN-sra-precache-service.md section 7.2), so this is a second, independent
// eutils call, made only when --sra-metadata-file is actually given. That
// makes it a rare, best-effort, single-batch request -- not worth reusing
// BV-BRC-Go-SDK/sra.Client for, since that package's fetch method is
// unexported (it returns parsed experimentPackages, not raw bytes) and its
// batching/retry machinery is sized for a very different workload (up to
// hundreds of accessions across multiple requests) than "however many
// accessions one p3-sra-dump invocation was given, in one request". A small,
// standalone GET here is judged cheaper than exporting a raw-fetch method
// from a different Go module for this one caller.
func FetchRawSRAXML(ctx context.Context, httpClient *http.Client, accessions []string) ([]byte, error) {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if len(accessions) == 0 {
		return nil, fmt.Errorf("dumper: FetchRawSRAXML: no accessions given")
	}

	params := url.Values{}
	params.Set("db", "sra")
	params.Set("rettype", "docset")
	params.Set("retmode", "xml")
	params.Set("id", strings.Join(accessions, ","))
	if key := os.Getenv("NCBI_API_KEY"); key != "" {
		params.Set("api_key", key)
	}
	reqURL := eutilsEfetchURL + "?" + params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dumper: building eutils request: %w", err)
	}
	req.Header.Set("User-Agent", version.UserAgent())

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dumper: fetching raw SRA XML: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("dumper: reading raw SRA XML response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dumper: eutils returned HTTP %d: %s", resp.StatusCode, truncateForError(body))
	}
	return body, nil
}

func truncateForError(b []byte) string {
	const max = 2048
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}
