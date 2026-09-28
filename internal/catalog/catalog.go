// Package catalog is the sqlite-backed accession catalog: one row per SRA
// accession the service knows about, tracking precache state and the
// bookkeeping (size, timestamps, pin lease, error count) the cache-eviction
// logic in internal/cache needs.
//
// See PLAN-sra-cache-service-implementation.md, Phase 5, and
// PLAN-sra-precache-service.md §3/§6 for the design this implements.
//
// # modernc.org/sqlite DSN and pragma notes
//
// There is no other Go sqlite usage anywhere in this repo to copy from, and
// this driver (a pure-Go, cgo-free port of SQLite -- required because the
// whole module builds with CGO_ENABLED=0) does not share mattn/go-sqlite3's
// DSN conventions. What actually works, read directly out of the driver's
// own source (modernc.org/sqlite@.../driver.go's Open doc comment and
// sqlite.go's applyQueryParams/getVFSName):
//
//   - The registered driver name is "sqlite", not "sqlite3".
//   - The DSN is a filename optionally followed by "?query params", or a
//     "file:" URI with the same query string. Pragmas are NOT expressed as
//     mattn-style "?_journal_mode=WAL&_busy_timeout=5000" key=value pairs --
//     that convention is not understood by this driver at all and such
//     params would silently do nothing. Instead every value of a repeated
//     "_pragma=" parameter becomes a literal "PRAGMA <value>" statement run
//     against each newly opened connection, e.g.
//     "file:path?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)".
//   - "_pragma=busy_timeout(...)" is deliberately sorted first by the driver
//     itself regardless of query-string order, so lock-wait behavior is
//     already in effect before any other pragma statement runs.
//   - WAL mode plus NORMAL synchronous is the standard "fast, still durable
//     across a process crash" combination recommended by SQLite's own docs
//     for a single-writer workload like this one.
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// State is one of the precache_state enum values from SRACache.spec /
// PLAN-sra-precache-service.md §6. A distinct type (rather than a bare
// string) makes a typo'd state literal a compile error at every call site
// except the deliberately-generic sqlite scan/bind code in this file.
type State string

// The precache_state enum, verbatim from SRACache.spec -- do not add values
// here without updating the spec first.
const (
	StateQueued     State = "queued"
	StateInProgress State = "in_progress"
	StateCached     State = "cached"
	StateInvalidID  State = "invalid_id"
	StateNotFound   State = "not_found"
	StateError      State = "error"
)

// ErrNotFound is returned by operations that target one existing accession
// (SetState, TouchAccess, Pin) when no row for that accession exists.
// Enqueue and Upsert never return it -- they create the row themselves.
var ErrNotFound = errors.New("catalog: accession not found")

// Entry is one row of the accession catalog. A zero-value time.Time field
// means "unset" (stored as SQL NULL in that column), never the Unix epoch.
type Entry struct {
	Accession      string
	State          State
	Message        string
	SizeBytes      int64
	QueuedTime     time.Time
	CachedTime     time.Time
	LastAccessTime time.Time
	PinnedUntil    time.Time
	ErrorCount     int
}

// Catalog is a sqlite-backed store of accession rows.
//
// V1 is explicitly single-host/single-writer (see
// PLAN-sra-precache-service.md §8, item 4), so Catalog wraps exactly one
// *sql.DB capped at a single open connection (SetMaxOpenConns(1)), shared by
// both reads and writes -- not a separate read pool. That keeps every
// operation serialized through one sqlite connection, which sidesteps
// SQLITE_BUSY entirely for in-process concurrency and is far more headroom
// than this catalog's scale (tens of thousands of accession rows, not a
// high-QPS workload) needs. If a future phase needs concurrent readers under
// load, that is the point to revisit this, not before.
type Catalog struct {
	db *sql.DB
}

// schemaSQL is applied with CREATE TABLE/INDEX IF NOT EXISTS -- sufficient
// for V1 per the implementation plan; no migration framework.
const schemaSQL = `
CREATE TABLE IF NOT EXISTS accessions (
	accession        TEXT PRIMARY KEY,
	state            TEXT NOT NULL,
	message          TEXT NOT NULL DEFAULT '',
	size_bytes       INTEGER NOT NULL DEFAULT 0,
	queued_time      INTEGER,
	cached_time      INTEGER,
	last_access_time INTEGER,
	pinned_until     INTEGER,
	error_count      INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_accessions_state ON accessions(state);
CREATE INDEX IF NOT EXISTS idx_accessions_eviction ON accessions(state, pinned_until, last_access_time);
`

