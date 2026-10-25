package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// §9.4 trusted-host effect orchestration for worktree provision: the
// read-only preflight derivation, the idempotent Git/filesystem effect that
// runs with no SQLite transaction open, and the exact compensation that
// proves absence. Planning and finalization live in worktree_provision.go;
// this file never opens a transaction and never mutates the store.

// errWorktreeBaseUnresolvable is the pinned §9.4 base refusal (exit 1).
var errWorktreeBaseUnresolvable = errors.New("cannot resolve worktree base: origin/HEAD and main do not resolve to commits")

// WorktreeProvisionIdentity is the immutable effect identity derived by the
// §9.4 trusted preflight before the planning transaction runs.
type WorktreeProvisionIdentity struct {
	OwnerBoundary    string
	RegistrationRoot string
	TargetPath       string
	Branch           string
	BaseCommit       string
}

// PreflightWorktreeProvision derives the §9.4 immutable effect identity with
// read-only Git/filesystem access. reserved reports that requestID already
// names a saga row (the original request reservation): the row is then the
// replay authority, so the ignore proof and filesystem collision refusals
// are skipped and left to the phase-2 repeat, which reruns them, and
// reservedBase — the row's stored base — replaces base resolution, because
// the family's default branch may have advanced since planning while the
// saga identity is immutable. Collision checks refuse with the exact §9.4
// diagnostics — a not-ignored target is a
// usage error (exit 2), an existing target or branch a typed conflict
// (exit 3) — and an unresolvable base or unavailable owner boundary is the
// pinned exit-1 refusal. The ignore proof runs at the owner boundary: the
// target lives beneath that family-owned root, so an invocation from a
// linked worktree still evaluates the ignore rules of the root that
// contains the target, never of the linked checkout.
func PreflightWorktreeProvision(boundary, ownerBoundary, tag, taskID string, reserved bool, reservedBase string) (*WorktreeProvisionIdentity, error) {
	if info, err := os.Stat(ownerBoundary); err != nil || !info.IsDir() {
		return nil, &WorktreeOwnerBoundaryUnavailable{Path: ownerBoundary}
	}
	if reserved {
		if err := checkHistoricalWorktreeTag(tag); err != nil {
			return nil, err
		}
	} else if err := checkWorktreeTag(tag); err != nil {
		return nil, err
	}
	if err := checkWorktreeID(tag, taskID); err != nil {
		return nil, err
	}
	id := &WorktreeProvisionIdentity{
		OwnerBoundary: ownerBoundary,
		TargetPath:    filepath.Join(WorktreeRoot(ownerBoundary), WorktreeName(tag, taskID)),
		Branch:        WorktreeBranch(tag, taskID),
	}
	if err := checkWorktreeBranchFormat(boundary, tag, id.Branch); err != nil {
		return nil, err
	}
	roots := registrationRoots(boundary)
	if len(roots) == 0 {
		return nil, fmt.Errorf("cannot resolve the Git common directory for %s", boundary)
	}
	id.RegistrationRoot = roots[0]
	if !reserved {
		// Fresh requests prove the ignore precondition — at the owner
		// boundary, the working tree that contains the target, since Git
		// rejects a check-ignore path outside the invoked checkout — and
		// collide with nothing; a reserved request leaves those to the
		// phase-2 repeat, where the saga row is the authority.
		if err := checkWorktreeIgnored(ownerBoundary, id.TargetPath); err != nil {
			return nil, err
		}
		if err := checkProvisionCollision(boundary, id, tag, taskID); err != nil {
			return nil, err
		}
	}
	if reserved && validWorktreeBaseOID(reservedBase) {
		// §9.4 replay submits the saga's immutable stored base: a default
		// branch that advanced since planning must not turn the replay into
		// a different identity. A row without a valid stored base falls
		// through to fresh resolution; planning's replay validation is the
		// authority and refuses such a row as a typed conflict.
		id.BaseCommit = reservedBase
		return id, nil
	}
	ref, err := resolveDefaultRef(boundary)
	if err != nil {
		return nil, errWorktreeBaseUnresolvable
	}
	base, err := worktreeBaseOID(boundary, ref)
	if err != nil || len(base) != 40 && len(base) != 64 {
		return nil, errWorktreeBaseUnresolvable
	}
	id.BaseCommit = base
	return id, nil
}

