package store

import (
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// ValidateStore checks the complete physical authority before verify renders
// its task projection. It deliberately accepts each released catalog that the
// current binary can read, while rejecting hidden schema objects, SQLite
// integrity or foreign-key failures, malformed logical records, and v4's
// cross-row run invariants.
func (d *DB) ValidateStore() error {
	if err := d.IntegrityCheck(); err != nil {
		return err
	}
	if err := d.ForeignKeyCheck(); err != nil {
		return err
	}
	version, err := d.userVersion()
	if err != nil {
		return fmt.Errorf("read schema version for store validation: %w", err)
	}
	// Released schema v1 predates identity, receipts, and the exact backup
	// catalog. It remains a deliberately read-only compatibility surface until
	// the first mutation upgrades it transactionally, so verify applies the
	// SQLite checks above and the ordinary task projection below without
	// requiring v2+ digest or catalog records that do not exist yet.
	if version == 1 {
		return nil
	}
	if err := d.validateSchemaVersion(version); err != nil {
		return err
	}
	if err := d.ValidateRunState(); err != nil {
		return err
	}
	if err := d.ValidateRunReceipts(); err != nil {
		return err
	}
	if err := d.ValidateWorktreeSagaState(); err != nil {
		return err
	}
	// The digest traversal validates strict logical records and flattened
	// unknown-field collision rules for every persisted class.
	if _, _, _, err := d.ClassDigests(); err != nil {
		return err
	}
	return nil
}

// ValidateRunState checks the §9.3 cross-row invariants that cannot be
// expressed by schema-v4 CHECK, foreign-key, and uniqueness constraints.
// Older trusted catalogs have no physical run state and validate vacuously.
func (d *DB) ValidateRunState() error {
	return d.deferred(func(t *tx) error {
		var version int
		if err := t.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read schema version for run validation: %w", err)
		}
		if version < runSchemaVersion {
			return nil
		}
		return validateRunState(t)
	})
}

// ValidateRunReceipts checks every run lineage and every stored run receipt.
// Older trusted catalogs have no physical run state and validate vacuously.
func (d *DB) ValidateRunReceipts() error {
	return d.deferred(func(t *tx) error {
		var version int
		if err := t.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
			return fmt.Errorf("read schema version for run receipt validation: %w", err)
		}
		if version < runSchemaVersion {
			return nil
		}
		return validateRunReceipts(t)
	})
}

