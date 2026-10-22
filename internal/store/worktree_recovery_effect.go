package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// §9.4 recovery effect reconciliation: the trusted-host driver between one
// committed RecoverWorktreeSaga snapshot and the CommitWorktreeRecovery
// finalization CAS. Immediately before every direct or broker effect the
// driver rederives the exact Git common directory for the stored family and
// proves its registration root and owner boundary equal the immutable row;
// a runless legacy row derives its registration root at effect time because
// its stored value is null, and an empty stored root admits only that exact
// legacy shape — any other empty root is a cleared or corrupt identity —
// and every mismatch fails closed with no effect and no row rewrite
// (SPEC §9.4). Broker work that does not reach a reconciled
// outcome commits nothing: every §9.4 taxonomy failure below returns an
// error and never reaches the finalization CAS, so the current nonterminal
// status, the stored recovery generation, and the advertised deterministic
// recovery request id stay unchanged for an identical retry. Only a host
// outcome the driver has reconciled against the snapshot identities is
// committed; no SQLite transaction is ever held across an effect.

// WorktreeRecoveryCapabilityConflict reports §9.4's typed capability-conflict
// class of broker work that reached no reconciled outcome: a needs-broker
// recovery requires the exact §9.5 fd-3 capability and none was inherited.
// Nothing was committed and the identical request may be retried.
type WorktreeRecoveryCapabilityConflict struct{ SagaID int64 }

func (e *WorktreeRecoveryCapabilityConflict) Error() string {
	return fmt.Sprintf("worktree saga %d recovery requires the broker capability on fd 3: %v", e.SagaID, errWorktreeConfined)
}

// WorktreeRecoveryBrokerUncertain reports §9.4's uncertain broker result: the
// broker answered success but the reconciled observation could not be proved
// against the snapshot identities. Nothing was committed and the identical
// idempotent broker capability and request may be retried.
type WorktreeRecoveryBrokerUncertain struct {
	SagaID int64
	Verb   string
}

func (e *WorktreeRecoveryBrokerUncertain) Error() string {
	return fmt.Sprintf("worktree saga %d broker %s result could not be reconciled with the saga identity", e.SagaID, e.Verb)
}

// WorktreeRecoveryReconciliation is one completed recovery attempt: the
// committed or unchanged handoff result and the bounded observation cause of
// a committed failed outcome (nil otherwise). Recovery output never carries
// the saga token.
type WorktreeRecoveryReconciliation struct {
	Result *WorktreeMutationResult
	Cause  error
}

// ReconcileWorktreeRecoveryEffect performs the host half of one §9.4
// recovery attempt: it rederives the family identity, selects and performs
// the idempotent effect or compensation the snapshot status and operation
// require, and records the reconciled outcome through the finalization CAS.
// For a runless legacy cleanup row the effect-time-derived registration root
// is threaded onto the attempt's ObservedRegistrationRoot — the schema
// cannot store it and it is never persisted — so the prune-registration mint
// binding is exact. A fail-closed identity mismatch, a §9.4 broker taxonomy
// failure, or an unrecoverable store error returns no reconciliation and
// commits nothing.
func ReconcileWorktreeRecoveryEffect(d *DB, boundary string, a *WorktreeRecoveryAttempt) (*WorktreeRecoveryReconciliation, error) {
	if d == nil || a == nil || !a.EffectRequired || a.SagaID <= 0 || a.SagaToken <= 0 ||
		a.RequestID == "" || a.OriginalRequestID == "" ||
		(a.Operation != "provision" && a.Operation != "remove") ||
		(a.Status != "pending" && a.Status != "needs-broker" && a.Status != "cleanup-required") {
		return nil, fmt.Errorf("worktree recovery attempt is invalid")
	}
	registrationRoot, err := rederiveRecoveryFamily(boundary, a)
	if err != nil {
		return nil, err
	}
	if recoveryLegacyRemoval(a) {
		a.ObservedRegistrationRoot = registrationRoot
	}
	if a.Operation == "provision" {
		return reconcileProvisionRecovery(d, boundary, a)
	}
	return reconcileRemovalRecovery(d, boundary, a, registrationRoot)
}

