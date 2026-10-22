package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// WorktreeOwnerBoundaryUnavailable is the pinned §9.4 refusal when the
// immutable family root disappeared or no longer resolves to itself.
type WorktreeOwnerBoundaryUnavailable struct{ Path string }

func (e *WorktreeOwnerBoundaryUnavailable) Error() string {
	return fmt.Sprintf("worktree owner boundary is unavailable: %s", e.Path)
}

// WorktreeEffectIdentityMismatch reports a trusted-host rederivation that no
// longer equals the immutable saga identity. The effect must not run and the
// pending row must not be rewritten.
type WorktreeEffectIdentityMismatch struct{ SagaID int64 }

func (e *WorktreeEffectIdentityMismatch) Error() string {
	if e.SagaID > 0 {
		return fmt.Sprintf("worktree saga %d effect identity no longer matches the repository family", e.SagaID)
	}
	return "worktree effect identity no longer matches the repository family"
}

// ErrWorktreeRegistrationConfined is the public, pinned first-attempt
// confinement diagnostic. Direct saga effects never invoke the broker.
var ErrWorktreeRegistrationConfined = errWorktreeConfined

// worktreeRemoveEffectFaultHook pins filesystem crash boundaries for focused
// tests. Production keeps the no-op; an injected failure deliberately leaves
// the exact saga-named staging paths for idempotent replay.
var worktreeRemoveEffectFaultHook = func(string) error { return nil }

type removalEffectState struct {
	targetExists bool
	targetStage  string
	admin        string
	adminStage   string
}

func validateRemovalEffectIdentity(plan *WorktreeRemovePlan) error {
	if plan == nil || plan.Tag == "" || plan.Task == "" ||
		plan.OwnerBoundary == "" || plan.RegistrationRoot == "" || plan.TargetPath == "" ||
		plan.Branch == "" {
		return fmt.Errorf("worktree remove effect identity is invalid")
	}
	for _, path := range []string{plan.OwnerBoundary, plan.RegistrationRoot, plan.TargetPath} {
		if !canonicalAbsolutePath(path) {
			return fmt.Errorf("worktree remove effect identity is invalid")
		}
	}
	if plan.TargetPath != filepath.Join(plan.OwnerBoundary, ".worktrees", WorktreeName(plan.Tag, plan.Task)) ||
		plan.Branch != WorktreeBranch(plan.Tag, plan.Task) {
		return fmt.Errorf("worktree remove effect identity is invalid")
	}
	// A linked removal carries its exact source edge, registration root, and
	// immutable base, with or without a live run for §9.4's runless automatic
	// cleanup rows. A runless sourceless legacy row records no run, source,
	// or base; its registration root is derived at effect time. IDs are
	// nonnegative, and zero is admitted only for those two shapes: a source
	// with RunID zero is the runless automatic row, and both IDs zero the
	// legacy row (SPEC §9.4).
	if plan.RunID < 0 || plan.SourceSagaID < 0 {
		return fmt.Errorf("worktree remove effect identity is invalid")
	}
	if plan.SourceSagaID > 0 {
		if plan.BaseCommit == "" {
			return fmt.Errorf("worktree remove effect identity is invalid")
		}
	} else if plan.RunID != 0 || plan.BaseCommit != "" {
		return fmt.Errorf("worktree remove effect identity is invalid")
	}
	return nil
}

// rederiveRemovalFamily proves the exact immutable owner and registration
// roots from filesystem-anchored Git metadata. It intentionally does not use
// caller Git environment overrides.
func rederiveRemovalFamily(plan *WorktreeRemovePlan) error {
	if err := validateRemovalEffectIdentity(plan); err != nil {
		return err
	}
	info, err := os.Lstat(plan.OwnerBoundary)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || canonicalPath(plan.OwnerBoundary) != plan.OwnerBoundary {
		return &WorktreeOwnerBoundaryUnavailable{Path: plan.OwnerBoundary}
	}
	common, ok := backlog.GitCommonDir(plan.OwnerBoundary)
	if !ok {
		return &WorktreeOwnerBoundaryUnavailable{Path: plan.OwnerBoundary}
	}
	common = canonicalPath(common)
	if canonicalPath(filepath.Join(common, "worktrees")) != plan.RegistrationRoot {
		return &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
	}
	return nil
}

