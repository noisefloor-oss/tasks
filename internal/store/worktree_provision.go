package store

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// WorktreeProvisionOptions contains the caller authorization and immutable
// effect identity for the planning transaction. Paths and the base are
// derived and preflighted by the trusted host before this API is called; this
// layer verifies their canonical relationship to the durable run and family.
type WorktreeProvisionOptions struct {
	RunID            int64
	Owner            string
	ClaimToken       int64
	Attempt          int64
	AttemptToken     int64
	RequestID        string
	OwnerBoundary    string
	RegistrationRoot string
	TargetPath       string
	Branch           string
	BaseCommit       string
}

// WorktreeBrokerRequest is the non-secret §9.4 broker mint input. It never
// contains a claim, run, attempt, saga, broker, or credential capability.
type WorktreeBrokerRequest struct {
	FamilyID         string  `json:"familyId"`
	Verb             string  `json:"verb"`
	RegistrationRoot string  `json:"registrationRoot"`
	OwnerBoundary    string  `json:"ownerBoundary"`
	TargetPath       string  `json:"targetPath"`
	Branch           string  `json:"branch"`
	BaseCommit       *string `json:"baseCommit"`
	Force            bool    `json:"force"`
	RecoverRequestID string  `json:"recoverRequestId"`
}

// WorktreeMutationResult is the exact provision/remove mutation projection
// fixed by §9.4.
type WorktreeMutationResult struct {
	SagaID             int64                  `json:"sagaId"`
	Operation          string                 `json:"operation"`
	Status             string                 `json:"status"`
	RunID              *int64                 `json:"runId"`
	OwnerBoundary      string                 `json:"ownerBoundary"`
	Path               string                 `json:"path"`
	Branch             string                 `json:"branch"`
	BaseCommit         *string                `json:"baseCommit"`
	SagaToken          int64                  `json:"sagaToken"`
	RecoveryGeneration int64                  `json:"recoveryGeneration"`
	RecoverRequestID   *string                `json:"recoverRequestId"`
	BrokerRequest      *WorktreeBrokerRequest `json:"brokerRequest"`
}

// WorktreeProvisionPlan is the committed handoff across the SQLite/external
// effect boundary. EffectRequired is true only for pending generation zero;
// otherwise Result is the verified historical result or current handoff.
// The capability-bearing token is deliberately absent from receipts/events.
type WorktreeProvisionPlan struct {
	SagaID             int64
	SagaToken          int64
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
	RecoveryGeneration int64
	EffectRequired     bool
	Result             *WorktreeMutationResult
}

// WorktreeProvisionOutcome is the trusted host's observation after the
// idempotent external effect or compensation completed with no transaction
// open. Only §9.4 provision outcomes are admitted.
type WorktreeProvisionOutcome string

const (
	WorktreeProvisionBound           WorktreeProvisionOutcome = "bound"
	WorktreeProvisionNeedsBroker     WorktreeProvisionOutcome = "needs-broker"
	WorktreeProvisionFailed          WorktreeProvisionOutcome = "failed"
	WorktreeProvisionCleanupRequired WorktreeProvisionOutcome = "cleanup-required"
)

// WorktreeProvisionCompensationRequired means an observed create or confined
// handoff cannot commit because the run/task became ineligible. The caller
// must close this call, compensate the exact immutable effect, and finalize
// the same plan as failed (absence proved) or cleanup-required (not proved).
type WorktreeProvisionCompensationRequired struct{ SagaID int64 }

func (e *WorktreeProvisionCompensationRequired) Error() string {
	return fmt.Sprintf("worktree provision saga %d requires compensation", e.SagaID)
}

// WorktreeSagaConflict is a typed stale/CAS conflict containing only the
// non-secret saga identifier.
type WorktreeSagaConflict struct{ SagaID int64 }

func (e *WorktreeSagaConflict) Error() string {
	return fmt.Sprintf("worktree saga %d changed concurrently", e.SagaID)
}

// WorktreeProvisionStatusError reports an invalid trusted-host observation.
type WorktreeProvisionStatusError struct{ Status WorktreeProvisionOutcome }

func (e *WorktreeProvisionStatusError) Error() string {
	return fmt.Sprintf("worktree provision outcome %q is invalid", e.Status)
}

// worktreeProvisionFaultHook pins the transaction/crash boundaries in tests.
// Production keeps the no-op. No external effect is ever invoked by a hook.
var worktreeProvisionFaultHook = func(string) error { return nil }

