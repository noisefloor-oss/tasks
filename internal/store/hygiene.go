package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// HygieneStaleRun is one §8.9 stale-run residue row: a running run whose
// current running attempt's lease has expired.
type HygieneStaleRun struct {
	RunID            int64  `json:"runId"`
	Tag              string `json:"tag"`
	Task             string `json:"task"`
	Owner            string `json:"owner"`
	Attempt          int64  `json:"attempt"`
	TTLSeconds       int64  `json:"ttlSeconds"`
	AttemptExpiresAt string `json:"attemptExpiresAt"`
}

// HygieneExpiredClaim is one §8.9 expired-claim residue row.
type HygieneExpiredClaim struct {
	Tag       string `json:"tag"`
	Task      string `json:"task"`
	Owner     string `json:"owner"`
	ExpiredAt string `json:"expiredAt"`
}

// HygieneClaimlessTask is one §8.9 in-progress task with no live claim.
type HygieneClaimlessTask struct {
	Tag       string `json:"tag"`
	Task      string `json:"task"`
	UpdatedAt string `json:"updatedAt"`
}

// HygieneRetryableRun is one §8.9 retryable-run row: a run whose attempt
// terminalized and whose orchestrator owns an explicit retry-or-cancel
// decision. It is its own class, never lease-stale residue.
type HygieneRetryableRun struct {
	RunID     int64  `json:"runId"`
	Tag       string `json:"tag"`
	Task      string `json:"task"`
	Owner     string `json:"owner"`
	Attempt   int64  `json:"attempt"`
	UpdatedAt string `json:"updatedAt"`
}

// HygieneStuckSaga is one §8.9 stuck worktree saga: a nonterminal saga
// whose live effect never reconciled. Path and branch presence are
// stated explicitly, so a missing retained branch that blocks the §9.4
// removal proof is a fact the report names, never a silence — and a
// confirmed-absent path with no registration is visible as cleanup
// already finished out-of-band.
type HygieneStuckSaga struct {
	SagaID       int64  `json:"sagaId"`
	RunID        *int64 `json:"runId"`
	Tag          string `json:"tag"`
	Task         string `json:"task"`
	Operation    string `json:"operation"`
	Status       string `json:"status"`
	Path         string `json:"path"`
	PathExists   bool   `json:"pathExists"`
	Branch       string `json:"branch"`
	BranchExists bool   `json:"branchExists"`
	UpdatedAt    string `json:"updatedAt"`
}

// HygieneReport is the §8.9 read-only residue report. The arrays are
// always non-nil so JSON renders empty classes as [], never null, and
// never carry tokens or task text.
type HygieneReport struct {
	SchemaVersion       int                    `json:"schemaVersion"`
	Now                 string                 `json:"now"`
	StaleRuns           []HygieneStaleRun      `json:"staleRuns"`
	ExpiredClaims       []HygieneExpiredClaim  `json:"expiredClaims"`
	ClaimlessInProgress []HygieneClaimlessTask `json:"claimlessInProgress"`
	RetryableRuns       []HygieneRetryableRun  `json:"retryableRuns"`
	Sagas               []HygieneStuckSaga     `json:"sagas"`
}

