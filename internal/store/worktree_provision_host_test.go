package store

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// provisionHostFixture builds a real Git repository with an ignored
// .worktrees root, its family store, one claimed task with a running run, and
// the preflight-derived §9.4 options for requestID.
func provisionHostFixture(t *testing.T, requestID string) (*DB, string, WorktreeProvisionOptions) {
	return provisionHostFixtureInit(t, requestID, nil)
}

// provisionHostFixtureInit is provisionHostFixture with extra `git init`
// arguments, for alternative object formats.
func provisionHostFixtureInit(t *testing.T, requestID string, initArgs []string) (*DB, string, WorktreeProvisionOptions) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, append([]string{"init", "-b", "main"}, initArgs...)...)
	return provisionHostSeed(t, requestID, repo)
}

// provisionHostSeed seeds an existing Git repository on branch main with an
// ignored .worktrees root: its family store, one claimed task with a running
// run, and the preflight-derived §9.4 options for requestID.
func provisionHostSeed(t *testing.T, requestID, repo string) (*DB, string, WorktreeProvisionOptions) {
	t.Helper()
	git(t, repo, "config", "user.email", "test@example.invalid")
	git(t, repo, "config", "user.name", "test")
	git(t, repo, "commit", "--allow-empty", "-m", "init")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitignore")
	git(t, repo, "commit", "-m", "ignore")
	dbPath, _ := Path(repo)
	db, err := CreateWithOwner(dbPath, repo)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	id := addTask(t, db, "main", "provision host")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim."+requestID)
	if err != nil {
		t.Fatal(err)
	}
	run, replayed, err := db.StartRun("main", id, "agent-a", claim.Token, "run."+requestID, 3600)
	if err != nil || replayed {
		t.Fatalf("start run = %+v replayed=%v err=%v", run, replayed, err)
	}
	identity, err := PreflightWorktreeProvision(repo, repo, "main", id, false, "")
	if err != nil {
		t.Fatal(err)
	}
	return db, repo, WorktreeProvisionOptions{
		RunID: run.RunID, Owner: "agent-a", ClaimToken: claim.Token,
		Attempt: run.Attempt, AttemptToken: run.AttemptToken, RequestID: requestID,
		OwnerBoundary: identity.OwnerBoundary, RegistrationRoot: identity.RegistrationRoot,
		TargetPath: identity.TargetPath, Branch: identity.Branch, BaseCommit: identity.BaseCommit,
	}
}

