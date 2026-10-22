package store

import (
	"fmt"
	"time"
)

type RunTaskResult struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Revision int    `json:"revision"`
}

type RunFinishResult struct {
	RunID                int64          `json:"runId"`
	RunStatus            string         `json:"runStatus"`
	Attempt              int64          `json:"attempt"`
	AttemptStatus        string         `json:"attemptStatus"`
	Task                 *RunTaskResult `json:"task"`
	TerminalResultDigest *string        `json:"terminalResultDigest"`
}

func optionalRunDigest(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// FinishRun publishes the current live attempt's terminal outcome. An
// optional task status transition is committed under the same task revision
// and capability fences, but is summarized by the single run event.
func (d *DB) FinishRun(runID int64, owner string, claimToken, attempt, attemptToken int64,
	outcome, requestID, taskStatus string, revision int, terminalResultDigest string) (*RunFinishResult, bool, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, false, fmt.Errorf("run owner %v", err)
	}
	if runID <= 0 || attempt <= 0 || claimToken <= 0 || attemptToken <= 0 {
		return nil, false, fmt.Errorf("run finish ids and tokens must be positive integers")
	}
	if outcome != "succeeded" && outcome != "failed" {
		return nil, false, fmt.Errorf("run finish outcome must be succeeded or failed")
	}
	if !ValidLocalKey(requestID) {
		return nil, false, fmt.Errorf("run request id is invalid: must match %s", localKeyGrammar)
	}
	if (taskStatus == "") != (revision == 0) {
		return nil, false, fmt.Errorf("run finish task status and revision must be supplied together")
	}
	if taskStatus != "" && !validStatuses[taskStatus] {
		return nil, false, fmt.Errorf("invalid status %q", taskStatus)
	}
	if taskStatus != "" && revision < 1 {
		return nil, false, fmt.Errorf("run finish revision must be a positive integer")
	}
	if outcome == "failed" && taskStatus == "done" {
		return nil, false, fmt.Errorf("failed run finish cannot set task status done")
	}
	if terminalResultDigest != "" && !ValidDigest(terminalResultDigest) {
		return nil, false, fmt.Errorf("run finish result digest must be lowercase sha256:<64-hex>")
	}

	r := runRequest{operation: "finish", requestID: requestID, owner: owner, runID: runID,
		attempt: attempt, outcome: outcome, revision: revision, taskStatus: taskStatus,
		terminalResultDigest: terminalResultDigest}
	inputDigest, err := runInputDigest(r)
	if err != nil {
		return nil, false, err
	}
	var result *RunFinishResult
	replayed := false
	err = d.immediate(func(t *tx) error {
		var found bool
		result, found, err = lookupFinishRunReplay(t, r, inputDigest)
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
		conflict := runCapabilityConflict("finish", runID, attempt)
		task, err := t.getTask(state.Tag, state.Task)
		if err != nil {
			return err
		}
		if task == nil {
			return fmt.Errorf("run %d references missing task", runID)
		}
		// The caller's independently meaningful task CAS is checked before
		// capability diagnostics so a fence cannot serve as a revision oracle.
		if taskStatus != "" && task.Revision != revision {
			return conflict
		}
		now := time.Now().UTC()
		if state.Owner != owner || state.RunStatus != "running" || state.CurrentAttempt != attempt ||
			state.AttemptStatus != "running" || state.AttemptToken != attemptToken || state.ClaimToken != claimToken {
			return conflict
		}
		attemptExpiry, err := time.Parse(time.RFC3339, state.AttemptExpiresAt)
		if err != nil {
			return fmt.Errorf("run %d attempt %d has malformed expiry", runID, attempt)
		}
		if !now.Before(attemptExpiry) {
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
		claimSnapshot := cloneClaim(task.Claim)
		at := now.Format(time.RFC3339)
		attemptStatus := outcome
		runStatus := "retryable"
		var runTerminal any
		var runDigest any
		if outcome == "succeeded" {
			runStatus = "succeeded"
			runTerminal = at
			runDigest = nullString(terminalResultDigest)
		}

		var taskResult *RunTaskResult
		if taskStatus != "" {
			query := `UPDATE tasks SET status=?,revision=revision+1,updated_at=?`
			if taskStatus == "done" {
				query += `,claim_owner=NULL,claim_token=NULL,claim_ttl=NULL,claim_acquired_at=NULL,
					claim_expires_at=NULL,claim_extra=NULL`
			}
			query += ` WHERE tag=? AND id=? AND revision=? AND claim_owner=? AND claim_token=? AND claim_expires_at>?
				AND EXISTS(SELECT 1 FROM runs r JOIN attempts a
					ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
					WHERE r.run_id=? AND r.tag=tasks.tag AND r.task_id=tasks.id
					AND r.owner=? AND r.claim_acquisition_id=? AND r.claim_token=?
					AND r.status='running' AND r.current_attempt=?
					AND a.status='running' AND a.attempt_token=? AND a.expires_at>?)`
			taskWrite, err := t.Exec(query, taskStatus, at, task.Tag, task.ID, revision, owner, claimToken, at,
				runID, owner, state.ClaimAcquisition, claimToken, attempt, attemptToken, at)
			if err != nil {
				return fmt.Errorf("publish task status for run %d: %w", runID, err)
			}
			if n, err := taskWrite.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return conflict
			}
			task.Status = taskStatus
			task.UpdatedAt = at
			task.Revision++
			if taskStatus == "done" {
				task.Claim = nil
			}
			if err := t.recountTag(task.Tag); err != nil {
				return err
			}
			taskResult = &RunTaskResult{ID: task.ID, Status: task.Status, Revision: task.Revision}
		}
		attemptResult, err := t.Exec(`UPDATE attempts SET status=?,updated_at=?,terminal_at=?,terminal_result_digest=?
			WHERE run_id=? AND attempt_no=? AND status='running' AND attempt_token=? AND expires_at>?`,
			attemptStatus, at, at, nullString(terminalResultDigest), runID, attempt, attemptToken, at)
		if err != nil {
			return fmt.Errorf("finish attempt %d for run %d: %w", attempt, runID, err)
		}
		if n, err := attemptResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return conflict
		}
		runResult, err := t.Exec(`UPDATE runs SET status=?,terminal_result_digest=?,updated_at=?,terminal_at=?
			WHERE run_id=? AND owner=? AND claim_acquisition_id=? AND current_attempt=? AND status='running' AND claim_token=?`,
			runStatus, runDigest, at, runTerminal, runID, owner, state.ClaimAcquisition, attempt, claimToken)
		if err != nil {
			return fmt.Errorf("finish run %d: %w", runID, err)
		}
		if n, err := runResult.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return conflict
		}
		result = &RunFinishResult{RunID: runID, RunStatus: runStatus, Attempt: attempt,
			AttemptStatus: attemptStatus, Task: taskResult, TerminalResultDigest: optionalRunDigest(terminalResultDigest)}
		// §9.4: a run finish recording succeeded establishes the automatic
		// worktree cleanup authority in this same transaction, before the
		// receipt records the final state.
		if outcome == "succeeded" {
			if err := t.scheduleWorktreeAutoCleanup(state.Tag, state.Task, runID, at); err != nil {
				return err
			}
		}
		if err := writeFinishRunReceipt(t, r, inputDigest, task, claimSnapshot, state, result, at); err != nil {
			return err
		}
		return runFaultHook("before-commit")
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}
