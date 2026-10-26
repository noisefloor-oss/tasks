// Package store implements the SPEC §8 SQLite live authority: the single
// task-state database shared by every checkout of a repository family.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"

	_ "modernc.org/sqlite"
)

// SchemaVersion is the schema version this binary owns, recorded in
// PRAGMA user_version (SPEC §8.2). Version 2 added immutable identity and
// receipts; version 3 adds the §9.2 plans, mappings, and assignments; version
// 4 adds the §9.3 runs and attempts; version 5 adds recoverable worktree
// sagas; version 6 adds the §1 external-link columns and their unique index.
const SchemaVersion = 6

const (
	identitySchemaVersion     = 2
	recordExtraSchemaVersion  = 2
	orchestratorSchemaVersion = 3
	runSchemaVersion          = 4
	externalLinkSchemaVersion = 6
)

// MaxRetainedEvents bounds the change-feed retention window (SPEC §8.4).
const MaxRetainedEvents = 10000

// DefaultClaimTTL is the claim expiry in seconds when ttl is omitted.
const DefaultClaimTTL = 900

// Diagnostics fixed by SPEC §8.2/§8.5.
const (
	SchemaNewerDiagnostic = "noise-tasks: task store schema is newer than this binary; upgrade noise-tasks"
	corruptDiagnostic     = "noise-tasks: task store is not a valid SQLite database: "
)

// NotSQLiteError reports a corrupt, truncated, or otherwise unreadable
// database (SPEC §8.5, exit 1). No implicit repair is ever attempted.
type NotSQLiteError struct{ Path string }

func (e *NotSQLiteError) Error() string { return corruptDiagnostic + e.Path }

// UninitializedError reports a database file that exists but carries no
// released schema (PRAGMA user_version 0): an unpublished creation artifact
// or a foreign SQLite file, never a v1 store (SPEC §8.2, exit 1). It fails
// closed rather than being treated as an upgradeable released schema.
type UninitializedError struct{ Path string }

func (e *UninitializedError) Error() string {
	return "noise-tasks: task store is not initialized with a released schema: " + e.Path
}

// SchemaNewerError reports a database whose user_version is newer than this
// binary (SPEC §8.2, exit 1).
type SchemaNewerError struct{}

func (e *SchemaNewerError) Error() string { return SchemaNewerDiagnostic }

// Path derives the §8.1 authority path for a repository boundary: the Git
// common directory's noise-tasks/tasks.sqlite when resolvable, else the
// outside-Git fallback <boundary>/.tasks/tasks.sqlite. ok is false only when
// the boundary does not exist; the fallback always resolves.
func Path(boundary string) (path string, inGit bool) {
	if common, ok := backlog.GitCommonDir(boundary); ok {
		return filepath.Join(common, "noise-tasks", "tasks.sqlite"), true
	}
	return filepath.Join(boundary, ".tasks", "tasks.sqlite"), false
}

// DB is one process's handle on the §8 authority. It is safe for concurrent
// goroutine use; cross-process serialization comes from SQLite immediate
// transactions and the busy timeout, plus the shared maintenance lock held
// from open to close against offline exclusive holders.
type DB struct {
	db     *sql.DB
	path   string
	unlock func()
	// ownerBoundary is the configured canonical §8.1 owner boundary this
	// handle mints the family identity with when it upgrades a v1 store.
	// It is set once, before the first mutation, and only read afterwards.
	ownerBoundary string
	// legacyClaimAcquisitionIDs selects the deterministic §9.3 claim identity
	// derivation for a verified legacy-source v3 staging database. Normal
	// SQLite authorities and SQLite-source restore stages keep the random path.
	legacyClaimAcquisitionIDs bool
}

// Path returns the database file path.
func (d *DB) Path() string { return d.path }

// UserVersion reports the store's PRAGMA user_version (§8.2) without
// mutating anything. It backs read-only consumer facts such as the §2.2
// schema-5 context pair.
func (d *DB) UserVersion() (int, error) { return d.userVersion() }

// Open opens an existing database, enforcing §8.5 fail-closed behavior:
// symlink/special-file refusal, corruption diagnostic, and the §8.2
// future-schema refusal. A missing file is an os.ErrNotExist error.
func Open(path string) (*DB, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("open task store %s: not a regular file", path)
	}
	d, err := open(path, false, "")
	if err != nil {
		return nil, err
	}
	return d, nil
}

