package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hygieneGit runs one Git command in dir against the test fixture.
func hygieneGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	gitCLIOutput(t, dir, args...)
}

// hygieneProvisionOwned provisions the engine-owned worktree for task 1
// through the real CLI, leaving one active provision saga row.
func hygieneProvisionOwned(t *testing.T, dir string) {
	t.Helper()
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "hygiene worktree", "hygiene.wt")
	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "hygiene.wt"), "--json")...)
	if code != 0 || stderr != "" {
		t.Fatalf("provision exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestHygieneReportsUnownedRegisteredWorktrees(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	hygieneProvisionOwned(t, dir)

	root := filepath.Dir(dir)
	// An engine-owned worktree under .worktrees: never reported.
	ownedPath := canonTestPath(t, dir, "main-1")
	// A clean review checkout on a branch.
	reviewClean := filepath.Join(root, "review-clean")
	hygieneGit(t, dir, "worktree", "add", "-q", reviewClean, "-b", "review-clean")
	// A dirty review checkout on a branch.
	reviewDirty := filepath.Join(root, "review-dirty")
	hygieneGit(t, dir, "worktree", "add", "-q", reviewDirty, "-b", "review-dirty")
	if err := os.WriteFile(filepath.Join(reviewDirty, "local.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A detached review checkout.
	reviewDetached := filepath.Join(root, "review-detached")
	hygieneGit(t, dir, "worktree", "add", "-q", "--detach", reviewDetached)
	// A registered worktree whose path is gone from the filesystem.
	reviewGone := filepath.Join(root, "review-gone")
	hygieneGit(t, dir, "worktree", "add", "-q", reviewGone, "-b", "review-gone")
	if err := os.RemoveAll(reviewGone); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeHygiene(t, stdout)
	if len(got.UnownedWorktrees) != 4 {
		t.Fatalf("unowned worktrees = %+v", got.UnownedWorktrees)
	}
	byPath := make(map[string]HygieneWorktreeRow)
	for _, row := range got.UnownedWorktrees {
		byPath[row.Path] = row
	}
	if _, ok := byPath[ownedPath]; ok {
		t.Fatalf("owned worktree %s reported unowned: %+v", ownedPath, got.UnownedWorktrees)
	}

	row, ok := byPath[reviewClean]
	if !ok || row.Branch != "review-clean" || row.Detached || !row.Exists || row.Dirty || row.Head == "" {
		t.Fatalf("clean review row = %+v", row)
	}
	row, ok = byPath[reviewDirty]
	if !ok || row.Branch != "review-dirty" || !row.Exists || !row.Dirty {
		t.Fatalf("dirty review row = %+v", row)
	}
	row, ok = byPath[reviewDetached]
	if !ok || !row.Detached || row.Branch != "" || !row.Exists || row.Head == "" {
		t.Fatalf("detached review row = %+v", row)
	}
	row, ok = byPath[reviewGone]
	if !ok || row.Branch != "review-gone" || row.Detached || row.Exists || row.Dirty {
		t.Fatalf("missing review row = %+v", row)
	}

	// The default-branch-quiet primary is present but not flagged.
	if got.PrimaryCheckout.Path != dir || got.PrimaryCheckout.Branch != "main" ||
		got.PrimaryCheckout.Detached || got.PrimaryCheckout.DefaultBranch != "main" ||
		got.PrimaryCheckout.Flagged || got.PrimaryCheckout.Dirty {
		t.Fatalf("primary checkout = %+v", got.PrimaryCheckout)
	}

	code, stdout, _ = runHygiene()
	if code != 0 ||
		!strings.Contains(stdout, "unowned worktree "+reviewClean+" branch review-clean exists") ||
		!strings.Contains(stdout, "unowned worktree "+reviewDirty+" branch review-dirty exists dirty") ||
		!strings.Contains(stdout, "unowned worktree "+reviewDetached+" detached at ") ||
		!strings.Contains(stdout, "unowned worktree "+reviewGone+" branch review-gone missing") ||
		!strings.Contains(stdout, "4 unowned registered worktree(s)") ||
		strings.Contains(stdout, "primary checkout "+dir) {
		t.Fatalf("human hygiene = code %d stdout %q", code, stdout)
	}
}

func TestHygieneReportsPrimaryCheckoutOffDefault(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main", "primary state")

	// On the default branch (no origin/HEAD recorded, so the fallback
	// default is main): quiet, and the human report stays residue-only.
	code, stdout, stderr := runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeHygiene(t, stdout)
	if got.PrimaryCheckout.Flagged || got.PrimaryCheckout.Branch != "main" {
		t.Fatalf("primary = %+v", got.PrimaryCheckout)
	}
	code, stdout, _ = runHygiene()
	if code != 0 || stdout != "no lifecycle residue found\n" || strings.Contains(stdout, "primary checkout") {
		t.Fatalf("quiet human hygiene = code %d stdout %q", code, stdout)
	}
	// The empty class renders [], never null, on the raw JSON surface.
	code, stdout, _ = runHygiene("--json")
	if !strings.Contains(stdout, `"unownedWorktrees": []`) {
		t.Fatalf("empty unownedWorktrees not rendered as []: %s", stdout)
	}

	// On a feature branch: flagged with the default named.
	hygieneGit(t, dir, "checkout", "-q", "-b", "feature-x")
	code, stdout, stderr = runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got = decodeHygiene(t, stdout)
	if !got.PrimaryCheckout.Flagged || got.PrimaryCheckout.Branch != "feature-x" ||
		got.PrimaryCheckout.DefaultBranch != "main" || got.PrimaryCheckout.Detached {
		t.Fatalf("primary = %+v", got.PrimaryCheckout)
	}
	code, stdout, _ = runHygiene()
	if code != 0 || !strings.Contains(stdout, "primary checkout "+dir+" branch feature-x (default main)") ||
		!strings.Contains(stdout, "1 primary checkout finding(s)") {
		t.Fatalf("human hygiene = code %d stdout %q", code, stdout)
	}

	// Detached: flagged as detached.
	hygieneGit(t, dir, "checkout", "-q", "--detach")
	code, stdout, stderr = runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got = decodeHygiene(t, stdout)
	if !got.PrimaryCheckout.Flagged || !got.PrimaryCheckout.Detached || got.PrimaryCheckout.Branch != "" {
		t.Fatalf("primary = %+v", got.PrimaryCheckout)
	}
	code, stdout, _ = runHygiene()
	if code != 0 || !strings.Contains(stdout, "primary checkout "+dir+" detached (default main)") {
		t.Fatalf("human hygiene = code %d stdout %q", code, stdout)
	}
}
func TestHygieneDoesNotTouchUnownedWorktreeIndexes(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main", "index untouched")

	review := filepath.Join(filepath.Dir(dir), "review-index")
	hygieneGit(t, dir, "worktree", "add", "-q", review, "-b", "review-index")
	// A linked worktree's index lives in the registration directory;
	// resolve it through Git rather than guessing the layout.
	gitDir := strings.TrimSpace(gitCLIOutput(t, review, "rev-parse", "--git-dir"))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(review, gitDir)
	}
	index := filepath.Join(gitDir, "index")
	// Make the index's stat cache stale so an ordinary `git status`
	// would want to rewrite it: bump the tracked .gitignore's mtime.
	if err := os.Chtimes(filepath.Join(review, ".gitignore"), nowPlus(2), nowPlus(2)); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() || !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("hygiene rewrote the unowned worktree's index: before %v/%d after %v/%d",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}

func TestHygieneTerminalSagaAndSymlinkedSpellingStillOwn(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, _, _, _ := provisionCLISeed(t, dir, "terminal saga", "hygiene.terminal")

	// A Git-registered worktree under .worktrees named only by a
	// terminal (failed, inactive) saga row, spelled through a symlinked
	// directory component: still owned, never reported unowned.
	terminalPath := filepath.Join(dir, ".worktrees", "main-7")
	hygieneGit(t, dir, "worktree", "add", "-q", terminalPath, "-b", "tasks/main/7")
	linkRoot := filepath.Join(filepath.Dir(dir), "lane")
	if err := os.Symlink(dir, linkRoot); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := gitRevParseMain(t, dir)
	if _, err := db.Exec(`INSERT INTO worktree_sagas(run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra)
		VALUES (?,NULL,'main','1','provision','failed',0,9100001,'hygiene.terminal.7',0,?,?,?,?,?,0,'2026-10-25T00:00:00Z','2026-10-25T00:00:00Z',NULL)`,
		runID, dir, "/git-admin/worktrees", filepath.Join(linkRoot, ".worktrees", "main-7"),
		"tasks/main/7", base); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeHygiene(t, stdout)
	if len(got.UnownedWorktrees) != 0 {
		t.Fatalf("terminal-saga worktree reported unowned: %+v", got.UnownedWorktrees)
	}
	if !strings.Contains(stdout, `"unownedWorktrees": []`) {
		t.Fatalf("empty unownedWorktrees not rendered as []: %s", stdout)
	}
}

// nowPlus returns the time n seconds from now; a future mtime
// guarantees the index's stat cache is stale.
func nowPlus(n int) time.Time {
	return time.Now().Add(time.Duration(n) * time.Second)
}