// checkProvisionCollision refuses an existing target directory or retained
// branch with the §9.4 typed exit-3 conflicts.
func checkProvisionCollision(boundary string, id *WorktreeProvisionIdentity, tag, taskID string) error {
	if _, err := os.Lstat(id.TargetPath); err == nil {
		return &WorktreeConflict{Kind: "path-exists", Tag: tag, ID: taskID, Path: id.TargetPath}
	} else if !os.IsNotExist(err) {
		return err
	}
	exists, err := worktreeBranchExists(boundary, id.Branch)
	if err != nil {
		return err
	}
	if exists {
		return &WorktreeConflict{Kind: "branch-exists", Tag: tag, ID: taskID, Branch: id.Branch}
	}
	return nil
}

// EffectWorktreeProvision performs the §9.4 phase-2 idempotent
// Git/filesystem effect for a committed plan. It runs with no SQLite
// transaction open, immediately rederives the exact common-dir registration
// root and requires the owner boundary and registration root to equal the
// immutable row (a mismatch fails closed with no effect and no row rewrite),
// then repeats the safety and collision checks exactly as preflight — the
// ignore proof at the owner boundary first, then the target and branch
// refusals — because a reserved replay skips them in preflight and the
// ignore state may have flipped since planning. It never delegates to the
// broker: exact permission-class confinement of the registration area is
// reported as needs-broker for later §9.5 recovery. A failed or
// unverifiable effect is compensated exactly; the returned outcome records
// whether absence was proved (failed) or not (cleanup-required).
func EffectWorktreeProvision(boundary, ownerBoundary string, plan *WorktreeProvisionPlan) (WorktreeProvisionOutcome, error) {
	if plan == nil || plan.SagaID <= 0 || plan.SagaToken <= 0 {
		return "", fmt.Errorf("worktree provision plan is invalid")
	}
	roots := registrationRoots(boundary)
	if len(roots) == 0 {
		return "", fmt.Errorf("cannot resolve the Git common directory for %s", boundary)
	}
	if roots[0] != plan.RegistrationRoot {
		return "", fmt.Errorf("worktree registration root %s does not match the saga registration root %s", roots[0], plan.RegistrationRoot)
	}
	if ownerBoundary != plan.OwnerBoundary {
		return "", fmt.Errorf("worktree owner boundary %s does not match the saga owner boundary %s", ownerBoundary, plan.OwnerBoundary)
	}
	// §9.4 phase 2 "repeats collision and safety checks": the ignore proof
	// runs at the owner boundary — the working tree that contains the
	// target, exactly as in preflight — before any Git work, so a replay
	// whose ignore state flipped between planning and now fails closed with
	// the pinned usage refusal, no effect, and no row rewrite.
	if err := checkWorktreeIgnored(ownerBoundary, plan.TargetPath); err != nil {
		return "", err
	}
	_, statErr := os.Lstat(plan.TargetPath)
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	branchExists, err := worktreeBranchExists(boundary, plan.Branch)
	if err != nil {
		return "", err
	}
	if statErr == nil {
		// The exact effect is already present only when the target is a
		// registered worktree whose branch still points at the immutable
		// base commit: that is the idempotent crash-before-finalization
		// state. Anything else is an unattributable collision.
		if !branchExists || !provisionEffectIntact(boundary, plan) {
			return "", &WorktreeConflict{Kind: "path-exists", Tag: plan.Tag, ID: plan.Task, Path: plan.TargetPath}
		}
		return WorktreeProvisionBound, nil
	}
	if branchExists {
		return "", &WorktreeConflict{Kind: "branch-exists", Tag: plan.Tag, ID: plan.Task, Branch: plan.Branch}
	}
	if _, err := gitRegistrationRun(boundary, "worktree", "add", "-b", plan.Branch, "--", plan.TargetPath, plan.BaseCommit); err != nil {
		if registrationPermissionFailure(boundary, err) {
			return WorktreeProvisionNeedsBroker, nil
		}
		return effectOutcomeAfterCompensation(boundary, plan)
	}
	if err := verifyCreatedWorktree(boundary, plan.TargetPath); err != nil {
		return effectOutcomeAfterCompensation(boundary, plan)
	}
	return WorktreeProvisionBound, nil
}

// provisionEffectIntact proves a present target is exactly this saga's
// effect: a registered worktree whose branch resolves to the immutable base.
func provisionEffectIntact(boundary string, plan *WorktreeProvisionPlan) bool {
	if err := verifyCreatedWorktree(boundary, plan.TargetPath); err != nil {
		return false
	}
	return worktreeBranchAtBase(boundary, plan)
}

// worktreeBranchAtBase reports whether the saga branch still points at the
// immutable base commit.
func worktreeBranchAtBase(boundary string, plan *WorktreeProvisionPlan) bool {
	out, err := gitRegistrationRun(boundary, "rev-parse", "--verify", "refs/heads/"+plan.Branch)
	return err == nil && strings.TrimSpace(string(out)) == plan.BaseCommit
}