func mainOID(t *testing.T, repo string) string {
	t.Helper()
	out, err := gitRegistrationRun(repo, "rev-parse", "main")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestPreflightWorktreeProvisionDerivesIdentity(t *testing.T) {
	_, repo, opts := provisionHostFixture(t, "wt.host.derive")
	wantBase := mainOID(t, repo)
	if opts.OwnerBoundary != repo || opts.RegistrationRoot != filepath.Join(repo, ".git", "worktrees") ||
		opts.TargetPath != filepath.Join(repo, ".worktrees", "main-1") ||
		opts.Branch != "tasks/main/1" || opts.BaseCommit != wantBase {
		t.Fatalf("derived identity = %+v; want base %s", opts, wantBase)
	}
	if len(opts.BaseCommit) != 40 {
		t.Fatalf("base oid width = %d", len(opts.BaseCommit))
	}
}

func TestPreflightWorktreeProvisionRefusals(t *testing.T) {
	_, repo, opts := provisionHostFixture(t, "wt.host.refuse")

	if _, err := PreflightWorktreeProvision(repo, filepath.Join(repo, "gone"), "main", "1", false, ""); err == nil ||
		err.Error() != "worktree owner boundary is unavailable: "+filepath.Join(repo, "gone") {
		t.Fatalf("unavailable boundary = %v", err)
	}
	if _, err := PreflightWorktreeProvision(repo, repo, "a/b", "1", false, ""); err == nil ||
		err.Error() != "tag a/b cannot name a worktree" {
		t.Fatalf("slash tag = %v", err)
	}
	if _, err := PreflightWorktreeProvision(repo, repo, "main", "1.2", false, ""); err == nil ||
		!strings.Contains(err.Error(), "cannot name a worktree") {
		t.Fatalf("dotted id = %v", err)
	}

	// An existing target directory is the typed path-exists conflict.
	if err := os.MkdirAll(opts.TargetPath, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := PreflightWorktreeProvision(repo, repo, "main", "1", false, "")
	var conflict *WorktreeConflict
	if err == nil || !errors.As(err, &conflict) || conflict.Kind != "path-exists" ||
		err.Error() != "worktree path exists: "+opts.TargetPath {
		t.Fatalf("existing target = %v", err)
	}
	// A reserved request skips the collision refusals: the saga row is the
	// replay authority.
	if _, err := PreflightWorktreeProvision(repo, repo, "main", "1", true, ""); err != nil {
		t.Fatalf("reserved preflight = %v", err)
	}
	if err := os.Remove(opts.TargetPath); err != nil {
		t.Fatal(err)
	}

	// A retained branch is the typed branch-exists conflict.
	git(t, repo, "branch", opts.Branch)
	_, err = PreflightWorktreeProvision(repo, repo, "main", "1", false, "")
	if err == nil || !errors.As(err, &conflict) || conflict.Kind != "branch-exists" ||
		err.Error() != "branch tasks/main/1 already exists" {
		t.Fatalf("existing branch = %v", err)
	}

	// A not-ignored .worktrees root is the exit-2 usage refusal.
	other := t.TempDir()
	git(t, other, "init", "-b", "main")
	git(t, other, "config", "user.email", "test@example.invalid")
	git(t, other, "config", "user.name", "test")
	git(t, other, "commit", "--allow-empty", "-m", "init")
	_, err = PreflightWorktreeProvision(other, other, "main", "1", false, "")
	var usage *WorktreeUsageError
	if err == nil || !errors.As(err, &usage) ||
		err.Error() != ".worktrees is not git-ignored; add it to .gitignore or .git/info/exclude" {
		t.Fatalf("not ignored = %v", err)
	}

	// No origin/HEAD and no local main: the pinned base refusal.
	baseless := t.TempDir()
	git(t, baseless, "init", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(baseless, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PreflightWorktreeProvision(baseless, baseless, "main", "1", false, ""); err == nil ||
		err.Error() != "cannot resolve worktree base: origin/HEAD and main do not resolve to commits" {
		t.Fatalf("baseless = %v", err)
	}
}

func TestEffectWorktreeProvisionBindsAndReplaysIdempotently(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.effect")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := EffectWorktreeProvision(repo, repo, plan)
	if err != nil || outcome != WorktreeProvisionBound {
		t.Fatalf("effect = %v, %v", outcome, err)
	}
	if info, err := os.Stat(plan.TargetPath); err != nil || !info.IsDir() {
		t.Fatalf("target after effect = %v, %v", info, err)
	}
	if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || !registered {
		t.Fatalf("registration = %v, %v", registered, err)
	}
	if !worktreeBranchAtBase(repo, plan) {
		t.Fatal("branch not at the immutable base")
	}

	// The crash-before-finalization repeat is read-only: the already-present
	// exact effect is observed, not re-created.
	if outcome, err := EffectWorktreeProvision(repo, repo, plan); err != nil || outcome != WorktreeProvisionBound {
		t.Fatalf("idempotent repeat = %v, %v", outcome, err)
	}
	result, err := db.FinalizeWorktreeProvision(plan, outcome)
	if err != nil || result.Status != "bound" {
		t.Fatalf("finalize = %+v, %v", result, err)
	}
}

func TestEffectWorktreeProvisionSafetyIdentityFailsClosed(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.safety")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	tampered := *plan
	tampered.RegistrationRoot = filepath.Join(repo, "elsewhere", "worktrees")
	if _, err := EffectWorktreeProvision(repo, repo, &tampered); err == nil ||
		!strings.Contains(err.Error(), "does not match the saga registration root") {
		t.Fatalf("registration mismatch = %v", err)
	}
	if _, err := EffectWorktreeProvision(repo, filepath.Join(repo, "other"), plan); err == nil ||
		!strings.Contains(err.Error(), "does not match the saga owner boundary") {
		t.Fatalf("boundary mismatch = %v", err)
	}
	// Failing closed leaves no effect and no row rewrite.
	if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("target created by refused effect: %v", err)
	}
	if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || exists {
		t.Fatalf("branch created by refused effect: %v", exists)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("row after closed refusal = %q, %v", status, err)
	}
}

func TestEffectWorktreeProvisionCollisionAtEffectTime(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.collide")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(plan.TargetPath, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = EffectWorktreeProvision(repo, repo, plan)
	var conflict *WorktreeConflict
	if err == nil || !errors.As(err, &conflict) || conflict.Kind != "path-exists" {
		t.Fatalf("effect-time target collision = %v", err)
	}
	if err := os.Remove(plan.TargetPath); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "branch", plan.Branch)
	_, err = EffectWorktreeProvision(repo, repo, plan)
	if err == nil || !errors.As(err, &conflict) || conflict.Kind != "branch-exists" {
		t.Fatalf("effect-time branch collision = %v", err)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("collided saga = %q, %v", status, err)
	}
}

func TestEffectWorktreeProvisionConfinementMapsToNeedsBroker(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.confine")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	confineRegistration(t, repo)
	outcome, err := EffectWorktreeProvision(repo, repo, plan)
	if err != nil || outcome != WorktreeProvisionNeedsBroker {
		t.Fatalf("confined effect = %v, %v", outcome, err)
	}
	result, err := db.FinalizeWorktreeProvision(plan, outcome)
	if err != nil || result.Status != "needs-broker" || result.BrokerRequest == nil ||
		result.BrokerRequest.Verb != "create" || result.BrokerRequest.FamilyID == "" ||
		result.RecoverRequestID == nil || !strings.HasPrefix(*result.RecoverRequestID, "recover-") {
		t.Fatalf("needs-broker handoff = %+v, %v", result, err)
	}
}

func TestCompensateWorktreeProvisionProvesAbsence(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.compensate")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EffectWorktreeProvision(repo, repo, plan); err != nil {
		t.Fatal(err)
	}
	if proved, err := CompensateWorktreeProvision(repo, plan); err != nil || !proved {
		t.Fatalf("compensation did not prove absence: %v", err)
	}
	if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("target survived compensation: %v", err)
	}
	if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || registered {
		t.Fatalf("registration survived compensation: %v", registered)
	}
	if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || exists {
		t.Fatalf("branch survived compensation: %v", exists)
	}
}