// Open opens (creating the file if necessary) the sqlite database at path
// and ensures the schema exists. Callers must Close the returned Catalog.
func Open(ctx context.Context, path string) (*Catalog, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)",
		path,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("catalog: opening %s: %w", path, err)
	}

	// Single connection for the whole handle -- see the Catalog doc comment.
	// MaxIdleConns matches MaxOpenConns so the pool keeps that one
	// connection warm rather than closing and reopening it between bursts of
	// activity (harmless either way since pragmas are DSN-driven and reapply
	// to any new connection, but there's no reason to pay the reopen cost).
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("catalog: creating schema: %w", err)
	}
	return &Catalog{db: db}, nil
}

// Close closes the underlying database handle.
func (c *Catalog) Close() error {
	return c.db.Close()
}

const selectColumns = `accession, state, message, size_bytes, queued_time, cached_time, last_access_time, pinned_until, error_count`

// scanner is satisfied by both *sql.Row and *sql.Rows, letting Get and the
// list-returning queries share one scan routine.
type scanner interface {
	Scan(dest ...any) error
}

func scanEntry(s scanner) (Entry, error) {
	var (
		e                                       Entry
		state                                   string
		queued, cached, lastAccess, pinnedUntil sql.NullInt64
	)
	if err := s.Scan(
		&e.Accession, &state, &e.Message, &e.SizeBytes,
		&queued, &cached, &lastAccess, &pinnedUntil, &e.ErrorCount,
	); err != nil {
		return Entry{}, err
	}
	e.State = State(state)
	e.QueuedTime = nullToTime(queued)
	e.CachedTime = nullToTime(cached)
	e.LastAccessTime = nullToTime(lastAccess)
	e.PinnedUntil = nullToTime(pinnedUntil)
	return e, nil
}

// timeToNull and nullToTime store timestamps as nanoseconds-since-epoch
// (not seconds) so that two events in the same test (or the same busy
// second in production) still order correctly against each other --
// EvictionCandidates' whole job is a stable ORDER BY on this column.
func timeToNull(t time.Time) sql.NullInt64 {
	if t.IsZero() {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UTC().UnixNano(), Valid: true}
}

func nullToTime(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.Unix(0, n.Int64).UTC()
}

// Get returns the catalog row for accession, or ok == false if none exists.
func (c *Catalog) Get(ctx context.Context, accession string) (Entry, bool, error) {
	row := c.db.QueryRowContext(ctx, "SELECT "+selectColumns+" FROM accessions WHERE accession = ?", accession)
	e, err := scanEntry(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("catalog: get %s: %w", accession, err)
	}
	return e, true, nil
}

// GetMany returns every existing row among accessions, keyed by accession.
// Accessions with no row are simply absent from the result map (not an
// error) -- callers distinguish "unknown" from "known but not cached" by
// checking map membership vs. the returned Entry's State.
func (c *Catalog) GetMany(ctx context.Context, accessions []string) (map[string]Entry, error) {
	out := make(map[string]Entry, len(accessions))
	if len(accessions) == 0 {
		return out, nil
	}

	placeholders := make([]string, len(accessions))
	args := make([]any, len(accessions))
	for i, a := range accessions {
		placeholders[i] = "?"
		args[i] = a
	}
	query := "SELECT " + selectColumns + " FROM accessions WHERE accession IN (" + strings.Join(placeholders, ",") + ")"

	rows, err := c.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("catalog: get many: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: get many: scanning: %w", err)
		}
		out[e.Accession] = e
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: get many: %w", err)
	}
	return out, nil
}

