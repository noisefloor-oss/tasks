package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorktreeRemoveOptions carries the current controller authorization for one
// manual removal. The immutable effect paths/base are never caller overrides:
// the planning transaction snapshots them from the exact active provision.
type WorktreeRemoveOptions struct {
	RunID        int64
	Owner        string
	ClaimToken   int64
	Attempt      int64
	AttemptToken int64
	RequestID    string
	Force        bool
}

// WorktreeRemovePlan is the committed handoff across the SQLite/external
// effect boundary. It contains the exact source identity needed by the
// trusted idempotent remover and no planning claim/attempt capabilities.
type WorktreeRemovePlan struct {
	SagaID             int64
	SagaToken          int64
	SourceSagaID       int64
	RunID              int64
	Tag                string
	Task               string
	Owner              string
	RequestID          string
	OwnerBoundary      string
	RegistrationRoot   string
	TargetPath         string
	Branch             string
	BaseCommit         string
	Force              bool
	RecoveryGeneration int64
	EffectRequired     bool
	Result             *WorktreeMutationResult
}

// InspectWorktreeRemoval performs the read-only half of trusted-host
// preflight. It returns the immutable active-source identity that a later
// planning transaction would snapshot, without validating capabilities,
// allocating a fence, or mutating the store. The planning transaction remains
// the sole authorization point and the caller must compare its committed plan
// with this inspection before performing an effect.
func (d *DB) InspectWorktreeRemoval(o WorktreeRemoveOptions) (*WorktreeRemovePlan, error) {
	if err := validateWorktreeRemoveOptions(o); err != nil {
		return nil, err
	}
	var inspected *WorktreeRemovePlan
	err := d.deferred(func(t *tx) error {
		s, err := loadWorktreeSagaByRequest(t, o.RequestID)
		if err != nil {
			return err
		}
		if s != nil {
			if !removalIdentityMatches(s, o) {
				return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: o.RequestID}
			}
			_, owner, _, _, err := validateLinkedRemoval(t, s)
			if err != nil {
				return err
			}
			if owner != o.Owner {
				return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: o.RequestID}
			}
			inspected = removePlanFromSaga(s, owner)
			inspected.EffectRequired = s.status == "pending" && s.generation == 0
			if !inspected.EffectRequired {
				inspected.Result, err = worktreeResultFromSaga(t, s, s.fence)
			}
			return err
		}

		var tag, task, owner string
		if err := t.QueryRow(`SELECT tag,task_id,owner FROM runs WHERE run_id=?`, o.RunID).
			Scan(&tag, &task, &owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &RunConflict{Operation: "remove", RunID: o.RunID, Attempt: o.Attempt}
			}
			return fmt.Errorf("read removal run: %w", err)
		}
		if owner != o.Owner {
			return &RunConflict{Operation: "remove", RunID: o.RunID, Attempt: o.Attempt}
		}
		var sourceID int64
		if err := t.QueryRow(`SELECT saga_id FROM worktree_sagas
			WHERE run_id=? AND tag=? AND task_id=? AND operation='provision'
			AND status='bound' AND binding_active=1`, o.RunID, tag, task).Scan(&sourceID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &WorktreeConflict{Kind: "absent", Tag: tag, ID: task}
			}
			return fmt.Errorf("read active worktree provision: %w", err)
		}
		source, err := loadWorktreeSaga(t, sourceID)
		if err != nil {
			return err
		}
		active := true
		if sourceOwner, _, _, err := validateBoundProvisionSource(t, source, &active); err != nil {
			return err
		} else if sourceOwner != o.Owner {
			return &RunConflict{Operation: "remove", RunID: o.RunID, Attempt: o.Attempt}
		}
		inspected = &WorktreeRemovePlan{SourceSagaID: source.id, RunID: o.RunID,
			Tag: source.tag, Task: source.task, Owner: owner, RequestID: o.RequestID,
			OwnerBoundary: source.boundary, RegistrationRoot: source.registration.String,
			TargetPath: source.path, Branch: source.branch, BaseCommit: source.base.String,
			Force: o.Force, EffectRequired: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return inspected, nil
}

// WorktreeRemoveOutcome is the trusted host's observation after the exact
// idempotent removal effect completes outside SQLite.
type WorktreeRemoveOutcome string

const (
	WorktreeRemoveRemoved         WorktreeRemoveOutcome = "removed"
	WorktreeRemoveNeedsBroker     WorktreeRemoveOutcome = "needs-broker"
	WorktreeRemoveFailed          WorktreeRemoveOutcome = "failed"
	WorktreeRemoveCleanupRequired WorktreeRemoveOutcome = "cleanup-required"
)

