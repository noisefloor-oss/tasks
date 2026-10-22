package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type currentRunAttempt struct {
	RunID            int64
	Tag              string
	Task             string
	Owner            string
	ClaimAcquisition string
	ClaimToken       int64
	RunStatus        string
	CurrentAttempt   int64
	RunUpdatedAt     string
	AttemptID        string
	AttemptToken     int64
	AttemptStatus    string
	AttemptTTL       int64
	LeaseGeneration  int64
	CreatedRequestID string
	AttemptStartedAt string
	AttemptExpiresAt string
	AttemptUpdatedAt string
}

func (t *tx) currentRunAttempt(runID int64) (*currentRunAttempt, error) {
	var s currentRunAttempt
	err := t.QueryRow(`SELECT r.run_id,r.tag,r.task_id,r.owner,r.claim_acquisition_id,r.claim_token,
		r.status,r.current_attempt,r.updated_at,a.acquisition_id,a.attempt_token,a.status,
		a.ttl_seconds,a.lease_generation,a.created_request_id,a.started_at,a.expires_at,a.updated_at
		FROM runs r JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
		WHERE r.run_id=?`, runID).Scan(&s.RunID, &s.Tag, &s.Task, &s.Owner,
		&s.ClaimAcquisition, &s.ClaimToken, &s.RunStatus, &s.CurrentAttempt, &s.RunUpdatedAt,
		&s.AttemptID, &s.AttemptToken, &s.AttemptStatus, &s.AttemptTTL, &s.LeaseGeneration,
		&s.CreatedRequestID, &s.AttemptStartedAt, &s.AttemptExpiresAt, &s.AttemptUpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read run %d current attempt: %w", runID, err)
	}
	return &s, nil
}

func runCapabilityConflict(operation string, runID, attempt int64) error {
	return &RunConflict{Operation: operation, RunID: runID, Attempt: attempt}
}

// RetryRun replaces the current expired/failed/lost attempt with a fresh
// running successor. The returned capability is replayable only while that
// exact successor and its originating claim remain current and live.
func (d *DB) RetryRun(runID int64, owner string, claimToken, attempt, attemptToken int64,
	requestID string, ttl int) (*RunStartResult, bool, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, false, fmt.Errorf("run owner %v", err)
	}
	if runID <= 0 || attempt <= 0 || claimToken <= 0 || attemptToken <= 0 {
		return nil, false, fmt.Errorf("run retry ids and tokens must be positive integers")
	}
	if !ValidLocalKey(requestID) {
		return nil, false, fmt.Errorf("run request id is invalid: must match %s", localKeyGrammar)
	}
	if ttl == 0 {
		ttl = DefaultClaimTTL
	}
	if ttl < 1 || ttl > MaxRunTTL {
		return nil, false, fmt.Errorf("run ttl must be between 1 and %d seconds", MaxRunTTL)
	}
	r := runRequest{operation: "retry", requestID: requestID, owner: owner, runID: runID, attempt: attempt, ttl: ttl}
	inputDigest, err := runInputDigest(r)
	if err != nil {
		return nil, false, err
	}
	var result *RunStartResult
	replayed := false
	err = d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		var found bool
		result, found, err = lookupRetryRunReplay(t, r, inputDigest, now)
		if err != nil {
			return err
		}
		if found {
			replayed = true
			return nil
		}
		if err := refuseOrphanRunRequest(t, requestID); err != nil {
			return err
		}
		state, err := t.currentRunAttempt(runID)
		if err != nil {
			return err
		}
		if state == nil {
			return fmt.Errorf("run %d not found", runID)
		}
		conflict := runCapabilityConflict("retry", runID, attempt)
		if state.Owner != owner || state.CurrentAttempt != attempt || state.AttemptToken != attemptToken {
			return conflict
		}
		task, err := t.getTask(state.Tag, state.Task)
		if err != nil {
			return err
		}
		if task == nil {
			return fmt.Errorf("run %d references missing task", runID)
		}
		expireClaim(task, now)
		if task.Claim == nil || task.Claim.Owner != owner || task.Claim.Token != claimToken ||
			claimAcquisitionID(task.Claim) != state.ClaimAcquisition || state.ClaimToken != claimToken {
			return conflict
		}
		attemptExpiry, err := time.Parse(time.RFC3339, state.AttemptExpiresAt)
		if err != nil {
			return fmt.Errorf("run %d attempt %d has malformed expiry", runID, attempt)
		}
		expiredRunning := state.RunStatus == "running" && state.AttemptStatus == "running" && !now.Before(attemptExpiry)
		terminalRetryable := state.RunStatus == "retryable" &&
			(state.AttemptStatus == "failed" || state.AttemptStatus == "lost")
		if !expiredRunning && !terminalRetryable {
			return conflict
		}
		if attempt == int64(^uint64(0)>>1) {
			return conflict
		}
		at := now.Format(time.RFC3339)
		if expiredRunning {
			res, err := t.Exec(`UPDATE attempts SET status='lost',updated_at=?,terminal_at=?
				WHERE run_id=? AND attempt_no=? AND status='running' AND attempt_token=?`,
				at, at, runID, attempt, attemptToken)
			if err != nil {
				return fmt.Errorf("mark expired attempt %d lost: %w", attempt, err)
			}
			if n, err := res.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return conflict
			}
		}
		nextAttempt := attempt + 1
		attemptID, err := newAcquisitionID()
		if err != nil {
			return err
		}
		newToken, err := t.nextFence()
		if err != nil {
			return err
		}
		expiresAt := now.Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
		if _, err := t.Exec(`INSERT INTO attempts(
			run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
			created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
			VALUES (?,?,?,?, 'running',?,1,?,?,?,?,NULL,NULL,NULL)`,
			runID, nextAttempt, attemptID, newToken, ttl, requestID, at, expiresAt, at); err != nil {
			return fmt.Errorf("create retry attempt %d for run %d: %w", nextAttempt, runID, err)
		}
		res, err := t.Exec(`UPDATE runs SET status='running',current_attempt=?,updated_at=?
			WHERE run_id=? AND current_attempt=? AND status IN ('running','retryable')`,
			nextAttempt, at, runID, attempt)
		if err != nil {
			return fmt.Errorf("advance run %d to attempt %d: %w", runID, nextAttempt, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return conflict
		}
		result = &RunStartResult{RunID: runID, Attempt: nextAttempt, LeaseGeneration: 1,
			AttemptToken: newToken, ExpiresAt: expiresAt}
		if err := writeRunningRunReceipt(t, r, inputDigest, task, task.Claim, result, attemptID,
			"attempt", "retried", at); err != nil {
			return err
		}
		return runFaultHook("before-commit")
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}
