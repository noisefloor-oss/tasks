package store

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// removalEffectFixture builds a real Git repository family with one task, a
// live claim/run, a committed bound provision saga, and the exact on-disk
// worktree and registration that provision would have created. It returns the
// planned removal saga, ready for the trusted effect and finalization.
func removalEffectFixture(t *testing.T, suffix string, force bool) (*DB, string, WorktreeRemoveOptions, *WorktreeRemovePlan) {
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
	dbPath, inGit := Path(repo)
	if !inGit {
		t.Fatal("fixture repository reported outside Git")
	}
	db, err := CreateWithOwner(dbPath, canonicalPath(repo))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id := addTask(t, db, "main", "effect "+suffix)
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.wt.effect."+suffix)
	if err != nil {
		t.Fatal(err)
	}
	run, replayed, err := db.StartRun("main", id, "agent-a", claim.Token, "run.wt.effect."+suffix, 3600)
	if err != nil || replayed {
		t.Fatalf("start run = %+v replayed=%v err=%v", run, replayed, err)
	}
	_, boundary, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	boundary = canonicalPath(boundary)
	common, ok := backlog.GitCommonDir(boundary)
	if !ok {
		t.Fatal("fixture boundary has no Git common directory")
	}
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(string(out))
	provision := WorktreeProvisionOptions{
		RunID: run.RunID, Owner: "agent-a", ClaimToken: claim.Token,
		Attempt: run.Attempt, AttemptToken: run.AttemptToken,
		RequestID:        "wt.effect.provision." + suffix,
		OwnerBoundary:    boundary,
		RegistrationRoot: canonicalPath(filepath.Join(common, "worktrees")),
		TargetPath:       filepath.Join(boundary, ".worktrees", WorktreeName("main", id)),
		Branch:           WorktreeBranch("main", id), BaseCommit: base,
	}
	pplan, err := db.PlanWorktreeProvision(provision)
	if err != nil {
		t.Fatal(err)
	}
	git(t, repo, "worktree", "add", "-b", provision.Branch, provision.TargetPath, base)
	if _, err := db.FinalizeWorktreeProvision(pplan, WorktreeProvisionBound); err != nil {
		t.Fatal(err)
	}
	options := WorktreeRemoveOptions{RunID: run.RunID, Owner: "agent-a",
		ClaimToken: claim.Token, Attempt: run.Attempt, AttemptToken: run.AttemptToken,
		RequestID: "wt.effect.remove." + suffix, Force: force}
	rplan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	if !rplan.EffectRequired {
		t.Fatalf("fresh removal plan = %+v, want EffectRequired", rplan)
	}
	return db, repo, options, rplan
}

func removalAdminDir(t *testing.T, plan *WorktreeRemovePlan) string {
	t.Helper()
	entries, err := os.ReadDir(plan.RegistrationRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".noise-remove-") {
			return filepath.Join(plan.RegistrationRoot, entry.Name())
		}
	}
	t.Fatalf("no registration entry under %s", plan.RegistrationRoot)
	return ""
}

