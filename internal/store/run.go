package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

const (
	ReceiptKindRun = "run"
	MaxRunTTL      = 86400
)

// runFaultHook is the deterministic rollback boundary for run mutations.
// Tests replace it briefly; production keeps the no-op default.
var runFaultHook = func(string) error { return nil }

// RunStartResult is the only capability-bearing result of StartRun. The
// attempt token is returned after commit and is never persisted in a receipt
// or event.
type RunStartResult struct {
	RunID           int64  `json:"runId"`
	Attempt         int64  `json:"attempt"`
	LeaseGeneration int64  `json:"leaseGeneration"`
	AttemptToken    int64  `json:"attemptToken"`
	ExpiresAt       string `json:"expiresAt"`
}

// RunConflict is a typed §9.3 conflict whose diagnostic contains only the
// caller's non-secret identifiers.
type RunConflict struct {
	Operation string
	Tag       string
	Task      string
	RunID     int64
	Attempt   int64
}

func (e *RunConflict) Error() string {
	if e.RunID > 0 && e.Attempt > 0 {
		return fmt.Sprintf("run %s conflict: run %d attempt %d", e.Operation, e.RunID, e.Attempt)
	}
	if e.RunID > 0 {
		return fmt.Sprintf("run %s conflict: run %d", e.Operation, e.RunID)
	}
	return fmt.Sprintf("run %s conflict: task %s in tag %q", e.Operation, e.Task, e.Tag)
}

// StartRun starts attempt 1 for one top-level task under its effective claim.
// The request id is replay-safe across the store-wide run receipt namespace.
func (d *DB) StartRun(tag, rawTask, owner string, claimToken int64, requestID string, ttl int) (*RunStartResult, bool, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, false, fmt.Errorf("run owner %v", err)
	}
	if !ValidLocalKey(requestID) {
		return nil, false, fmt.Errorf("run request id is invalid: must match %s", localKeyGrammar)
	}
	parentID, subtaskID, err := backlog.ParseTaskReference(rawTask)
	if err != nil {
		return nil, false, err
	}
	if subtaskID != "" {
		return nil, false, fmt.Errorf("run start requires a top-level task id")
	}
	if claimToken <= 0 {
		return nil, false, fmt.Errorf("run claim token must be a positive integer")
	}
	if ttl == 0 {
		ttl = DefaultClaimTTL
	}
	if ttl < 1 || ttl > MaxRunTTL {
		return nil, false, fmt.Errorf("run ttl must be between 1 and %d seconds", MaxRunTTL)
	}

	r := runRequest{
		operation: "start", requestID: requestID, tag: tag, task: parentID,
		owner: owner, ttl: ttl,
	}
	inputDigest, err := runInputDigest(r)
	if err != nil {
		return nil, false, err
	}
	var result *RunStartResult
	replayed := false
	err = d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		var found bool
		result, found, err = lookupStartRunReplay(t, r, inputDigest, now)
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

		task, err := t.getTask(tag, parentID)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: parentID}
		}
		expireClaim(task, now)
		if task.Claim == nil || task.Claim.Owner != owner || task.Claim.Token != claimToken {
			return &RunConflict{Operation: "start", Tag: tag, Task: parentID}
		}
		claimID := claimAcquisitionID(task.Claim)
		if !validStoredAcquisitionID(claimID) {
			return fmt.Errorf("task %s/%s has malformed claim acquisition id", tag, parentID)
		}
		var existing int64
		err = t.QueryRow(`SELECT run_id FROM runs
			WHERE tag=? AND task_id=? AND status IN ('running','retryable') LIMIT 1`, tag, parentID).Scan(&existing)
		switch {
		case err == nil:
			return &RunConflict{Operation: "start", Tag: tag, Task: parentID}
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("check nonterminal run for task %s: %w", parentID, err)
		}

		attemptID, err := newAcquisitionID()
		if err != nil {
			return err
		}
		attemptToken, err := t.nextFence()
		if err != nil {
			return err
		}
		at := now.Format(time.RFC3339)
		expiresAt := now.Add(time.Duration(ttl) * time.Second).Format(time.RFC3339)
		insert, err := t.Exec(`INSERT INTO runs(
			tag,task_id,owner,claim_acquisition_id,claim_token,status,current_attempt,
			start_request_id,terminal_result_digest,force_cancel_fence,started_at,updated_at,terminal_at,extra)
			VALUES (?,?,?,?,?,'running',1,?,NULL,NULL,?,?,NULL,NULL)`,
			tag, parentID, owner, claimID, claimToken, requestID, at, at)
		if err != nil {
			return fmt.Errorf("start run for task %s: %w", parentID, err)
		}
		runID, err := insert.LastInsertId()
		if err != nil {
			return fmt.Errorf("read allocated run id: %w", err)
		}
		if runID <= 0 {
			return fmt.Errorf("read allocated run id: invalid id %d", runID)
		}
		if _, err := t.Exec(`INSERT INTO attempts(
			run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
			created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
			VALUES (?,1,?,?,'running',?,1,?,?,?,?,NULL,NULL,NULL)`,
			runID, attemptID, attemptToken, ttl, requestID, at, expiresAt, at); err != nil {
			return fmt.Errorf("start attempt 1 for run %d: %w", runID, err)
		}

		result = &RunStartResult{
			RunID: runID, Attempt: 1, LeaseGeneration: 1,
			AttemptToken: attemptToken, ExpiresAt: expiresAt,
		}
		if err := writeStartRunReceipt(t, r, inputDigest, task, task.Claim, result, attemptID, at); err != nil {
			return err
		}
		return runFaultHook("before-commit")
	})
	if err != nil {
		return nil, false, err
	}
	return result, replayed, nil
}