// recoveryLegacyRemoval reports the exact §9.4 legacy cleanup shape: a
// removal row storing no run, source, base, or registration root, because
// the converted v4 row could not record one. It is the only shape whose
// registration root may be derived at effect time; every other empty root
// is a cleared or corrupt identity (SPEC §9.4).
func recoveryLegacyRemoval(a *WorktreeRecoveryAttempt) bool {
	return a.Operation == "remove" && a.RunID == 0 && a.SourceSagaID == 0 &&
		a.RegistrationRoot == "" && a.BaseCommit == ""
}

// rederiveRecoveryFamily proves the exact immutable owner and registration
// roots from repository state before any recovery effect: the current owner
// boundary is rederived from the invocation boundary — never accepted from
// an argument — and the canonical <common-dir>/worktrees root must equal the
// immutable row. An empty stored root admits only the exact runless
// sourceless legacy removal shape, whose root is derived at effect time;
// every other empty root is a cleared or corrupt identity that fails closed
// here (SPEC §9.4).
func rederiveRecoveryFamily(boundary string, a *WorktreeRecoveryAttempt) (string, error) {
	ownerNow, err := worktreeOwnerBoundary(boundary)
	if err != nil {
		return "", fmt.Errorf("%v: %w", &WorktreeOwnerBoundaryUnavailable{Path: a.OwnerBoundary}, err)
	}
	if ownerNow != a.OwnerBoundary {
		return "", &WorktreeEffectIdentityMismatch{SagaID: a.SagaID}
	}
	roots := registrationRoots(boundary)
	if len(roots) == 0 {
		return "", fmt.Errorf("cannot resolve the Git common directory for %s", boundary)
	}
	if a.RegistrationRoot == "" {
		if !recoveryLegacyRemoval(a) {
			return "", &WorktreeEffectIdentityMismatch{SagaID: a.SagaID}
		}
		return roots[0], nil
	}
	if roots[0] != a.RegistrationRoot {
		return "", &WorktreeEffectIdentityMismatch{SagaID: a.SagaID}
	}
	return roots[0], nil
}

// worktreeRecoveryProveHook pins the between-effects identity boundary for
// focused tests: it runs immediately before each per-effect family
// rederivation, after any earlier effect of the same attempt completed, so
// a test can change the repository family between two effects. Production
// keeps the no-op.
var worktreeRecoveryProveHook = func() error { return nil }

// reproveRecoveryFamily reruns the mandatory §9.4 fail-closed rederivation
// immediately before one direct or broker effect of the attempt. No
// derivation from the driver's entry survives to vouch for a later effect:
// the family is rederived and proved against the immutable row again, and a
// mismatch fails closed without the effect (SPEC §9.4).
func reproveRecoveryFamily(boundary string, a *WorktreeRecoveryAttempt) error {
	if err := worktreeRecoveryProveHook(); err != nil {
		return err
	}
	_, err := rederiveRecoveryFamily(boundary, a)
	return err
}

// reproveRemovalFamily adds the removal plan's exact shape and registration
// proof to the per-effect family rederivation: the staged removal acts only
// on family-named paths with nonnegative IDs in the admitted shapes, and a
// legacy row's plan carries the root derived for this attempt, which must
// still be the family's registration root at effect time (SPEC §9.4).
func reproveRemovalFamily(boundary string, a *WorktreeRecoveryAttempt, plan *WorktreeRemovePlan) error {
	if err := reproveRecoveryFamily(boundary, a); err != nil {
		return err
	}
	return rederiveRemovalFamily(plan)
}