func TestPerformWorktreeRemovalEffectRemovesExactlyAndRetainsBranch(t *testing.T) {
	db, _, options, plan := removalEffectFixture(t, "removed", false)
	admin := removalAdminDir(t, plan)
	if err := PreflightWorktreeRemovalEffect(plan); err != nil {
		t.Fatalf("preflight = %v", err)
	}
	outcome, err := PerformWorktreeRemovalEffect(plan)
	if outcome != WorktreeRemoveRemoved || err != nil {
		t.Fatalf("effect = %q, %v", outcome, err)
	}
	if _, err := os.Lstat(plan.TargetPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target still exists: %v", err)
	}
	if _, err := os.Lstat(admin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registration still exists: %v", err)
	}
	entries, _ := os.ReadDir(plan.RegistrationRoot)
	for _, entry := range entries {
		t.Fatalf("registration root retains %s", entry.Name())
	}
	// Branch retention: removal never deletes the derived branch.
	if exists, err := worktreeBranchExists(plan.OwnerBoundary, plan.Branch); err != nil || !exists {
		t.Fatalf("retained branch exists=%v err=%v", exists, err)
	}
	// Idempotent replay of the exact effect is again a clean removal.
	again, err := PerformWorktreeRemovalEffect(plan)
	if again != WorktreeRemoveRemoved || err != nil {
		t.Fatalf("effect replay = %q, %v", again, err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, outcome)
	if err != nil || result.Status != "removed" {
		t.Fatalf("finalize = %+v, %v", result, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate removed state = %v", err)
	}
	if _, err := db.PlanWorktreeRemoval(options); err != nil {
		t.Fatalf("request replay after exact removal = %v", err)
	}
}

// TestPerformWorktreeRemovalEffectCrashBoundaries pins every filesystem crash
// boundary: the injected crash leaves the exact saga-named staging paths, the
// observation is cleanup-required, and reperforming the unchanged effect
// completes the removal without any broker.
func TestPerformWorktreeRemovalEffectCrashBoundaries(t *testing.T) {
	for _, point := range []string{
		"after-target-stage", "after-registration-stage",
		"after-registration-remove", "after-target-remove",
	} {
		t.Run(point, func(t *testing.T) {
			db, _, _, plan := removalEffectFixture(t, "crash-"+point, false)
			old := worktreeRemoveEffectFaultHook
			worktreeRemoveEffectFaultHook = func(p string) error {
				if p == point {
					return errors.New("injected crash")
				}
				return nil
			}
			outcome, err := PerformWorktreeRemovalEffect(plan)
			if outcome != WorktreeRemoveCleanupRequired || err == nil {
				t.Fatalf("crashed effect = %q, %v", outcome, err)
			}
			worktreeRemoveEffectFaultHook = old
			t.Cleanup(func() { worktreeRemoveEffectFaultHook = old })
			// The staged state must be visible before replay so the test
			// proves replay converges from it (except the final boundary,
			// where everything is already gone).
			if point != "after-target-remove" {
				staged := false
				if _, err := os.Lstat(removalTargetStage(plan)); err == nil {
					staged = true
				}
				entries, _ := os.ReadDir(plan.RegistrationRoot)
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".noise-remove-") {
						staged = true
					}
				}
				if !staged {
					t.Fatalf("crash at %s left no saga-named staging path", point)
				}
			}
			again, err := PerformWorktreeRemovalEffect(plan)
			if again != WorktreeRemoveRemoved || err != nil {
				t.Fatalf("effect replay after %s = %q, %v", point, again, err)
			}
			if exists, err := worktreeBranchExists(plan.OwnerBoundary, plan.Branch); err != nil || !exists {
				t.Fatalf("retained branch after %s exists=%v err=%v", point, exists, err)
			}
			result, err := db.FinalizeWorktreeRemoval(plan, again)
			if err != nil || result.Status != "removed" {
				t.Fatalf("finalize after %s = %+v, %v", point, result, err)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate state after %s = %v", point, err)
			}
		})
	}
}

