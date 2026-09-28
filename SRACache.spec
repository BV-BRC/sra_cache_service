/*
 * SRACache: precache and serve NCBI SRA run data, and answer SRA run
 * metadata queries, for BV-BRC tooling built on top of the SRA toolkit
 * (prefetch / fasterq-dump / fastq-dump).
 *
 * This is a direct transcription of section 6 ("API sketch") of
 * PLAN-sra-precache-service.md (dev_container modules tree) into TypeSpec.
 * Do not add fields beyond what that design doc specifies without updating
 * it first -- every field here traces to a decision recorded there.
 *
 * Control plane only. The data plane (plain HTTP, streaming cached .sra
 * bytes via the signed URL get_sra_data mints) is a separate, non-JSONRPC
 * listener -- see the implementation plan's Phase 7.
 */
module SRACache
{
    /* An SRA accession: run (SRR/DRR/ERR), experiment (SRX/DRX/ERX), or
       study/project (SRP/DRP/ERP). */
    typedef string sra_accession;

    typedef string timestamp;

    /* queued      - accepted, not yet started
       in_progress - prefetch running
       cached      - .sra present and verified in local cache
       invalid_id  - fails accession syntax check (fast, no NCBI round trip),
                     or (for start_precache/get_sra_data) is not a run-level
                     accession
       not_found   - well-formed id, NCBI has no such accession
       error       - prefetch failed; message set; caller may retry */
    typedef string precache_state;

    /* Per-accession precache status.

       sra_accession accession - the accession this status is for
       precache_state state - current state, see precache_state
       string message - human-readable detail; set for invalid_id/not_found/error
       int size_bytes - size of the cached .sra file; 0 until cached
       timestamp queued_time - when the accession was queued
       timestamp cached_time - when the accession finished caching; unset until cached
    */
    typedef structure {
        sra_accession accession;
        precache_state state;
        string message;
        int size_bytes;
        timestamp queued_time;
        timestamp cached_time;
    } precache_status;

    /* Options controlling a precache request.

       int priority - relative scheduling priority; higher runs sooner
    */
    typedef structure {
        int priority;
    } precache_options;

    /* Queue a list of run-level accessions (SRR/DRR/ERR) for precaching.
       SRX/SRP accessions are rejected with state = invalid_id -- expand them
       via get_metadata first (see the design doc, section 6). */
    funcdef start_precache(list<sra_accession> ids, precache_options options)
        returns (list<precache_status> status) authentication required;

    /* Poll the current precache state of a list of run-level accessions. */
    funcdef precache_status(list<sra_accession> ids)
        returns (list<precache_status> status) authentication optional;

    /* Run-level SRA metadata, reconciled against real p3-sra consumers
       (design doc section 7.2) rather than exposing every field
       sra_tools.parse_accession_metadata computes. Only the fields with a
       confirmed reader are typed; everything else lands in extra.

       sra_accession accession - always a run-level (SRR/DRR/ERR) accession,
           even when the request named an experiment or study
       string platform - NCBI's raw PLATFORM tag (ILLUMINA, PACBIO_SMRT,
           OXFORD_NANOPORE, ION_TORRENT, ...), not canonicalized
       string library_layout - "PAIRED" or "SINGLE"
       int n_reads - read count, using the Statistics-children-with-count>0
           rule (the naive @nreads attribute lies for some runs)
       int size_bytes - run size in bytes
       mapping<string,string> extra - every other field
           parse_accession_metadata computes today (exp_id, study_id,
           instrument_model, library_strategy/selection, sample_*, spots,
           bases, estimated_size, read_length, ...), carried through
           uninterpreted since nothing in this codebase reads them yet
    */
    typedef structure {
        sra_accession accession;
        string platform;
        string library_layout;
        int n_reads;
        int size_bytes;
        mapping<string,string> extra;
    } sra_metadata;

    /* Accepts any accession type (run, experiment, or study) and always
       returns run-level records -- an experiment/study id expands to one
       record per constituent run, matching
       sra_tools.parse_accession_metadata's existing behavior. */
    funcdef get_metadata(list<sra_accession> ids)
        returns (list<sra_metadata> metadata) authentication optional;

    /* Where to fetch a cached run's data once it's ready.

       sra_accession accession - the run-level accession this location is for
       precache_state state - reflects whether a precache was already
           complete or was just triggered as a side effect of this call
       string url - a short-lived signed data-plane URL; only valid once
           state == cached
       timestamp expires - when url stops being valid; also the eviction
           lease pinning this accession in the cache until then
    */
    typedef structure {
        sra_accession accession;
        precache_state state;
        string url;
        timestamp expires;
    } sra_data_location;

    /* Get (and, if necessary, trigger caching of) run-level accession data.
       If an accession isn't cached yet, this call enqueues it as a side
       effect and returns its current state with an empty url; poll again
       once state == cached to get a usable url. */
    funcdef get_sra_data(list<sra_accession> ids)
        returns (list<sra_data_location> locations) authentication optional;
};