// recoveryProvisionEligible reads the §9.4 immutable task/run liveness gate
// for one provisioning recovery outside any effect: the task must not be
// done and the referenced run must still carry the same tag/task identity
// and be running or retryable.
func (d *DB) recoveryProvisionEligible(a *WorktreeRecoveryAttempt) (bool, error) {
	var eligible bool
	err := d.deferred(func(t *tx) error {
		s := &worktreeSagaState{id: a.SagaID, operation: "provision", tag: a.Tag, task: a.Task,
			run: sql.NullInt64{Int64: a.RunID, Valid: true}}
		var err error
		eligible, err = provisionStillEligible(t, s)
		return err
	})
	return eligible, err
}

func recoveryProvisionPlan(a *WorktreeRecoveryAttempt) *WorktreeProvisionPlan {
	return &WorktreeProvisionPlan{
		SagaID: a.SagaID, SagaToken: a.SagaToken, RunID: a.RunID, Tag: a.Tag, Task: a.Task,
		RequestID: a.OriginalRequestID, OwnerBoundary: a.OwnerBoundary,
		RegistrationRoot: a.RegistrationRoot, TargetPath: a.TargetPath, Branch: a.Branch,
		BaseCommit: a.BaseCommit, RecoveryGeneration: a.RecoveryGeneration,
	}
}

// reconcileProvisionRecovery selects the §9.4 provisioning effect for the
// snapshot status: a pending attempt performs the direct bind effect
// immediately behind its own family rederivation — the effect proves the
// registration root itself but compares only caller-supplied owner values,
// so a core.worktree relocation retaining the common directory is invisible
// without the rederived owner boundary — a needs-broker attempt delegates
// the completed registration step through the §9.5 broker, and a
// cleanup-required attempt stays cleanup-only. A provisioning recovery
// rechecks only the immutable task/run relationship, task status, and run
// status: while the task is done or the run is terminal it never creates,
// binds, or delegates creation to the broker — it compensates any exact
// effect an earlier phase left and commits failed when absence is proved or
// cleanup-required otherwise (SPEC §9.4).
func reconcileProvisionRecovery(d *DB, boundary string, a *WorktreeRecoveryAttempt) (*WorktreeRecoveryReconciliation, error) {
	plan := recoveryProvisionPlan(a)
	if a.Status == "cleanup-required" {
		return reconcileProvisionCleanup(d, boundary, a, plan)
	}
	eligible, err := d.recoveryProvisionEligible(a)
	if err != nil {
		return nil, err
	}
	var outcome WorktreeRecoveryOutcome
	switch {
	case !eligible:
		if outcome, err = compensateRecoveryProvision(boundary, plan); err != nil {
			return nil, err
		}
	case a.Status == "needs-broker":
		if outcome, err = recoverProvisionThroughBroker(boundary, a, plan); err != nil {
			return nil, err
		}
	default:
		if err := reproveRecoveryFamily(boundary, a); err != nil {
			return nil, err
		}
		provision, perr := EffectWorktreeProvision(boundary, a.OwnerBoundary, plan)
		if perr != nil {
			return nil, perr
		}
		outcome = WorktreeRecoveryOutcome(provision)
	}
	return commitWorktreeRecoveryOutcome(d, boundary, a, plan, outcome, nil)
}

// compensateRecoveryProvision compensates the exact immutable effect an
// earlier provisioning phase left and maps the proof to the §9.4
// observation: failed when absence is proved, cleanup-required otherwise.
// The ineligible path never delegates to the broker: a confined
// compensation simply stays unproved and the committed cleanup-required
// status is what later opens §9.4's confined-compensation rule.
func compensateRecoveryProvision(boundary string, plan *WorktreeProvisionPlan) (WorktreeRecoveryOutcome, error) {
	proved, _, err := compensateWorktreeProvision(boundary, plan)
	if err != nil {
		return "", err
	}
	if proved {
		return WorktreeRecoveryFailed, nil
	}
	return WorktreeRecoveryCleanupRequired, nil
}