// hasNonterminalRun reports whether one task is fenced by an active run. It
// is used by every live acquisition path so advertised work is actionable.
func (t *tx) hasNonterminalRun(tag, task string) (bool, error) {
	run, err := t.nonterminalRunForTask(tag, task)
	return run != nil, err
}

type activeRun struct {
	RunID         int64
	Attempt       int64
	RunStatus     string
	AttemptStatus string
}

func (t *tx) nonterminalRunForTask(tag, task string) (*activeRun, error) {
	var run activeRun
	err := t.QueryRow(`SELECT r.run_id,r.current_attempt,r.status,a.status
		FROM runs r JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
		WHERE r.tag=? AND r.task_id=? AND r.status IN ('running','retryable') LIMIT 1`, tag, task).
		Scan(&run.RunID, &run.Attempt, &run.RunStatus, &run.AttemptStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read nonterminal run for task %s/%s: %w", tag, task, err)
	}
	return &run, nil
}

// nonterminalRunTaskIDs snapshots the active-run exclusion for one tag in the
// caller's transaction.
func (t *tx) nonterminalRunTaskIDs(tag string) (map[string]bool, error) {
	rows, err := t.Query(`SELECT task_id FROM runs
		WHERE tag=? AND status IN ('running','retryable') ORDER BY CAST(task_id AS INTEGER)`, tag)
	if err != nil {
		return nil, fmt.Errorf("list nonterminal runs in tag %q: %w", tag, err)
	}
	defer rows.Close()
	ids := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

// forceCancelRunForRelease fences and terminates a task's nonterminal run as
// part of force release. Failed/lost attempt evidence remains byte-identical;
// a running attempt is cancelled at the shared operation timestamp. The
// terminalized run establishes the §9.4 automatic worktree cleanup authority
// in this same transaction.
func (t *tx) forceCancelRunForRelease(run *activeRun, tag, task, at string) error {
	if run == nil {
		return nil
	}
	switch {
	case run.RunStatus == "running" && run.AttemptStatus == "running":
		res, err := t.Exec(`UPDATE attempts SET status='cancelled',updated_at=?,terminal_at=?
			WHERE run_id=? AND attempt_no=? AND status='running'`, at, at, run.RunID, run.Attempt)
		if err != nil {
			return fmt.Errorf("cancel attempt %d for force release: %w", run.Attempt, err)
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("cancel attempt %d for force release: current attempt changed", run.Attempt)
		}
	case run.RunStatus == "retryable" && (run.AttemptStatus == "failed" || run.AttemptStatus == "lost"):
		// Preserve terminal attempt evidence exactly.
	default:
		return fmt.Errorf("run %d has invalid nonterminal current attempt state", run.RunID)
	}
	fence, err := t.nextFence()
	if err != nil {
		return err
	}
	res, err := t.Exec(`UPDATE runs SET status='cancelled',force_cancel_fence=?,updated_at=?,terminal_at=?
		WHERE run_id=? AND current_attempt=? AND status IN ('running','retryable')`,
		fence, at, at, run.RunID, run.Attempt)
	if err != nil {
		return fmt.Errorf("cancel run %d for force release: %w", run.RunID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("cancel run %d for force release: run changed", run.RunID)
	}
	if err := t.scheduleWorktreeAutoCleanup(tag, task, run.RunID, at); err != nil {
		return err
	}
	return nil
}

func refuseOrphanRunRequest(t *tx, requestID string) error {
	var found int
	err := t.QueryRow(`SELECT 1 WHERE
		EXISTS(SELECT 1 FROM runs WHERE start_request_id=?) OR
		EXISTS(SELECT 1 FROM attempts WHERE created_request_id=?)`, requestID, requestID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("check run request linkage: %w", err)
	}
	return receiptIntegrityError(ReceiptKindRun)
}

func runEventExtra(runID, attempt int64, receiptID, digest string) map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"runId":         json.RawMessage(strconv.FormatInt(runID, 10)),
		"attempt":       json.RawMessage(strconv.FormatInt(attempt, 10)),
		"receiptId":     json.RawMessage(strconv.Quote(receiptID)),
		"receiptDigest": json.RawMessage(strconv.Quote(digest)),
	}
}