func TestPerformWorktreeRemovalEffectDirtyRefusalAndForce(t *testing.T) {
	t.Run("refused without force", func(t *testing.T) {
		db, _, _, plan := removalEffectFixture(t, "dirty", false)
		if err := os.WriteFile(filepath.Join(plan.TargetPath, "dirty.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		var conflict *WorktreeConflict
		err := PreflightWorktreeRemovalEffect(plan)
		if !errors.As(err, &conflict) || conflict.Kind != "dirty" || conflict.Path != plan.TargetPath {
			t.Fatalf("dirty preflight = %v (%T)", err, err)
		}
		// When preflight is skipped or dirt arrives after it, the effect
		// observes a failed removal without touching anything.
		outcome, err := PerformWorktreeRemovalEffect(plan)
		if outcome != WorktreeRemoveFailed || !errors.As(err, &conflict) || conflict.Kind != "dirty" {
			t.Fatalf("dirty effect = %q, %v", outcome, err)
		}
		if _, err := os.Lstat(filepath.Join(plan.TargetPath, "dirty.txt")); err != nil {
			t.Fatalf("dirty target was touched: %v", err)
		}
		result, err := db.FinalizeWorktreeRemoval(plan, outcome)
		if err != nil || result.Status != "failed" {
			t.Fatalf("dirty finalize = %+v, %v", result, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate failed-dirty state = %v", err)
		}
	})
	t.Run("force removes dirty worktree and retains branch", func(t *testing.T) {
		db, _, _, plan := removalEffectFixture(t, "dirty-force", true)
		if err := os.WriteFile(filepath.Join(plan.TargetPath, "dirty.txt"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := PreflightWorktreeRemovalEffect(plan); err != nil {
			t.Fatalf("force preflight = %v", err)
		}
		outcome, err := PerformWorktreeRemovalEffect(plan)
		if outcome != WorktreeRemoveRemoved || err != nil {
			t.Fatalf("force effect = %q, %v", outcome, err)
		}
		if exists, err := worktreeBranchExists(plan.OwnerBoundary, plan.Branch); err != nil || !exists {
			t.Fatalf("force retained branch exists=%v err=%v", exists, err)
		}
		result, err := db.FinalizeWorktreeRemoval(plan, outcome)
		if err != nil || result.Status != "removed" {
			t.Fatalf("force finalize = %+v, %v", result, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate force-removed state = %v", err)
		}
	})
}

// TestPerformWorktreeRemovalEffectConfinementNeedsBrokerWithoutBroker pins the
// §9.5 first-attempt contract: an exact permission-class failure of the direct
// registration rename records needs-broker, restores the staged target, keeps
// the branch, and never invokes any broker.
func TestPerformWorktreeRemovalEffectConfinementNeedsBrokerWithoutBroker(t *testing.T) {
	db, repo, _, plan := removalEffectFixture(t, "confined", false)
	confineRegistration(t, repo)
	log := filepath.Join(t.TempDir(), "broker-invoked")
	fakeBroker(t, "#!/bin/sh\nprintf invoked > '"+log+"'\nexit 0\n")

	outcome, err := PerformWorktreeRemovalEffect(plan)
	if outcome != WorktreeRemoveNeedsBroker || !errors.Is(err, errWorktreeConfined) {
		t.Fatalf("confined effect = %q, %v", outcome, err)
	}
	if !errors.Is(err, ErrWorktreeRegistrationConfined) {
		t.Fatalf("public confinement diagnostic = %v", err)
	}
	if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
		t.Fatalf("direct removal invoked the broker: %v", statErr)
	}
	// The staged target was restored byte-identically and nothing was removed.
	if dirty, err := worktreeDirty(plan.TargetPath); err != nil || dirty {
		t.Fatalf("restored target dirty=%v err=%v", dirty, err)
	}
	admin := removalAdminDir(t, plan)
	head, err := os.ReadFile(filepath.Join(admin, "HEAD"))
	if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/"+plan.Branch {
		t.Fatalf("registration after confinement = %q, %v", head, err)
	}
	if exists, err := worktreeBranchExists(plan.OwnerBoundary, plan.Branch); err != nil || !exists {
		t.Fatalf("confined branch exists=%v err=%v", exists, err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, outcome)
	if err != nil || result.Status != "needs-broker" || result.BrokerRequest == nil ||
		result.BrokerRequest.Verb != "remove" || result.RecoverRequestID == nil {
		t.Fatalf("confined finalize = %+v, %v", result, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate needs-broker state = %v", err)
	}
}

func TestPerformWorktreeRemovalEffectFailClosedIdentity(t *testing.T) {
	t.Run("owner boundary unavailable", func(t *testing.T) {
		_, repo, _, plan := removalEffectFixture(t, "boundary", false)
		// The family root disappears: both preflight and effect refuse
		// without an effect or a row rewrite.
		if err := os.RemoveAll(repo); err != nil {
			t.Fatal(err)
		}
		var unavailable *WorktreeOwnerBoundaryUnavailable
		if err := PreflightWorktreeRemovalEffect(plan); !errors.As(err, &unavailable) {
			t.Fatalf("preflight = %v (%T)", err, err)
		}
		outcome, err := PerformWorktreeRemovalEffect(plan)
		if outcome != "" || !errors.As(err, &unavailable) {
			t.Fatalf("effect = %q, %v (%T)", outcome, err, err)
		}
	})
	t.Run("registration root mismatch", func(t *testing.T) {
		_, _, _, plan := removalEffectFixture(t, "mismatch", false)
		tampered := *plan
		tampered.RegistrationRoot = filepath.Join(filepath.Dir(plan.RegistrationRoot), "other-worktrees")
		var mismatch *WorktreeEffectIdentityMismatch
		if err := PreflightWorktreeRemovalEffect(&tampered); !errors.As(err, &mismatch) {
			t.Fatalf("preflight = %v (%T)", err, err)
		}
		outcome, err := PerformWorktreeRemovalEffect(&tampered)
		if outcome != "" || !errors.As(err, &mismatch) {
			t.Fatalf("effect = %q, %v (%T)", outcome, err, err)
		}
		// A failed-closed mismatch leaves the exact worktree untouched.
		if dirty, err := worktreeDirty(plan.TargetPath); err != nil || dirty {
			t.Fatalf("target after failed-closed mismatch dirty=%v err=%v", dirty, err)
		}
	})
}

// TestInspectWorktreeRemovalReadOnly pins the read-only preflight half: it
// never allocates a saga or fence and it returns exactly the identity the
// later planning transaction snapshots.
func TestInspectWorktreeRemovalReadOnly(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "inspect", true)
	beforeFence, _ := db.Fencing()
	var beforeSagas, beforeEvents int
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas`).Scan(&beforeSagas)
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind=?`, ReceiptKindWorktree).Scan(&beforeEvents)
	inspected, err := db.InspectWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	if !inspected.EffectRequired || inspected.Result != nil || inspected.SagaID != 0 || inspected.SagaToken != 0 ||
		inspected.SourceSagaID != source.SagaID || inspected.RunID != options.RunID ||
		inspected.OwnerBoundary != source.OwnerBoundary || inspected.RegistrationRoot != source.RegistrationRoot ||
		inspected.TargetPath != source.TargetPath || inspected.Branch != source.Branch ||
		inspected.BaseCommit != source.BaseCommit || inspected.Force != options.Force ||
		inspected.RequestID != options.RequestID {
		t.Fatalf("inspected = %+v; source=%+v", inspected, source)
	}
	afterFence, _ := db.Fencing()
	var sagas, events int
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas`).Scan(&sagas)
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind=?`, ReceiptKindWorktree).Scan(&events)
	if sagas != beforeSagas || events != beforeEvents || afterFence != beforeFence {
		t.Fatalf("inspection mutated state: sagas=%d events=%d fence=%d->%d", sagas, events, beforeFence, afterFence)
	}
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SourceSagaID != inspected.SourceSagaID || plan.OwnerBoundary != inspected.OwnerBoundary ||
		plan.RegistrationRoot != inspected.RegistrationRoot || plan.TargetPath != inspected.TargetPath ||
		plan.Branch != inspected.Branch || plan.BaseCommit != inspected.BaseCommit ||
		plan.Force != inspected.Force || plan.RequestID != inspected.RequestID {
		t.Fatalf("plan diverged from inspection: %+v vs %+v", plan, inspected)
	}
}

func TestInspectWorktreeRemovalReplay(t *testing.T) {
	t.Run("pending replay repeats the effect", func(t *testing.T) {
		db, options, _, _, _ := removalFixture(t, "inspect-pending", false)
		plan, err := db.PlanWorktreeRemoval(options)
		if err != nil {
			t.Fatal(err)
		}
		inspected, err := db.InspectWorktreeRemoval(options)
		if err != nil || !inspected.EffectRequired || inspected.SagaID != plan.SagaID ||
			inspected.SagaToken != plan.SagaToken || inspected.Result != nil {
			t.Fatalf("pending inspection = %+v, %v", inspected, err)
		}
	})
	t.Run("terminal replay returns the committed handoff", func(t *testing.T) {
		db, options, _, _, _ := removalFixture(t, "inspect-terminal", false)
		plan, err := db.PlanWorktreeRemoval(options)
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveRemoved)
		if err != nil {
			t.Fatal(err)
		}
		inspected, err := db.InspectWorktreeRemoval(options)
		if err != nil || inspected.EffectRequired || inspected.Result == nil ||
			inspected.Result.Status != "removed" || inspected.SagaID != plan.SagaID {
			t.Fatalf("terminal inspection = %+v, %v", inspected, err)
		}
		if inspected.Result.SagaToken != result.SagaToken || inspected.Result.Status != result.Status {
			t.Fatalf("inspection result = %+v, want %+v", inspected.Result, result)
		}
	})
	t.Run("needs-broker replay performs no effect", func(t *testing.T) {
		db, options, _, _, _ := removalFixture(t, "inspect-broker", false)
		plan, err := db.PlanWorktreeRemoval(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveNeedsBroker); err != nil {
			t.Fatal(err)
		}
		inspected, err := db.InspectWorktreeRemoval(options)
		if err != nil || inspected.EffectRequired || inspected.Result == nil ||
			inspected.Result.Status != "needs-broker" || inspected.Result.BrokerRequest == nil {
			t.Fatalf("needs-broker inspection = %+v, %v", inspected, err)
		}
	})
	t.Run("request collision and owner fence", func(t *testing.T) {
		db, options, _, _, _ := removalFixture(t, "inspect-collision", false)
		if _, err := db.PlanWorktreeRemoval(options); err != nil {
			t.Fatal(err)
		}
		collision := options
		collision.Force = !options.Force
		if _, err := db.InspectWorktreeRemoval(collision); !errors.As(err, new(*ReplayConflict)) {
			t.Fatalf("force collision = %v", err)
		}
		other := options
		other.Owner = "agent-b"
		if _, err := db.InspectWorktreeRemoval(other); !errors.As(err, new(*ReplayConflict)) {
			t.Fatalf("owner mismatch against existing saga = %v", err)
		}
	})
	t.Run("fresh request refusals", func(t *testing.T) {
		db, options, _, _, _ := removalFixture(t, "inspect-refuse", false)
		unknown := options
		unknown.RunID += 1000
		unknown.RequestID = "wt.remove.unknown-run"
		if _, err := db.InspectWorktreeRemoval(unknown); !errors.As(err, new(*RunConflict)) {
			t.Fatalf("unknown run = %v", err)
		}
		wrongOwner := options
		wrongOwner.Owner = "agent-b"
		wrongOwner.RequestID = "wt.remove.wrong-owner"
		if _, err := db.InspectWorktreeRemoval(wrongOwner); !errors.As(err, new(*RunConflict)) {
			t.Fatalf("run owner mismatch = %v", err)
		}
	})
}

// TestValidateRemovalEffectIdentityExactIDShapes pins the exact nullable-ID
// shapes of a removal identity: IDs are nonnegative, and zero is admitted
// only for the runless automatic row (RunID zero with a source) and the
// runless sourceless legacy row (both zero, no base). Every other
// combination — negative ids included — is a corrupt identity that refuses
// before any effect (SPEC §9.4).
func TestValidateRemovalEffectIdentityExactIDShapes(t *testing.T) {
	_, _, _, plan := removalEffectFixture(t, "id.shapes", false)
	shapes := []struct {
		name        string
		source, run int64
		base        string
		valid       bool
	}{
		{"linked run", plan.SourceSagaID, plan.RunID, plan.BaseCommit, true},
		{"runless automatic", plan.SourceSagaID, 0, plan.BaseCommit, true},
		{"runless sourceless legacy", 0, 0, "", true},
		{"negative source", -1, 0, "", false},
		{"negative source with a run", -1, plan.RunID, plan.BaseCommit, false},
		{"negative run with a source", plan.SourceSagaID, -1, plan.BaseCommit, false},
		{"negative run without a source", 0, -1, "", false},
		{"both negative", -1, -1, "", false},
		{"run without a source", 0, plan.RunID, plan.BaseCommit, false},
		{"legacy with a base", 0, 0, plan.BaseCommit, false},
		{"source without a base", plan.SourceSagaID, plan.RunID, "", false},
	}
	for _, tt := range shapes {
		t.Run(tt.name, func(t *testing.T) {
			shaped := *plan
			shaped.SourceSagaID, shaped.RunID, shaped.BaseCommit = tt.source, tt.run, tt.base
			err := validateRemovalEffectIdentity(&shaped)
			if tt.valid && err != nil {
				t.Fatalf("valid shape refused: %v", err)
			}
			if !tt.valid && (err == nil || err.Error() != "worktree remove effect identity is invalid") {
				t.Fatalf("corrupt shape = %v", err)
			}
		})
	}
	t.Run("negative ids refuse before any effect", func(t *testing.T) {
		_, _, _, plan := removalEffectFixture(t, "id.negative", false)
		corrupt := *plan
		corrupt.SourceSagaID = -plan.SourceSagaID
		outcome, err := PerformWorktreeRemovalEffect(&corrupt)
		if outcome != "" || err == nil || err.Error() != "worktree remove effect identity is invalid" {
			t.Fatalf("negative-source effect = %q, %v", outcome, err)
		}
		if dirty, derr := worktreeDirty(plan.TargetPath); derr != nil || dirty {
			t.Fatalf("target after refused effect dirty=%v err=%v", dirty, derr)
		}
		if admin := removalAdminDir(t, plan); admin == "" {
			t.Fatal("registration lost by refused effect")
		}
	})
}

// stageDegradedQuarantine reproduces the stuck cleanup-required shape of SPEC
// §9.4: a partial deletion already removed the staged target's .git marker and
// the staged registration, while undeletable content may remain inside the
// saga-named quarantine directory.
func stageDegradedQuarantine(t *testing.T, plan *WorktreeRemovePlan, residual bool) string {
	t.Helper()
	admin := removalAdminDir(t, plan)
	stage := removalTargetStage(plan)
	if err := os.Rename(plan.TargetPath, stage); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(stage, ".git")); err != nil {
		t.Fatal(err)
	}
	adminStage := removalAdminStage(plan, admin)
	if err := os.Rename(admin, adminStage); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(adminStage); err != nil {
		t.Fatal(err)
	}
	if residual {
		dir := filepath.Join(stage, "deploy", "__pycache__")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "app.cpython-312.pyc"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		// A read-only quarantine directory denies unlink to the invoking
		// (non-root) test principal — the portable EACCES stand-in for a
		// root-owned residual the host may never chown or chmod.
		if err := os.Chmod(stage, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(stage, 0o755) })
	}
	return stage
}

