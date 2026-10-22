package store

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// §9 task-bound worktree lifecycle. The git operations and binding mutations
// run inside one immediate transaction so compound operations (claim
// --worktree, release, done transitions) fold their worktree effect into the
// single §8.4 event the mutation appends; a failing creation or a claim
// conflict rolls the whole transaction back.

// WorktreeUsageError is a §9.1/§9.2 usage error (exit 2).
type WorktreeUsageError struct{ Message string }

func (e *WorktreeUsageError) Error() string { return e.Message }

// WorktreeConflict is a typed §9.2 worktree conflict (exit 3).
type WorktreeConflict struct {
	Kind     string // "exists" | "absent" | "path-exists" | "branch-exists" | "dirty" | "foreign" | "history"
	Tag      string
	ID       string
	Path     string
	Branch   string
	Boundary string // owning boundary, set for "foreign"
}

func (e *WorktreeConflict) Error() string {
	switch e.Kind {
	case "exists":
		return fmt.Sprintf("task %s already has a worktree", e.ID)
	case "absent":
		return fmt.Sprintf("task %s has no active worktree", e.ID)
	case "path-exists":
		return fmt.Sprintf("worktree path exists: %s", e.Path)
	case "branch-exists":
		return fmt.Sprintf("branch %s already exists", e.Branch)
	case "dirty":
		return fmt.Sprintf("worktree has uncommitted changes: %s", e.Path)
	case "foreign":
		return fmt.Sprintf("task %s worktree is bound to another checkout: %s", e.ID, e.Boundary)
	case "history":
		return fmt.Sprintf("task %s has retained worktree history", e.ID)
	default:
		return "worktree conflict"
	}
}

// WorktreeRoot is the §9.1 worktree root for a repository boundary.
func WorktreeRoot(boundary string) string {
	return filepath.Join(boundary, ".worktrees")
}

// WorktreeName is the §9.1 directory name for a task's worktree.
func WorktreeName(tag, id string) string { return tag + "-" + id }

// WorktreeBranch is the §9.1 branch name for a task's worktree.
func WorktreeBranch(tag, id string) string { return "tasks/" + tag + "/" + id }

// canonicalPath normalizes a path for identity comparisons: absolute,
// symlink-resolved (when it exists), and cleaned. Paths that differ only by
// symlinked or relative spelling (e.g. /tmp vs /private/tmp) compare equal.
func canonicalPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

// checkWorktreeTag enforces the §9.1 slash-tag refusal (exit 2).
func checkWorktreeTag(tag string) error {
	if strings.Contains(tag, "/") {
		return &WorktreeUsageError{Message: fmt.Sprintf("tag %s cannot name a worktree", tag)}
	}
	return nil
}

// checkWorktreeID pins §9 worktree names to plain integer ids: the
// constructed directory name must round-trip through parseWorktreeName, so
// dotted subtask references and non-numeric ids refuse as usage errors.
func checkWorktreeID(tag, id string) error {
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return &WorktreeUsageError{Message: fmt.Sprintf("task %s cannot name a worktree: ids must be plain integers", id)}
	}
	parsedTag, parsedID, ok := parseWorktreeName(WorktreeName(tag, id))
	if !ok || parsedTag != tag || parsedID != id {
		return &WorktreeUsageError{Message: fmt.Sprintf("task %s cannot name a worktree: ids must be plain integers", id)}
	}
	return nil
}

// parseWorktreeName splits a directory name on the final "-" into tag and a
// numeric id, reporting whether the name matches the §9.1 shape.
func parseWorktreeName(name string) (tag, id string, ok bool) {
	i := strings.LastIndex(name, "-")
	if i <= 0 || i == len(name)-1 {
		return "", "", false
	}
	id = name[i+1:]
	if _, err := strconv.ParseUint(id, 10, 64); err != nil {
		return "", "", false
	}
	return name[:i], id, true
}

