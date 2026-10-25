package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// §9.4 recovery effect reconciliation tests: the trusted-host driver between
// one committed RecoverWorktreeSaga snapshot and the finalization CAS. Real
// Git repositories and worktrees pin the identity rederivation, every
// status/operation effect path, the §9.5 confinement signal handling, the
// broker taxonomy's commits-nothing contract, the unproved cleanup no-burn
// rule, and the runless legacy row's effect-time registration-root
// derivation.

// recoveryReconcile reads one fresh effect-required attempt and drives the
// reconciliation, failing the test when the read half refuses.
func recoveryReconcile(t *testing.T, db *DB, boundary string, sagaID, generation int64) (*WorktreeRecoveryReconciliation, error) {
	t.Helper()
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, generation))
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.EffectRequired {
		t.Fatalf("attempt for saga %d requires no effect: %+v", sagaID, attempt)
	}
	return ReconcileWorktreeRecoveryEffect(db, boundary, attempt)
}

// recoveryAttemptOf reads one fresh effect-required attempt.
func recoveryAttemptOf(t *testing.T, db *DB, sagaID, generation int64) *WorktreeRecoveryAttempt {
	t.Helper()
	attempt := mustAttempt(t, db, sagaID, generation)
	if !attempt.EffectRequired {
		t.Fatalf("attempt for saga %d requires no effect: %+v", sagaID, attempt)
	}
	return attempt
}

// recoveryNothingCommitted pins §9.4's commits-nothing contract after one
// refused reconciliation: the same status, stored recovery generation, and
// advertised request id, no new receipt or event, and an identical retry
// that still reaches an effect.
func recoveryNothingCommitted(t *testing.T, db *DB, sagaID int64, before *ClassInventory, status string, generation int64, request string) {
	t.Helper()
	if got, gen, _ := sagaRow(t, db, sagaID); got != status || gen != generation {
		t.Fatalf("row after refused reconciliation = %s/%d; want %s/%d", got, gen, status, generation)
	}
	after := storeInventory(t, db)
	if after.Events != before.Events || after.Receipts != before.Receipts {
		t.Fatalf("refused reconciliation committed events %d->%d receipts %d->%d",
			before.Events, after.Events, before.Receipts, after.Receipts)
	}
	retry, err := db.RecoverWorktreeSaga(WorktreeRecoveryOptions{
		SagaID: sagaID, SagaToken: sagaFenceOf(t, db, sagaID), RequestID: request})
	if err != nil || !retry.EffectRequired || retry.RecoveryGeneration != generation {
		t.Fatalf("identical retry after refusal = %+v, %v", retry, err)
	}
}

func TestReconcileWorktreeRecoveryRejectsInvalidAttempts(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "reconcile.invalid", WorktreeProvisionNeedsBroker)
	if _, err := ReconcileWorktreeRecoveryEffect(db, "", nil); err == nil {
		t.Fatal("nil attempt reconciled")
	}
	attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
	replay := *attempt
	replay.EffectRequired = false
	// The driver refuses before any store or Git access: only the exact
	// invalid-attempt diagnostic is accepted.
	if _, err := ReconcileWorktreeRecoveryEffect(db, "", &replay); err == nil ||
		err.Error() != "worktree recovery attempt is invalid" {
		t.Fatalf("replay attempt = %v", err)
	}
}

// TestReconcileWorktreeRecoveryIdentityFailsClosed pins the mandatory §9.4
// rederivation before every recovery effect: a registration root or owner
// boundary that no longer equals the immutable row refuses with the typed
// mismatch, no effect, and no row rewrite.
func TestReconcileWorktreeRecoveryIdentityFailsClosed(t *testing.T) {
	t.Run("provision", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.mismatch.provision")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		tampered := *attempt
		tampered.RegistrationRoot = filepath.Join(repo, "elsewhere", "worktrees")
		var mismatch *WorktreeEffectIdentityMismatch
		if _, err := ReconcileWorktreeRecoveryEffect(db, repo, &tampered); !errors.As(err, &mismatch) {
			t.Fatalf("registration mismatch = %v", err)
		}
		moved := *attempt
		moved.OwnerBoundary = filepath.Join(repo, "moved")
		if _, err := ReconcileWorktreeRecoveryEffect(db, repo, &moved); !errors.As(err, &mismatch) {
			t.Fatalf("owner mismatch = %v", err)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target created by refused reconciliation: %v", err)
		}
		if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || exists {
			t.Fatalf("branch created by refused reconciliation: %v", exists)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refusals = %v", err)
		}
	})
	t.Run("removal", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.mismatch.removal", false)
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		tampered := *attempt
		tampered.RegistrationRoot = filepath.Join(filepath.Dir(plan.RegistrationRoot), "other-worktrees")
		var mismatch *WorktreeEffectIdentityMismatch
		if _, err := ReconcileWorktreeRecoveryEffect(db, repo, &tampered); !errors.As(err, &mismatch) {
			t.Fatalf("registration mismatch = %v", err)
		}
		if dirty, err := worktreeDirty(plan.TargetPath); err != nil || dirty {
			t.Fatalf("target after refused reconciliation dirty=%v err=%v", dirty, err)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refusals = %v", err)
		}
	})
}