// TestPerformWorktreeRemovalEffectQuarantineGitGoneCompletes pins the §9.4
// idempotent replay when the quarantine directory has already lost its .git:
// the saga-named stage path is the ownership proof, the effect finishes the
// deletion, and the saga finalizes removed.
func TestPerformWorktreeRemovalEffectQuarantineGitGoneCompletes(t *testing.T) {
	db, _, options, plan := removalEffectFixture(t, "gitgone", false)
	stage := stageDegradedQuarantine(t, plan, false)
	outcome, err := PerformWorktreeRemovalEffect(plan)
	if outcome != WorktreeRemoveRemoved || err != nil {
		t.Fatalf("effect = %q, %v", outcome, err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine still exists: %v", err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, outcome)
	if err != nil || result.Status != "removed" {
		t.Fatalf("finalize = %+v, %v", result, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate removed state = %v", err)
	}
	if _, err := db.PlanWorktreeRemoval(options); err != nil {
		t.Fatalf("request replay after exact removal = %v", err)
	}
}

// TestPerformWorktreeRemovalEffectQuarantineResidualKeepsCleanup pins the
// §9.4 residual contract: when the .git-less quarantine still holds an
// undeletable file, replay never fails with a missing-marker error, never
// changes ownership or permissions, reports the named residual path with its
// permission error, and keeps the actionable cleanup-required recovery state.
func TestPerformWorktreeRemovalEffectQuarantineResidualKeepsCleanup(t *testing.T) {
	db, _, _, plan := removalEffectFixture(t, "residual", false)
	stage := stageDegradedQuarantine(t, plan, true)
	outcome, err := PerformWorktreeRemovalEffect(plan)
	if outcome != WorktreeRemoveCleanupRequired || err == nil {
		t.Fatalf("effect = %q, %v", outcome, err)
	}
	if !strings.Contains(err.Error(), stage) {
		t.Fatalf("effect error does not name the residual path %s: %v", stage, err)
	}
	if !permissionClass(err) {
		t.Fatalf("effect error is not a permission error: %v", err)
	}
	if info, lerr := os.Lstat(stage); lerr != nil || !info.IsDir() {
		t.Fatalf("residual quarantine was modified or removed: %v", lerr)
	}
	// The ownership and permissions of the residual are untouched.
	if mode := fileInfoMode(t, stage); mode != 0o555 {
		t.Fatalf("quarantine mode changed to %o", mode)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, outcome)
	if err != nil || result.Status != "cleanup-required" || result.RecoverRequestID == nil {
		t.Fatalf("finalize = %+v, %v", result, err)
	}
	// Once the operator clears the permission barrier, the identical
	// recovery replay completes the removal and terminalizes the saga.
	if err := os.Chmod(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	recon, rerr := recoveryReconcile(t, db, plan.OwnerBoundary, plan.SagaID, 1)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if recon.Result == nil || recon.Result.Status != "removed" {
		t.Fatalf("recovery result = %+v, %v", recon.Result, rerr)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate state = %v", err)
	}
}

func fileInfoMode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}