// gitRun executes one Git command (argv, no shell) with dir as the working
// directory and returns its combined output.
func gitRun(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// gitExit runs one Git command and returns its exit code. Only the codes in
// allowed are accepted as answers (e.g. 0/1 for check-ignore, show-ref,
// rev-parse); any other exit — 128 usage errors, signal deaths — surfaces as
// a real error so callers never read a failure as a boolean false.
func gitExit(dir string, allowed []int, args ...string) (int, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if exitErr, ok := err.(*exec.ExitError); ok {
		code := exitErr.ExitCode()
		for _, a := range allowed {
			if code == a {
				return code, nil
			}
		}
		return code, fmt.Errorf("git %s: exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(string(out)))
	}
	if err != nil {
		return -1, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return 0, nil
}

// resolveDefaultRef resolves the family's default branch per §9.1:
// origin/HEAD when it resolves to a commit, else the local main branch.
func resolveDefaultRef(boundary string) (string, error) {
	if code, err := gitExit(boundary, []int{0, 1}, "rev-parse", "--verify", "--quiet", "origin/HEAD^{commit}"); err != nil {
		return "", err
	} else if code == 0 {
		return "origin/HEAD", nil
	}
	if code, err := gitExit(boundary, []int{0, 1}, "rev-parse", "--verify", "--quiet", "refs/heads/main^{commit}"); err != nil {
		return "", err
	} else if code == 0 {
		return "main", nil
	}
	return "", fmt.Errorf("cannot resolve default branch")
}

// checkWorktreeIgnored verifies the concrete target path is ignored by Git
// (§9.1); the tool never edits ignore files.
func checkWorktreeIgnored(boundary, target string) error {
	rel, err := filepath.Rel(boundary, target)
	if err != nil {
		return err
	}
	code, err := gitExit(boundary, []int{0, 1}, "check-ignore", "-q", "--", filepath.ToSlash(rel))
	if err != nil {
		return err
	}
	if code != 0 {
		return &WorktreeUsageError{Message: ".worktrees is not git-ignored; add it to .gitignore or .git/info/exclude"}
	}
	return nil
}

// checkWorktreeBranchFormat pre-validates the constructed branch name so a
// bad tag yields a pinned usage error instead of raw git output.
func checkWorktreeBranchFormat(boundary, tag, branch string) error {
	code, err := gitExit(boundary, []int{0, 1}, "check-ref-format", "refs/heads/"+branch)
	if err != nil {
		return err
	}
	if code != 0 {
		return &WorktreeUsageError{Message: fmt.Sprintf("tag %s cannot name a worktree branch: %s is not a valid ref name", tag, branch)}
	}
	return nil
}

// worktreeBranchExists reports whether refs/heads/<branch> exists.
func worktreeBranchExists(boundary, branch string) (bool, error) {
	code, err := gitExit(boundary, []int{0, 1}, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return code == 0, nil
}

// worktreeDirty reports whether the worktree at path has modified or
// untracked files.
func worktreeDirty(path string) (bool, error) {
	out, err := gitRun(path, "status", "--porcelain")
	if err != nil {
		return false, err
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

// worktreeRegistered reports whether Git's common-dir registration names the
// exact target. The porcelain NUL form is unambiguous even when an owner path
// contains whitespace.
func worktreeRegistered(boundary, target string) (bool, error) {
	out, err := gitRegistrationRun(boundary, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return false, err
	}
	target = canonicalPath(target)
	for _, field := range bytes.Split(out, []byte{0}) {
		const prefix = "worktree "
		if bytes.HasPrefix(field, []byte(prefix)) && canonicalPath(string(field[len(prefix):])) == target {
			return true, nil
		}
	}
	return false, nil
}

func worktreeBaseOID(boundary, ref string) (string, error) {
	out, err := gitRegistrationRun(boundary, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// compensateFailedWorktreeCreate compare-deletes only the branch side effect
// of this exact add attempt. A target or registration means ownership is no
// longer unambiguous, so the durable §9.4 saga recovery must handle that
// rarer state instead of this compatibility compensation guessing.
func compensateFailedWorktreeCreate(boundary, target, branch, expectedOID string) {
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		return
	}
	registered, err := worktreeRegistered(boundary, target)
	if err != nil || registered {
		return
	}
	out, err := gitRegistrationRun(boundary, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil || strings.TrimSpace(string(out)) != expectedOID {
		return
	}
	_, _ = gitRegistrationRun(boundary, "update-ref", "-d", "refs/heads/"+branch, expectedOID)
}

func verifyCreatedWorktree(boundary, target string) error {
	info, err := os.Stat(target)
	if err != nil || !info.IsDir() {
		return &WorktreeBrokerRefusal{Reason: "broker reported success without creating the worktree"}
	}
	registered, err := worktreeRegistered(boundary, target)
	if err != nil {
		return err
	}
	if !registered {
		return &WorktreeBrokerRefusal{Reason: "broker reported success without registering the worktree"}
	}
	return nil
}

// worktreeAdminDir resolves the target's Git administrative registration and
// proves it is one direct directory beneath this family's exact common-dir
// registration root. Removal never trusts a caller- or leaf-selected path.
func worktreeAdminDir(boundary, target string) (string, error) {
	marker := filepath.Join(target, ".git")
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
		admin = filepath.Join(target, admin)
	}
	admin = canonicalPath(admin)
	roots := registrationRoots(boundary)
	if len(roots) == 0 || filepath.Dir(admin) != canonicalPath(roots[0]) {
		return "", fmt.Errorf("unsafe worktree registration: %s", admin)
	}
	info, err := os.Lstat(admin)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("unsafe worktree registration: %s", admin)
	}
	backref, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return "", err
	}
	if canonicalPath(strings.TrimSpace(string(backref))) != canonicalPath(marker) {
		return "", fmt.Errorf("unsafe worktree registration binding: %s", admin)
	}
	return admin, nil
}

// removeRegistration performs the actual registration mutation without
// touching the sandbox target. An atomic rename from the exact registration
// root either succeeds completely or yields the permission-class confinement
// signal without Git first emptying the administrative directory.
func removeRegistration(boundary, target, admin string) (staged string, err error) {
	staged = filepath.Join(filepath.Dir(admin), ".noise-remove-"+filepath.Base(admin))
	if _, err := os.Lstat(staged); err == nil {
		return "", fmt.Errorf("worktree registration staging path exists: %s", staged)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.Rename(admin, staged); err == nil {
		return staged, nil
	} else if !permissionClass(err) {
		return "", err
	}
	if err := delegateWorktreeBroker(boundary, "remove"); err != nil {
		return "", err
	}
	return "", nil
}

// createWorktree performs the §9.2 creation preconditions and the git
// worktree add and sets the binding on the in-memory task, without writing or
// appending an event — the enclosing transaction persists and folds the event.
// keep preserves the retention intent of a just-cleared stale binding.
// boundary must already be canonicalized.
func (t *tx) createWorktree(boundary string, task *Task, keep bool, at string) (*WorktreeBinding, error) {
	if err := checkWorktreeTag(task.Tag); err != nil {
		return nil, err
	}
	if err := checkWorktreeID(task.Tag, task.ID); err != nil {
		return nil, err
	}
	if task.Status == "done" {
		return nil, &WorktreeUsageError{Message: fmt.Sprintf("task %s is done", task.ID)}
	}

	target := filepath.Join(WorktreeRoot(boundary), WorktreeName(task.Tag, task.ID))
	if b := task.Binding; b != nil {
		if canonicalPath(b.Boundary) != boundary {
			return nil, &WorktreeConflict{Kind: "foreign", Tag: task.Tag, ID: task.ID, Path: b.Path, Branch: b.Branch, Boundary: b.Boundary}
		}
		if _, err := os.Lstat(b.Path); err == nil {
			return nil, &WorktreeConflict{Kind: "exists", Tag: task.Tag, ID: task.ID, Path: b.Path, Branch: b.Branch}
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		// Stale binding: the directory vanished out-of-band. Clear it and
		// proceed; the retained branch normally surfaces as the branch-exists
		// conflict below (§9.2).
		keep = keep || b.Keep
		task.Binding = nil
	}

	if _, err := os.Lstat(target); err == nil {
		return nil, &WorktreeConflict{Kind: "path-exists", Tag: task.Tag, ID: task.ID, Path: target}
	} else if !os.IsNotExist(err) {
		return nil, err
	}

	branch := WorktreeBranch(task.Tag, task.ID)
	if err := checkWorktreeBranchFormat(boundary, task.Tag, branch); err != nil {
		return nil, err
	}
	exists, err := worktreeBranchExists(boundary, branch)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, &WorktreeConflict{Kind: "branch-exists", Tag: task.Tag, ID: task.ID, Branch: branch}
	}

	if err := checkWorktreeIgnored(boundary, target); err != nil {
		return nil, err
	}

	ref, err := resolveDefaultRef(boundary)
	if err != nil {
		return nil, err
	}
	baseOID, err := worktreeBaseOID(boundary, ref)
	if err != nil {
		return nil, err
	}
	if err := runWorktreeRegistration(boundary, "create",
		[]string{"worktree", "add", "-b", branch, "--", target, ref}); err != nil {
		compensateFailedWorktreeCreate(boundary, target, branch, baseOID)
		return nil, err
	}
	if err := verifyCreatedWorktree(boundary, target); err != nil {
		compensateFailedWorktreeCreate(boundary, target, branch, baseOID)
		return nil, err
	}

	binding := &WorktreeBinding{
		Path:      target,
		Boundary:  boundary,
		Branch:    branch,
		CreatedAt: at,
		Keep:      keep,
	}
	task.Binding = binding
	return binding, nil
}

// removeWorktreeDir removes a worktree directory and its exact Git
// registration. A directory that already vanished is a no-op. Without force,
// a dirty worktree refuses with a typed dirty conflict; automatic removal paths
// always run unforced.
func removeWorktreeDir(boundary, path string, force bool) error {
	if _, err := os.Lstat(path); err == nil {
		if !force {
			dirty, err := worktreeDirty(path)
			if err != nil {
				return err
			}
			if dirty {
				return &WorktreeConflict{Kind: "dirty", Path: path}
			}
		}
		admin, err := worktreeAdminDir(boundary, path)
		if err != nil {
			return err
		}
		// Stage the already-checked target by same-directory atomic rename. The
		// broker then sees an absent original path, while a confinement refusal
		// can restore the target without data loss.
		staged := filepath.Join(filepath.Dir(path), ".noise-remove-"+filepath.Base(path))
		if _, err := os.Lstat(staged); err == nil {
			return fmt.Errorf("worktree removal staging path exists: %s", staged)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(path, staged); err != nil {
			return err
		}
		restore := func(cause error) error {
			if err := os.Rename(staged, path); err != nil {
				return fmt.Errorf("%w; restore staged worktree %s: %v", cause, path, err)
			}
			return cause
		}

		stagedRegistration, err := removeRegistration(boundary, path, admin)
		if err != nil {
			return restore(err)
		}
		if stagedRegistration != "" {
			if err := os.RemoveAll(stagedRegistration); err != nil {
				_ = os.Rename(stagedRegistration, admin)
				return restore(fmt.Errorf("remove staged worktree registration %s: %w", stagedRegistration, err))
			}
		}
		registered, err := worktreeRegistered(boundary, path)
		if err != nil {
			return restore(err)
		}
		if registered {
			return restore(&WorktreeBrokerRefusal{Reason: "broker reported success without removing the registration"})
		}
		if err := os.RemoveAll(staged); err != nil {
			return fmt.Errorf("remove staged worktree %s: %w", staged, err)
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// foldWorktreeRemoval attempts automatic worktree removal for release and
// done transitions (SPEC §9.2): removal never blocks the transition. On
// success the binding is cleared and removed is true. A dirty worktree or a
// failed removal degrades to keep: true with a note for the caller to
// report; a foreign binding is left untouched with a note.
func foldWorktreeRemoval(boundary string, task *Task) (removed bool, note string) {
	b := task.Binding
	if b == nil {
		return false, ""
	}
	boundary = canonicalPath(boundary)
	if canonicalPath(b.Boundary) != boundary {
		return false, fmt.Sprintf("task worktree is bound to another checkout: %s; binding left untouched", b.Boundary)
	}
	if err := removeWorktreeDir(boundary, b.Path, false); err != nil {
		b.Keep = true
		return false, fmt.Sprintf("worktree removal failed (%v); binding kept with keep: true", err)
	}
	task.Binding = nil
	return true, ""
}

// CreateWorktree creates the task's bound worktree and records the binding on
// the task row (SPEC §9.1/§9.2), appending one worktree/created feed event.
// The binding mutation is a claim-style exception under §8.2: it neither
// requires nor increments the task revision. keep preserves the retention
// intent of a just-cleared stale binding.
func (d *DB) CreateWorktree(boundary, tag, id string, keep bool) (*WorktreeBinding, error) {
	boundary = canonicalPath(boundary)
	// §9.2: the stale-binding clear commits first, so the retained-branch
	// conflict on the re-attempt does not leave a dead binding behind.
	clearedKeep, err := d.clearStaleBinding(boundary, tag, id)
	if err != nil {
		return nil, err
	}
	keep = keep || clearedKeep
	var binding *WorktreeBinding
	err = d.immediate(func(t *tx) error {
		// Usage refusals precede the lookup so a malformed id is a usage
		// error, not a not-found (§9.1).
		if err := checkWorktreeTag(tag); err != nil {
			return err
		}
		if err := checkWorktreeID(tag, id); err != nil {
			return err
		}
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		at := nowUTC()
		binding, err = t.createWorktree(boundary, task, keep, at)
		if err != nil {
			return err
		}
		if _, err := t.writeTask(task, -1); err != nil {
			return err
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "worktree", Action: "created", Revision: task.Revision, At: at})
	})
	if err != nil {
		return nil, err
	}
	return binding, nil
}

// clearStaleBinding commits the §9.2 stale-binding clear as its own
// transaction when the binding names this boundary and its directory has
// vanished, returning the cleared binding's keep flag. The clear landing
// before the creation re-attempt keeps a retained-branch conflict from
// leaving the dead binding behind. Read-gated, so a vanished task or absent
// binding is a no-op.
func (d *DB) clearStaleBinding(boundary, tag, id string) (bool, error) {
	var keep bool
	err := d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, id)
		if err != nil || task == nil || task.Binding == nil {
			return err
		}
		b := task.Binding
		if canonicalPath(b.Boundary) != boundary {
			return nil
		}
		if _, err := os.Lstat(b.Path); !os.IsNotExist(err) {
			return err // nil when present: not stale
		}
		keep = b.Keep
		task.Binding = nil
		_, err = t.writeTask(task, -1)
		return err
	})
	return keep, err
}

// RemoveWorktree removes the task's bound worktree and clears the binding
// (SPEC §9.2). The branch is retained. A dirty worktree refuses unless force
// passes --force to git. A binding whose directory already vanished is simply
// cleared. Binding mutations neither require nor increment the task revision.
func (d *DB) RemoveWorktree(boundary, tag, id string, force bool) (string, error) {
	boundary = canonicalPath(boundary)
	var path string
	err := d.immediate(func(t *tx) error {
		if err := checkWorktreeTag(tag); err != nil {
			return err
		}
		if err := checkWorktreeID(tag, id); err != nil {
			return err
		}
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		b := task.Binding
		if b == nil {
			return fmt.Errorf("task %s has no worktree binding", id)
		}
		if canonicalPath(b.Boundary) != boundary {
			return &WorktreeConflict{Kind: "foreign", Tag: tag, ID: id, Path: b.Path, Branch: b.Branch, Boundary: b.Boundary}
		}
		if err := removeWorktreeDir(boundary, b.Path, force); err != nil {
			var wc *WorktreeConflict
			if errors.As(err, &wc) && wc.Kind == "dirty" {
				wc.Tag, wc.ID, wc.Branch = tag, id, b.Branch
			}
			return err
		}
		path = b.Path
		task.Binding = nil
		if _, err := t.writeTask(task, -1); err != nil {
			return err
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "worktree", Action: "removed", Revision: task.Revision, At: nowUTC()})
	})
	if err != nil {
		return "", err
	}
	return path, nil
}

// ClaimWorktree acquires the claim and creates the bound worktree in one
// immediate transaction (SPEC §9.2): if the claim fences fail, no worktree is
// created; if worktree creation fails, the claim is never committed. One
// folded claim event carries action "created" (§8.4/§9.1). keep preserves the
// retention intent of a just-cleared stale binding. The returned revision is
// the task's, read inside the same transaction.
func (d *DB) ClaimWorktree(boundary, tag, id, owner string, ttl int, keep bool) (*Claim, *WorktreeBinding, int, error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return nil, nil, 0, fmt.Errorf("claim owner %v", err)
	}
	boundary = canonicalPath(boundary)
	// §9.2: the stale-binding clear commits first, so the retained-branch
	// conflict on the re-attempt does not leave a dead binding behind.
	clearedKeep, err := d.clearStaleBinding(boundary, tag, id)
	if err != nil {
		return nil, nil, 0, err
	}
	keep = keep || clearedKeep
	var claim *Claim
	var binding *WorktreeBinding
	revision := 0
	err = d.immediate(func(t *tx) error {
		// Usage refusals precede the lookup so a malformed id is a usage
		// error, not a not-found (§9.1).
		if err := checkWorktreeTag(tag); err != nil {
			return err
		}
		if err := checkWorktreeID(tag, id); err != nil {
			return err
		}
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		revision = task.Revision
		now := time.Now().UTC()
		expireClaim(task, now)
		if task.Claim != nil {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "claim",
				Owner: task.Claim.Owner, Expiry: task.Claim.ExpiresAt, FencingToken: task.Claim.Token}
		}
		at := nowUTC() // binding createdAt and the folded event timestamp
		binding, err = t.createWorktree(boundary, task, keep, at)
		if err != nil {
			return err
		}
		if ttl <= 0 {
			ttl = DefaultClaimTTL
		}
		token, err := t.nextFence()
		if err != nil {
			return err
		}
		// The claim clock starts after creation succeeds, so acquiredAt and
		// expiresAt derive from one timestamp taken past the git operation.
		acquired := time.Now().UTC()
		claim = &Claim{
			Owner:      owner,
			Token:      token,
			TTL:        ttl,
			AcquiredAt: acquired.Format(time.RFC3339),
			ExpiresAt:  acquired.Add(time.Duration(ttl) * time.Second).Format(time.RFC3339),
		}
		task.Claim = claim
		res, err := t.Exec(`UPDATE tasks SET claim_owner=?, claim_token=?, claim_ttl=?, claim_acquired_at=?, claim_expires_at=?,
			wt_path=?, wt_boundary=?, wt_branch=?, wt_created_at=?, wt_keep=?, wt_extra=NULL
			WHERE tag=? AND id=? AND (claim_owner IS NULL OR claim_expires_at <= ?)`,
			claim.Owner, claim.Token, claim.TTL, claim.AcquiredAt, claim.ExpiresAt,
			binding.Path, binding.Boundary, binding.Branch, binding.CreatedAt, binding.Keep,
			tag, id, now.Format(time.RFC3339))
		if err != nil {
			return fmt.Errorf("acquire claim on task %s: %w", id, err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			current, err := t.getTask(tag, id)
			if err != nil {
				return err
			}
			if current == nil {
				return &TaskNotFound{Tag: tag, ID: id}
			}
			conflict := &ClaimConflict{Tag: tag, ID: id, Kind: "claim"}
			if current.Claim != nil {
				conflict.Owner = current.Claim.Owner
				conflict.Expiry = current.Claim.ExpiresAt
				conflict.FencingToken = current.Claim.Token
			}
			return conflict
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "claim", Owner: owner, Revision: task.Revision, Action: "created", At: at})
	})
	if err != nil {
		return nil, nil, 0, err
	}
	return claim, binding, revision, nil
}

// ReleaseClaimWorktree drops a claim by CAS token (or by force) and, when the
// task has a bound worktree, folds the §9.2 worktree removal into the same
// transaction: the worktree is removed and the binding cleared unless
// keepWorktree, which marks the binding keep: true instead. A failed or dirty
// removal never blocks the release: the claim is released, the binding is
// kept with keep: true, and a note is returned for the caller to report. A
// binding owned by another checkout (foreign boundary) is never folded: it is
// left untouched with a note. The release event carries action "removed" only
// when removal actually happened.
func (d *DB) ReleaseClaimWorktree(boundary, tag, id, owner string, token int64, force, keepWorktree bool) (note string, err error) {
	if err := ValidateOwnerLabel(owner); err != nil {
		return "", fmt.Errorf("claim owner %v", err)
	}
	err = d.immediate(func(t *tx) error {
		now := time.Now().UTC()
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		physicalClaim := task.Claim
		expireClaim(task, now)
		if force {
			if physicalClaim == nil {
				// No-op force release (never claimed, or the claim expired and
				// has no durable row): any binding is deliberately left in
				// place — reclaiming those is prune's job (§9.2).
				run, err := t.nonterminalRunForTask(tag, id)
				if err != nil {
					return err
				}
				if run != nil {
					return fmt.Errorf("task %s/%s has a nonterminal run without a physical claim", tag, id)
				}
				return nil
			}
			owner = physicalClaim.Owner
		} else {
			if task.Claim == nil || task.Claim.Token != token || task.Claim.Owner != owner {
				conflict := &ClaimConflict{Tag: tag, ID: id, Kind: "release"}
				if task.Claim != nil {
					conflict.Owner = task.Claim.Owner
					conflict.Expiry = task.Claim.ExpiresAt
					conflict.FencingToken = task.Claim.Token
				}
				return conflict
			}
		}
		run, err := t.nonterminalRunForTask(tag, id)
		if err != nil {
			return err
		}
		if !force && run != nil {
			return &ClaimConflict{Tag: tag, ID: id, Kind: "release"}
		}
		if force {
			if err := t.forceCancelRunForRelease(run, tag, id, now.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		action := ""
		if task.Binding != nil {
			if keepWorktree {
				if canonicalPath(task.Binding.Boundary) == canonicalPath(boundary) {
					task.Binding.Keep = true
				} else {
					note = fmt.Sprintf("task worktree is bound to another checkout: %s; binding left untouched", task.Binding.Boundary)
				}
			} else {
				var removed bool
				removed, note = foldWorktreeRemoval(boundary, task)
				if removed {
					action = "removed"
				}
			}
		}
		task.Claim = nil
		if _, err := t.writeTask(task, -1); err != nil {
			return err
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "release", Owner: owner, Revision: task.Revision, Action: action, At: now.Format(time.RFC3339)})
	})
	return note, err
}

// SetStatusWorktree applies a status transition with CAS revision and claim
// gating and, for a top-level transition to done on a task with a bound
// worktree, folds the §9.2 worktree removal into the same transaction: the
// worktree is removed and the binding cleared unless keepWorktree, in which
// case the binding is marked keep: true. A failed or dirty removal never
// blocks the transition (the done floor): it degrades to keep: true with a
// note the caller should report. A foreign binding is left untouched with a
// note. The folded status event carries action "removed" only when the
// removal actually happened.
func (d *DB) SetStatusWorktree(boundary, tag, raw, status string, revision int, gate *ClaimGate, keepWorktree bool) (previous, note string, err error) {
	if !validStatuses[status] {
		return "", "", fmt.Errorf("invalid status %q", status)
	}
	parentID, subtaskID, err := backlog.ParseTaskReference(raw)
	if err != nil {
		return "", "", err
	}
	if err := ValidateTaskRunGate(gate); err != nil {
		return "", "", err
	}
	err = d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, parentID)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: raw}
		}
		now := time.Now().UTC()
		expireClaim(task, now)
		if task.Revision != revision {
			return t.staleConflict(tag, parentID, revision)
		}
		claim := cloneClaim(task.Claim)
		at := now.Format(time.RFC3339)
		if subtaskID != "" {
			// Subtask transitions carry no worktree fold (§9 binds tasks).
			reloaded, prev, err := prepareSubtaskStatus(task, parentID, subtaskID, status)
			if err != nil {
				return err
			}
			fence, err := t.taskMutationRunFence(task, gate, now, "status")
			if err != nil {
				return err
			}
			previous = prev
			if prev == status {
				return nil
			}
			return t.publishSubtaskStatus(task, reloaded, parentID, subtaskID, status, revision,
				gate, claim, fence, at)
		}
		fence, err := t.taskMutationRunFence(task, gate, now, "status")
		if err != nil {
			return err
		}
		previous = task.Status
		if previous == status {
			return nil
		}
		action := ""
		if status == "done" && task.Binding != nil {
			if keepWorktree {
				if canonicalPath(task.Binding.Boundary) == canonicalPath(boundary) {
					task.Binding.Keep = true
				} else {
					note = fmt.Sprintf("task worktree is bound to another checkout: %s; binding left untouched", task.Binding.Boundary)
				}
			} else {
				var removed bool
				removed, note = foldWorktreeRemoval(boundary, task)
				if removed {
					action = "removed"
				}
			}
		}
		task.Status = status
		task.UpdatedAt = at
		released := false
		if status == "done" && task.Claim != nil {
			task.Claim = nil
			released = true
		}
		oldRevision := task.Revision
		task.Revision++
		n, err := t.writeTaskWithRunFence(task, oldRevision, gate, claim, fence, at)
		if err != nil {
			return err
		}
		if n == 0 {
			return t.taskMutationWriteConflict(fence, claim, gate, "status", tag, parentID, revision)
		}
		if err := t.recountTag(tag); err != nil {
			return err
		}
		// §9.4: a direct top-level transition to done establishes the
		// runless automatic worktree cleanup authority in this same
		// transaction, audited by the status event below.
		if status == "done" {
			if err := t.scheduleWorktreeAutoCleanup(tag, parentID, 0, at); err != nil {
				return err
			}
		}
		if err := t.appendEvent(Event{Task: parentID, Tag: tag, Kind: "status", Status: status, Revision: task.Revision, Action: action, At: at}); err != nil {
			return err
		}
		if released {
			return t.appendEvent(Event{Task: parentID, Tag: tag, Kind: "release", Revision: task.Revision, At: at})
		}
		return nil
	})
	return previous, note, err
}
