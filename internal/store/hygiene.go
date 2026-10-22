package store

import "fmt"

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

// HygieneReport is the §8.9 read-only residue report. The arrays are
// always non-nil so JSON renders empty classes as [], never null, and
// never carry tokens or task text.
type HygieneReport struct {
	SchemaVersion       int                    `json:"schemaVersion"`
	Now                 string                 `json:"now"`
	StaleRuns           []HygieneStaleRun      `json:"staleRuns"`
	ExpiredClaims       []HygieneExpiredClaim  `json:"expiredClaims"`
	ClaimlessInProgress []HygieneClaimlessTask `json:"claimlessInProgress"`
}

// Hygiene reads the §8.9 residue classes in one deferred transaction,
// judging every expiry against the database process's UTC clock exactly
// like §8.3. It appends no event and mutates nothing.
func (d *DB) Hygiene() (*HygieneReport, error) {
	report := &HygieneReport{
		SchemaVersion:       1,
		StaleRuns:           []HygieneStaleRun{},
		ExpiredClaims:       []HygieneExpiredClaim{},
		ClaimlessInProgress: []HygieneClaimlessTask{},
	}
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
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return report, nil
}