func removalTargetStage(plan *WorktreeRemovePlan) string {
	if plan.SagaToken <= 0 {
		return ""
	}
	return filepath.Join(filepath.Dir(plan.TargetPath), ".noise-remove-"+
		strconv.FormatInt(plan.SagaToken, 10)+"-"+filepath.Base(plan.TargetPath))
}

func removalAdminStage(plan *WorktreeRemovePlan, admin string) string {
	return filepath.Join(plan.RegistrationRoot, ".noise-remove-"+
		strconv.FormatInt(plan.SagaToken, 10)+"-"+filepath.Base(admin))
}

func lstatDirectory(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("unsafe worktree path: %s", path)
	}
	return true, nil
}

func readRemovalAdmin(plan *WorktreeRemovePlan, marker string) (string, error) {
	admin, err := parseRemovalAdminMarker(plan, marker)
	if err != nil {
		return "", err
	}
	if err := validateRemovalAdmin(plan, admin); err != nil {
		return "", err
	}
	return admin, nil
}

func parseRemovalAdminMarker(plan *WorktreeRemovePlan, marker string) (string, error) {
	data, err := os.ReadFile(marker)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir: "
	if !strings.HasPrefix(line, prefix) || strings.Contains(line[len(prefix):], "\n") {
		return "", fmt.Errorf("unsafe worktree git marker: %s", marker)
	}
	admin := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(admin) {
		admin = filepath.Join(filepath.Dir(marker), admin)
	}
	admin = canonicalPath(admin)
	if filepath.Dir(admin) != plan.RegistrationRoot {
		return "", &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
	}
	return admin, nil
}

func validateRemovalAdmin(plan *WorktreeRemovePlan, admin string) error {
	exists, err := lstatDirectory(admin)
	if err != nil || !exists {
		if err != nil {
			return err
		}
		return fmt.Errorf("worktree registration is unavailable: %s", admin)
	}
	backref, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil || canonicalPath(strings.TrimSpace(string(backref))) != canonicalPath(filepath.Join(plan.TargetPath, ".git")) {
		return &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
	}
	head, err := os.ReadFile(filepath.Join(admin, "HEAD"))
	if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/"+plan.Branch {
		return &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
	}
	return nil
}

func findRemovalAdmin(plan *WorktreeRemovePlan) (string, error) {
	entries, err := os.ReadDir(plan.RegistrationRoot)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".noise-remove-") {
			continue
		}
		admin := filepath.Join(plan.RegistrationRoot, entry.Name())
		if ok, err := lstatDirectory(admin); err != nil || !ok {
			continue
		}
		backref, err := os.ReadFile(filepath.Join(admin, "gitdir"))
		if err != nil || canonicalPath(strings.TrimSpace(string(backref))) != canonicalPath(filepath.Join(plan.TargetPath, ".git")) {
			continue
		}
		if found != "" {
			return "", &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
		}
		if err := validateRemovalAdmin(plan, admin); err != nil {
			return "", err
		}
		found = admin
	}
	return found, nil
}

func findRemovalAdminStage(plan *WorktreeRemovePlan) (string, error) {
	if plan.SagaToken <= 0 {
		return "", nil
	}
	prefix := ".noise-remove-" + strconv.FormatInt(plan.SagaToken, 10) + "-"
	entries, err := os.ReadDir(plan.RegistrationRoot)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		stage := filepath.Join(plan.RegistrationRoot, entry.Name())
		if ok, err := lstatDirectory(stage); err != nil || !ok {
			continue
		}
		backref, err := os.ReadFile(filepath.Join(stage, "gitdir"))
		if err != nil || canonicalPath(strings.TrimSpace(string(backref))) != canonicalPath(filepath.Join(plan.TargetPath, ".git")) {
			continue
		}
		head, err := os.ReadFile(filepath.Join(stage, "HEAD"))
		if err != nil || strings.TrimSpace(string(head)) != "ref: refs/heads/"+plan.Branch || found != "" {
			return "", &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
		}
		found = stage
	}
	return found, nil
}

