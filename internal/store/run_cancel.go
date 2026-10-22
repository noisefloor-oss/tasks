package store

import (
	"fmt"
	"time"
)

type RunCancelResult struct {
	RunID         int64  `json:"runId"`
	RunStatus     string `json:"runStatus"`
	Attempt       int64  `json:"attempt"`
	AttemptStatus string `json:"attemptStatus"`
	Forced        bool   `json:"forced"`
}

// CancelRun terminates a nonterminal run while preserving failed/lost
// attempt evidence. Force mode is an operator path and therefore accepts no
// owner or capability values; its allocated fence is never returned.
func (d *DB) CancelRun(runID int64, owner string, claimToken, attempt, attemptToken int64,
	force bool, requestID string) (*RunCancelResult, bool, error) {
	if runID <= 0 {
		return nil, false, fmt.Errorf("run cancel id must be a positive integer")
	}
	if !ValidLocalKey(requestID) {
		return nil, false, fmt.Errorf("run request id is invalid: must match %s", localKeyGrammar)
	}
	if force {
		if owner != "" || claimToken != 0 || attempt != 0 || attemptToken != 0 {
			return nil, false, fmt.Errorf("forced run cancel accepts no owner or capabilities")
		}
	} else {
		if err := ValidateOwnerLabel(owner); err != nil {
			return nil, false, fmt.Errorf("run owner %v", err)
		}
		if claimToken <= 0 || attempt <= 0 || attemptToken <= 0 {
			return nil, false, fmt.Errorf("run cancel ids and tokens must be positive integers")
		}
	}
	r := runRequest{operation: "cancel", requestID: requestID, owner: owner, runID: runID,
		attempt: attempt, force: force}
	inputDigest, err := runInputDigest(r)
	if err != nil {
		return nil, false, err
	}
	var result *RunCancelResult
	replayed := false
	err = d.immediate(func(t *tx) error {
		var found bool
		result, found, err = lookupCancelRunReplay(t, r, inputDigest)
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
		conflict := runCapabilityConflict("cancel", runID, attempt)
		validPair := state.RunStatus == "running" && state.AttemptStatus == "running" ||
			state.RunStatus == "retryable" && (state.AttemptStatus == "failed" || state.AttemptStatus == "lost")
		if !validPair {
			return conflict
		}
		task, err := t.getTask(state.Tag, state.Task)
		if err != nil {
			return err
		}
		if task == nil {
			return fmt.Errorf("run %d references missing task", runID)
		}
		now := time.Now().UTC()
		at := now.Format(time.RFC3339)
		var claimSnapshot *Claim
		var attemptExpiry time.Time
		if !force {
			if state.Owner != owner || state.CurrentAttempt != attempt || state.AttemptToken != attemptToken ||
				state.ClaimToken != claimToken {
				return conflict
			}
			if task.Claim != nil {
				claimExpiry, err := time.Parse(time.RFC3339, task.Claim.ExpiresAt)
				if err != nil {
					return fmt.Errorf("task %s/%s has malformed claim expiry", task.Tag, task.ID)
				}
				if !now.Before(claimExpiry) {
					task.Claim = nil
				}
			}
			if task.Claim == nil || task.Claim.Owner != owner || task.Claim.Token != claimToken ||
				claimAcquisitionID(task.Claim) != state.ClaimAcquisition {
				return conflict
			}
			claimSnapshot = cloneClaim(task.Claim)
			attemptExpiry, err = time.Parse(time.RFC3339, state.AttemptExpiresAt)
			if err != nil {
				return fmt.Errorf("run %d attempt %d has malformed expiry", runID, attempt)
			}
		}

		if state.AttemptStatus == "running" {
			query := `UPDATE attempts SET status='cancelled',updated_at=?,terminal_at=?
				WHERE run_id=? AND attempt_no=? AND status='running'`
			args := []any{at, at, runID, state.CurrentAttempt}
			if force {
				query += ` AND attempt_token=?`
				args = append(args, state.AttemptToken)
			} else {
				if !now.Before(attemptExpiry) {
					return conflict
				}
				query += ` AND attempt_token=? AND expires_at>?`
				args = append(args, attemptToken, at)
			}
			attemptWrite, err := t.Exec(query, args...)
			if err != nil {
				return fmt.Errorf("cancel attempt %d for run %d: %w", state.CurrentAttempt, runID, err)
			}
			if n, err := attemptWrite.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return conflict
			}
		}

		var forceFence any
		if force {
			fence, err := t.nextFence()
			if err != nil {
				return err
			}
			forceFence = fence
		}
		query := `UPDATE runs SET status='cancelled',terminal_result_digest=NULL,force_cancel_fence=?,updated_at=?,terminal_at=?
			WHERE run_id=? AND current_attempt=? AND status=?`
		args := []any{forceFence, at, at, runID, state.CurrentAttempt, state.RunStatus}
		if force {
			query += ` AND owner=? AND claim_acquisition_id=? AND claim_token=?`
			args = append(args, state.Owner, state.ClaimAcquisition, state.ClaimToken)
		} else {
			query += ` AND owner=? AND claim_acquisition_id=? AND claim_token=?`
			args = append(args, owner, state.ClaimAcquisition, claimToken)
		}
		runWrite, err := t.Exec(query, args...)
		if err != nil {
			return fmt.Errorf("cancel run %d: %w", runID, err)
		}
		if n, err := runWrite.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return conflict
		}
		attemptStatus := state.AttemptStatus
		if attemptStatus == "running" {
			attemptStatus = "cancelled"
		}
		result = &RunCancelResult{RunID: runID, RunStatus: "cancelled", Attempt: state.CurrentAttempt,
			AttemptStatus: attemptStatus, Forced: force}
		// §9.4: cancelling terminalizes the run and establishes the
		// automatic worktree cleanup authority in this same transaction,
		// before the receipt records the final state.
		if err := t.scheduleWorktreeAutoCleanup(state.Tag, state.Task, runID, at); err != nil {
			return err
		}
		if err := writeCancelRunReceipt(t, r, inputDigest, task, claimSnapshot, state, result, at); err != nil {
			return err
		}
		return runFaultHook("before-commit")
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}