// effectOutcomeAfterCompensation compensates the exact failed effect attempt
// and maps the proof to the §9.4 outcome: failed when absence is proved,
// cleanup-required otherwise. A compensation-identity mismatch fails closed
// as an operational error with no effect and no row rewrite.
func effectOutcomeAfterCompensation(boundary string, plan *WorktreeProvisionPlan) (WorktreeProvisionOutcome, error) {
	proved, err := CompensateWorktreeProvision(boundary, plan)
	if err != nil {
		return "", err
	}
	if proved {
		return WorktreeProvisionFailed, nil
	}
	return WorktreeProvisionCleanupRequired, nil
}

// CompensateWorktreeProvision reverses the exact immutable effect recorded by
// plan and reports whether absence of everything uniquely attributable to it
// is proved. The compensation is itself an effect, so the mandatory §9.4
// fail-closed identity check runs immediately before it: the exact Git
// common-dir registration root is rederived for the stored family, the
// current owner boundary is rederived from repository state — never accepted
// from an argument, so a stale caller value cannot vouch for the stored
// owner — and both must equal the immutable row. A mismatch, or an
// unresolvable root or owner, is returned as an error with no effect
// performed and no row rewrite. The compensation never guesses: an
// unattributable branch (moved off the base), an unsafe registration
// binding, or a confined registration root is left in place and reported
// unproved, and no broker is ever invoked — a confined registration stays
// for §9.5 recovery.
func CompensateWorktreeProvision(boundary string, plan *WorktreeProvisionPlan) (bool, error) {
	proved, _, err := compensateWorktreeProvision(boundary, plan)
	return proved, err
}

// compensateWorktreeProvision performs the exact compensation of the
// immutable effect recorded by plan and additionally reports whether the
// compensating registration write itself produced the §9.5 permission-class
// confinement signal (SPEC §9.4 confined compensation): the staged rename of
// the exact registration is that write. It runs the mandatory §9.4
// fail-closed identity check immediately before the effect: the exact Git
// common-dir registration root is rederived for the stored family, the
// current owner boundary is rederived from repository state — never accepted
// from an argument, so a stale caller value cannot vouch for the stored
// owner — and both must equal the immutable row. A mismatch, or an
// unresolvable root or owner, is returned as an error with no effect
// performed and no row rewrite. The compensation never guesses: an
// unattributable branch (moved off the base), an unsafe registration
// binding, or a confined registration root is left in place and reported
// unproved, and no broker is ever invoked here — a confined registration
// stays for §9.5 recovery, whose cleanup-required rule may delegate exactly
// that registration step.
func compensateWorktreeProvision(boundary string, plan *WorktreeProvisionPlan) (proved, confined bool, err error) {
	if plan == nil {
		return false, false, fmt.Errorf("worktree provision plan is invalid")
	}
	roots := registrationRoots(boundary)
	if len(roots) == 0 {
		return false, false, fmt.Errorf("cannot resolve the Git common directory for %s", boundary)
	}
	if roots[0] != plan.RegistrationRoot {
		return false, false, fmt.Errorf("worktree registration root %s does not match the saga registration root %s", roots[0], plan.RegistrationRoot)
	}
	ownerNow, oerr := worktreeOwnerBoundary(boundary)
	if oerr != nil {
		return false, false, fmt.Errorf("%v: %w", &WorktreeOwnerBoundaryUnavailable{Path: plan.OwnerBoundary}, oerr)
	}
	if ownerNow != plan.OwnerBoundary {
		return false, false, fmt.Errorf("worktree owner boundary %s does not match the saga owner boundary %s", ownerNow, plan.OwnerBoundary)
	}
	if registered, rerr := worktreeRegistered(boundary, plan.TargetPath); rerr != nil {
		return false, false, nil
	} else if registered {
		admin, aerr := worktreeAdminDir(boundary, plan.TargetPath)
		if _, terr := os.Lstat(plan.TargetPath); errors.Is(aerr, os.ErrNotExist) && os.IsNotExist(terr) {
			admin, aerr = findWorktreeAdmin(plan.RegistrationRoot, plan.TargetPath, plan.SagaID)
			if aerr == nil && admin == "" {
				aerr = fmt.Errorf("worktree registration is unavailable for %s", plan.TargetPath)
			}
		}
		if aerr != nil {
			return false, false, nil
		}
		// Direct same-root staging rename only; that rename is the
		// compensating registration write, and a permission-class failure of
		// it is the §9.5 confinement signal (SPEC §9.5).
		staged := filepath.Join(filepath.Dir(admin), ".noise-remove-"+filepath.Base(admin))
		if _, serr := os.Lstat(staged); serr == nil {
			return false, false, nil
		} else if !os.IsNotExist(serr) {
			return false, false, nil
		}
		if rerr := os.Rename(admin, staged); rerr != nil {
			if permissionClass(rerr) {
				confined = true
			}
			return false, confined, nil
		}
		if rerr := os.RemoveAll(staged); rerr != nil {
			return false, false, nil
		}
	}
	if _, terr := os.Lstat(plan.TargetPath); terr == nil {
		if rerr := os.RemoveAll(plan.TargetPath); rerr != nil {
			return false, false, nil
		}
	} else if !os.IsNotExist(terr) {
		return false, false, nil
	}
	if exists, berr := worktreeBranchExists(boundary, plan.Branch); berr != nil {
		return false, false, nil
	} else if exists {
		// The branch is uniquely attributable only while it still points at
		// the saga's immutable base commit.
		if !worktreeBranchAtBase(boundary, plan) {
			return false, false, nil
		}
		if _, gerr := gitRegistrationRun(boundary, "update-ref", "-d", "refs/heads/"+plan.Branch, plan.BaseCommit); gerr != nil {
			return false, false, nil
		}
	}
	if _, terr := os.Lstat(plan.TargetPath); !os.IsNotExist(terr) {
		return false, false, nil
	}
	if registered, rerr := worktreeRegistered(boundary, plan.TargetPath); rerr != nil || registered {
		return false, false, nil
	}
	exists, berr := worktreeBranchExists(boundary, plan.Branch)
	return berr == nil && !exists, false, nil
}