func inspectRemovalEffect(plan *WorktreeRemovePlan) (*removalEffectState, error) {
	state := &removalEffectState{}
	var err error
	state.targetExists, err = lstatDirectory(plan.TargetPath)
	if err != nil {
		return nil, err
	}
	stageExists := false
	targetStage := removalTargetStage(plan)
	if targetStage != "" {
		stageExists, err = lstatDirectory(targetStage)
		if err != nil {
			return nil, err
		}
		if stageExists {
			state.targetStage = targetStage
		}
	}
	if state.targetExists && stageExists {
		return nil, fmt.Errorf("worktree removal has both target and saga staging path")
	}
	switch {
	case state.targetExists:
		state.admin, err = readRemovalAdmin(plan, filepath.Join(plan.TargetPath, ".git"))
	case stageExists:
		var expected string
		expected, err = parseRemovalAdminMarker(plan, filepath.Join(state.targetStage, ".git"))
		if err == nil {
			var exists bool
			exists, err = lstatDirectory(expected)
			if err == nil && exists {
				err = validateRemovalAdmin(plan, expected)
				state.admin = expected
			}
		}
	default:
		state.admin, err = findRemovalAdmin(plan)
	}
	if err != nil {
		return nil, err
	}
	state.adminStage, err = findRemovalAdminStage(plan)
	if err != nil {
		return nil, err
	}
	if state.admin != "" && state.adminStage != "" {
		return nil, fmt.Errorf("worktree removal has both registration and saga registration staging path")
	}
	if state.targetExists && state.admin == "" && state.adminStage == "" {
		return nil, &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
	}
	return state, nil
}

// PreflightWorktreeRemovalEffect performs all read-only Git/filesystem safety
// checks before the planning write. A pending replay may already have exact
// saga-named stages; those are accepted for the idempotent effect.
func PreflightWorktreeRemovalEffect(plan *WorktreeRemovePlan) error {
	if err := rederiveRemovalFamily(plan); err != nil {
		return err
	}
	state, err := inspectRemovalEffect(plan)
	if err != nil {
		return err
	}
	if state.targetExists && !plan.Force {
		dirty, err := worktreeDirty(plan.TargetPath)
		if err != nil {
			return err
		}
		if dirty {
			return &WorktreeConflict{Kind: "dirty", Tag: plan.Tag, ID: plan.Task,
				Path: plan.TargetPath, Branch: plan.Branch}
		}
	}
	exists, err := worktreeBranchExists(plan.OwnerBoundary, plan.Branch)
	if err != nil {
		return err
	}
	if !exists {
		return &WorktreeEffectIdentityMismatch{SagaID: plan.SagaID}
	}
	return nil
}

func syncRemovalDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func restoreRemovalTarget(plan *WorktreeRemovePlan, stage string) error {
	if stage == "" {
		return nil
	}
	exists, err := lstatDirectory(stage)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("staged worktree is unavailable: %s", stage)
	}
	if target, err := lstatDirectory(plan.TargetPath); err != nil || target {
		if err != nil {
			return err
		}
		return fmt.Errorf("cannot restore staged worktree: target exists")
	}
	if err := os.Rename(stage, plan.TargetPath); err != nil {
		return err
	}
	return syncRemovalDir(filepath.Dir(plan.TargetPath))
}

// PerformWorktreeRemovalEffect performs the exact direct, idempotent removal
// outside SQLite. It never invokes the broker and never deletes the branch.
// The returned outcome is safe to pass to FinalizeWorktreeRemoval. An empty
// outcome means identity rederivation failed and the saga must not be changed.
func PerformWorktreeRemovalEffect(plan *WorktreeRemovePlan) (WorktreeRemoveOutcome, error) {
	if err := rederiveRemovalFamily(plan); err != nil {
		return "", err
	}
	return performWorktreeRemovalEffect(plan, true)
}