// sqliteURI renders a file: URI whose path component is escaped so
// legal filename characters (%, ?, #) cannot become URI syntax and
// redirect the open at another file (SQLite URI filename rules).
func sqliteURI(abs string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")
	return "file:" + r.Replace(abs)
}

// OpenReadOnly opens an existing database strictly read-only in SQLite's
// immutable mode: no maintenance lock is taken and nothing is ever
// created — not the lock file, not WAL sidecars — so inspection surfaces
// (§2.4) cannot mutate a family they walk. The same fail-closed
// regular-file and schema fences as Open apply. An actively mutating
// store may be observed at its last checkpoint rather than failing; a
// corrupt or unreadable database is an error, never a repair.
func OpenReadOnly(path string) (*DB, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("open task store %s: not a regular file", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := sqliteURI(abs) + "?mode=ro&immutable=1&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open task store %s: %w", path, err)
	}
	d := &DB{db: db, path: abs}
	if err := d.checkReadable(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// storeExistsError reports that Create found an existing regular file at the
// store path. It unwraps to os.ErrExist so callers racing a concurrent create
// can adopt the winner's store.
type storeExistsError struct{ path string }

func (e *storeExistsError) Error() string {
	return fmt.Sprintf("create task store %s: already exists", e.path)
}

func (e *storeExistsError) Unwrap() error { return os.ErrExist }

// Create bootstraps a new database at path (mode 0600) with the current schema
// applied transactionally, and returns the open handle. An existing file is
// an error wrapping os.ErrExist.
func Create(path string) (*DB, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("create task store %s: not a regular file", path)
		}
		return nil, &storeExistsError{path}
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create task store directory: %w", err)
	}
	d, err := open(path, true, "")
	if err != nil {
		return nil, err
	}
	return d, nil
}

// open acquires the store directory's shared maintenance lock, then opens
// SQLite through openUnlocked. The lock is released by Close, after SQLite
// closes. Contention (an exclusive offline holder) fails closed.
func open(path string, create bool, ownerBoundary string) (*DB, error) {
	unlock, err := LockShared(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	d, err := openUnlocked(path, create, ownerBoundary)
	if err != nil {
		unlock()
		return nil, err
	}
	d.unlock = unlock
	return d, nil
}

// openUnlocked opens SQLite without any maintenance lock. It exists for the
// exclusive holder (offline restore, bundle verification on staged copies)
// which must not deadlock against its own lock.
func openUnlocked(path string, create bool, ownerBoundary string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	// synchronous=NORMAL: in WAL mode every commit is still durable across a
	// per-commit latency cost v1 does not buy. journal_mode is deliberately
	// absent: WAL is persistent in the database header, so it is set once at
	// create time — a per-connection pragma would make every pooled
	// connection re-run the lock-exclusive mode transition, which answers
	// SQLITE_BUSY without honoring busy_timeout when a peer holds the lock.
	dsn := sqliteURI(abs) + "?_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	if !create {
		dsn += "&mode=rw"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open task store %s: %w", path, err)
	}
	d := &DB{db: db, path: abs}
	if create {
		// The driver creates the file lazily; create it now with mode 0600 so
		// the store never exists with wider permissions. O_EXCL is the real
		// create-race guard (Create's Lstat is only a fast path); both report
		// an existing file as os.ErrExist so a racing caller can adopt the
		// winner's store.
		f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err != nil {
			db.Close()
			return nil, fmt.Errorf("create task store %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			db.Close()
			return nil, err
		}
		if err := d.enableWAL(); err != nil {
			db.Close()
			os.Remove(abs)
			return nil, err
		}
		if err := d.bootstrap(ownerBoundary); err != nil {
			db.Close()
			os.Remove(abs)
			return nil, err
		}
		return d, nil
	}
	if err := d.checkReadable(); err != nil {
		db.Close()
		return nil, err
	}
	return d, nil
}

// enableWAL switches a freshly created store to WAL. The mode persists in
// the database header, so later connections inherit it without paying the
// lock-exclusive transition. A concurrent opener that grabbed the file
// mid-create makes the transition answer SQLITE_BUSY immediately
// (busy_timeout does not cover journal-mode changes), so retry briefly.
func (d *DB) enableWAL() error {
	var mode string
	var lastErr error
	for range 100 {
		err := d.db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode)
		if err == nil {
			if !strings.EqualFold(mode, "wal") {
				return fmt.Errorf("task store journal mode %q, want wal", mode)
			}
			return nil
		}
		if !isBusy(err) {
			return fmt.Errorf("enable WAL on task store %s: %w", d.path, err)
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("enable WAL on task store %s: %w", d.path, lastErr)
}

// isBusy reports whether err is SQLite's SQLITE_BUSY family.
func isBusy(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "SQLITE_BUSY") || strings.Contains(msg, "database is locked")
}

// checkReadable runs SQLite's own validation of the file and enforces the
// schema-version fence (SPEC §8.2/§8.5).
func (d *DB) checkReadable() error {
	var version int
	if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		if isNotADatabase(err) {
			return &NotSQLiteError{Path: d.path}
		}
		return fmt.Errorf("read task store %s: %w", d.path, err)
	}
	if version == 0 {
		return &UninitializedError{Path: d.path}
	}
	if version > SchemaVersion {
		return &SchemaNewerError{}
	}
	var check string
	if err := d.db.QueryRow("PRAGMA quick_check").Scan(&check); err != nil || check != "ok" {
		if err != nil && isNotADatabase(err) {
			return &NotSQLiteError{Path: d.path}
		}
		if err == nil {
			return &NotSQLiteError{Path: d.path}
		}
		return fmt.Errorf("check task store %s: %w", d.path, err)
	}
	return nil
}