// TestReconcileWorktreeRecoveryProvisionPending pins the pending-provision
// recovery: an eligible run binds through the direct idempotent effect, an
// already-present exact effect is observed rather than re-created, and a
// task done or terminal run never creates or binds — the terminal parent's
// own transaction has already flipped the nonterminal provision to
// cleanup-required without advancing its generation (§9.4 automatic
// cleanup), so its recovery is the cleanup-only one: it commits failed only
// when absence is proved and commits nothing at all otherwise.
func TestReconcileWorktreeRecoveryProvisionPending(t *testing.T) {
	t.Run("binds through the direct effect", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.pending.bind")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "bound" || rec.Result.RecoveryGeneration != 1 || rec.Result.SagaToken != 0 ||
			rec.Result.BrokerRequest != nil || rec.Cause != nil {
			t.Fatalf("bound recovery = %+v cause=%v", rec.Result, rec.Cause)
		}
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || !registered {
			t.Fatalf("registration after recovery = %v, %v", registered, err)
		}
		if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, plan.SagaID); active != 1 {
			t.Fatalf("binding active after bound recovery = %d", active)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate bound recovery = %v", err)
		}
	})
	t.Run("already-present exact effect is observed", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.pending.present")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		// The crash-before-finalization repeat: the exact effect exists.
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "bound" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("idempotent bound recovery = %+v", rec.Result)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate idempotent recovery = %v", err)
		}
	})
	t.Run("terminal run compensates and fails", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.pending.terminal")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.CancelRun(opts.RunID, opts.Owner, opts.ClaimToken, opts.Attempt,
			opts.AttemptToken, false, "run.cancel.wt.rec.pending.terminal"); err != nil {
			t.Fatal(err)
		}
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "failed" || rec.Result.RecoveryGeneration != 1 || rec.Result.SagaToken != 0 {
			t.Fatalf("compensated recovery = %+v", rec.Result)
		}
		if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || exists {
			t.Fatalf("branch after compensated recovery exists=%v err=%v", exists, err)
		}
		receipt, err := db.FindReceipt(ReceiptKindWorktree, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		if err != nil || receipt == nil || receipt["status"] != "failed" {
			t.Fatalf("compensated failed recovery receipt = %v, %v", receipt, err)
		}
		original, err := db.FindReceipt(ReceiptKindWorktree, plan.RequestID)
		if err != nil || original == nil || original["status"] != "failed" {
			t.Fatalf("compensated failed original receipt = %v, %v", original, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate compensated recovery = %v", err)
		}
	})
	t.Run("terminal run with the effect present compensates it", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.pending.effect")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		if _, _, err := db.CancelRun(opts.RunID, opts.Owner, opts.ClaimToken, opts.Attempt,
			opts.AttemptToken, false, "run.cancel.wt.rec.pending.effect"); err != nil {
			t.Fatal(err)
		}
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "failed" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("compensated effect recovery = %+v", rec.Result)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target after compensated effect: %v", err)
		}
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || registered {
			t.Fatalf("registration after compensated effect = %v, %v", registered, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate compensated effect recovery = %v", err)
		}
	})
	t.Run("unattributable branch keeps the no-burn handoff", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.pending.unattributable")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		// A branch pointing off the immutable base is no longer uniquely
		// attributable, so absence cannot be proved.
		git(t, repo, "branch", plan.Branch, mainOID(t, repo)+"~1")
		if _, _, err := db.CancelRun(opts.RunID, opts.Owner, opts.ClaimToken, opts.Attempt,
			opts.AttemptToken, false, "run.cancel.wt.rec.pending.unattributable"); err != nil {
			t.Fatal(err)
		}
		// Cancel terminalizes the run, and its own transaction has already
		// applied §9.4's automatic cleanup: the nonterminal provision is
		// cleanup-required at generation zero before any recovery runs.
		if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "cleanup-required" || generation != 0 {
			t.Fatalf("row after cancel = %s/%d; want cleanup-required/0", status, generation)
		}
		request := recoveryOptions(t, db, plan.SagaID, 1).RequestID
		before := storeInventory(t, db)
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		// The unproved cleanup-required provision attempt is §9.4's no-burn
		// exception: no CAS, receipt, event, or generation is committed, and
		// the unchanged handoff still advertises the identical request.
		if rec.Result.Status != "cleanup-required" || rec.Result.RecoveryGeneration != 0 ||
			rec.Result.RecoverRequestID == nil || *rec.Result.RecoverRequestID != request ||
			rec.Result.BrokerRequest != nil {
			t.Fatalf("unattributable recovery = %+v", rec.Result)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "cleanup-required", 0, request)
		if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || !exists {
			t.Fatalf("moved branch after unproved compensation exists=%v err=%v", exists, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate unattributable recovery = %v", err)
		}
	})
}

// TestReconcileWorktreeRecoveryProvisionNeedsBroker pins the needs-broker
// provisioning recovery: creation is delegated exactly once through the §9.5
// fd-3 capability — never by repeating the completed direct attempt — and a
// present exact effect is the reconciled bound state without an invocation.
func TestReconcileWorktreeRecoveryProvisionNeedsBroker(t *testing.T) {
	t.Run("delegates creation through the broker", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.broker.create")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		confineRegistration(t, repo)
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker); err != nil {
			t.Fatal(err)
		}
		functionalFakeBroker(t, "")
		var rec *WorktreeRecoveryReconciliation
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			rec, err = recoveryReconcile(t, db, repo, plan.SagaID, 1)
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "bound" || rec.Result.RecoveryGeneration != 1 || rec.Result.BrokerRequest != nil {
			t.Fatalf("broker-bound recovery = %+v", rec.Result)
		}
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || !registered {
			t.Fatalf("registration after broker create = %v, %v", registered, err)
		}
		if !worktreeBranchAtBase(repo, plan) {
			t.Fatal("broker-created branch is not at the immutable base")
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate broker-bound recovery = %v", err)
		}
	})
	t.Run("present exact effect binds without an invocation", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.broker.present")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker); err != nil {
			t.Fatal(err)
		}
		unresolveBroker(t)
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "bound" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("idempotent broker-bound recovery = %+v", rec.Result)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate idempotent broker recovery = %v", err)
		}
	})
	t.Run("terminal run compensates instead of delegating", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.broker.terminal")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.CancelRun(opts.RunID, opts.Owner, opts.ClaimToken, opts.Attempt,
			opts.AttemptToken, false, "run.cancel.wt.rec.broker.terminal"); err != nil {
			t.Fatal(err)
		}
		// A broker is resolvable and a capability present: the ineligible
		// recovery still never delegates creation.
		functionalFakeBroker(t, "")
		var rec *WorktreeRecoveryReconciliation
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			rec, err = recoveryReconcile(t, db, repo, plan.SagaID, 1)
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "failed" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("ineligible needs-broker recovery = %+v", rec.Result)
		}
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || registered {
			t.Fatalf("registration after ineligible recovery = %v, %v", registered, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate ineligible needs-broker recovery = %v", err)
		}
	})
}

