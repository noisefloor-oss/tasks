package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type taskRunFence struct {
	runID            int64
	attempt          int64
	attemptToken     int64
	owner            string
	claimAcquisition string
	claimToken       int64
}

var taskMutationFaultHook = func(*tx, string) error { return nil }

func ValidateTaskRunGate(gate *ClaimGate) error {
	if gate == nil {
		return nil
	}
	ownerSet := gate.Owner != ""
	tokenSet := gate.Token != 0
	if ownerSet != tokenSet {
		return fmt.Errorf("claim owner and token must be supplied together")
	}
	if ownerSet {
		if err := ValidateOwnerLabel(gate.Owner); err != nil {
			return fmt.Errorf("claim owner %v", err)
		}
		if gate.Token <= 0 {
			return fmt.Errorf("claim token must be a positive integer")
		}
	}
	set := 0
	if gate.RunID != 0 {
		set++
	}
	if gate.Attempt != 0 {
		set++
	}
	if gate.AttemptToken != 0 {
		set++
	}
	if set != 0 && set != 3 {
		return fmt.Errorf("run, attempt, and attempt token must be supplied together")
	}
	if set == 3 && (gate.RunID <= 0 || gate.Attempt <= 0 || gate.AttemptToken <= 0) {
		return fmt.Errorf("run, attempt, and attempt token must be positive integers")
	}
	if set == 3 && !ownerSet {
		return fmt.Errorf("run mutation gate requires claim owner and token")
	}
	return nil
}

func suppliedTaskRunGate(gate *ClaimGate) bool {
	return gate != nil && gate.RunID > 0 && gate.Attempt > 0 && gate.AttemptToken > 0
}

func taskRunConflict(operation, tag, task string, gate *ClaimGate) error {
	if suppliedTaskRunGate(gate) {
		return &RunConflict{Operation: operation, RunID: gate.RunID, Attempt: gate.Attempt}
	}
	return &RunConflict{Operation: operation, Tag: tag, Task: task}
}

func taskClaimGate(task *Task, gate *ClaimGate, operation string) error {
	if task.Claim != nil && (gate == nil || task.Claim.Owner != gate.Owner || task.Claim.Token != gate.Token) {
		return &ClaimConflict{Tag: task.Tag, ID: task.ID, Kind: taskClaimConflictKind(operation),
			Owner: task.Claim.Owner, Expiry: task.Claim.ExpiresAt, FencingToken: task.Claim.Token}
	}
	return nil
}

func taskClaimConflictKind(operation string) string {
	if operation == "remove" {
		return "update"
	}
	return operation
}

func (t *tx) taskMutationWriteConflict(fence *taskRunFence, claim *Claim, gate *ClaimGate,
	operation, tag, task string, revision int) error {
	if fence != nil {
		return taskRunConflict(operation, tag, task, gate)
	}
	if claim != nil {
		return &ClaimConflict{Tag: tag, ID: task, Kind: taskClaimConflictKind(operation),
			Owner: claim.Owner, Expiry: claim.ExpiresAt, FencingToken: claim.Token}
	}
	return t.staleConflict(tag, task, revision)
}

func (t *tx) hasTaskRun(tag, task string) (bool, error) {
	var found int
	err := t.QueryRow(`SELECT 1 FROM runs WHERE tag=? AND task_id=? LIMIT 1`, tag, task).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read run history for task %s/%s: %w", tag, task, err)
	}
	return true, nil
}

func (t *tx) hasTaskWorktreeSaga(tag, task string) (bool, error) {
	var found int
	err := t.QueryRow(`SELECT 1 FROM worktree_sagas WHERE tag=? AND task_id=? LIMIT 1`, tag, task).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read worktree history for task %s/%s: %w", tag, task, err)
	}
	return true, nil
}

// taskMutationRunFence resolves the effective live attempt, if any, and
// verifies only caller-supplied capabilities. Missing attempt credentials do
// not reveal the current run or attempt in the conflict diagnostic.
func (t *tx) taskMutationRunFence(task *Task, gate *ClaimGate, now time.Time, operation string) (*taskRunFence, error) {
	if task.Claim != nil {
		claimExpiry, err := time.Parse(time.RFC3339, task.Claim.ExpiresAt)
		if err != nil {
			return nil, fmt.Errorf("task %s/%s has malformed claim expiry", task.Tag, task.ID)
		}
		if !now.Before(claimExpiry) {
			task.Claim = nil
		}
	}
	var fence taskRunFence
	var runStatus, attemptStatus, attemptExpires string
	var attemptCount, greatestAttempt int64
	var badPredecessor bool
	err := t.QueryRow(`SELECT r.run_id,r.current_attempt,a.attempt_token,r.owner,r.claim_acquisition_id,
		r.claim_token,r.status,a.status,a.expires_at,
		(SELECT COUNT(*) FROM attempts history WHERE history.run_id=r.run_id),
		(SELECT MAX(history.attempt_no) FROM attempts history WHERE history.run_id=r.run_id),
		EXISTS(SELECT 1 FROM attempts history WHERE history.run_id=r.run_id
			AND history.attempt_no<>r.current_attempt AND history.status NOT IN ('failed','lost'))
		FROM runs r JOIN attempts a
		ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
		WHERE r.tag=? AND r.task_id=? AND r.status IN ('running','retryable') LIMIT 1`, task.Tag, task.ID).
		Scan(&fence.runID, &fence.attempt, &fence.attemptToken, &fence.owner, &fence.claimAcquisition,
			&fence.claimToken, &runStatus, &attemptStatus, &attemptExpires, &attemptCount, &greatestAttempt,
			&badPredecessor)
	if errors.Is(err, sql.ErrNoRows) {
		if suppliedTaskRunGate(gate) {
			return nil, taskRunConflict(operation, task.Tag, task.ID, gate)
		}
		return nil, taskClaimGate(task, gate, operation)
	}
	if err != nil {
		return nil, fmt.Errorf("read live run fence for task %s/%s: %w", task.Tag, task.ID, err)
	}
	coherentStatus := (runStatus == "running" && attemptStatus == "running") ||
		(runStatus == "retryable" && (attemptStatus == "failed" || attemptStatus == "lost"))
	if greatestAttempt != fence.attempt || attemptCount != fence.attempt || badPredecessor || !coherentStatus {
		return nil, fmt.Errorf("run state invariant failed: task %s/%s has incoherent nonterminal attempt history", task.Tag, task.ID)
	}
	expires, err := time.Parse(time.RFC3339, attemptExpires)
	if err != nil {
		return nil, fmt.Errorf("run %d attempt %d has malformed expiry", fence.runID, fence.attempt)
	}
	if task.Claim != nil && (task.Claim.Owner != fence.owner || task.Claim.Token != fence.claimToken ||
		claimAcquisitionID(task.Claim) != fence.claimAcquisition) {
		return nil, fmt.Errorf("run state invariant failed: task %s/%s claim does not match its running run", task.Tag, task.ID)
	}
	effective := runStatus == "running" && now.Before(expires) && task.Claim != nil
	if !effective {
		if suppliedTaskRunGate(gate) {
			return nil, taskRunConflict(operation, task.Tag, task.ID, gate)
		}
		return nil, taskClaimGate(task, gate, operation)
	}
	if !suppliedTaskRunGate(gate) || gate.Owner != fence.owner || gate.Token != fence.claimToken ||
		gate.RunID != fence.runID || gate.Attempt != fence.attempt ||
		gate.AttemptToken != fence.attemptToken {
		return nil, taskRunConflict(operation, task.Tag, task.ID, gate)
	}
	return &fence, nil
}