// recoverProvisionThroughBroker binds a needs-broker provision through the
// one §9.5 broker delegation: the completed direct registration attempt is
// never repeated, the registration step is delegated exactly once through
// the fd-3 capability with the create verb, and only a reconciled bound
// observation returns. The family identity is rederived immediately before
// the delegation — the driver's entry derivation never vouches for a broker
// effect. The broker's verbs are idempotent for identical bound inputs, so
// an already-present exact effect is the reconciled bound state without an
// invocation (SPEC §9.4/§9.5).
func recoverProvisionThroughBroker(boundary string, a *WorktreeRecoveryAttempt, plan *WorktreeProvisionPlan) (WorktreeRecoveryOutcome, error) {
	if provisionEffectIntact(boundary, plan) {
		return WorktreeRecoveryBound, nil
	}
	if !brokerCapabilityAvailable() {
		return "", &WorktreeRecoveryCapabilityConflict{SagaID: a.SagaID}
	}
	if err := reproveRecoveryFamily(boundary, a); err != nil {
		return "", err
	}
	if err := delegateWorktreeBroker(boundary, "create"); err != nil {
		return "", err
	}
	if !provisionEffectIntact(boundary, plan) {
		return "", &WorktreeRecoveryBrokerUncertain{SagaID: a.SagaID, Verb: "create"}
	}
	return WorktreeRecoveryBound, nil
}

// reconcileProvisionCleanup performs the §9.4 cleanup-only compensation of a
// cleanup-required provision: it terminalizes failed only after exact
// absence is proved. When the compensating registration write itself
// produces the §9.5 permission-class confinement signal, that exact step is
// delegated through the broker — the remove verb over the same immutable
// identity, under the same advertised recover- id, immediately behind its
// own family rederivation — and an attempt that observed the signal but
// stayed unproved returns the confined-compensation observation, whose
// unchanged handoff carries the non-secret mint bindings.
// Every unproved attempt commits nothing: no CAS, receipt, event, or
// generation (SPEC §9.4).
func reconcileProvisionCleanup(d *DB, boundary string, a *WorktreeRecoveryAttempt, plan *WorktreeProvisionPlan) (*WorktreeRecoveryReconciliation, error) {
	proved, confined, err := compensateWorktreeProvision(boundary, plan)
	if err != nil {
		return nil, err
	}
	if !proved && confined && brokerCapabilityAvailable() {
		if err := reproveRecoveryFamily(boundary, a); err != nil {
			return nil, err
		}
		if berr := delegateWorktreeBroker(boundary, "remove"); berr != nil {
			// Broker work that reached no reconciled outcome commits
			// nothing; the identical request may be retried (SPEC §9.4).
			return nil, berr
		}
		// The broker completed the confined registration step; the remaining
		// direct compensation steps and the absence proof rerun unchanged.
		proved, confined, err = compensateWorktreeProvision(boundary, plan)
		if err != nil {
			return nil, err
		}
	}
	outcome := WorktreeRecoveryCleanupRequired
	switch {
	case proved:
		outcome = WorktreeRecoveryFailed
	case confined:
		outcome = WorktreeRecoveryCleanupConfined
	}
	return commitWorktreeRecoveryOutcome(d, boundary, a, plan, outcome, nil)
}