// WorktreeRemovalFailedForbidden reports §9.4 cleanup authority: while the
// source run is terminal or its task is done, removal may terminalize only as
// removed. The diagnostic carries no capability or effect path.
type WorktreeRemovalFailedForbidden struct{ SagaID int64 }

func (e *WorktreeRemovalFailedForbidden) Error() string {
	return fmt.Sprintf("worktree removal saga %d cannot fail while cleanup is required", e.SagaID)
}

// WorktreeRemoveStatusError reports an invalid trusted-host observation.
type WorktreeRemoveStatusError struct{ Status WorktreeRemoveOutcome }

func (e *WorktreeRemoveStatusError) Error() string {
	return fmt.Sprintf("worktree remove outcome %q is invalid", e.Status)
}

// worktreeRemoveFaultHook pins the two SQLite crash boundaries. Production
// keeps the no-op and no external effect is ever called through this hook.
var worktreeRemoveFaultHook = func(string) error { return nil }

func validateWorktreeRemoveOptions(o WorktreeRemoveOptions) error {
	if err := ValidateOwnerLabel(o.Owner); err != nil {
		return fmt.Errorf("worktree owner %v", err)
	}
	if o.RunID <= 0 || o.ClaimToken <= 0 || o.Attempt <= 0 || o.AttemptToken <= 0 {
		return fmt.Errorf("worktree run, claim token, attempt, and attempt token must be positive integers")
	}
	if !ValidLocalKey(o.RequestID) {
		return fmt.Errorf("worktree request id is invalid: must match %s", localKeyGrammar)
	}
	if strings.HasPrefix(o.RequestID, "auto-") || strings.HasPrefix(o.RequestID, "recover-") {
		return fmt.Errorf("worktree request id uses a reserved prefix")
	}
	return nil
}