func validateRunReceipts(t *tx) error {
	rows, err := t.Query(`SELECT run_id FROM runs ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("list runs for receipt validation: %w", err)
	}
	var orderedRunIDs []int64
	runIDs := map[int64]bool{}
	for rows.Next() {
		var runID int64
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return fmt.Errorf("read run for receipt validation: %w", err)
		}
		orderedRunIDs = append(orderedRunIDs, runID)
		runIDs[runID] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("list runs for receipt validation: %w", err)
	}
	rows.Close()
	for _, runID := range orderedRunIDs {
		if _, err := readRun(t, runID); err != nil {
			return fmt.Errorf("validate run %d receipts: %w", runID, err)
		}
	}

	receiptRows, err := t.Query(`SELECT request_id FROM receipts WHERE kind=? ORDER BY request_id`, ReceiptKindRun)
	if err != nil {
		return fmt.Errorf("list run receipts for store validation: %w", err)
	}
	var requestIDs []string
	for receiptRows.Next() {
		var requestID sql.NullString
		if err := receiptRows.Scan(&requestID); err != nil {
			receiptRows.Close()
			return fmt.Errorf("read run receipt for store validation: %w", err)
		}
		if !requestID.Valid {
			receiptRows.Close()
			return receiptIntegrityError(ReceiptKindRun)
		}
		requestIDs = append(requestIDs, requestID.String)
	}
	if err := receiptRows.Err(); err != nil {
		receiptRows.Close()
		return fmt.Errorf("list run receipts for store validation: %w", err)
	}
	receiptRows.Close()
	for _, requestID := range requestIDs {
		receipt, err := t.findVerifiedReceipt(ReceiptKindRun, requestID)
		if err != nil {
			return err
		}
		if receipt == nil {
			return receiptIntegrityError(ReceiptKindRun)
		}
		runID, ok := receiptInt64(receipt["runId"])
		if !ok || !runIDs[runID] {
			return fmt.Errorf("validate run receipt %q: %w", requestID, receiptIntegrityError(ReceiptKindRun))
		}
	}
	return nil
}

func validateRunState(t *tx) error {
	checks := []struct {
		name  string
		query string
	}{
		{
			name: "current attempt is greatest",
			query: `SELECT r.run_id
				FROM runs r
				WHERE r.current_attempt != (
					SELECT MAX(a.attempt_no) FROM attempts a WHERE a.run_id=r.run_id
				)
				LIMIT 1`,
		},
		{
			name: "attempt numbers are contiguous from one",
			query: `SELECT a.run_id
				FROM attempts a
				WHERE a.attempt_no > 1
				  AND NOT EXISTS (
					SELECT 1 FROM attempts prior
					WHERE prior.run_id=a.run_id AND prior.attempt_no=a.attempt_no-1
				  )
				LIMIT 1`,
		},
		{
			name: "current run and attempt statuses are coherent",
			query: `SELECT r.run_id
				FROM runs r
				JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
				WHERE NOT (
					(r.status='running' AND a.status='running') OR
					(r.status='retryable' AND a.status IN ('failed','lost')) OR
					(r.status='succeeded' AND a.status='succeeded') OR
					(r.status='cancelled' AND a.status IN ('cancelled','failed','lost'))
				)
				LIMIT 1`,
		},
		{
			name: "non-current attempts are failed or lost",
			query: `SELECT a.run_id
				FROM attempts a
				JOIN runs r ON r.run_id=a.run_id
				WHERE a.attempt_no != r.current_attempt
				  AND a.status NOT IN ('failed','lost')
				LIMIT 1`,
		},
	}
	if err := validateRunTimestamps(t); err != nil {
		return err
	}
	for _, check := range checks {
		var runID int64
		err := t.QueryRow(check.query).Scan(&runID)
		switch {
		case err == nil:
			return fmt.Errorf("run state invariant failed: %s (run %d)", check.name, runID)
		case err == sql.ErrNoRows:
			// No violation.
		case err != nil:
			return fmt.Errorf("check run state invariant %q: %w", check.name, err)
		}
	}

	var rawCounter string
	if err := t.QueryRow(`SELECT value FROM meta WHERE key='fencing'`).Scan(&rawCounter); err != nil {
		return fmt.Errorf("read fencing counter for run validation: %w", err)
	}
	counter, err := strconv.ParseInt(rawCounter, 10, 64)
	if err != nil || counter < 0 || strconv.FormatInt(counter, 10) != rawCounter {
		return fmt.Errorf("run state invariant failed: fencing counter is not a canonical nonnegative integer")
	}
	if counter == int64(^uint64(0)>>1) {
		return fmt.Errorf("run state invariant failed: fencing counter is exhausted")
	}

	var maxFence sql.NullInt64
	if err := t.QueryRow(`SELECT MAX(fence) FROM (
		SELECT claim_token AS fence FROM tasks WHERE claim_token IS NOT NULL
		UNION ALL SELECT claim_token FROM runs
		UNION ALL SELECT attempt_token FROM attempts
		UNION ALL SELECT force_cancel_fence FROM runs WHERE force_cancel_fence IS NOT NULL
	)`).Scan(&maxFence); err != nil {
		return fmt.Errorf("read allocated fencing floor: %w", err)
	}
	if maxFence.Valid && counter < maxFence.Int64 {
		return fmt.Errorf("run state invariant failed: fencing counter %d is below allocated fence %d", counter, maxFence.Int64)
	}
	return nil
}

func validRunTimestamp(raw string) bool {
	_, err := time.Parse(time.RFC3339, raw)
	return err == nil
}

func validateRunTimestamps(t *tx) error {
	runRows, err := t.Query(`SELECT run_id, started_at, updated_at, terminal_at FROM runs ORDER BY run_id`)
	if err != nil {
		return fmt.Errorf("read run timestamps: %w", err)
	}
	for runRows.Next() {
		var runID int64
		var started, updated string
		var terminal sql.NullString
		if err := runRows.Scan(&runID, &started, &updated, &terminal); err != nil {
			runRows.Close()
			return fmt.Errorf("read run timestamp: %w", err)
		}
		if !validRunTimestamp(started) || !validRunTimestamp(updated) || (terminal.Valid && !validRunTimestamp(terminal.String)) {
			runRows.Close()
			return fmt.Errorf("run state invariant failed: run %d has a non-RFC3339 timestamp", runID)
		}
	}
	if err := runRows.Err(); err != nil {
		runRows.Close()
		return fmt.Errorf("read run timestamps: %w", err)
	}
	runRows.Close()

	attemptRows, err := t.Query(`SELECT run_id, attempt_no, started_at, expires_at, updated_at, terminal_at FROM attempts ORDER BY run_id, attempt_no`)
	if err != nil {
		return fmt.Errorf("read attempt timestamps: %w", err)
	}
	defer attemptRows.Close()
	for attemptRows.Next() {
		var runID, attempt int64
		var started, expires, updated string
		var terminal sql.NullString
		if err := attemptRows.Scan(&runID, &attempt, &started, &expires, &updated, &terminal); err != nil {
			return fmt.Errorf("read attempt timestamp: %w", err)
		}
		if !validRunTimestamp(started) || !validRunTimestamp(expires) || !validRunTimestamp(updated) || (terminal.Valid && !validRunTimestamp(terminal.String)) {
			return fmt.Errorf("run state invariant failed: run %d attempt %d has a non-RFC3339 timestamp", runID, attempt)
		}
	}
	return attemptRows.Err()
}