// TestReconcileWorktreeRecoveryProvisionBrokerTaxonomy pins §9.4's
// commits-nothing rule for broker work that reaches no reconciled outcome:
// every failure class leaves the nonterminal status, generation, and
// advertised id unchanged with no receipt or event.
func TestReconcileWorktreeRecoveryProvisionBrokerTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		test func(t *testing.T)
	}{
		{"no capability", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerProvision(t, "nocap")
			before := storeInventory(t, db)
			withoutBrokerCapabilityFD(t, func() {
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				var conflict *WorktreeRecoveryCapabilityConflict
				if !errors.As(err, &conflict) || conflict.SagaID != plan.SagaID {
					t.Fatalf("no capability = %v", err)
				}
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
		{"unresolvable broker", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerProvision(t, "unresolved")
			before := storeInventory(t, db)
			withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
				unresolveBroker(t)
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				requireConfined(t, err)
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
		{"explicit refusal", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerProvision(t, "refusal")
			fakeBroker(t, "#!/bin/sh\necho refused for reasons >&2\nexit 9\n")
			before := storeInventory(t, db)
			withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				var refusal *WorktreeBrokerRefusal
				if !errors.As(err, &refusal) || refusal.Reason != "refused for reasons" {
					t.Fatalf("refusal = %v", err)
				}
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
		{"timeout", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerProvision(t, "timeout")
			fakeBroker(t, "#!/bin/sh\nexec sleep 30\n")
			oldTimeout := brokerTimeout
			brokerTimeout = 250 * time.Millisecond
			t.Cleanup(func() { brokerTimeout = oldTimeout })
			before := storeInventory(t, db)
			withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				var refusal *WorktreeBrokerRefusal
				if !errors.As(err, &refusal) || refusal.Reason != "broker timed out" {
					t.Fatalf("timeout = %v", err)
				}
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
		{"uncertain success", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerProvision(t, "uncertain")
			fakeBroker(t, "#!/bin/sh\nexit 0\n")
			before := storeInventory(t, db)
			withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				var uncertain *WorktreeRecoveryBrokerUncertain
				if !errors.As(err, &uncertain) || uncertain.Verb != "create" {
					t.Fatalf("uncertain = %v", err)
				}
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, tt.test)
	}
}

// recoveryNeedsBrokerProvision plans and finalizes one provision saga to
// needs-broker at generation zero in a real repository, with nothing created
// on disk.
func recoveryNeedsBrokerProvision(t *testing.T, suffix string) (*DB, string, WorktreeProvisionOptions, *WorktreeProvisionPlan) {
	t.Helper()
	db, repo, opts := provisionHostFixture(t, "wt.rec.taxonomy."+suffix)
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker); err != nil {
		t.Fatal(err)
	}
	return db, repo, opts, plan
}

// TestReconcileWorktreeRecoveryProvisionCleanupRequired pins the §9.4
// cleanup-only provision: proved absence terminalizes failed, an unproved
// attempt commits nothing, a confined compensating registration write carries
// the remove-verb mint bindings on the unchanged handoff, and the delegated
// exact step terminalizes only after absence is proved.
func TestReconcileWorktreeRecoveryProvisionCleanupRequired(t *testing.T) {
	t.Run("proved absence terminalizes failed", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.cleanup.proved")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionCleanupRequired); err != nil {
			t.Fatal(err)
		}
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "failed" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("proved cleanup recovery = %+v", rec.Result)
		}
		receipt, err := db.FindReceipt(ReceiptKindWorktree, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		if err != nil || receipt == nil || receipt["status"] != "failed" {
			t.Fatalf("failed recovery receipt = %v, %v", receipt, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate proved cleanup recovery = %v", err)
		}
	})
	t.Run("deleted target recovers through registration backref", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.cleanup.deleted-target")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		admin, err := worktreeAdminDir(repo, plan.TargetPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionCleanupRequired); err != nil {
			t.Fatal(err)
		}
		if err := os.RemoveAll(plan.TargetPath); err != nil {
			t.Fatal(err)
		}
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || !registered {
			t.Fatalf("dangling registration before recovery = %v, %v", registered, err)
		}
		unrecognized := filepath.Join(plan.RegistrationRoot, "unrecognized")
		if err := os.Mkdir(unrecognized, 0o755); err != nil {
			t.Fatal(err)
		}

		first, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if first.Result.Status != "failed" || first.Result.RecoveryGeneration != 1 {
			t.Fatalf("first recovery after target deletion = %+v", first.Result)
		}
		second, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		if second.EffectRequired || second.Result == nil || second.Result.Status != "failed" ||
			second.Result.RecoveryGeneration != 1 {
			t.Fatalf("second recovery after target deletion = %+v", second)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target after recovery: %v", err)
		}
		if _, err := os.Lstat(admin); !os.IsNotExist(err) {
			t.Fatalf("admin registration after recovery: %v", err)
		}
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || registered {
			t.Fatalf("registration after recovery = %v, %v", registered, err)
		}
		if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || exists {
			t.Fatalf("base-guarded branch after recovery exists=%v err=%v", exists, err)
		}
		if info, err := os.Lstat(unrecognized); err != nil || !info.IsDir() {
			t.Fatalf("unrecognized registration entry was disturbed: %v, %v", info, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate deleted-target recovery = %v", err)
		}
	})
	t.Run("unproved commits nothing", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.cleanup.unproved")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionCleanupRequired); err != nil {
			t.Fatal(err)
		}
		// An unattributable branch keeps absence unproved.
		git(t, repo, "branch", plan.Branch, mainOID(t, repo)+"~1")
		request := recoveryOptions(t, db, plan.SagaID, 1).RequestID
		before := storeInventory(t, db)
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "cleanup-required" || rec.Result.RecoveryGeneration != 0 ||
			rec.Result.RecoverRequestID == nil || *rec.Result.RecoverRequestID != request ||
			rec.Result.BrokerRequest != nil {
			t.Fatalf("unproved cleanup handoff = %+v", rec.Result)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "cleanup-required", 0, request)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate unproved cleanup recovery = %v", err)
		}
	})
	t.Run("confined compensation carries the mint bindings", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.cleanup.confined")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionCleanupRequired); err != nil {
			t.Fatal(err)
		}
		confineRegistration(t, repo)
		unresolveBroker(t)
		request := recoveryOptions(t, db, plan.SagaID, 1).RequestID
		before := storeInventory(t, db)
		withoutBrokerCapabilityFD(t, func() {
			rec, rerr := recoveryReconcile(t, db, repo, plan.SagaID, 1)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if rec.Result.Status != "cleanup-required" || rec.Result.RecoveryGeneration != 0 ||
				rec.Result.BrokerRequest == nil {
				t.Fatalf("confined cleanup handoff = %+v", rec.Result)
			}
			mint := rec.Result.BrokerRequest
			if mint.Verb != "remove" || mint.TargetPath != plan.TargetPath ||
				mint.RegistrationRoot != plan.RegistrationRoot || mint.Force ||
				mint.RecoverRequestID != request || mint.BaseCommit == nil || *mint.BaseCommit != plan.BaseCommit {
				t.Fatalf("confined compensation mint bindings = %+v", mint)
			}
		})
		recoveryNothingCommitted(t, db, plan.SagaID, before, "cleanup-required", 0, request)
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || !registered {
			t.Fatalf("confined registration disturbed = %v, %v", registered, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate confined cleanup recovery = %v", err)
		}
	})
	t.Run("delegated confined step terminalizes failed", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.cleanup.delegate")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionCleanupRequired); err != nil {
			t.Fatal(err)
		}
		confineRegistration(t, repo)
		functionalFakeBroker(t, "")
		var rec *WorktreeRecoveryReconciliation
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			rec, err = recoveryReconcile(t, db, repo, plan.SagaID, 1)
		})
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "failed" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("delegated cleanup recovery = %+v", rec.Result)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target after delegated cleanup: %v", err)
		}
		if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || exists {
			t.Fatalf("branch after delegated cleanup exists=%v err=%v", exists, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate delegated cleanup recovery = %v", err)
		}
	})
	t.Run("broker refusal during confined compensation commits nothing", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.cleanup.refused")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionCleanupRequired); err != nil {
			t.Fatal(err)
		}
		confineRegistration(t, repo)
		fakeBroker(t, "#!/bin/sh\necho not today >&2\nexit 5\n")
		request := recoveryOptions(t, db, plan.SagaID, 1).RequestID
		before := storeInventory(t, db)
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
			var refusal *WorktreeBrokerRefusal
			if !errors.As(err, &refusal) || refusal.Reason != "not today" {
				t.Fatalf("confined compensation refusal = %v", err)
			}
		})
		recoveryNothingCommitted(t, db, plan.SagaID, before, "cleanup-required", 0, request)
		if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || !registered {
			t.Fatalf("registration after refused delegation = %v, %v", registered, err)
		}
	})
}