// validWorktreeBaseOID reports whether base is a 40- or 64-character
// lowercase hexadecimal object id: the repository's exact object-id width.
func validWorktreeBaseOID(base string) bool {
	if len(base) != 40 && len(base) != 64 || strings.ToLower(base) != base {
		return false
	}
	return strings.IndexFunc(base, func(r rune) bool {
		return r < '0' || r > '9' && (r < 'a' || r > 'f')
	}) < 0
}
func validateWorktreeProvisionOptions(o WorktreeProvisionOptions) error {
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
	for _, path := range []struct{ label, value string }{
		{"owner boundary", o.OwnerBoundary},
		{"registration root", o.RegistrationRoot},
		{"target path", o.TargetPath},
	} {
		if !canonicalAbsolutePath(path.value) {
			return fmt.Errorf("worktree %s is not an absolute canonical path", path.label)
		}
	}
	if filepath.Base(o.RegistrationRoot) != "worktrees" {
		return fmt.Errorf("worktree registration root is invalid")
	}
	if !validWorktreeBaseOID(o.BaseCommit) {
		return fmt.Errorf("worktree base commit is not a 40- or 64-character lowercase hexadecimal object id")
	}
	return nil
}
func loadWorktreeSagaByRequest(t *tx, requestID string) (*worktreeSagaState, error) {
	s, err := scanWorktreeSaga(t.QueryRow(`SELECT `+worktreeSagaColumns+`
		FROM worktree_sagas WHERE request_id=?`, requestID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read worktree saga request: %w", err)
	}
	return s, nil
}

func provisionIdentityMatches(s *worktreeSagaState, o WorktreeProvisionOptions, tag, task string) bool {
	return s.operation == "provision" && s.run.Valid && s.run.Int64 == o.RunID && !s.source.Valid &&
		s.tag == tag && s.task == task && !s.force && s.request == o.RequestID &&
		s.boundary == o.OwnerBoundary && s.registration.Valid && s.registration.String == o.RegistrationRoot &&
		s.path == o.TargetPath && s.branch == o.Branch && s.base.Valid && s.base.String == o.BaseCommit
}

func provisionPlanFromSaga(s *worktreeSagaState, owner string) *WorktreeProvisionPlan {
	return &WorktreeProvisionPlan{
		SagaID: s.id, SagaToken: s.fence, RunID: s.run.Int64, Tag: s.tag, Task: s.task,
		Owner: owner, RequestID: s.request, OwnerBoundary: s.boundary,
		RegistrationRoot: s.registration.String, TargetPath: s.path, Branch: s.branch,
		BaseCommit: s.base.String, RecoveryGeneration: s.generation,
	}
}

// PlanWorktreeProvision atomically reserves the original request, allocates
// the saga/fence, and persists pending before any Git/filesystem effect. An
// identical crash-boundary replay returns the same immutable snapshot and
// fence; later handoff/terminal replay performs no mutation or effect.
func (d *DB) PlanWorktreeProvision(o WorktreeProvisionOptions) (*WorktreeProvisionPlan, error) {
	if err := validateWorktreeProvisionOptions(o); err != nil {
		return nil, err
	}
	inputDigest, err := worktreeInputDigest(o.Owner, o.RequestID, o.RunID, false, "provision")
	if err != nil {
		return nil, err
	}
	var plan *WorktreeProvisionPlan
	err = d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		s, err := loadWorktreeSagaByRequest(t, o.RequestID)
		if err != nil {
			return err
		}
		if s != nil {
			if s.operation != "provision" || !s.run.Valid || s.run.Int64 != o.RunID {
				return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: o.RequestID}
			}
			var storedOwner, runTag, runTask string
			if err := t.QueryRow(`SELECT owner,tag,task_id FROM runs WHERE run_id=?`, s.run.Int64).
				Scan(&storedOwner, &runTag, &runTask); err != nil {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			if runTag != s.tag || runTask != s.task {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			if storedOwner != o.Owner {
				return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: o.RequestID}
			}
			task, err := t.getTask(s.tag, s.task)
			if err != nil || task == nil {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			gate := &ClaimGate{Owner: o.Owner, Token: o.ClaimToken, RunID: o.RunID, Attempt: o.Attempt, AttemptToken: o.AttemptToken}
			if _, err := t.taskMutationRunFence(task, gate, now, "provision"); err != nil {
				return err
			}
			if !provisionIdentityMatches(s, o, s.tag, s.task) {
				return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: o.RequestID}
			}
			plan = provisionPlanFromSaga(s, storedOwner)
			receipt, err := t.findVerifiedReceipt(ReceiptKindWorktree, o.RequestID)
			if err != nil {
				return err
			}
			if receipt != nil {
				if err := validateOriginalWorktreeReceipt(t, receipt, s, inputDigest); err != nil {
					return receiptIntegrityError(ReceiptKindWorktree)
				}
				result, err := worktreeResultFromReceipt(receipt, s.fence)
				if err != nil {
					return err
				}
				plan.Result = result
				return nil
			}
			if isTerminalWorktreeStatus(s.status) {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			if s.status == "pending" && s.generation == 0 {
				plan.EffectRequired = true
				return nil
			}
			result, err := worktreeResultFromSaga(t, s, s.fence)
			if err != nil {
				return err
			}
			plan.Result = result
			return nil
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
		if err := t.QueryRow(`SELECT tag,task_id,owner FROM runs WHERE run_id=?`, o.RunID).Scan(&tag, &taskID, &runOwner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return &RunConflict{Operation: "provision", RunID: o.RunID, Attempt: o.Attempt}
			}
			return fmt.Errorf("read provision run: %w", err)
		}
		if runOwner != o.Owner {
			return &RunConflict{Operation: "provision", RunID: o.RunID, Attempt: o.Attempt}
		}
		task, err := t.getTask(tag, taskID)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: taskID}
		}
		gate := &ClaimGate{Owner: o.Owner, Token: o.ClaimToken, RunID: o.RunID, Attempt: o.Attempt, AttemptToken: o.AttemptToken}
		fence, err := t.taskMutationRunFence(task, gate, now, "provision")
		if err != nil {
			return err
		}
		if fence == nil || fence.runID != o.RunID {
			return &RunConflict{Operation: "provision", RunID: o.RunID, Attempt: o.Attempt}
		}
		if err := checkWorktreeTag(tag); err != nil {
			return err
		}
		if o.OwnerBoundary != filepath.Clean(o.OwnerBoundary) || o.TargetPath != filepath.Join(o.OwnerBoundary, ".worktrees", WorktreeName(tag, taskID)) ||
			o.Branch != WorktreeBranch(tag, taskID) {
			return fmt.Errorf("worktree effect identity does not match run %d", o.RunID)
		}
		_, familyBoundary, err := t.requireIdentity()
		if err != nil {
			return err
		}
		if o.OwnerBoundary != familyBoundary {
			return fmt.Errorf("worktree owner boundary does not match the repository family")
		}
		if task.Status == "done" {
			return &WorktreeConflict{Kind: "history", Tag: tag, ID: taskID}
		}
		var existing int64
		err = t.QueryRow(`SELECT saga_id FROM worktree_sagas WHERE tag=? AND task_id=?
			AND (binding_active=1 OR status IN ('pending','needs-broker','cleanup-required')) LIMIT 1`, tag, taskID).Scan(&existing)
		if err == nil {
			return &WorktreeConflict{Kind: "exists", Tag: tag, ID: taskID}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check existing worktree saga: %w", err)
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
			VALUES (?,NULL,?,?,'provision','pending',0,?,?,0,?,?,?,?,?,0,?,?,NULL)`,
			o.RunID, tag, taskID, sagaFence, o.RequestID, o.OwnerBoundary, o.RegistrationRoot,
			o.TargetPath, o.Branch, o.BaseCommit, at, at)
		if err != nil {
			return fmt.Errorf("plan worktree provision: %w", err)
		}
		sagaID, err := insert.LastInsertId()
		if err != nil || sagaID <= 0 {
			return fmt.Errorf("read allocated worktree saga id")
		}
		if err := t.appendEvent(Event{Task: taskID, Tag: tag, Kind: ReceiptKindWorktree,
			Action: "planned", Owner: o.Owner, At: at}); err != nil {
			return err
		}
		if err := worktreeProvisionFaultHook("planned-before-commit"); err != nil {
			return err
		}
		plan = &WorktreeProvisionPlan{SagaID: sagaID, SagaToken: sagaFence, RunID: o.RunID,
			Tag: tag, Task: taskID, Owner: o.Owner, RequestID: o.RequestID,
			OwnerBoundary: o.OwnerBoundary, RegistrationRoot: o.RegistrationRoot,
			TargetPath: o.TargetPath, Branch: o.Branch, BaseCommit: o.BaseCommit,
			RecoveryGeneration: 0, EffectRequired: true}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func provisionPlanMatchesSaga(plan *WorktreeProvisionPlan, s *worktreeSagaState) bool {
	return plan != nil && s != nil && s.operation == "provision" && s.run.Valid &&
		plan.SagaID == s.id && plan.SagaToken == s.fence && plan.RunID == s.run.Int64 &&
		plan.Tag == s.tag && plan.Task == s.task && plan.OwnerBoundary == s.boundary &&
		s.registration.Valid && plan.RegistrationRoot == s.registration.String &&
		plan.TargetPath == s.path && plan.Branch == s.branch && s.base.Valid &&
		plan.BaseCommit == s.base.String && plan.RequestID == s.request &&
		plan.RecoveryGeneration == s.generation
}

func provisionStillEligible(t *tx, s *worktreeSagaState) (bool, error) {
	var runTag, runTask, runStatus, taskStatus string
	err := t.QueryRow(`SELECT r.tag,r.task_id,r.status,t.status FROM runs r
		JOIN tasks t ON t.tag=r.tag AND t.id=r.task_id WHERE r.run_id=?`, s.run.Int64).
		Scan(&runTag, &runTask, &runStatus, &taskStatus)
	if err != nil {
		return false, err
	}
	if runTag != s.tag || runTask != s.task {
		return false, sagaInvariant(s, "does not match owning run")
	}
	return taskStatus != "done" && (runStatus == "running" || runStatus == "retryable"), nil
}

// FinalizeWorktreeProvision conditionally records one observed initial
// outcome. It never performs external work and never revalidates a claim,
// current attempt, capability, generation, or lease. A terminal replay is
// receipt-based; a repeated nonterminal outcome is a read-only handoff.
func (d *DB) FinalizeWorktreeProvision(plan *WorktreeProvisionPlan, outcome WorktreeProvisionOutcome) (*WorktreeMutationResult, error) {
	switch outcome {
	case WorktreeProvisionBound, WorktreeProvisionNeedsBroker, WorktreeProvisionFailed, WorktreeProvisionCleanupRequired:
	default:
		return nil, &WorktreeProvisionStatusError{Status: outcome}
	}
	if plan == nil || plan.SagaID <= 0 || plan.SagaToken <= 0 {
		return nil, fmt.Errorf("worktree provision plan is invalid")
	}
	var result *WorktreeMutationResult
	err := d.immediate(func(t *tx) error {
		s, err := loadWorktreeSagaByRequest(t, plan.RequestID)
		if err != nil {
			return err
		}
		if !provisionPlanMatchesSaga(plan, s) {
			return &WorktreeSagaConflict{SagaID: plan.SagaID}
		}
		var storedOwner string
		if err := t.QueryRow(`SELECT owner FROM runs WHERE run_id=?`, s.run.Int64).Scan(&storedOwner); err != nil {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		if storedOwner != plan.Owner {
			return &WorktreeSagaConflict{SagaID: plan.SagaID}
		}
		eligible, err := provisionStillEligible(t, s)
		if err != nil {
			return err
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
				inputDigest, err := worktreeInputDigest(storedOwner, s.request, s.run.Int64, false, "provision")
				if err != nil || validateOriginalWorktreeReceipt(t, receipt, s, inputDigest) != nil {
					return receiptIntegrityError(ReceiptKindWorktree)
				}
				result, err = worktreeResultFromReceipt(receipt, s.fence)
				return err
			}
			result, err = worktreeResultFromSaga(t, s, s.fence)
			return err
		}
		if outcome == WorktreeProvisionBound || outcome == WorktreeProvisionNeedsBroker {
			if !eligible {
				return &WorktreeProvisionCompensationRequired{SagaID: s.id}
			}
		}
		at := nowUTC()
		active := outcome == WorktreeProvisionBound
		updated, err := t.Exec(`UPDATE worktree_sagas SET status=?,binding_active=?,updated_at=?
			WHERE saga_id=? AND saga_fence=? AND operation='provision' AND status='pending'
			AND recovery_generation=0 AND run_id=? AND source_saga_id IS NULL
			AND tag=? AND task_id=? AND request_id=? AND owner_boundary=?
			AND registration_root=? AND target_path=? AND branch=? AND base_commit=? AND force=0`,
			string(outcome), active, at, s.id, s.fence, s.run.Int64, s.tag, s.task,
			s.request, s.boundary, s.registration.String, s.path, s.branch, s.base.String)
		if err != nil {
			return fmt.Errorf("finalize worktree provision saga %d: %w", s.id, err)
		}
		if n, err := updated.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return &WorktreeSagaConflict{SagaID: s.id}
		}
		s.status, s.active, s.updated = string(outcome), active, at
		if isTerminalWorktreeStatus(s.status) {
			if err := writeOriginalWorktreeReceipt(t, s, storedOwner, at); err != nil {
				return err
			}
		} else {
			if err := t.appendEvent(Event{Task: s.task, Tag: s.tag, Kind: ReceiptKindWorktree,
				Action: s.status, Owner: storedOwner, At: at}); err != nil {
				return err
			}
		}
		if err := worktreeProvisionFaultHook("finalized-before-commit"); err != nil {
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