// Upsert writes e in full, creating the row if absent or replacing every
// column if present. Use this when the caller already has a complete Entry
// to write (e.g. the Phase-6 fetcher publishing a finished download);
// use the narrower Enqueue/SetState/TouchAccess/Pin for partial updates.
func (c *Catalog) Upsert(ctx context.Context, e Entry) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO accessions (accession, state, message, size_bytes, queued_time, cached_time, last_access_time, pinned_until, error_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(accession) DO UPDATE SET
			state = excluded.state,
			message = excluded.message,
			size_bytes = excluded.size_bytes,
			queued_time = excluded.queued_time,
			cached_time = excluded.cached_time,
			last_access_time = excluded.last_access_time,
			pinned_until = excluded.pinned_until,
			error_count = excluded.error_count
	`,
		e.Accession, string(e.State), e.Message, e.SizeBytes,
		timeToNull(e.QueuedTime), timeToNull(e.CachedTime), timeToNull(e.LastAccessTime), timeToNull(e.PinnedUntil),
		e.ErrorCount,
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert %s: %w", e.Accession, err)
	}
	return nil
}

// Enqueue creates a row for accession in the queued state with no side
// effects if one already exists -- it never clobbers an in_progress or
// cached (or any other) row back to queued.
func (c *Catalog) Enqueue(ctx context.Context, accession string) error {
	_, err := c.db.ExecContext(ctx, `
		INSERT INTO accessions (accession, state, message, size_bytes, queued_time, error_count)
		VALUES (?, ?, '', 0, ?, 0)
		ON CONFLICT(accession) DO NOTHING
	`, accession, string(StateQueued), timeToNull(time.Now()))
	if err != nil {
		return fmt.Errorf("catalog: enqueue %s: %w", accession, err)
	}
	return nil
}

// SetState transitions accession to state with the given message. Moving
// into StateCached also stamps cached_time = now; moving into StateError
// also increments error_count. Returns ErrNotFound if accession has no row.
func (c *Catalog) SetState(ctx context.Context, accession string, state State, message string) error {
	var (
		res sql.Result
		err error
	)
	switch state {
	case StateCached:
		res, err = c.db.ExecContext(ctx,
			`UPDATE accessions SET state = ?, message = ?, cached_time = ? WHERE accession = ?`,
			string(state), message, timeToNull(time.Now()), accession)
	case StateError:
		res, err = c.db.ExecContext(ctx,
			`UPDATE accessions SET state = ?, message = ?, error_count = error_count + 1 WHERE accession = ?`,
			string(state), message, accession)
	default:
		res, err = c.db.ExecContext(ctx,
			`UPDATE accessions SET state = ?, message = ? WHERE accession = ?`,
			string(state), message, accession)
	}
	if err != nil {
		return fmt.Errorf("catalog: set state %s -> %s: %w", accession, state, err)
	}
	return checkAffected(res, accession)
}

// TouchAccess updates accession's last_access_time to now. Returns
// ErrNotFound if accession has no row.
func (c *Catalog) TouchAccess(ctx context.Context, accession string) error {
	res, err := c.db.ExecContext(ctx,
		`UPDATE accessions SET last_access_time = ? WHERE accession = ?`,
		timeToNull(time.Now()), accession)
	if err != nil {
		return fmt.Errorf("catalog: touch access %s: %w", accession, err)
	}
	return checkAffected(res, accession)
}

// Pin sets accession's pinned_until lease. This is the eviction lease
// described in PLAN-sra-precache-service.md §3: an entry with an unexpired
// pinned_until is never chosen by EvictionCandidates. Returns ErrNotFound if
// accession has no row.
func (c *Catalog) Pin(ctx context.Context, accession string, until time.Time) error {
	res, err := c.db.ExecContext(ctx,
		`UPDATE accessions SET pinned_until = ? WHERE accession = ?`,
		timeToNull(until), accession)
	if err != nil {
		return fmt.Errorf("catalog: pin %s: %w", accession, err)
	}
	return checkAffected(res, accession)
}

// TotalCachedBytes returns the sum of size_bytes across every row in the
// cached state.
func (c *Catalog) TotalCachedBytes(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	err := c.db.QueryRowContext(ctx,
		`SELECT SUM(size_bytes) FROM accessions WHERE state = ?`, string(StateCached),
	).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("catalog: total cached bytes: %w", err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// EvictionCandidates returns every cached-state row whose pinned_until lease
// is either unset or has already expired as of now, ordered by
// last_access_time ascending -- i.e. least-recently-used first, exactly the
// order Evict (internal/cache/evict.go) walks.
func (c *Catalog) EvictionCandidates(ctx context.Context, now time.Time) ([]Entry, error) {
	rows, err := c.db.QueryContext(ctx, `
		SELECT `+selectColumns+` FROM accessions
		WHERE state = ? AND (pinned_until IS NULL OR pinned_until <= ?)
		ORDER BY last_access_time ASC
	`, string(StateCached), timeToNull(now))
	if err != nil {
		return nil, fmt.Errorf("catalog: eviction candidates: %w", err)
	}
	defer rows.Close()

	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: eviction candidates: scanning: %w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: eviction candidates: %w", err)
	}
	return out, nil
}

// Requeue is restart recovery: any row stuck in_progress (the process that
// was fetching it died mid-fetch) moves back to queued. There is no
// in-flight work to resume, only state to reset -- rows in any other state
// are left alone.
func (c *Catalog) Requeue(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx,
		`UPDATE accessions SET state = ? WHERE state = ?`,
		string(StateQueued), string(StateInProgress))
	if err != nil {
		return fmt.Errorf("catalog: requeue: %w", err)
	}
	return nil
}

// Delete removes accession's row outright. There is no distinct "evicted"
// state in the precache_state enum (SRACache.spec), so eviction
// (internal/cache/evict.go) deletes the row rather than transitioning it --
// simpler, and NCBI remains the source of truth for anything evicted.
// Deleting an accession with no row is a no-op, not an error.
func (c *Catalog) Delete(ctx context.Context, accession string) error {
	_, err := c.db.ExecContext(ctx, `DELETE FROM accessions WHERE accession = ?`, accession)
	if err != nil {
		return fmt.Errorf("catalog: delete %s: %w", accession, err)
	}
	return nil
}

func checkAffected(res sql.Result, accession string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("catalog: %s: %w", accession, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, accession)
	}
	return nil
}