// reconcileRemovalRecovery selects the §9.4 removal effect for the snapshot
// status. Pending and cleanup-required attempts prove the family again
// immediately before the exact idempotent staged removal — the plan's exact
// shape and registration root included, so a corrupt row or a family that
// moved since the entry derivation fails closed without an effect. An exact
// permission-class confinement of the registration write records
// needs-broker, which a later recovery — and only that recovery — delegates
// through the §9.5 broker with the remove verb, or the prune-registration
// verb for a runless legacy row, each behind its own immediate
// rederivation. A failed observation under cleanup authority (source run
// terminal or task done) is reconciled to cleanup-required by the commit
// mapping below (SPEC §9.4/§9.5).
func reconcileRemovalRecovery(d *DB, boundary string, a *WorktreeRecoveryAttempt, registrationRoot string) (*WorktreeRecoveryReconciliation, error) {
	legacy := recoveryLegacyRemoval(a)
	plan := recoveryRemovalPlan(a, registrationRoot)
	var outcome WorktreeRecoveryOutcome
	var cause error
	if a.Status == "needs-broker" {
		verb := "remove"
		if legacy {
			verb = "prune-registration"
		}
		var err error
		if outcome, err = recoverRemovalThroughBroker(boundary, a, plan, verb, legacy); err != nil {
			return nil, err
		}
	} else {
		if err := reproveRemovalFamily(boundary, a, plan); err != nil {
			return nil, err
		}
		removed, err := performWorktreeRemovalEffect(plan, !legacy)
		if removed == "" {
			return nil, err
		}
		outcome, cause = WorktreeRecoveryOutcome(removed), err
	}
	return commitWorktreeRecoveryOutcome(d, boundary, a, nil, outcome, cause)
}

func recoveryRemovalPlan(a *WorktreeRecoveryAttempt, registrationRoot string) *WorktreeRemovePlan {
	return &WorktreeRemovePlan{
		SagaID: a.SagaID, SagaToken: a.SagaToken, SourceSagaID: a.SourceSagaID, RunID: a.RunID,
		Tag: a.Tag, Task: a.Task, RequestID: a.OriginalRequestID,
		OwnerBoundary: a.OwnerBoundary, RegistrationRoot: registrationRoot,
		TargetPath: a.TargetPath, Branch: a.Branch, BaseCommit: a.BaseCommit,
		Force: a.Force, RecoveryGeneration: a.RecoveryGeneration,
	}
}

// recoverRemovalThroughBroker completes a needs-broker removal through the
// one §9.5 broker delegation: the completed direct registration attempt is
// never repeated, the registration step is delegated exactly once through
// the fd-3 capability, and only an absence proof reconciled against the
// snapshot identities returns the removed outcome. A legacy cleanup first
// completes the unconfined target half of the exact staged removal inside
// the owner boundary, because the broker's prune-registration verb refuses a
// present target; the family identity is rederived immediately before that
// half and again immediately before the delegation (SPEC §9.4/§9.5).
func recoverRemovalThroughBroker(boundary string, a *WorktreeRecoveryAttempt, plan *WorktreeRemovePlan, verb string, legacy bool) (WorktreeRecoveryOutcome, error) {
	proved, err := recoveryRemovalAbsenceProved(plan, !legacy)
	if err != nil {
		return "", err
	}
	if !proved {
		if legacy {
			if rerr := reproveRemovalFamily(boundary, a, plan); rerr != nil {
				return "", rerr
			}
			if terr := removeRecoveryLegacyTarget(plan); terr != nil {
				return "", terr
			}
		}
		if !brokerCapabilityAvailable() {
			return "", &WorktreeRecoveryCapabilityConflict{SagaID: a.SagaID}
		}
		if rerr := reproveRemovalFamily(boundary, a, plan); rerr != nil {
			return "", rerr
		}
		if berr := delegateWorktreeBroker(boundary, verb); berr != nil {
			return "", berr
		}
		if proved, err = recoveryRemovalAbsenceProved(plan, !legacy); err != nil || !proved {
			return "", &WorktreeRecoveryBrokerUncertain{SagaID: a.SagaID, Verb: verb}
		}
	}
	return WorktreeRecoveryRemoved, nil
}

// recoveryRemovalAbsenceProved proves the exact removed state read-only: no
// target, no registration entry, and no saga staging path — and, for a
// registered (non-legacy) identity, the retained branch still present.
func recoveryRemovalAbsenceProved(plan *WorktreeRemovePlan, retainedBranch bool) (bool, error) {
	state, err := inspectRemovalEffect(plan)
	if err != nil {
		return false, err
	}
	if state.targetExists || state.admin != "" || state.adminStage != "" || state.targetStage != "" {
		return false, nil
	}
	if !retainedBranch {
		return true, nil
	}
	return worktreeBranchExists(plan.OwnerBoundary, plan.Branch)
}