// TestReconcileWorktreeRecoveryRemoval pins the removal recovery paths: a
// pending or cleanup-required attempt performs the exact staged removal
// directly, a runless automatic row removes through its source identity,
// confinement records needs-broker for the later §9.5 delegation, the
// delegated remove completes the removal, and a failed observation under
// cleanup authority reconciles to cleanup-required.
func TestReconcileWorktreeRecoveryRemoval(t *testing.T) {
	t.Run("pending removal removes exactly", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.removed", false)
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "removed" || rec.Result.RecoveryGeneration != 1 ||
			rec.Result.SagaToken != 0 || rec.Result.BrokerRequest != nil || rec.Cause != nil {
			t.Fatalf("removed recovery = %+v cause=%v", rec.Result, rec.Cause)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target after removed recovery: %v", err)
		}
		if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || !exists {
			t.Fatalf("retained branch after removed recovery exists=%v err=%v", exists, err)
		}
		if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, plan.SourceSagaID); active != 0 {
			t.Fatalf("source active after removed recovery = %d", active)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate removed recovery = %v", err)
		}
	})
	t.Run("cleanup-required removal completes", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.cleanup", false)
		if _, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveCleanupRequired); err != nil {
			t.Fatal(err)
		}
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "removed" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("completed cleanup recovery = %+v", rec.Result)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target after completed cleanup recovery: %v", err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate completed cleanup recovery = %v", err)
		}
	})
	t.Run("runless automatic row removes through its source", func(t *testing.T) {
		db, repo, opts := provisionHostFixture(t, "wt.rec.auto")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		git(t, repo, "worktree", "add", "-b", plan.Branch, plan.TargetPath, plan.BaseCommit)
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionBound); err != nil {
			t.Fatal(err)
		}
		counter, err := db.Fencing()
		if err != nil {
			t.Fatal(err)
		}
		fence := counter + 1
		if _, err := db.db.Exec(`UPDATE meta SET value=? WHERE key='fencing'`, fence); err != nil {
			t.Fatal(err)
		}
		request := automaticCleanupRequestID(testFamilyID(t, db), plan.SagaID, 0)
		created := nowUTC()
		insert, err := db.db.Exec(`INSERT INTO worktree_sagas(
			run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
			request_id,recovery_generation,owner_boundary,registration_root,target_path,
			branch,base_commit,force,created_at,updated_at,extra)
			VALUES (NULL,?,?,?,'remove','pending',0,?,?,0,?,?,?,?,?,0,?,?,NULL)`,
			plan.SagaID, plan.Tag, plan.Task, fence, request, plan.OwnerBoundary,
			plan.RegistrationRoot, plan.TargetPath, plan.Branch, plan.BaseCommit, created, created)
		if err != nil {
			t.Fatal(err)
		}
		sagaID, err := insert.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		rec, err := recoveryReconcile(t, db, repo, sagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "removed" || rec.Result.RecoveryGeneration != 1 || rec.Result.RunID != nil {
			t.Fatalf("runless automatic removed recovery = %+v", rec.Result)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target after runless automatic recovery: %v", err)
		}
		if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, plan.SagaID); active != 0 {
			t.Fatalf("source active after runless automatic recovery = %d", active)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate runless automatic recovery = %v", err)
		}
	})
	t.Run("confinement records needs-broker then the broker completes", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.confined", false)
		confineRegistration(t, repo)
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "needs-broker" || rec.Result.RecoveryGeneration != 1 ||
			rec.Result.BrokerRequest == nil || rec.Result.BrokerRequest.Verb != "remove" ||
			rec.Result.BrokerRequest.RegistrationRoot != plan.RegistrationRoot ||
			rec.Result.RecoverRequestID == nil ||
			*rec.Result.RecoverRequestID != recoveryOptions(t, db, plan.SagaID, 2).RequestID {
			t.Fatalf("confined removal handoff = %+v", rec.Result)
		}
		if rec.Cause == nil || rec.Cause.Error() != errWorktreeConfined.Error() {
			t.Fatalf("confined removal cause = %v", rec.Cause)
		}
		// The staged target was restored: the worktree is intact.
		if dirty, err := worktreeDirty(plan.TargetPath); err != nil || dirty {
			t.Fatalf("restored target dirty=%v err=%v", dirty, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate confined removal recovery = %v", err)
		}
		functionalFakeBroker(t, "")
		var second *WorktreeRecoveryReconciliation
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			second, err = recoveryReconcile(t, db, repo, plan.SagaID, 2)
		})
		if err != nil {
			t.Fatal(err)
		}
		if second.Result.Status != "removed" || second.Result.RecoveryGeneration != 2 {
			t.Fatalf("broker-removed recovery = %+v", second.Result)
		}
		if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
			t.Fatalf("target after broker removal: %v", err)
		}
		if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, plan.SourceSagaID); active != 0 {
			t.Fatalf("source active after broker removal = %d", active)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate broker removal recovery = %v", err)
		}
	})
	t.Run("dirty task-done removal reconciles to cleanup-required", func(t *testing.T) {
		db, repo, options, plan := removalEffectFixture(t, "rec.dirty", false)
		task, err := db.GetTask(plan.Tag, plan.Task)
		if err != nil {
			t.Fatal(err)
		}
		gate := &ClaimGate{Owner: options.Owner, Token: options.ClaimToken, RunID: options.RunID,
			Attempt: options.Attempt, AttemptToken: options.AttemptToken}
		if _, err := db.SetStatus(plan.Tag, plan.Task, "done", task.Revision, gate); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(plan.TargetPath, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		rec, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "cleanup-required" || rec.Result.RecoveryGeneration != 1 {
			t.Fatalf("dirty task-done recovery = %+v", rec.Result)
		}
		var conflict *WorktreeConflict
		if !errors.As(rec.Cause, &conflict) || conflict.Kind != "dirty" {
			t.Fatalf("dirty task-done cause = %v", rec.Cause)
		}
		if _, err := os.Lstat(plan.TargetPath); err != nil {
			t.Fatalf("dirty target disturbed by refused removal: %v", err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate dirty task-done recovery = %v", err)
		}
	})
}

// TestReconcileWorktreeRecoveryRemovalBrokerTaxonomy pins the removal
// counterpart of §9.4's commits-nothing broker taxonomy for the delegated
// remove verb.
func TestReconcileWorktreeRecoveryRemovalBrokerTaxonomy(t *testing.T) {
	cases := []struct {
		name string
		test func(t *testing.T)
	}{
		{"no capability", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerRemoval(t, "nocap")
			before := storeInventory(t, db)
			withoutBrokerCapabilityFD(t, func() {
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				var conflict *WorktreeRecoveryCapabilityConflict
				if !errors.As(err, &conflict) || conflict.SagaID != plan.SagaID {
					t.Fatalf("no capability = %v", err)
				}
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
		{"unresolvable broker", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerRemoval(t, "unresolved")
			before := storeInventory(t, db)
			withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
				unresolveBroker(t)
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				requireConfined(t, err)
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
		{"explicit refusal", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerRemoval(t, "refusal")
			fakeBroker(t, "#!/bin/sh\necho registration busy >&2\nexit 4\n")
			before := storeInventory(t, db)
			withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				var refusal *WorktreeBrokerRefusal
				if !errors.As(err, &refusal) || refusal.Reason != "registration busy" {
					t.Fatalf("refusal = %v", err)
				}
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
		{"uncertain success", func(t *testing.T) {
			db, repo, _, plan := recoveryNeedsBrokerRemoval(t, "uncertain")
			fakeBroker(t, "#!/bin/sh\nexit 0\n")
			before := storeInventory(t, db)
			withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
				_, err := recoveryReconcile(t, db, repo, plan.SagaID, 1)
				var uncertain *WorktreeRecoveryBrokerUncertain
				if !errors.As(err, &uncertain) || uncertain.Verb != "remove" {
					t.Fatalf("uncertain = %v", err)
				}
			})
			recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, recoveryOptions(t, db, plan.SagaID, 1).RequestID)
		}},
	}
	for _, tt := range cases {
		t.Run(tt.name, tt.test)
	}
}

// recoveryNeedsBrokerRemoval plans and finalizes one linked removal saga to
// needs-broker at generation zero with the worktree still on disk.
func recoveryNeedsBrokerRemoval(t *testing.T, suffix string) (*DB, string, WorktreeRemoveOptions, *WorktreeRemovePlan) {
	t.Helper()
	db, repo, options, plan := removalEffectFixture(t, "rec.taxonomy."+suffix, false)
	if _, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveNeedsBroker); err != nil {
		t.Fatal(err)
	}
	return db, repo, options, plan
}

// TestReconcileWorktreeRecoveryLegacyCleanup pins the runless sourceless
// legacy rows: the registration root is derived at effect time because the
// row stores none, the derived root is threaded onto the prune-registration
// mint binding without ever being persisted, the direct staged removal
// terminalizes removed, and a confined registration write defers to the
// broker's prune-registration verb.
func TestReconcileWorktreeRecoveryLegacyCleanup(t *testing.T) {
	t.Run("derives the root and removes", func(t *testing.T) {
		db, repo, sagaID, target := recoveryLegacyFixture(t, "removed", true)
		branch := WorktreeBranch("main", legacyTaskID(t, db, sagaID))
		rec, err := recoveryReconcile(t, db, repo, sagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "removed" || rec.Result.RecoveryGeneration != 1 ||
			rec.Result.RunID != nil || rec.Result.BaseCommit != nil {
			t.Fatalf("legacy removed recovery = %+v", rec.Result)
		}
		if _, err := os.Lstat(target); !os.IsNotExist(err) {
			t.Fatalf("legacy target after removed recovery: %v", err)
		}
		if exists, err := worktreeBranchExists(repo, branch); err != nil || !exists {
			t.Fatalf("legacy branch after removed recovery exists=%v err=%v", exists, err)
		}
		if path := scalarInt64(t, db, `SELECT COUNT(*) FROM tasks WHERE wt_path IS NOT NULL`); path != 0 {
			t.Fatalf("legacy binding retained after removed recovery: %d", path)
		}
		if root := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE saga_id=? AND registration_root IS NOT NULL`, sagaID); root != 0 {
			t.Fatalf("derived root persisted into the legacy row: %d", root)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate legacy removed recovery = %v", err)
		}
	})
	t.Run("confined prune handoff carries the derived root", func(t *testing.T) {
		db, repo, sagaID, target := recoveryLegacyFixture(t, "prune", true)
		// Only the registration remains, stale under a confined root.
		if err := os.RemoveAll(target); err != nil {
			t.Fatal(err)
		}
		confineRegistration(t, repo)
		rec, err := recoveryReconcile(t, db, repo, sagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if rec.Result.Status != "needs-broker" || rec.Result.RecoveryGeneration != 1 ||
			rec.Result.BrokerRequest == nil || rec.Result.BrokerRequest.Verb != "prune-registration" ||
			rec.Result.BrokerRequest.RegistrationRoot != filepath.Join(repo, ".git", "worktrees") ||
			rec.Result.BrokerRequest.RecoverRequestID != recoveryOptions(t, db, sagaID, 2).RequestID ||
			rec.Result.BrokerRequest.Force {
			t.Fatalf("legacy prune handoff = %+v mint=%+v", rec.Result, rec.Result.BrokerRequest)
		}
		if rec.Cause == nil || rec.Cause.Error() != errWorktreeConfined.Error() {
			t.Fatalf("legacy prune cause = %v", rec.Cause)
		}
		if root := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE saga_id=? AND registration_root IS NOT NULL`, sagaID); root != 0 {
			t.Fatalf("derived root persisted into the legacy row: %d", root)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate legacy prune handoff = %v", err)
		}
		// The delegated prune completes the stale registration's removal.
		functionalFakeBroker(t, "")
		var second *WorktreeRecoveryReconciliation
		withBrokerCapabilityFD(t, brokerCapabilityDoc(target, WorktreeBranch("main", legacyTaskID(t, db, sagaID)), ""), func() {
			second, err = recoveryReconcile(t, db, repo, sagaID, 2)
		})
		if err != nil {
			t.Fatal(err)
		}
		if second.Result.Status != "removed" || second.Result.RecoveryGeneration != 2 {
			t.Fatalf("legacy broker-pruned recovery = %+v", second.Result)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate legacy broker-pruned recovery = %v", err)
		}
	})
}

// recoveryLegacyFixture seeds one v4→v5 converted legacy cleanup row inside a
// real Git repository family, optionally with the exact worktree on disk.
func recoveryLegacyFixture(t *testing.T, suffix string, worktree bool) (*DB, string, int64, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.invalid")
	git(t, repo, "config", "user.name", "test")
	git(t, repo, "commit", "--allow-empty", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "-m", "ignore")
	dbPath, _ := Path(repo)
	db, err := CreateWithOwner(dbPath, canonicalPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	// The seeded legacy row carries the conversion's high saga fence; the
	// store-wide fencing counter must not sit below it.
	if _, err := db.db.Exec(`UPDATE meta SET value='12345' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	task := addTask(t, db, "main", "legacy recovery "+suffix)
	created := time.Now().UTC().Format(time.RFC3339)
	sagaID := seedLegacyCleanupRow(t, db, task, created)
	target := filepath.Join(repo, ".worktrees", WorktreeName("main", task))
	if worktree {
		git(t, repo, "worktree", "add", "-b", WorktreeBranch("main", task), target, mainOID(t, repo))
	}
	return db, repo, sagaID, target
}

// legacyTaskID reads the task id of one legacy cleanup row.
func legacyTaskID(t *testing.T, db *DB, sagaID int64) string {
	t.Helper()
	var task string
	if err := db.db.QueryRow(`SELECT task_id FROM worktree_sagas WHERE saga_id=?`, sagaID).Scan(&task); err != nil {
		t.Fatal(err)
	}
	return task
}

// TestReconcileWorktreeRecoveryNonlegacyEmptyRootFailsClosed pins the exact
// legacy-only admission of an empty stored registration root: a cleared or
// corrupt root on any other attempt — linked removal, runless automatic
// removal, or any provision — fails closed with the typed mismatch before
// any direct or broker effect (SPEC §9.4).
func TestReconcileWorktreeRecoveryNonlegacyEmptyRootFailsClosed(t *testing.T) {
	t.Run("linked removal", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.empty.root.linked", false)
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		cleared := *attempt
		cleared.RegistrationRoot = ""
		var mismatch *WorktreeEffectIdentityMismatch
		if _, err := ReconcileWorktreeRecoveryEffect(db, repo, &cleared); !errors.As(err, &mismatch) {
			t.Fatalf("cleared linked root = %v", err)
		}
		if _, err := os.Lstat(plan.TargetPath); err != nil {
			t.Fatalf("target disturbed by refused reconciliation: %v", err)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refusal = %v", err)
		}
	})
	t.Run("runless automatic removal", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.empty.root.auto", false)
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		// The runless automatic shape: no live run, but the source edge and
		// immutable base keep it a registered row, never a legacy one.
		cleared := *attempt
		cleared.RegistrationRoot = ""
		cleared.RunID = 0
		var mismatch *WorktreeEffectIdentityMismatch
		if _, err := ReconcileWorktreeRecoveryEffect(db, repo, &cleared); !errors.As(err, &mismatch) {
			t.Fatalf("cleared runless automatic root = %v", err)
		}
		if _, err := os.Lstat(plan.TargetPath); err != nil {
			t.Fatalf("target disturbed by refused reconciliation: %v", err)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
	})
	t.Run("provision never derives a root", func(t *testing.T) {
		db, repo, _, plan := recoveryNeedsBrokerProvision(t, "emptyroot")
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		log := filepath.Join(t.TempDir(), "broker-invoked")
		fakeBroker(t, "#!/bin/sh\nprintf invoked > '"+log+"'\nexit 0\n")
		cleared := *attempt
		cleared.RegistrationRoot = ""
		var mismatch *WorktreeEffectIdentityMismatch
		var err error
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			_, err = ReconcileWorktreeRecoveryEffect(db, repo, &cleared)
		})
		if !errors.As(err, &mismatch) {
			t.Fatalf("cleared provision root = %v", err)
		}
		if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
			t.Fatalf("refused reconciliation invoked the broker: %v", statErr)
		}
		if registered, rerr := worktreeRegistered(repo, plan.TargetPath); rerr != nil || registered {
			t.Fatalf("registration after refused delegation = %v, %v", registered, rerr)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, attempt.RequestID)
	})
}

// divertRepoFamily moves the repository's Git directory aside and leaves a
// gitdir marker pointing at a foreign family, so the next §9.4 family
// rederivation can no longer equal the immutable row. The returned func
// reinstates the original family and is safe to call more than once.
func divertRepoFamily(t *testing.T, repo string) func() {
	t.Helper()
	foreign := t.TempDir()
	git(t, foreign, "init", "-b", "main")
	moved := filepath.Join(t.TempDir(), "moved-git")
	if err := os.Rename(filepath.Join(repo, ".git"), moved); err != nil {
		t.Fatal(err)
	}
	marker := "gitdir: " + filepath.Join(foreign, ".git") + "\n"
	if err := os.WriteFile(filepath.Join(repo, ".git"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if restored {
			return
		}
		restored = true
		if err := os.Remove(filepath.Join(repo, ".git")); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, filepath.Join(repo, ".git")); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(restore)
	return restore
}

// divertFamilyOnCall installs the prove hook so the repository family
// changes immediately before the call-th per-effect rederivation — after the
// driver's entry derivation and any earlier effect of the same attempt — and
// returns how many rederivations ran and the family's restore func.
func divertFamilyOnCall(t *testing.T, repo string, call int) (calls func() int, restore func()) {
	t.Helper()
	count := 0
	var reinstate func()
	old := worktreeRecoveryProveHook
	worktreeRecoveryProveHook = func() error {
		count++
		if count == call {
			reinstate = divertRepoFamily(t, repo)
		}
		return nil
	}
	t.Cleanup(func() { worktreeRecoveryProveHook = old })
	return func() int { return count }, func() {
		if reinstate != nil {
			reinstate()
		}
	}
}

// relocateOwnerOnCall installs the prove hook so the family's registered
// primary working tree relocates immediately before the call-th per-effect
// rederivation — after the driver's entry derivation — for a common
// directory that is not a plain .git directory: core.worktree is repointed
// at a verbatim copy of the working tree, which validates against the same
// common directory. The relocation retains the common directory, so every
// registration-root proof — including the one inside the direct provision
// effect — still passes and only the rederived owner boundary can refuse.
// It returns how many rederivations ran and the registration's restore func.
func relocateOwnerOnCall(t *testing.T, work, gitdir, moved string, call int) (calls func() int, restore func()) {
	t.Helper()
	count := 0
	relocated := false
	config := filepath.Join(gitdir, "config")
	old := worktreeRecoveryProveHook
	worktreeRecoveryProveHook = func() error {
		count++
		if count == call && !relocated {
			relocated = true
			copyTree(t, work, moved)
			git(t, work, "config", "--file", config, "core.worktree", moved)
		}
		return nil
	}
	restore = func() {
		if relocated {
			relocated = false
			git(t, work, "config", "--file", config, "core.worktree", work)
		}
	}
	t.Cleanup(func() {
		worktreeRecoveryProveHook = old
		restore()
	})
	return func() int { return count }, restore
}

// TestReconcileWorktreeRecoveryRederivesBeforeEveryEffect pins §9.4's
// per-effect rederivation: when the repository family changes after the
// driver's entry derivation — between two effects of one attempt — the
// immediate rederivation before the next direct or broker effect fails
// closed with the typed mismatch, no effect, and no row rewrite.
func TestReconcileWorktreeRecoveryRederivesBeforeEveryEffect(t *testing.T) {
	assertMismatch := func(t *testing.T, err error) {
		t.Helper()
		var mismatch *WorktreeEffectIdentityMismatch
		if !errors.As(err, &mismatch) {
			t.Fatalf("identity change between effects = %v", err)
		}
	}
	t.Run("direct removal", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.prove.direct", false)
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		calls, restore := divertFamilyOnCall(t, repo, 1)
		_, err := ReconcileWorktreeRecoveryEffect(db, repo, attempt)
		restore()
		assertMismatch(t, err)
		if calls() != 1 {
			t.Fatalf("per-effect rederivations = %d", calls())
		}
		if _, terr := os.Lstat(plan.TargetPath); terr != nil {
			t.Fatalf("target disturbed by refused reconciliation: %v", terr)
		}
		if _, serr := os.Lstat(removalTargetStage(plan)); !os.IsNotExist(serr) {
			t.Fatalf("staging path left by refused reconciliation: %v", serr)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refusal = %v", err)
		}
	})
	t.Run("direct pending provision", func(t *testing.T) {
		// The family's common directory is a separate git directory, so the
		// owner boundary is the core.worktree registration it carries — the
		// shape a relocated family has. Relocating that registration after
		// the entry proof retains the common directory: the effect's own
		// registration-root proof still passes and its owner check compares
		// two row-supplied values, so only the immediate per-effect
		// rederivation of the owner boundary can refuse.
		root := t.TempDir()
		work := filepath.Join(root, "work")
		gitdir := filepath.Join(root, "gitdir")
		git(t, root, "init", "-b", "main", "--separate-git-dir", gitdir, work)
		git(t, work, "config", "--file", filepath.Join(gitdir, "config"), "core.worktree", work)
		db, _, opts := provisionHostSeed(t, "wt.rec.prove.direct", work)
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		calls, restore := relocateOwnerOnCall(t, work, gitdir, filepath.Join(root, "moved"), 1)
		_, err = ReconcileWorktreeRecoveryEffect(db, work, attempt)
		restore()
		assertMismatch(t, err)
		if calls() != 1 {
			t.Fatalf("per-effect rederivations = %d", calls())
		}
		if _, terr := os.Lstat(plan.TargetPath); !os.IsNotExist(terr) {
			t.Fatalf("target created by refused reconciliation: %v", terr)
		}
		if registered, rerr := worktreeRegistered(work, plan.TargetPath); rerr != nil || registered {
			t.Fatalf("registration after refused reconciliation = %v, %v", registered, rerr)
		}
		if exists, berr := worktreeBranchExists(work, plan.Branch); berr != nil || exists {
			t.Fatalf("branch created by refused reconciliation = %v, %v", exists, berr)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refusal = %v", err)
		}
	})
	t.Run("delegated removal", func(t *testing.T) {
		db, repo, _, plan := recoveryNeedsBrokerRemoval(t, "prove")
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		log := filepath.Join(t.TempDir(), "broker-invoked")
		fakeBroker(t, "#!/bin/sh\nprintf invoked > '"+log+"'\nexit 0\n")
		calls, restore := divertFamilyOnCall(t, repo, 1)
		var err error
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			_, err = ReconcileWorktreeRecoveryEffect(db, repo, attempt)
		})
		restore()
		assertMismatch(t, err)
		if calls() != 1 {
			t.Fatalf("per-effect rederivations = %d", calls())
		}
		if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
			t.Fatalf("identity change still delegated to the broker: %v", statErr)
		}
		if _, terr := os.Lstat(plan.TargetPath); terr != nil {
			t.Fatalf("target disturbed by refused delegation: %v", terr)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, attempt.RequestID)
	})
	t.Run("delegated provision creation", func(t *testing.T) {
		db, repo, _, plan := recoveryNeedsBrokerProvision(t, "prove")
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		log := filepath.Join(t.TempDir(), "broker-invoked")
		fakeBroker(t, "#!/bin/sh\nprintf invoked > '"+log+"'\nexit 0\n")
		calls, restore := divertFamilyOnCall(t, repo, 1)
		var err error
		withBrokerCapabilityFD(t, brokerCapabilityDoc(plan.TargetPath, plan.Branch, plan.BaseCommit), func() {
			_, err = ReconcileWorktreeRecoveryEffect(db, repo, attempt)
		})
		restore()
		assertMismatch(t, err)
		if calls() != 1 {
			t.Fatalf("per-effect rederivations = %d", calls())
		}
		if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
			t.Fatalf("identity change still delegated to the broker: %v", statErr)
		}
		if registered, rerr := worktreeRegistered(repo, plan.TargetPath); rerr != nil || registered {
			t.Fatalf("registration after refused delegation = %v, %v", registered, rerr)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "needs-broker", 0, attempt.RequestID)
	})
	t.Run("legacy target half then delegation", func(t *testing.T) {
		db, repo, sagaID, target := recoveryLegacyFixture(t, "prove.legacy", true)
		confineRegistration(t, repo)
		branch := WorktreeBranch("main", legacyTaskID(t, db, sagaID))
		first, err := recoveryReconcile(t, db, repo, sagaID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if first.Result.Status != "needs-broker" || first.Result.BrokerRequest == nil ||
			first.Result.BrokerRequest.Verb != "prune-registration" {
			t.Fatalf("confined legacy first attempt = %+v", first.Result)
		}
		attempt := recoveryAttemptOf(t, db, sagaID, 2)
		before := storeInventory(t, db)
		log := filepath.Join(t.TempDir(), "broker-invoked")
		fakeBroker(t, "#!/bin/sh\nprintf invoked > '"+log+"'\nexit 0\n")
		// The first rederivation gates the target half; the family changes
		// between that completed effect and the broker delegation.
		calls, restore := divertFamilyOnCall(t, repo, 2)
		withBrokerCapabilityFD(t, brokerCapabilityDoc(target, branch, ""), func() {
			_, err = ReconcileWorktreeRecoveryEffect(db, repo, attempt)
		})
		restore()
		assertMismatch(t, err)
		if calls() != 2 {
			t.Fatalf("per-effect rederivations = %d", calls())
		}
		if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
			t.Fatalf("identity change still delegated to the broker: %v", statErr)
		}
		// The earlier target half legitimately completed before the change;
		// the gated registration write and the commit never ran.
		if _, terr := os.Lstat(target); !os.IsNotExist(terr) {
			t.Fatalf("legacy target after target half: %v", terr)
		}
		if exists, berr := worktreeBranchExists(repo, branch); berr != nil || !exists {
			t.Fatalf("legacy branch after refused delegation exists=%v err=%v", exists, berr)
		}
		recoveryNothingCommitted(t, db, sagaID, before, "needs-broker", 1, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refused delegation = %v", err)
		}
	})
}

// TestReconcileWorktreeRecoveryRejectsCorruptIDShapes pins the exact
// nullable-ID shapes at the driver: a handoff carrying negative run or
// source ids — with the stored registration root otherwise intact — passes
// the entry root proof but never reaches an effect, because the per-effect
// plan proof refuses the corrupt shape before the staged removal (SPEC §9.4).
func TestReconcileWorktreeRecoveryRejectsCorruptIDShapes(t *testing.T) {
	t.Run("negative run id", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.ids.run", false)
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		corrupt := *attempt
		corrupt.RunID = -3
		_, err := ReconcileWorktreeRecoveryEffect(db, repo, &corrupt)
		if err == nil || err.Error() != "worktree remove effect identity is invalid" {
			t.Fatalf("negative run id = %v", err)
		}
		if _, terr := os.Lstat(plan.TargetPath); terr != nil {
			t.Fatalf("target disturbed by refused reconciliation: %v", terr)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refusal = %v", err)
		}
	})
	t.Run("negative source saga id", func(t *testing.T) {
		db, repo, _, plan := removalEffectFixture(t, "rec.ids.source", false)
		attempt := recoveryAttemptOf(t, db, plan.SagaID, 1)
		before := storeInventory(t, db)
		// The near-legacy shape: runless and baseless, but a negative
		// source edge is a corrupt identity, never a legacy row.
		corrupt := *attempt
		corrupt.RunID = 0
		corrupt.SourceSagaID = -7
		corrupt.BaseCommit = ""
		_, err := ReconcileWorktreeRecoveryEffect(db, repo, &corrupt)
		if err == nil || err.Error() != "worktree remove effect identity is invalid" {
			t.Fatalf("negative source saga id = %v", err)
		}
		if _, terr := os.Lstat(plan.TargetPath); terr != nil {
			t.Fatalf("target disturbed by refused reconciliation: %v", terr)
		}
		recoveryNothingCommitted(t, db, plan.SagaID, before, "pending", 0, attempt.RequestID)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate after refusal = %v", err)
		}
	})
}