func TestCompensateWorktreeProvisionUnprovedForMovedBranch(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.moved")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EffectWorktreeProvision(repo, repo, plan); err != nil {
		t.Fatal(err)
	}
	// An operator commit moves the branch off the immutable base: the branch
	// is no longer unambiguously attributable, so compensation never guesses.
	git(t, plan.TargetPath, "config", "user.email", "test@example.invalid")
	git(t, plan.TargetPath, "config", "user.name", "test")
	git(t, plan.TargetPath, "commit", "--allow-empty", "-m", "operator work", "--no-gpg-sign")
	if proved, err := CompensateWorktreeProvision(repo, plan); err != nil || proved {
		t.Fatalf("compensation claimed proof over a moved branch: %v", err)
	}
	if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || !exists {
		t.Fatalf("moved branch must be retained: %v", exists)
	}
	if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("target should still be removed: %v", err)
	}
}

func TestPreflightWorktreeProvisionFromLinkedWorktree(t *testing.T) {
	_, repo, opts := provisionHostFixture(t, "wt.host.linked")
	linked := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", "-b", "linked", linked)
	// §9.4: invocation from a linked worktree still provisions at the
	// family-owned root, so the ignore proof must run there — Git rejects a
	// check-ignore path outside the invoked checkout — and the derived
	// identity is the owner root's, never the linked checkout's.
	id, err := PreflightWorktreeProvision(linked, repo, "main", "1", false, "")
	if err != nil {
		t.Fatalf("linked-worktree preflight = %v", err)
	}
	if id.OwnerBoundary != opts.OwnerBoundary || id.RegistrationRoot != opts.RegistrationRoot ||
		id.TargetPath != opts.TargetPath || id.Branch != opts.Branch || id.BaseCommit != opts.BaseCommit {
		t.Fatalf("linked identity = %+v; want the owner-root identity %+v", id, opts)
	}
	if strings.Contains(id.TargetPath, linked) {
		t.Fatalf("linked invocation derived a target inside the linked checkout: %s", id.TargetPath)
	}
}