// removeRecoveryLegacyTarget completes the target half of one legacy cleanup
// removal — the dirty-checked staged rename and deletion inside the owner
// boundary — leaving the confined registration write to the delegated
// prune-registration verb. A dirty target refuses exactly as the direct
// effect would; the saga is left untouched for an identical retry.
func removeRecoveryLegacyTarget(plan *WorktreeRemovePlan) error {
	state, err := inspectRemovalEffect(plan)
	if err != nil {
		return err
	}
	if state.targetStage != "" {
		if err := os.RemoveAll(state.targetStage); err != nil {
			return err
		}
		if err := syncRemovalDir(filepath.Dir(plan.TargetPath)); err != nil {
			return err
		}
	}
	if !state.targetExists {
		return nil
	}
	if !plan.Force {
		dirty, err := worktreeDirty(plan.TargetPath)
		if err != nil {
			return err
		}
		if dirty {
			return &WorktreeConflict{Kind: "dirty", Tag: plan.Tag, ID: plan.Task,
				Path: plan.TargetPath, Branch: plan.Branch}
		}
	}
	stage := removalTargetStage(plan)
	if stage == "" {
		return fmt.Errorf("worktree remove effect requires a committed saga fence")
	}
	if err := os.Rename(plan.TargetPath, stage); err != nil {
		return err
	}
	if err := syncRemovalDir(filepath.Dir(plan.TargetPath)); err != nil {
		return err
	}
	if err := os.RemoveAll(stage); err != nil {
		return err
	}
	return syncRemovalDir(filepath.Dir(plan.TargetPath))
}

// commitWorktreeRecoveryOutcome records one reconciled observation through
// the finalization CAS. The two §9.4 reconciliation refusals are absorbed
// here, with no SQLite transaction open across the intervening effect: an
// ineligible provision's bound or confined handoff is compensated and
// re-committed as failed (absence proved) or cleanup-required, and a failed
// removal observation under cleanup authority — source run terminal or task
// done — is re-committed as cleanup-required, because such cleanup may
// terminalize only as removed (SPEC §9.4).
func commitWorktreeRecoveryOutcome(d *DB, boundary string, a *WorktreeRecoveryAttempt,
	provision *WorktreeProvisionPlan, outcome WorktreeRecoveryOutcome, cause error) (*WorktreeRecoveryReconciliation, error) {
	result, err := d.CommitWorktreeRecovery(a, outcome)
	if err == nil {
		return &WorktreeRecoveryReconciliation{Result: result, Cause: cause}, nil
	}
	var compensation *WorktreeProvisionCompensationRequired
	if errors.As(err, &compensation) {
		proved, _, cerr := compensateWorktreeProvision(boundary, provision)
		if cerr != nil {
			return nil, cerr
		}
		final := WorktreeRecoveryCleanupRequired
		if proved {
			final = WorktreeRecoveryFailed
		}
		if result, err = d.CommitWorktreeRecovery(a, final); err != nil {
			return nil, err
		}
		return &WorktreeRecoveryReconciliation{Result: result}, nil
	}
	var forbidden *WorktreeRemovalFailedForbidden
	if errors.As(err, &forbidden) {
		if result, err = d.CommitWorktreeRecovery(a, WorktreeRecoveryCleanupRequired); err != nil {
			return nil, err
		}
		return &WorktreeRecoveryReconciliation{Result: result, Cause: cause}, nil
	}
	return nil, err
}

// brokerCapabilityAvailable reports whether the trusted host inherited a
// §9.5 fd-3 capability document it can hand to one broker delegation.
func brokerCapabilityAvailable() bool {
	capability, err := openBrokerCapabilityFile()
	if err != nil {
		return false
	}
	capability.Close()
	return true
}