// performWorktreeRemovalEffect performs the exact direct, idempotent staged
// removal once the family identity has been proven by the caller. It never
// invokes the broker and never deletes the branch. retainedBranch requires
// the saga's registered branch to survive the removal — part of the linked
// and runless-automatic proof — while a runless legacy row's recorded branch
// is stale conversion evidence whose absence never blocks the cleanup proof.
// The returned outcome is safe to pass to FinalizeWorktreeRemoval. An empty
// outcome means the staged effect failed and the saga must not be changed.
func performWorktreeRemovalEffect(plan *WorktreeRemovePlan, retainedBranch bool) (WorktreeRemoveOutcome, error) {
	state, err := inspectRemovalEffect(plan)
	if err != nil {
		return "", err
	}
	if state.targetExists && !plan.Force {
		dirty, err := worktreeDirty(plan.TargetPath)
		if err != nil {
			return WorktreeRemoveFailed, err
		}
		if dirty {
			return WorktreeRemoveFailed, &WorktreeConflict{Kind: "dirty", Tag: plan.Tag, ID: plan.Task,
				Path: plan.TargetPath, Branch: plan.Branch}
		}
	}

	targetStage := state.targetStage
	if state.targetExists {
		targetStage = removalTargetStage(plan)
		if targetStage == "" {
			return "", fmt.Errorf("worktree remove effect requires a committed saga fence")
		}
		if err := os.Rename(plan.TargetPath, targetStage); err != nil {
			return WorktreeRemoveFailed, err
		}
		if err := syncRemovalDir(filepath.Dir(plan.TargetPath)); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
		if err := worktreeRemoveEffectFaultHook("after-target-stage"); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
	}

	admin, adminStage := state.admin, state.adminStage
	if admin != "" {
		adminStage = removalAdminStage(plan, admin)
		if err := os.Rename(admin, adminStage); err != nil {
			if permissionClass(err) {
				if restoreErr := restoreRemovalTarget(plan, targetStage); restoreErr != nil {
					return WorktreeRemoveCleanupRequired, fmt.Errorf("%w; restore staged worktree: %v", errWorktreeConfined, restoreErr)
				}
				return WorktreeRemoveNeedsBroker, errWorktreeConfined
			}
			if restoreErr := restoreRemovalTarget(plan, targetStage); restoreErr != nil {
				return WorktreeRemoveCleanupRequired, fmt.Errorf("%w; restore staged worktree: %v", err, restoreErr)
			}
			return WorktreeRemoveFailed, err
		}
		if err := syncRemovalDir(plan.RegistrationRoot); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
		if err := worktreeRemoveEffectFaultHook("after-registration-stage"); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
	}
	if adminStage != "" {
		if err := os.RemoveAll(adminStage); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
		if err := syncRemovalDir(plan.RegistrationRoot); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
		if err := worktreeRemoveEffectFaultHook("after-registration-remove"); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
	}
	if targetStage != "" {
		if err := os.RemoveAll(targetStage); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
		if err := syncRemovalDir(filepath.Dir(plan.TargetPath)); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
		if err := worktreeRemoveEffectFaultHook("after-target-remove"); err != nil {
			return WorktreeRemoveCleanupRequired, err
		}
	}

	final, err := inspectRemovalEffect(plan)
	if err != nil {
		return WorktreeRemoveCleanupRequired, err
	}
	if final.targetExists || final.admin != "" || final.targetStage != "" || final.adminStage != "" {
		return WorktreeRemoveCleanupRequired, fmt.Errorf("worktree removal absence could not be proved")
	}
	if retainedBranch {
		exists, err := worktreeBranchExists(plan.OwnerBoundary, plan.Branch)
		if err != nil || !exists {
			if err == nil {
				err = fmt.Errorf("retained worktree branch is unavailable: %s", plan.Branch)
			}
			return WorktreeRemoveCleanupRequired, err
		}
	}
	return WorktreeRemoveRemoved, nil
}
