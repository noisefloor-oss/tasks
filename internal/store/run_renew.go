package store

import (
	"fmt"
	"time"
)

type RunRenewResult struct {
	RunID            int64  `json:"runId"`
	Attempt          int64  `json:"attempt"`
	LeaseGeneration  int64  `json:"leaseGeneration"`
	ClaimToken       int64  `json:"claimToken"`
	ClaimExpiresAt   string `json:"claimExpiresAt"`
	AttemptToken     int64  `json:"attemptToken"`
	AttemptExpiresAt string `json:"attemptExpiresAt"`
}

// RenewRun jointly rotates the task claim and current attempt capabilities.
// It is the only permitted renewal while a run remains nonterminal.
func (d *DB) RenewRun(runID int64, owner string, claimToken, attempt, attemptToken int64,
	requestID string, ttl int) (*RunRenewResult, bool, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, false, fmt.Errorf("run owner %v", err)
	}
	if runID <= 0 || attempt <= 0 || claimToken <= 0 || attemptToken <= 0 {
		return nil, false, fmt.Errorf("run renew ids and tokens must be positive integers")
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
	r := runRequest{operation: "renew", requestID: requestID, owner: owner, runID: runID, attempt: attempt, ttl: ttl}
	inputDigest, err := runInputDigest(r)
	if err != nil {
		return nil, false, err
	}
	var result *RunRenewResult
	replayed := false
	err = d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		var found bool
		result, found, err = lookupRenewRunReplay(t, r, inputDigest, now)
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
		conflict := runCapabilityConflict("renew", runID, attempt)
		if state.Owner != owner || state.RunStatus != "running" || state.CurrentAttempt != attempt ||
			state.AttemptStatus != "running" || state.AttemptToken != attemptToken || state.ClaimToken != claimToken {
			return conflict
		}
		attemptExpiry, err := time.Parse(time.RFC3339, state.AttemptExpiresAt)
		if err != nil {
			return fmt.Errorf("run %d attempt %d has malformed expiry", runID, attempt)
		}
		if !now.Before(attemptExpiry) || state.LeaseGeneration == int64(^uint64(0)>>1) {
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
			claimAcquisitionID(task.Claim) != state.ClaimAcquisition {
			return conflict
		}
		newClaimToken, err := t.nextFence()
		if err != nil {
			return err
		}
		newAttemptToken, err := t.nextFence()
		if err != nil {
			return err
		}
		at := now.Format(time.RFC3339)
		expiresAt := now.Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
		claimResult, err := t.Exec(`UPDATE tasks SET claim_token=?,claim_ttl=?,claim_expires_at=?
			WHERE tag=? AND id=? AND claim_owner=? AND claim_token=?`,
			newClaimToken, ttl, expiresAt, state.Tag, state.Task, owner, claimToken)
		if err != nil {
			return fmt.Errorf("rotate claim for run %d: %w", runID, err)
		}
		if n, err := claimResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return conflict
		}
		attemptResult, err := t.Exec(`UPDATE attempts SET attempt_token=?,ttl_seconds=?,lease_generation=lease_generation+1,
			expires_at=?,updated_at=? WHERE run_id=? AND attempt_no=? AND status='running' AND attempt_token=? AND lease_generation=?`,
			newAttemptToken, ttl, expiresAt, at, runID, attempt, attemptToken, state.LeaseGeneration)
		if err != nil {
			return fmt.Errorf("rotate attempt %d for run %d: %w", attempt, runID, err)
		}
		if n, err := attemptResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return conflict
		}
		runResult, err := t.Exec(`UPDATE runs SET claim_token=?,updated_at=?
			WHERE run_id=? AND current_attempt=? AND status='running' AND claim_token=?`,
			newClaimToken, at, runID, attempt, claimToken)
		if err != nil {
			return fmt.Errorf("rotate captured claim for run %d: %w", runID, err)
		}
		if n, err := runResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return conflict
		}
		task.Claim.Token = newClaimToken
		task.Claim.TTL = ttl
		task.Claim.ExpiresAt = expiresAt
		generation := state.LeaseGeneration + 1
		result = &RunRenewResult{RunID: runID, Attempt: attempt, LeaseGeneration: generation,
			ClaimToken: newClaimToken, ClaimExpiresAt: expiresAt,
			AttemptToken: newAttemptToken, AttemptExpiresAt: expiresAt}
		receiptResult := &RunStartResult{RunID: runID, Attempt: attempt, LeaseGeneration: generation,
			AttemptToken: newAttemptToken, ExpiresAt: expiresAt}
		if err := writeRunningRunReceipt(t, r, inputDigest, task, task.Claim, receiptResult, state.AttemptID,
			"attempt", "renewed", at); err != nil {
			return err
		}
		return runFaultHook("before-commit")
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}