// Hygiene reads the §8.9 residue classes in one deferred transaction,
// judging every expiry against the database process's UTC clock exactly
// like §8.3. It appends no event and mutates nothing. Stuck-saga path
// and branch presence are read-only Git/filesystem observations made
// after the read transaction closes, never inside it; a Git operational
// failure is an error, never a guessed absence.
func (d *DB) Hygiene() (*HygieneReport, error) {
	report := &HygieneReport{
		SchemaVersion:       2,
		StaleRuns:           []HygieneStaleRun{},
		ExpiredClaims:       []HygieneExpiredClaim{},
		ClaimlessInProgress: []HygieneClaimlessTask{},
		RetryableRuns:       []HygieneRetryableRun{},
		Sagas:               []HygieneStuckSaga{},
	}
	type sagaIdentity struct {
		idx      int
		boundary string
	}
	var identities []sagaIdentity
	err := d.deferred(func(t *tx) error {
		if err := t.QueryRow(`SELECT strftime('%Y-%m-%dT%H:%M:%SZ','now')`).Scan(&report.Now); err != nil {
			return fmt.Errorf("read hygiene clock: %w", err)
		}
		rows, err := t.Query(`
			SELECT r.run_id, r.tag, r.task_id, r.owner, r.current_attempt,
			       a.ttl_seconds, a.expires_at
			FROM runs r
			JOIN attempts a ON a.run_id = r.run_id AND a.attempt_no = r.current_attempt
			WHERE r.status = 'running' AND a.status = 'running' AND a.expires_at <= ?
			ORDER BY r.run_id`, report.Now)
		if err != nil {
			return fmt.Errorf("read stale runs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row HygieneStaleRun
			if err := rows.Scan(&row.RunID, &row.Tag, &row.Task, &row.Owner,
				&row.Attempt, &row.TTLSeconds, &row.AttemptExpiresAt); err != nil {
				return err
			}
			report.StaleRuns = append(report.StaleRuns, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = t.Query(`
			SELECT tag, id, claim_owner, claim_expires_at
			FROM tasks
			WHERE claim_owner IS NOT NULL AND claim_expires_at <= ?
			ORDER BY tag, id`, report.Now)
		if err != nil {
			return fmt.Errorf("read expired claims: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row HygieneExpiredClaim
			if err := rows.Scan(&row.Tag, &row.Task, &row.Owner, &row.ExpiredAt); err != nil {
				return err
			}
			report.ExpiredClaims = append(report.ExpiredClaims, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = t.Query(`
			SELECT tag, id, updated_at
			FROM tasks
			WHERE status = 'in-progress'
			  AND (claim_owner IS NULL OR claim_expires_at <= ?)
			ORDER BY tag, id`, report.Now)
		if err != nil {
			return fmt.Errorf("read claimless in-progress tasks: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row HygieneClaimlessTask
			if err := rows.Scan(&row.Tag, &row.Task, &row.UpdatedAt); err != nil {
				return err
			}
			report.ClaimlessInProgress = append(report.ClaimlessInProgress, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = t.Query(`
			SELECT run_id, tag, task_id, owner, current_attempt, updated_at
			FROM runs
			WHERE status = 'retryable'
			ORDER BY run_id`)
		if err != nil {
			return fmt.Errorf("read retryable runs: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row HygieneRetryableRun
			if err := rows.Scan(&row.RunID, &row.Tag, &row.Task, &row.Owner,
				&row.Attempt, &row.UpdatedAt); err != nil {
				return err
			}
			report.RetryableRuns = append(report.RetryableRuns, row)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = t.Query(`
			SELECT saga_id, run_id, tag, task_id, operation, status,
			       owner_boundary, target_path, branch, updated_at
			FROM worktree_sagas
			WHERE status IN ('pending','needs-broker','cleanup-required')
			ORDER BY saga_id`)
		if err != nil {
			return fmt.Errorf("read stuck worktree sagas: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row HygieneStuckSaga
			var boundary string
			if err := rows.Scan(&row.SagaID, &row.RunID, &row.Tag, &row.Task,
				&row.Operation, &row.Status, &boundary, &row.Path, &row.Branch, &row.UpdatedAt); err != nil {
				return err
			}
			identities = append(identities, sagaIdentity{idx: len(report.Sagas), boundary: boundary})
			report.Sagas = append(report.Sagas, row)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	// Path and branch presence are read-only observations made after the
	// read transaction closes; a Git operational failure is an error,
	// never a guessed absence.
	for _, id := range identities {
		row := &report.Sagas[id.idx]
		row.PathExists = !pathMissing(row.Path)
		exists, err := worktreeBranchExists(id.boundary, row.Branch)
		if err != nil {
			return nil, fmt.Errorf("read stuck saga %d branch: %w", row.SagaID, err)
		}
		row.BranchExists = exists
	}
	return report, nil
}

// pathMissing reports whether the path is confirmed gone: only
// fs.ErrNotExist is missing, read with Lstat so a dangling symlink is
// the presence removal recovery also refuses, never an absence. Any
// other stat failure leaves the path existing-but-unexaminable, judged
// never guessed.
func pathMissing(path string) bool {
	_, err := os.Lstat(path)
	return errors.Is(err, fs.ErrNotExist)
}