// isNotADatabase reports whether err is SQLite's "file is not a database"
// family (SQLITE_NOTADB / corrupt header), including truncated files.
func isNotADatabase(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not a database") ||
		strings.Contains(msg, "database disk image is malformed") ||
		strings.Contains(msg, "SQLITE_NOTADB") ||
		strings.Contains(msg, "SQLITE_CORRUPT")
}

// bootstrap applies the current schema transactionally, minting the §8.2
// family identity in the same transaction when a canonical owner boundary
// is supplied. It uses a raw transaction: maybeUpgrade (triggered by
// immediate) is for databases that already carry a released schema, not for
// one being created.
func (d *DB) bootstrap(ownerBoundary string) error {
	return d.transact("BEGIN IMMEDIATE", func(tx *tx) error {
		if _, err := tx.Exec(schemaDDL); err != nil {
			return fmt.Errorf("bootstrap task store schema: %w", err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion)); err != nil {
			return fmt.Errorf("set task store schema version: %w", err)
		}
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES ('fencing', '0')`); err != nil {
			return fmt.Errorf("bootstrap fencing counter: %w", err)
		}
		// The §8.2 identity is minted in this same transaction when a
		// canonical owner boundary is supplied, so a crash can never publish
		// a current-schema authority lacking its family identity.
		if ownerBoundary != "" {
			if err := tx.ensureIdentity(ownerBoundary); err != nil {
				return err
			}
		}
		return nil
	})
}

// orchestratorSchemaDDL is exactly the v2→v3 schema delta.
const orchestratorSchemaDDL = `
CREATE TABLE plans (
	plan_id        TEXT PRIMARY KEY,
	tag            TEXT NOT NULL REFERENCES tags(name),
	digest         TEXT NOT NULL,
	canonical_json TEXT NOT NULL,
	request_id     TEXT NOT NULL UNIQUE,
	receipt_id     TEXT NOT NULL,
	applied_at     TEXT NOT NULL
);
CREATE TABLE plan_mappings (
	plan_id   TEXT NOT NULL REFERENCES plans(plan_id),
	local_key TEXT NOT NULL,
	task_id   TEXT NOT NULL,
	PRIMARY KEY (plan_id, local_key)
);
CREATE TABLE assignments (
	tag            TEXT NOT NULL,
	task_id        TEXT NOT NULL,
	assignment_key TEXT NOT NULL,
	owner          TEXT NOT NULL,
	plan_id        TEXT NOT NULL REFERENCES plans(plan_id),
	PRIMARY KEY (tag, task_id),
	UNIQUE (plan_id, assignment_key),
	FOREIGN KEY (tag, task_id) REFERENCES tasks(tag, id)
);
CREATE INDEX assignments_owner_task
	ON assignments(tag, owner, task_id);
`

// runSchemaDDL is exactly the additive v3→v4 run/attempt catalog. The
// circular current-attempt pointer is deliberately the sole deferred foreign
// key: a start transaction inserts the run and attempt together, and commit
// refuses either half. All history edges are restrictive.
const runSchemaDDL = `
CREATE TABLE runs (
	run_id                   INTEGER PRIMARY KEY AUTOINCREMENT CHECK(run_id > 0),
	tag                      TEXT NOT NULL,
	task_id                  TEXT NOT NULL,
	owner                    TEXT NOT NULL,
	claim_acquisition_id     TEXT NOT NULL CHECK(length(claim_acquisition_id) = 32 AND claim_acquisition_id NOT GLOB '*[^0-9a-f]*'),
	claim_token              INTEGER NOT NULL CHECK(typeof(claim_token) = 'integer' AND claim_token > 0),
	status                   TEXT NOT NULL CHECK(status IN ('running','retryable','succeeded','cancelled')),
	current_attempt          INTEGER NOT NULL CHECK(typeof(current_attempt) = 'integer' AND current_attempt > 0),
	start_request_id         TEXT NOT NULL UNIQUE,
	terminal_result_digest   TEXT CHECK(terminal_result_digest IS NULL OR (length(terminal_result_digest) = 71 AND substr(terminal_result_digest, 1, 7) = 'sha256:' AND substr(terminal_result_digest, 8) NOT GLOB '*[^0-9a-f]*')),
	force_cancel_fence       INTEGER UNIQUE CHECK(force_cancel_fence IS NULL OR (typeof(force_cancel_fence) = 'integer' AND force_cancel_fence > 0)),
	started_at               TEXT NOT NULL CHECK(length(started_at) > 0),
	updated_at               TEXT NOT NULL CHECK(length(updated_at) > 0),
	terminal_at              TEXT,
	extra                    TEXT,
	FOREIGN KEY (tag, task_id) REFERENCES tasks(tag, id) ON UPDATE RESTRICT ON DELETE RESTRICT,
	FOREIGN KEY (run_id, current_attempt) REFERENCES attempts(run_id, attempt_no) ON UPDATE RESTRICT ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
	CHECK((status IN ('running','retryable') AND terminal_at IS NULL) OR (status IN ('succeeded','cancelled') AND terminal_at IS NOT NULL)),
	CHECK(status = 'succeeded' OR terminal_result_digest IS NULL),
	CHECK(force_cancel_fence IS NULL OR status = 'cancelled')
);
CREATE TABLE attempts (
	run_id                   INTEGER NOT NULL,
	attempt_no               INTEGER NOT NULL CHECK(typeof(attempt_no) = 'integer' AND attempt_no > 0),
	acquisition_id           TEXT NOT NULL UNIQUE CHECK(length(acquisition_id) = 32 AND acquisition_id NOT GLOB '*[^0-9a-f]*'),
	attempt_token            INTEGER NOT NULL UNIQUE CHECK(typeof(attempt_token) = 'integer' AND attempt_token > 0),
	status                   TEXT NOT NULL CHECK(status IN ('running','succeeded','failed','lost','cancelled')),
	ttl_seconds              INTEGER NOT NULL CHECK(typeof(ttl_seconds) = 'integer' AND ttl_seconds BETWEEN 1 AND 86400),
	lease_generation         INTEGER NOT NULL CHECK(typeof(lease_generation) = 'integer' AND lease_generation > 0),
	created_request_id       TEXT NOT NULL UNIQUE,
	started_at               TEXT NOT NULL CHECK(length(started_at) > 0),
	expires_at               TEXT NOT NULL CHECK(length(expires_at) > 0),
	updated_at               TEXT NOT NULL CHECK(length(updated_at) > 0),
	terminal_at              TEXT,
	terminal_result_digest   TEXT CHECK(terminal_result_digest IS NULL OR (length(terminal_result_digest) = 71 AND substr(terminal_result_digest, 1, 7) = 'sha256:' AND substr(terminal_result_digest, 8) NOT GLOB '*[^0-9a-f]*')),
	extra                    TEXT,
	PRIMARY KEY (run_id, attempt_no),
	FOREIGN KEY (run_id) REFERENCES runs(run_id) ON UPDATE RESTRICT ON DELETE RESTRICT,
	CHECK((status = 'running' AND terminal_at IS NULL) OR (status <> 'running' AND terminal_at IS NOT NULL)),
	CHECK(status IN ('succeeded','failed') OR terminal_result_digest IS NULL)
);
CREATE UNIQUE INDEX runs_one_nonterminal_task
	ON runs(tag, task_id) WHERE status IN ('running','retryable');
CREATE INDEX runs_task_history
	ON runs(tag, task_id, run_id);
CREATE INDEX runs_status_owner
	ON runs(status, owner, run_id);
CREATE INDEX attempts_running_expiry
	ON attempts(expires_at, run_id, attempt_no) WHERE status = 'running';
`

// schemaV2DDL is the exact schema written by the preceding released binary.
// It remains executable as a trusted catalog reference for mixed-version
// backup verification; new stores use schemaDDL below.
const schemaV2DDL = `
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tags (
	name            TEXT PRIMARY KEY,
	next_id         INTEGER NOT NULL,
	task_count      INTEGER NOT NULL,
	completed_count INTEGER NOT NULL,
	milestones      TEXT,
	extra           TEXT
);
CREATE TABLE IF NOT EXISTS tasks (
	tag               TEXT NOT NULL REFERENCES tags(name),
	id                TEXT NOT NULL,
	title             TEXT NOT NULL,
	description       TEXT NOT NULL,
	details           TEXT NOT NULL,
	status            TEXT NOT NULL CHECK(status IN ('pending','in-progress','review','done','deferred','cancelled','blocked')),
	priority          TEXT,
	milestone         TEXT,
	dependencies      TEXT NOT NULL,
	subtasks          TEXT NOT NULL,
	extra             TEXT,
	record_extra      TEXT,
	revision          INTEGER NOT NULL,
	updated_at        TEXT NOT NULL,
	claim_owner       TEXT,
	claim_token       INTEGER,
	claim_ttl         INTEGER,
	claim_acquired_at TEXT,
	claim_expires_at  TEXT,
	claim_extra       TEXT,
	wt_path           TEXT,
	wt_boundary       TEXT,
	wt_branch         TEXT,
	wt_created_at     TEXT,
	wt_keep           INTEGER,
	wt_extra          TEXT,
	PRIMARY KEY (tag, id)
);
CREATE TABLE IF NOT EXISTS events (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	tag        TEXT,
	task_id    TEXT,
	kind       TEXT NOT NULL,
	status     TEXT,
	owner      TEXT,
	revision   INTEGER,
	count      INTEGER,
	action     TEXT,
	extra      TEXT,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS receipts (
	kind       TEXT NOT NULL,
	receipt_id TEXT NOT NULL,
	request_id TEXT,
	body       TEXT NOT NULL,
	digest     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (kind, receipt_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS receipts_request_id
	ON receipts(kind, request_id) WHERE request_id IS NOT NULL;
`

// externalLinkSchemaDDL is exactly the additive v5→v6 external-link delta:
// the three link columns on tasks and the store-wide unique index on the
// (family, repository, issue) triple — §3's uniqueness mechanism, enforced
// inside every writing transaction. Fresh and upgraded stores apply it in
// the same position (appended after wt_extra), so their catalogs agree.
const externalLinkSchemaDDL = `
ALTER TABLE tasks ADD COLUMN link_family TEXT;
ALTER TABLE tasks ADD COLUMN link_repository TEXT;
ALTER TABLE tasks ADD COLUMN link_issue INTEGER;
CREATE UNIQUE INDEX IF NOT EXISTS tasks_external_link
	ON tasks(link_family, link_repository, link_issue)
	WHERE link_family IS NOT NULL AND link_repository IS NOT NULL AND link_issue IS NOT NULL;
`

// schemaV5DDL is the exact v5 catalog (the schema written by the preceding
// released binary), retained for mixed-version backup verification.
const schemaV5DDL = schemaV2DDL + orchestratorSchemaDDL + runSchemaDDL + worktreeSagaSchemaDDL

const schemaDDL = schemaV5DDL + externalLinkSchemaDDL

// Close closes the SQLite handle and only then releases the shared
// maintenance lock.
func (d *DB) Close() error {
	err := d.db.Close()
	if d.unlock != nil {
		d.unlock()
	}
	return err
}

// tx is one transaction scoped to a single connection.
type tx struct {
	c *sql.Conn
}

func (t *tx) Exec(query string, args ...any) (sql.Result, error) {
	return t.c.ExecContext(context.Background(), query, args...)
}

func (t *tx) Query(query string, args ...any) (*sql.Rows, error) {
	return t.c.QueryContext(context.Background(), query, args...)
}

func (t *tx) QueryRow(query string, args ...any) *sql.Row {
	return t.c.QueryRowContext(context.Background(), query, args...)
}

// transact runs fn on one pinned connection between begin and commit/rollback.
func (d *DB) transact(begin string, fn func(t *tx) error) (err error) {
	conn, err := d.db.Conn(context.Background())
	if err != nil {
		return fmt.Errorf("acquire task store connection: %w", err)
	}
	defer conn.Close()
	if _, err = conn.ExecContext(context.Background(), begin); err != nil {
		return fmt.Errorf("begin task store transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()
	if err = fn(&tx{c: conn}); err != nil {
		return err
	}
	if _, err = conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		return fmt.Errorf("commit task store transaction: %w", err)
	}
	committed = true
	return nil
}

// immediate runs fn inside one BEGIN IMMEDIATE transaction (SPEC §8.2: every
// mutation — read, validation, write, event append — commits as one unit).
// A normal mutation is also the §8.2 schema-upgrade point: an older released
// schema is migrated transactionally before the mutation's own transaction,
// minting the family identity with the configured owner boundary. A v1 store
// with no configured boundary fails closed.
func (d *DB) immediate(fn func(t *tx) error) error {
	if err := d.maybeUpgrade(d.ownerBoundary); err != nil {
		return err
	}
	return d.transact("BEGIN IMMEDIATE", fn)
}

// SetOwnerBoundary configures the canonical §8.1 owner boundary this handle
// uses when upgrading a v1 store and minting its family identity (SPEC §8.2).
// It must be called before the first mutation; a conflicting reconfiguration
// is a caller bug and fails closed.
func (d *DB) SetOwnerBoundary(owner string) error {
	if owner == "" {
		return errors.New("owner boundary must not be empty")
	}
	if d.ownerBoundary == "" {
		d.ownerBoundary = owner
		return nil
	}
	if d.ownerBoundary != owner {
		return fmt.Errorf("task store owner boundary reconfigured from %q to %q", d.ownerBoundary, owner)
	}
	return nil
}

// maybeUpgrade applies the transactional v1→v2→v3→v4→v5→v6 migrations when the open
// database predates this binary's schema (SPEC §8.2/§8.7: earlier releases'
// databases are upgraded transactionally; the upgrade preserves tasks,
// revisions, counters, milestones, claims, fencing values, and events). The
// whole migration — DDL, version stamp, identity mint, and the single
// schema/upgraded event — commits as one BEGIN IMMEDIATE transaction whose
// first act is to recheck user_version, so two racing first mutations produce
// exactly one upgrade event. A v1 store with no configured owner boundary fails
// closed: identity is never minted from a guessed boundary.
func (d *DB) maybeUpgrade(ownerBoundary string) error {
	var version int
	if err := d.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read task store schema version: %w", err)
	}
	if version >= SchemaVersion {
		return nil
	}
	return d.transact("BEGIN IMMEDIATE", func(t *tx) error {
		if err := t.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return fmt.Errorf("read task store schema version: %w", err)
		}
		if version >= SchemaVersion {
			// A concurrent writer completed the same upgrade between
			// the unlocked precheck and this lock; nothing to do.
			return nil
		}
		if version < identitySchemaVersion {
			if ownerBoundary == "" {
				return errors.New("task store schema is older than this binary and no owner boundary is configured to mint its family identity; run a mutating command from the repository family that owns it")
			}
			if _, err := t.Exec(upgradeV2DDL); err != nil {
				return fmt.Errorf("upgrade task store schema v1 to v2: %w", err)
			}
			if err := t.ensureIdentity(ownerBoundary); err != nil {
				return err
			}
			if _, err := t.Exec("PRAGMA user_version=2"); err != nil {
				return fmt.Errorf("set task store schema version 2: %w", err)
			}
			version = identitySchemaVersion
		}
		if version < orchestratorSchemaVersion {
			// v2 already owns identity. Validate it before changing schema;
			// ownerBoundary may be empty only for trusted staged restore paths.
			if err := t.ensureIdentity(ownerBoundary); err != nil {
				return err
			}
			if _, err := t.Exec(upgradeV3DDL); err != nil {
				return fmt.Errorf("upgrade task store schema v2 to v3: %w", err)
			}
			if _, err := t.Exec("PRAGMA user_version=3"); err != nil {
				return fmt.Errorf("set task store schema version 3: %w", err)
			}
			version = orchestratorSchemaVersion
		}
		if version < runSchemaVersion {
			// Every v3 authority owns identity. Validate it before deriving any
			// deterministic legacy claim ids or changing the catalog.
			if err := t.ensureIdentity(ownerBoundary); err != nil {
				return err
			}
			if err := validateFencingCounter(t); err != nil {
				return err
			}
			if err := d.upgradeClaimAcquisitionIDs(t); err != nil {
				return err
			}
			if _, err := t.Exec(upgradeV4DDL); err != nil {
				return fmt.Errorf("upgrade task store schema v3 to v4: %w", err)
			}
			if _, err := t.Exec("PRAGMA user_version=4"); err != nil {
				return fmt.Errorf("set task store schema version 4: %w", err)
			}
			version = runSchemaVersion
		}
		if version < worktreeSagaSchemaVersion {
			if err := t.ensureIdentity(ownerBoundary); err != nil {
				return err
			}
			if err := validateFencingCounter(t); err != nil {
				return err
			}
			if _, err := t.Exec(upgradeV5DDL); err != nil {
				return fmt.Errorf("upgrade task store schema v4 to v5: %w", err)
			}
			if err := upgradeLegacyWorktreeBindings(t); err != nil {
				return err
			}
			if _, err := t.Exec("PRAGMA user_version=5"); err != nil {
				return fmt.Errorf("set task store schema version 5: %w", err)
			}
			if err := validateWorktreeSagaState(t); err != nil {
				return fmt.Errorf("validate upgraded worktree saga state: %w", err)
			}
		}
		if version < externalLinkSchemaVersion {
			if err := t.ensureIdentity(ownerBoundary); err != nil {
				return err
			}
			if _, err := t.Exec(upgradeV6DDL); err != nil {
				return fmt.Errorf("upgrade task store schema v5 to v6: %w", err)
			}
			if _, err := t.Exec("PRAGMA user_version=6"); err != nil {
				return fmt.Errorf("set task store schema version 6: %w", err)
			}
		}
		return t.appendEvent(Event{Kind: "schema", Action: "upgraded", At: nowUTC()})
	})
}

// upgradeV2DDL is the v1→v2 schema delta (the receipts table and its replay
// uniqueness index; the tasks.record_extra column carrying unknown fields of
// the outer legacy task record; the v2 identity values live in the existing
// meta table).
const upgradeV2DDL = `
CREATE TABLE IF NOT EXISTS receipts (
	kind       TEXT NOT NULL,
	receipt_id TEXT NOT NULL,
	request_id TEXT,
	body       TEXT NOT NULL,
	digest     TEXT NOT NULL,
	created_at TEXT NOT NULL,
	PRIMARY KEY (kind, receipt_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS receipts_request_id
	ON receipts(kind, request_id) WHERE request_id IS NOT NULL;
ALTER TABLE tasks ADD COLUMN record_extra TEXT;
`

// upgradeV3DDL is the additive v2→v3 orchestrator schema migration.
const upgradeV3DDL = orchestratorSchemaDDL

// upgradeV4DDL is the additive v3→v4 run/attempt schema migration.
const upgradeV4DDL = runSchemaDDL

// upgradeV5DDL is the additive v4→v5 recoverable-worktree schema migration.
const upgradeV5DDL = worktreeSagaSchemaDDL

// upgradeV6DDL is the additive v5→v6 external-link schema migration.
const upgradeV6DDL = externalLinkSchemaDDL

// useLegacyClaimAcquisitionIDs selects the deterministic claim-id derivation
// required while privately upgrading an exact legacy-source v3 bundle. It
// must be called on the staged handle before its first mutation; it never
// changes a source authority or compatibility bundle in place.
func (d *DB) useLegacyClaimAcquisitionIDs() error {
	d.legacyClaimAcquisitionIDs = true
	return nil
}

// legacyClaimAcquisitionID implements the fixed §9.3 derivation. The domain
// already carries its trailing NUL; every tuple separator is one additional
// NUL, and acquiredAt terminates the input without a trailing separator.
func legacyClaimAcquisitionID(familyID, tag, taskID string, token int64, acquiredAt string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("noise-tasks-legacy-claim-acquisition-v1\x00"))
	_, _ = h.Write([]byte(familyID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(tag))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(taskID))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(strconv.FormatInt(token, 10)))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(acquiredAt))
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func validStoredAcquisitionID(id string) bool {
	return validAcquisitionID(id) && id == strings.ToLower(id)
}

// validateFencingCounter rejects SQLite's otherwise-permissive integer casts:
// the shared counter must exist as one canonical nonnegative base-10 int64.
func validateFencingCounter(t *tx) error {
	var raw string
	if err := t.QueryRow(`SELECT value FROM meta WHERE key='fencing'`).Scan(&raw); err != nil {
		return fmt.Errorf("validate fencing counter: %w", err)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != raw || n == int64(^uint64(0)>>1) {
		return fmt.Errorf("task store fencing counter %q is malformed, negative, overflowing, or exhausted", raw)
	}
	return nil
}

// upgradeClaimAcquisitionIDs validates every live claim identity and mints a
// missing one in the same v4 upgrade transaction. Unknown claim fields remain
// byte-valued JSON members; only acquisitionId is added.
func (d *DB) upgradeClaimAcquisitionIDs(t *tx) error {
	type recordedClaim struct {
		tag, task, acquiredAt string
		token                 int64
		extra                 sql.NullString
	}
	rows, err := t.Query(`SELECT tag, id, claim_token, claim_acquired_at, claim_extra
		FROM tasks WHERE claim_owner IS NOT NULL ORDER BY tag, id`)
	if err != nil {
		return fmt.Errorf("read claims for v4 upgrade: %w", err)
	}
	var claims []recordedClaim
	for rows.Next() {
		var c recordedClaim
		var token sql.NullInt64
		var acquiredAt sql.NullString
		if err := rows.Scan(&c.tag, &c.task, &token, &acquiredAt, &c.extra); err != nil {
			rows.Close()
			return fmt.Errorf("read claim for v4 upgrade: %w", err)
		}
		if !token.Valid || token.Int64 <= 0 || !acquiredAt.Valid || acquiredAt.String == "" {
			rows.Close()
			return fmt.Errorf("task %s/%s has malformed claim fencing or acquisition time", c.tag, c.task)
		}
		c.token, c.acquiredAt = token.Int64, acquiredAt.String
		claims = append(claims, c)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close v4 claim scan: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read claims for v4 upgrade: %w", err)
	}

	familyID := ""
	if d.legacyClaimAcquisitionIDs && len(claims) != 0 {
		if err := t.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&familyID); err != nil {
			return fmt.Errorf("read family identity for legacy claim upgrade: %w", err)
		}
	}
	seen := make(map[string]string, len(claims))
	for _, c := range claims {
		extra := map[string]json.RawMessage{}
		if c.extra.Valid && c.extra.String != "" {
			if err := json.Unmarshal([]byte(c.extra.String), &extra); err != nil || extra == nil {
				return fmt.Errorf("task %s/%s has malformed claim unknown fields", c.tag, c.task)
			}
		}
		raw, present := extra["acquisitionId"]
		id := ""
		if present {
			if err := json.Unmarshal(raw, &id); err != nil || !validStoredAcquisitionID(id) {
				return fmt.Errorf("task %s/%s has malformed claim acquisition id", c.tag, c.task)
			}
		} else if d.legacyClaimAcquisitionIDs {
			id = legacyClaimAcquisitionID(familyID, c.tag, c.task, c.token, c.acquiredAt)
		} else {
			for {
				id, err = newAcquisitionID()
				if err != nil {
					return err
				}
				if _, collision := seen[id]; !collision {
					break
				}
			}
		}
		if prior, collision := seen[id]; collision {
			return fmt.Errorf("claim acquisition id collision between %s and %s/%s", prior, c.tag, c.task)
		}
		seen[id] = c.tag + "/" + c.task
		if present {
			continue
		}
		extra["acquisitionId"] = json.RawMessage(strconv.Quote(id))
		encoded, err := encodeExtraChecked(extra)
		if err != nil {
			return fmt.Errorf("encode task %s/%s claim unknown fields: %w", c.tag, c.task, err)
		}
		if _, err := t.Exec(`UPDATE tasks SET claim_extra=? WHERE tag=? AND id=?`, encoded, c.tag, c.task); err != nil {
			return fmt.Errorf("store task %s/%s claim acquisition id: %w", c.tag, c.task, err)
		}
	}
	return nil
}

// deferred runs fn inside one deferred read transaction, giving a consistent
// snapshot across multiple statements (SPEC §8.2 read-only commands).
func (d *DB) deferred(fn func(t *tx) error) error {
	return d.transact("BEGIN DEFERRED", fn)
}