// worktreeOwnerBoundary derives the current §8.1 owner boundary for the
// repository family containing boundary — the same derivation of record the
// command surface mints the family identity with: the <primary>/.git parent
// for an ordinary repository, and the Git-registered primary working tree
// for a common directory that is not a plain .git directory (a submodule's
// gitdir). The trusted host rederives it from repository state before any
// compensation effect and never accepts it from an argument; nothing is
// guessed from path spelling.
func worktreeOwnerBoundary(boundary string) (string, error) {
	common, ok := backlog.GitCommonDir(canonicalPath(boundary))
	if !ok {
		return "", fmt.Errorf("cannot resolve the Git common directory for %s", boundary)
	}
	common = canonicalPath(common)
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), nil
	}
	return registeredPrimaryWorktree(common)
}

// registeredPrimaryWorktree resolves the primary working tree Git registered
// for a common directory that is not a plain .git directory: the
// core.worktree registration, read only from that common directory's own
// config with ambient Git overrides stripped, then validated to resolve back
// to the same common directory. An absent or inconsistent registration
// refuses rather than guessing.
func registeredPrimaryWorktree(commonDir string) (string, error) {
	refuse := func(reason string) (string, error) {
		return "", fmt.Errorf("cannot determine the primary working tree for Git common directory %s (%s)", commonDir, reason)
	}
	cmd := exec.Command("git", "config", "--file="+filepath.Join(commonDir, "config"), "--get", "core.worktree")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return refuse("no working tree registered")
		}
		return refuse(fmt.Sprintf("git config core.worktree failed: %v", err))
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return refuse("no working tree registered")
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(commonDir, root)
	}
	root = canonicalPath(root)
	resolved, ok := backlog.GitCommonDir(root)
	if !ok {
		return refuse(fmt.Sprintf("%s has no resolvable .git marker", root))
	}
	if canonical := canonicalPath(resolved); canonical != commonDir {
		return refuse(fmt.Sprintf("%s is registered against common directory %s", root, canonical))
	}
	return root, nil
}

// WorktreeRequestReserved reports whether requestID already names a worktree
// saga row — the §9.4 original request reservation — and, for a reserved
// row, its stored immutable base commit (empty when the row records none).
// Replay must reuse the stored base rather than re-derive the family's
// current default branch, which may have advanced since planning. The
// trusted host routes a reserved request straight to planning, whose replay
// validation is the authority for identity and eligibility.
func (d *DB) WorktreeRequestReserved(requestID string) (bool, string, error) {
	if !ValidLocalKey(requestID) {
		return false, "", fmt.Errorf("worktree request id is invalid")
	}
	var stored sql.NullString
	var reserved bool
	err := d.deferred(func(t *tx) error {
		err := t.QueryRow(`SELECT base_commit FROM worktree_sagas WHERE request_id=?`, requestID).Scan(&stored)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		reserved = true
		return nil
	})
	if err != nil {
		return false, "", err
	}
	return reserved, stored.String, nil
}
