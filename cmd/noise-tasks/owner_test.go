package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// ownerGit runs one git command in dir with the hermetic environment.
func ownerGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "protocol.file.allow=always", "-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// makeGitRepo creates a real committed repository rooted at dir.
func makeGitRepo(t *testing.T, dir string) {
	t.Helper()
	ownerGit(t, dir, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ownerGit(t, dir, "add", "README")
	ownerGit(t, dir, "commit", "-m", "init", "--no-gpg-sign")
}

// TestDerivedOwnerBoundaryGitShapes proves the canonical owner boundary for
// an ordinary repository, a linked worktree of it, and a submodule: all
// resolve to the primary worktree root, stable across linked worktrees.
func TestDerivedOwnerBoundaryGitShapes(t *testing.T) {
	super := t.TempDir()
	makeGitRepo(t, super)

	// Ordinary repository.
	if got, err := derivedOwnerBoundary(super); err != nil || got != super {
		t.Fatalf("ordinary repo owner = (%q, %v), want %q", got, err, super)
	}

	// Linked worktree of the same repository: same canonical owner.
	linked := filepath.Join(t.TempDir(), "linked")
	ownerGit(t, super, "worktree", "add", linked, "-b", "linked")
	if got, err := derivedOwnerBoundary(linked); err != nil || got != super {
		t.Fatalf("linked worktree owner = (%q, %v), want %q", got, err, super)
	}

	// Submodule: the common dir lives under the superproject's
	// .git/modules path; the owner is the submodule's registered primary
	// worktree, resolved through Git's own registry — not path spelling.
	subURL := t.TempDir()
	makeGitRepo(t, subURL)
	ownerGit(t, super, "submodule", "add", subURL, "sub")
	ownerGit(t, super, "commit", "-m", "sub", "--no-gpg-sign")
	sub := filepath.Join(super, "sub")
	if got, err := derivedOwnerBoundary(sub); err != nil || got != sub {
		t.Fatalf("submodule owner = (%q, %v), want %q", got, err, sub)
	}

	// Ambient command-scope Git configuration cannot redirect the owner:
	// the resolver reads only the common directory's own config.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("GIT_CONFIG_VALUE_0", t.TempDir())
	if got, err := derivedOwnerBoundary(sub); err != nil || got != sub {
		t.Fatalf("submodule owner under ambient Git override = (%q, %v), want %q", got, err, sub)
	}

	// A linked worktree of the submodule is equally stable: it resolves to
	// the submodule's primary worktree.
	subLinked := filepath.Join(t.TempDir(), "sub-linked")
	ownerGit(t, sub, "worktree", "add", subLinked, "-b", "sublinked")
	if got, err := derivedOwnerBoundary(subLinked); err != nil || got != sub {
		t.Fatalf("submodule linked worktree owner = (%q, %v), want %q", got, err, sub)
	}
}

// TestSubmoduleOwnerEqualityAcrossCommands proves init, setup, and backup
// create all derive the same owner boundary for a submodule and record or
// validate it as the submodule root.
func TestSubmoduleOwnerEqualityAcrossCommands(t *testing.T) {
	super := t.TempDir()
	makeGitRepo(t, super)
	subURL := t.TempDir()
	makeGitRepo(t, subURL)
	ownerGit(t, super, "submodule", "add", subURL, "sub")
	ownerGit(t, super, "commit", "-m", "sub", "--no-gpg-sign")
	sub := filepath.Join(super, "sub")

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}

	// init mints the family identity with the derived owner.
	if code, _, stderr := runCmd(cmdInit); code != 0 {
		t.Fatalf("init exit %d: %s", code, stderr)
	}
	common, ok := store.Path(sub)
	if !ok {
		t.Fatal("submodule store path not in Git")
	}
	db, err := store.Open(common)
	if err != nil {
		t.Fatal(err)
	}
	familyID, stored, err := db.Identity()
	db.Close()
	if err != nil || familyID == "" || stored != sub {
		t.Fatalf("submodule identity = (%q, %q, %v), want owner %q", familyID, stored, err, sub)
	}

	// setup (already initialized) re-derives the same owner and succeeds.
	if code, _, stderr := runSetup("", false, "--adapter", "anthropic-api", "--model", "claude-fable-5", "--yes", "--json"); code != 0 {
		t.Fatalf("setup exit %d: %s", code, stderr)
	}

	// backup create validates the stored identity against the same derived
	// owner; a mismatched derivation would refuse.
	bundle := filepath.Join(t.TempDir(), "sub.ntbackup")
	if code, _, stderr := runCmd(cmdBackup, "create", "--output", bundle, "--json"); code != 0 {
		t.Fatalf("backup create exit %d: %s", code, stderr)
	}

	// The superproject derives a different owner boundary: the families
	// cannot collide.
	wrong, err := derivedOwnerBoundary(super)
	if err != nil {
		t.Fatal(err)
	}
	if wrong == sub {
		t.Fatal("superproject boundary collides with submodule boundary")
	}
}