func validateBoundProvisionSource(t *tx, source *worktreeSagaState, active *bool) (owner, runStatus, taskStatus string, err error) {
	if source == nil || source.operation != "provision" || source.status != "bound" ||
		!source.run.Valid || source.source.Valid || active != nil && source.active != *active ||
		!source.registration.Valid || !source.base.Valid {
		return "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	var runTag, runTask string
	err = t.QueryRow(`SELECT r.owner,r.tag,r.task_id,r.status,t.status FROM runs r
		JOIN tasks t ON t.tag=r.tag AND t.id=r.task_id WHERE r.run_id=?`, source.run.Int64).
		Scan(&owner, &runTag, &runTask, &runStatus, &taskStatus)
	if err != nil || runTag != source.tag || runTask != source.task {
		return "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	inputDigest, err := worktreeInputDigest(owner, source.request, source.run.Int64, false, "provision")
	if err != nil {
		return "", "", "", err
	}
	receipt, err := t.findVerifiedReceipt(ReceiptKindWorktree, source.request)
	if err != nil || receipt == nil || validateOriginalWorktreeReceipt(t, receipt, source, inputDigest) != nil {
		return "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	return owner, runStatus, taskStatus, nil
}

func removalMatchesSource(removal, source *worktreeSagaState) bool {
	return removal != nil && source != nil && removal.operation == "remove" &&
		removal.run.Valid && source.run.Valid && removal.run.Int64 == source.run.Int64 &&
		removal.source.Valid && removal.source.Int64 == source.id && source.id < removal.id &&
		removal.tag == source.tag && removal.task == source.task &&
		removal.boundary == source.boundary && removal.registration == source.registration &&
		removal.path == source.path && removal.branch == source.branch && removal.base == source.base
}

func validateLinkedRemoval(t *tx, removal *worktreeSagaState) (*worktreeSagaState, string, string, string, error) {
	if removal == nil || !removal.source.Valid || !removal.run.Valid {
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	source, err := loadWorktreeSaga(t, removal.source.Int64)
	if err != nil || !removalMatchesSource(removal, source) {
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	var expectedActive *bool
	if removal.status != "failed" {
		active := removal.status != "removed"
		expectedActive = &active
	}
	owner, runStatus, taskStatus, err := validateBoundProvisionSource(t, source, expectedActive)
	if err != nil {
		return nil, "", "", "", err
	}
	var removedID sql.NullInt64
	if err := t.QueryRow(`SELECT saga_id FROM worktree_sagas
		WHERE source_saga_id=? AND status='removed'`, source.id).Scan(&removedID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", "", err
	}
	switch {
	case source.active && removedID.Valid:
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	case !source.active && !removedID.Valid:
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	case removal.status == "removed" && (!removedID.Valid || removedID.Int64 != removal.id):
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	case removal.status == "failed" && !source.active && removedID.Int64 <= removal.id:
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	case removal.status != "removed" && removal.status != "failed" && removedID.Valid:
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	return source, owner, runStatus, taskStatus, nil
}

func removalIdentityMatches(s *worktreeSagaState, o WorktreeRemoveOptions) bool {
	return s != nil && s.operation == "remove" && s.run.Valid && s.run.Int64 == o.RunID &&
		s.source.Valid && s.request == o.RequestID && s.force == o.Force && !s.active
}

func removePlanFromSaga(s *worktreeSagaState, owner string) *WorktreeRemovePlan {
	return &WorktreeRemovePlan{
		SagaID: s.id, SagaToken: s.fence, SourceSagaID: s.source.Int64,
		RunID: s.run.Int64, Tag: s.tag, Task: s.task, Owner: owner, RequestID: s.request,
		OwnerBoundary: s.boundary, RegistrationRoot: s.registration.String,
		TargetPath: s.path, Branch: s.branch, BaseCommit: s.base.String, Force: s.force,
		RecoveryGeneration: s.generation,
	}
}

// PlanWorktreeRemoval atomically authorizes one direct manual removal,
// snapshots the exact active provision, and persists pending before any
// Git/filesystem effect. Original replay never allocates a second saga.
func (d *DB) PlanWorktreeRemoval(o WorktreeRemoveOptions) (*WorktreeRemovePlan, error) {
	if err := validateWorktreeRemoveOptions(o); err != nil {
		return nil, err
	}
	inputDigest, err := worktreeInputDigest(o.Owner, o.RequestID, o.RunID, o.Force, "remove")
	if err != nil {
		return nil, err
	}
	var plan *WorktreeRemovePlan
	err = d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		s, err := loadWorktreeSagaByRequest(t, o.RequestID)
		if err != nil {
			return err
		}
		if s != nil {
			if !removalIdentityMatches(s, o) {
				return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: o.RequestID}
			}
			_, owner, _, _, err := validateLinkedRemoval(t, s)
			if err != nil {
				return err
			}
			if owner != o.Owner {
				return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: o.RequestID}
			}
			task, err := t.getTask(s.tag, s.task)
			if err != nil || task == nil {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			gate := &ClaimGate{Owner: o.Owner, Token: o.ClaimToken, RunID: o.RunID,
				Attempt: o.Attempt, AttemptToken: o.AttemptToken}
			if _, err := t.taskMutationRunFence(task, gate, now, "remove"); err != nil {
				return err
			}
			plan = removePlanFromSaga(s, owner)
			receipt, err := t.findVerifiedReceipt(ReceiptKindWorktree, o.RequestID)
			if err != nil {
				return err
			}
			if receipt != nil {
				if err := validateOriginalWorktreeReceipt(t, receipt, s, inputDigest); err != nil {
					return err
				}
				plan.Result, err = worktreeResultFromReceipt(receipt, s.fence)
				return err
			}
			if isTerminalWorktreeStatus(s.status) {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			if s.status == "pending" && s.generation == 0 {
				plan.EffectRequired = true
				return nil
			}
			plan.Result, err = worktreeResultFromSaga(t, s, s.fence)
			return err
		}

		var receiptExists int
		err = t.QueryRow(`SELECT 1 FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindWorktree, o.RequestID).Scan(&receiptExists)
		if err == nil {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check worktree receipt reservation: %w", err)
		}
		var tag, taskID, runOwner string
		if err := t.QueryRow(`SELECT tag,task_id,owner FROM runs WHERE run_id=?`, o.RunID).
			Scan(&tag, &taskID, &runOwner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &RunConflict{Operation: "remove", RunID: o.RunID, Attempt: o.Attempt}
			}
			return fmt.Errorf("read removal run: %w", err)
		}
		if runOwner != o.Owner {
			return &RunConflict{Operation: "remove", RunID: o.RunID, Attempt: o.Attempt}
		}
		task, err := t.getTask(tag, taskID)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: taskID}
		}
		gate := &ClaimGate{Owner: o.Owner, Token: o.ClaimToken, RunID: o.RunID,
			Attempt: o.Attempt, AttemptToken: o.AttemptToken}
		fence, err := t.taskMutationRunFence(task, gate, now, "remove")
		if err != nil {
			return err
		}
		if fence == nil || fence.runID != o.RunID {
			return &RunConflict{Operation: "remove", RunID: o.RunID, Attempt: o.Attempt}
		}
		if task.Status == "done" {
			return &WorktreeConflict{Kind: "history", Tag: tag, ID: taskID}
		}
		var sourceID int64
		if err := t.QueryRow(`SELECT saga_id FROM worktree_sagas
			WHERE run_id=? AND tag=? AND task_id=? AND operation='provision'
			AND status='bound' AND binding_active=1`, o.RunID, tag, taskID).Scan(&sourceID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &WorktreeConflict{Kind: "absent", Tag: tag, ID: taskID}
			}
			return fmt.Errorf("read active worktree provision: %w", err)
		}
		source, err := loadWorktreeSaga(t, sourceID)
		if err != nil {
			return err
		}
		sourceActive := true
		owner, _, _, err := validateBoundProvisionSource(t, source, &sourceActive)
		if err != nil {
			return err
		}
		if owner != o.Owner {
			return &RunConflict{Operation: "remove", RunID: o.RunID, Attempt: o.Attempt}
		}
		var existing int64
		err = t.QueryRow(`SELECT saga_id FROM worktree_sagas WHERE tag=? AND task_id=?
			AND status IN ('pending','needs-broker','cleanup-required') LIMIT 1`, tag, taskID).Scan(&existing)
		if err == nil {
			return &WorktreeConflict{Kind: "exists", Tag: tag, ID: taskID}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check live worktree effect: %w", err)
		}
		sagaFence, err := t.nextFence()
		if err != nil {
			return err
		}
		at := now.Format(time.RFC3339)
		insert, err := t.Exec(`INSERT INTO worktree_sagas(
			run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
			request_id,recovery_generation,owner_boundary,registration_root,target_path,
			branch,base_commit,force,created_at,updated_at,extra)
			VALUES (?,?,?,?,'remove','pending',0,?,?,0,?,?,?,?,?,?,?, ?,NULL)`,
			o.RunID, source.id, source.tag, source.task, sagaFence, o.RequestID,
			source.boundary, source.registration.String, source.path, source.branch,
			source.base.String, o.Force, at, at)
		if err != nil {
			return fmt.Errorf("plan worktree removal: %w", err)
		}
		sagaID, err := insert.LastInsertId()
		if err != nil || sagaID <= 0 {
			return fmt.Errorf("read allocated worktree removal saga id")
		}
		if err := t.appendEvent(Event{Task: source.task, Tag: source.tag, Kind: ReceiptKindWorktree,
			Action: "removing", Owner: o.Owner, At: at}); err != nil {
			return err
		}
		if err := worktreeRemoveFaultHook("planned-before-commit"); err != nil {
			return err
		}
		plan = &WorktreeRemovePlan{SagaID: sagaID, SagaToken: sagaFence, SourceSagaID: source.id,
			RunID: o.RunID, Tag: source.tag, Task: source.task, Owner: o.Owner, RequestID: o.RequestID,
			OwnerBoundary: source.boundary, RegistrationRoot: source.registration.String,
			TargetPath: source.path, Branch: source.branch, BaseCommit: source.base.String,
			Force: o.Force, RecoveryGeneration: 0, EffectRequired: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func removePlanMatchesSaga(plan *WorktreeRemovePlan, s *worktreeSagaState) bool {
	return plan != nil && s != nil && s.operation == "remove" && s.run.Valid && s.source.Valid &&
		plan.SagaID == s.id && plan.SagaToken == s.fence && plan.SourceSagaID == s.source.Int64 &&
		plan.RunID == s.run.Int64 && plan.Tag == s.tag && plan.Task == s.task && plan.OwnerBoundary == s.boundary &&
		s.registration.Valid && plan.RegistrationRoot == s.registration.String && plan.TargetPath == s.path &&
		plan.Branch == s.branch && s.base.Valid && plan.BaseCommit == s.base.String &&
		plan.RequestID == s.request && plan.Force == s.force && plan.RecoveryGeneration == s.generation
}

// FinalizeWorktreeRemoval records one observed initial removal outcome. It
// never revalidates claim/attempt authority and never performs an external
// effect. Removed deactivates the exact source in this same transaction.
func (d *DB) FinalizeWorktreeRemoval(plan *WorktreeRemovePlan, outcome WorktreeRemoveOutcome) (*WorktreeMutationResult, error) {
	switch outcome {
	case WorktreeRemoveRemoved, WorktreeRemoveNeedsBroker, WorktreeRemoveFailed, WorktreeRemoveCleanupRequired:
	default:
		return nil, &WorktreeRemoveStatusError{Status: outcome}
	}
	if plan == nil || plan.SagaID <= 0 || plan.SagaToken <= 0 || plan.SourceSagaID <= 0 {
		return nil, fmt.Errorf("worktree remove plan is invalid")
	}
	var result *WorktreeMutationResult
	err := d.immediate(func(t *tx) error {
		s, err := loadWorktreeSagaByRequest(t, plan.RequestID)
		if err != nil {
			return err
		}
		if !removePlanMatchesSaga(plan, s) {
			return &WorktreeSagaConflict{SagaID: plan.SagaID}
		}
		source, owner, runStatus, taskStatus, err := validateLinkedRemoval(t, s)
		if err != nil {
			return err
		}
		if owner != plan.Owner {
			return &WorktreeSagaConflict{SagaID: plan.SagaID}
		}
		if s.status != "pending" {
			if s.status != string(outcome) {
				return &WorktreeSagaConflict{SagaID: s.id}
			}
			if isTerminalWorktreeStatus(s.status) {
				receipt, err := t.findVerifiedReceipt(ReceiptKindWorktree, s.request)
				if err != nil || receipt == nil {
					return receiptIntegrityError(ReceiptKindWorktree)
				}
				inputDigest, err := worktreeInputDigest(owner, s.request, s.run.Int64, s.force, "remove")
				if err != nil || validateOriginalWorktreeReceipt(t, receipt, s, inputDigest) != nil {
					return receiptIntegrityError(ReceiptKindWorktree)
				}
				result, err = worktreeResultFromReceipt(receipt, s.fence)
				return err
			}
			result, err = worktreeResultFromSaga(t, s, s.fence)
			return err
		}
		if outcome == WorktreeRemoveFailed &&
			(taskStatus == "done" || runStatus != "running" && runStatus != "retryable") {
			return &WorktreeRemovalFailedForbidden{SagaID: s.id}
		}
		at := nowUTC()
		updated, err := t.Exec(`UPDATE worktree_sagas SET status=?,updated_at=?
			WHERE saga_id=? AND saga_fence=? AND operation='remove' AND status='pending'
			AND recovery_generation=0 AND run_id=? AND source_saga_id=? AND tag=? AND task_id=?
			AND request_id=? AND owner_boundary=? AND registration_root=? AND target_path=?
			AND branch=? AND base_commit=? AND force=? AND binding_active=0`,
			string(outcome), at, s.id, s.fence, s.run.Int64, s.source.Int64, s.tag, s.task,
			s.request, s.boundary, s.registration.String, s.path, s.branch, s.base.String, s.force)
		if err != nil {
			return fmt.Errorf("finalize worktree removal saga %d: %w", s.id, err)
		}
		if n, err := updated.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return &WorktreeSagaConflict{SagaID: s.id}
		}
		if outcome == WorktreeRemoveRemoved {
			deactivated, err := t.Exec(`UPDATE worktree_sagas SET binding_active=0
				WHERE saga_id=? AND saga_fence=? AND operation='provision' AND status='bound'
				AND binding_active=1 AND run_id=? AND source_saga_id IS NULL AND tag=? AND task_id=?
				AND owner_boundary=? AND registration_root=? AND target_path=? AND branch=? AND base_commit=?
				AND NOT EXISTS(SELECT 1 FROM worktree_sagas child WHERE child.source_saga_id=?
					AND child.status='removed' AND child.saga_id<>?)`,
				source.id, source.fence, source.run.Int64, source.tag, source.task,
				source.boundary, source.registration.String, source.path, source.branch,
				source.base.String, source.id, s.id)
			if err != nil {
				return fmt.Errorf("deactivate worktree provision saga %d: %w", source.id, err)
			}
			if n, err := deactivated.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return &WorktreeSagaConflict{SagaID: s.id}
			}
			source.active = false
		}
		s.status, s.updated = string(outcome), at
		if isTerminalWorktreeStatus(s.status) {
			if err := writeOriginalWorktreeReceipt(t, s, owner, at); err != nil {
				return err
			}
		} else {
			if err := t.appendEvent(Event{Task: s.task, Tag: s.tag, Kind: ReceiptKindWorktree,
				Action: s.status, Owner: owner, At: at}); err != nil {
				return err
			}
		}
		if err := worktreeRemoveFaultHook("finalized-before-commit"); err != nil {
			return err
		}
		result, err = worktreeResultFromSaga(t, s, s.fence)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