func TestPreflightWorktreeProvisionSHA256ObjectID(t *testing.T) {
	db, repo, opts := provisionHostFixtureInit(t, "wt.host.sha256", []string{"--object-format=sha256"})
	// The base is the repository's exact object-id width: 64 lowercase hex
	// digits in a SHA-256 repository, accepted by planning and the effect.
	if len(opts.BaseCommit) != 64 || !validWorktreeBaseOID(opts.BaseCommit) {
		t.Fatalf("sha256 base = %q", opts.BaseCommit)
	}
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatalf("plan with a 64-hex base = %v", err)
	}
	outcome, err := EffectWorktreeProvision(repo, repo, plan)
	if err != nil || outcome != WorktreeProvisionBound {
		t.Fatalf("sha256 effect = %v, %v", outcome, err)
	}
	var stored string
	if err := db.db.QueryRow(`SELECT base_commit FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&stored); err != nil || stored != opts.BaseCommit {
		t.Fatalf("stored 64-hex base = %q, %v", stored, err)
	}
	if head, err := gitRegistrationRun(plan.TargetPath, "rev-parse", "HEAD"); err != nil || strings.TrimSpace(string(head)) != opts.BaseCommit {
		t.Fatalf("checked-out sha256 base = %q, %v", head, err)
	}
}

func TestCompensateWorktreeProvisionIdentityFailsClosed(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.compidentity")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := EffectWorktreeProvision(repo, repo, plan); err != nil {
		t.Fatal(err)
	}
	// The mandatory pre-compensation identity proof: the registration root
	// and owner boundary are rederived from current repository state and
	// must equal the immutable row. Invoking from another real family
	// rederives that family's own root and refuses.
	other := t.TempDir()
	git(t, other, "init", "-b", "main")
	if _, err := CompensateWorktreeProvision(other, plan); err == nil ||
		!strings.Contains(err.Error(), "does not match the saga registration root") {
		t.Fatalf("registration-root mismatch = %v", err)
	}
	if _, err := CompensateWorktreeProvision(t.TempDir(), plan); err == nil ||
		!strings.Contains(err.Error(), "cannot resolve the Git common directory") {
		t.Fatalf("unresolvable registration root = %v", err)
	}
	// Failing closed performed no effect and rewrote no row.
	if _, err := os.Lstat(plan.TargetPath); err != nil {
		t.Fatalf("refused compensation still removed the target: %v", err)
	}
	if registered, err := worktreeRegistered(repo, plan.TargetPath); err != nil || !registered {
		t.Fatalf("registration after refused compensation = %v, %v", registered, err)
	}
	if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || !exists {
		t.Fatalf("branch after refused compensation = %v, %v", exists, err)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("row after refused compensation = %q, %v", status, err)
	}
	// The exact identity still compensates cleanly afterwards.
	if proved, err := CompensateWorktreeProvision(repo, plan); err != nil || !proved {
		t.Fatalf("exact-identity compensation = %v, %v", proved, err)
	}
}

func TestCompensateWorktreeProvisionOwnerBoundaryIsRederived(t *testing.T) {
	// A family whose common directory is not a plain .git directory: the
	// owner boundary is the primary working tree Git registered there
	// (core.worktree), so relocating that registration changes the real
	// boundary. The trusted host must rederive it from repository state and
	// fail closed when it no longer equals the immutable row — the caller's
	// arguments cannot vouch for the stored owner.
	root := t.TempDir()
	work := filepath.Join(root, "work")
	gitdir := filepath.Join(root, "gitdir")
	git(t, root, "init", "-b", "main", "--separate-git-dir", gitdir, work)
	db, _, opts := provisionHostSeed(t, "wt.host.owner", work)
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := EffectWorktreeProvision(work, work, plan); err != nil || outcome != WorktreeProvisionBound {
		t.Fatalf("effect = %v, %v", outcome, err)
	}
	// The primary working tree relocates: a copy of the working tree is
	// registered against the same common directory and Git's registration
	// is repointed at it. The rederived registration root is unchanged, so
	// only the rederived owner boundary can refuse.
	moved := filepath.Join(root, "moved")
	copyTree(t, work, moved)
	git(t, root, "config", "--file", filepath.Join(gitdir, "config"), "core.worktree", moved)
	if _, err := CompensateWorktreeProvision(work, plan); err == nil ||
		!strings.Contains(err.Error(), "does not match the saga owner boundary") {
		t.Fatalf("relocated owner boundary = %v", err)
	}
	// The refusal performed no effect: everything the saga created is
	// exactly as the effect left it, and the row was rewritten never.
	if _, err := os.Lstat(plan.TargetPath); err != nil {
		t.Fatalf("refused compensation still removed the target: %v", err)
	}
	if registered, err := worktreeRegistered(work, plan.TargetPath); err != nil || !registered {
		t.Fatalf("registration after refused compensation = %v, %v", registered, err)
	}
	if exists, err := worktreeBranchExists(work, plan.Branch); err != nil || !exists {
		t.Fatalf("branch after refused compensation = %v, %v", exists, err)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("row after refused compensation = %q, %v", status, err)
	}
	// Restoring the registration lets the exact identity compensate cleanly.
	git(t, root, "config", "--file", filepath.Join(gitdir, "config"), "core.worktree", work)
	if proved, err := CompensateWorktreeProvision(work, plan); err != nil || !proved {
		t.Fatalf("restored-owner compensation = %v, %v", proved, err)
	}
}

func TestEffectWorktreeProvisionReplayReprovesIgnore(t *testing.T) {
	db, repo, opts := provisionHostFixture(t, "wt.host.reignore")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	// Crash between planning and the effect; the ignore rule then ceases to
	// cover .worktrees. The reserved preflight still passes — the saga row
	// is the replay authority and preflight skips the refusals — which is
	// exactly why the phase-2 repeat must own the ignore proof.
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("# nothing ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PreflightWorktreeProvision(repo, repo, plan.Tag, plan.Task, true, opts.BaseCommit); err != nil {
		t.Fatalf("reserved preflight while unignored = %v", err)
	}
	_, err = EffectWorktreeProvision(repo, repo, plan)
	var usage *WorktreeUsageError
	if err == nil || !errors.As(err, &usage) ||
		err.Error() != ".worktrees is not git-ignored; add it to .gitignore or .git/info/exclude" {
		t.Fatalf("unignored phase-2 repeat = %v", err)
	}
	if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("unignored repeat created the target: %v", err)
	}
	if exists, err := worktreeBranchExists(repo, plan.Branch); err != nil || exists {
		t.Fatalf("unignored repeat created the branch: %v, %v", exists, err)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("row after refused repeat = %q, %v", status, err)
	}
	// Restoring the rule lets the identical replay proceed: the refusal
	// failed closed without consuming anything.
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if outcome, err := EffectWorktreeProvision(repo, repo, plan); err != nil || outcome != WorktreeProvisionBound {
		t.Fatalf("restored replay = %v, %v", outcome, err)
	}
}

// copyTree copies a directory tree verbatim (structure and regular files,
// mode bits preserved) for relocation fixtures.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cannot copy irregular file %s", p)
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
}
