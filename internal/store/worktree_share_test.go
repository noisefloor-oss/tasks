package store

import (
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestLinkedWorktreesShareOneDatabase proves the §8.1 promise: two checkouts
// of one repository family resolve and share a single database.
func TestLinkedWorktreesShareOneDatabase(t *testing.T) {
	repo := t.TempDir()
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.email", "test@example.invalid")
	git(t, repo, "config", "user.name", "test")
	git(t, repo, "commit", "--allow-empty", "-m", "init")

	dbPath, inGit := Path(repo)
	if !inGit {
		t.Fatal("git checkout reported outside Git")
	}
	db, err := Create(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	id := addTask(t, db, "main", "shared state")
	db.Close()

	// A linked worktree resolves the same derived path through the shared
	// common directory.
	worktree := filepath.Join(t.TempDir(), "linked")
	git(t, repo, "worktree", "add", worktree)
	wtPath, wtInGit := Path(worktree)
	if !wtInGit {
		t.Fatal("linked worktree reported outside Git")
	}
	if wtPath != dbPath {
		t.Fatalf("worktree path = %q, want shared %q", wtPath, dbPath)
	}

	// A committed transaction is visible immediately from both checkouts.
	wt, err := Open(wtPath)
	if err != nil {
		t.Fatal(err)
	}
	task, err := wt.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.Title != "shared state" {
		t.Fatalf("task from worktree = %+v", task)
	}
	// And writes from the worktree land in the same database.
	newID, err := wt.AddTask("main", &Task{Title: "from worktree"})
	if err != nil {
		t.Fatal(err)
	}
	wt.Close()
	primary, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	task, err = primary.GetTask("main", newID)
	if err != nil {
		t.Fatal(err)
	}
	if task == nil || task.Title != "from worktree" {
		t.Fatalf("worktree write not visible from primary: %+v", task)
	}
}
