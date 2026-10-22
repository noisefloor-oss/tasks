package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// chdirWorktreeRepo creates a real temp Git repository on branch main with
// .worktrees/ git-ignored and chdirs into it.
func chdirWorktreeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".gitignore")
	git("commit", "-m", "init", "--no-gpg-sign")

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

// seedWorktreeStore seeds the project database: one task per title; titles
// prefixed with "done:" are seeded then transitioned to done.
func seedWorktreeStore(t *testing.T, dir string, titles ...string) {
	t.Helper()
	seedProjectStore(t, dir, "main", titles...)
}

// canonTestPath canonicalizes the expected .worktrees/<tag>-<id> path the way
// the store layer does (absolute, symlink-resolved parent, cleaned).
func canonTestPath(t *testing.T, dir, name string) string {
	t.Helper()
	p, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return filepath.Join(filepath.Clean(p), ".worktrees", name)
}

func TestWorktreeCreateRemovedCLI(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")

	// §9.4 v5 cutover: worktree create is a usage error with the pinned
	// guidance, before any store is opened.
	code, stdout, stderr := runCmd(cmdWorktree, "create", "1")
	want := "noise-tasks worktree: create was removed by the v5 cutover; acquire a claim, start a run, and use worktree provision\n"
	if code != 2 || stdout != "" || stderr != want {
		t.Fatalf("create exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The refusal is shape-independent: flags and --json change nothing.
	code, stdout, stderr = runCmd(cmdWorktree, "create", "--json", "1")
	if code != 2 || stdout != "" || stderr != want {
		t.Fatalf("create --json exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// No worktree directory or branch was created.
	if _, err := os.Lstat(canonTestPath(t, dir, "main-1")); !os.IsNotExist(err) {
		t.Fatalf("removed create left a directory: %v", err)
	}
	if exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/tasks/main/1").Run() == nil {
		t.Fatal("removed create left a branch")
	}
}

func TestWorktreeListCLISagaEntries(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "list cli", "wt.list")
	base := gitRevParseMain(t, dir)
	wantPath := filepath.Join(dir, ".worktrees", "main-1")

	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.list")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	var updatedAt string
	provisionScan(t, dir, `SELECT updated_at FROM worktree_sagas WHERE saga_id=1`, &updatedAt)

	// Unknown filesystem entries: a non-matching directory and a file,
	// bytewise sorted.
	notes := filepath.Join(dir, ".worktrees", "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, ".worktrees", "zz.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdWorktree, "list", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	// Exact output contract: exactly entries and unknown, in order, with
	// exactly the §9.4 entry fields and bytewise-sorted unknown objects
	// containing exactly path (SPEC §9.4).
	want := fmt.Sprintf("{\n  \"entries\": [\n    {\n      \"sagaId\": 1,\n      \"runId\": %d,\n      \"tag\": \"main\",\n      \"task\": \"1\",\n      \"operation\": \"provision\",\n      \"status\": \"bound\",\n      \"active\": true,\n      \"ownerBoundary\": %q,\n      \"path\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": %q,\n      \"force\": false,\n      \"updatedAt\": %q\n    }\n  ],\n  \"unknown\": [\n    {\n      \"path\": %q\n    },\n    {\n      \"path\": %q\n    }\n  ]\n}\n",
		runID, dir, wantPath, base, updatedAt, notes, stray)
	if stdout != want {
		t.Fatalf("list JSON =\n%s\nwant\n%s", stdout, want)
	}

	// Human form renders the same projection without secrets.
	code, stdout, stderr = runCmd(cmdWorktree, "list")
	if code != 0 {
		t.Fatalf("list human exit %d, stderr: %s", code, stderr)
	}
	wantLine := fmt.Sprintf("sagaId=1 runId=%d tag=main task=1 operation=provision status=bound active=true ownerBoundary=%s path=%s branch=tasks/main/1 baseCommit=%s force=false updatedAt=%s\n",
		runID, dir, wantPath, base, updatedAt)
	if !strings.Contains(stdout, wantLine) {
		t.Fatalf("list human output = %q, want line %q", stdout, wantLine)
	}
	if !strings.Contains(stdout, "unknown path="+notes+"\n") || !strings.Contains(stdout, "unknown path="+stray+"\n") {
		t.Fatalf("list human unknown entries = %q", stdout)
	}
}

// TestWorktreeListPruneCLISymlinkUnknown pins the unknown-entry membership
// the two read surfaces share: an unrecorded symlink under the owner
// boundary's .worktrees root is unknown even when it points at a
// saga-recorded target — no saga row names the link's own path — so list
// and prune both report it and neither resolves through the link (SPEC
// §9.4).
func TestWorktreeListPruneCLISymlinkUnknown(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "symlink unknown", "wt.symlink")
	base := gitRevParseMain(t, dir)
	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.symlink")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	target := filepath.Join(dir, ".worktrees", "main-1")
	link := filepath.Join(dir, ".worktrees", "main-1-alias")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	var updatedAt string
	provisionScan(t, dir, `SELECT updated_at FROM worktree_sagas WHERE saga_id=1`, &updatedAt)

	// List: the recorded workspace is entry-known; the alias pointing at
	// it is the only unknown entry.
	code, stdout, stderr := runCmd(cmdWorktree, "list", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	want := fmt.Sprintf("{\n  \"entries\": [\n    {\n      \"sagaId\": 1,\n      \"runId\": %d,\n      \"tag\": \"main\",\n      \"task\": \"1\",\n      \"operation\": \"provision\",\n      \"status\": \"bound\",\n      \"active\": true,\n      \"ownerBoundary\": %q,\n      \"path\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": %q,\n      \"force\": false,\n      \"updatedAt\": %q\n    }\n  ],\n  \"unknown\": [\n    {\n      \"path\": %q\n    }\n  ]\n}\n",
		runID, dir, target, base, updatedAt, link)
	if stdout != want {
		t.Fatalf("list JSON =\n%s\nwant\n%s", stdout, want)
	}
	code, stdout, stderr = runCmd(cmdWorktree, "list")
	if code != 0 {
		t.Fatalf("list human exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "unknown path="+link+"\n") ||
		strings.Contains(stdout, "unknown path="+target+"\n") {
		t.Fatalf("list human unknown entries = %q", stdout)
	}

	// Prune judges the same membership — the bound provision is not
	// eligible — and never touches the alias or its target.
	code, stdout, stderr = runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	want = fmt.Sprintf("{\n  \"results\": [],\n  \"brokerRequests\": [],\n  \"unknown\": [\n    {\n      \"path\": %q\n    }\n  ]\n}\n", link)
	if stdout != want {
		t.Fatalf("prune JSON =\n%s\nwant\n%s", stdout, want)
	}
	code, stdout, stderr = runCmd(cmdWorktree, "prune")
	if code != 0 {
		t.Fatalf("prune human exit %d, stderr: %s", code, stderr)
	}
	if stdout != "unknown path="+link+"\npruned: 0 sagas, 0 broker handoffs\n" {
		t.Fatalf("prune human output = %q", stdout)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("prune touched the recorded workspace: %v", err)
	}
	if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("prune touched the unknown symlink: %v %v", info, err)
	}
}

// TestWorktreeListPruneCLIRecordedSymlinkSiblingUnknown pins the row side of
// the unknown-entry membership the two read surfaces share: a saga-recorded
// entry that itself became a symlink to an unrecorded sibling never marks
// that sibling known — only a row naming the sibling's own path could — so
// the sibling is reported unknown, while a row recorded through a symlinked
// boundary component still resolves to the canonical root the scan walks and
// stays known (SPEC §9.4).
func TestWorktreeListPruneCLIRecordedSymlinkSiblingUnknown(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "recorded symlink", "wt.sibling")
	base := gitRevParseMain(t, dir)
	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.sibling")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	var updatedAt string
	provisionScan(t, dir, `SELECT updated_at FROM worktree_sagas WHERE saga_id=1`, &updatedAt)

	// The recorded entry is replaced by a symlink to an unrecorded sibling:
	// the workspace directory moves aside and the recorded name becomes a
	// link pointing at it.
	target := filepath.Join(dir, ".worktrees", "main-1")
	sibling := filepath.Join(dir, ".worktrees", "main-2")
	if err := os.Rename(target, sibling); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(sibling, target); err != nil {
		t.Fatal(err)
	}

	// One terminal legacy row recorded through a symlinked boundary
	// component: a real directory exists under the canonical root, but the
	// row names it through the alias spelling (the §9.4 conversion shape).
	alias := filepath.Join(filepath.Dir(dir), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	bound := filepath.Join(dir, ".worktrees", "main-3")
	if err := os.MkdirAll(bound, 0o755); err != nil {
		t.Fatal(err)
	}
	provisionExec(t, dir, `INSERT INTO worktree_sagas(
		run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
		request_id,recovery_generation,owner_boundary,registration_root,target_path,
		branch,base_commit,force,created_at,updated_at,extra)
		VALUES (NULL,NULL,'main','1','remove','removed',0,910000000000000002,'wt.sibling.alias',0,?,NULL,?,'tasks/main/1',NULL,0,'2026-08-29T00:00:00Z','2026-08-29T00:00:00Z',NULL)`,
		dir, filepath.Join(alias, ".worktrees", "main-3"))

	// List: the sibling the replaced entry points at is the only unknown
	// entry; the recorded main-1 stays known under its own name and the
	// alias-recorded row keeps main-3 known against the canonical root.
	code, stdout, stderr := runCmd(cmdWorktree, "list", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	want := fmt.Sprintf("{\n  \"entries\": [\n    {\n      \"sagaId\": 1,\n      \"runId\": %d,\n      \"tag\": \"main\",\n      \"task\": \"1\",\n      \"operation\": \"provision\",\n      \"status\": \"bound\",\n      \"active\": true,\n      \"ownerBoundary\": %q,\n      \"path\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": %q,\n      \"force\": false,\n      \"updatedAt\": %q\n    },\n    {\n      \"sagaId\": 2,\n      \"runId\": null,\n      \"tag\": \"main\",\n      \"task\": \"1\",\n      \"operation\": \"remove\",\n      \"status\": \"removed\",\n      \"active\": false,\n      \"ownerBoundary\": %q,\n      \"path\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": null,\n      \"force\": false,\n      \"updatedAt\": \"2026-08-29T00:00:00Z\"\n    }\n  ],\n  \"unknown\": [\n    {\n      \"path\": %q\n    }\n  ]\n}\n",
		runID, dir, target, base, updatedAt, dir, filepath.Join(alias, ".worktrees", "main-3"), sibling)
	if stdout != want {
		t.Fatalf("list JSON =\n%s\nwant\n%s", stdout, want)
	}

	// Prune judges the same membership: both rows are terminal, the
	// sibling is the only unknown entry, and nothing is touched.
	code, stdout, stderr = runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	want = fmt.Sprintf("{\n  \"results\": [],\n  \"brokerRequests\": [],\n  \"unknown\": [\n    {\n      \"path\": %q\n    }\n  ]\n}\n", sibling)
	if stdout != want {
		t.Fatalf("prune JSON =\n%s\nwant\n%s", stdout, want)
	}
	code, stdout, stderr = runCmd(cmdWorktree, "prune")
	if code != 0 {
		t.Fatalf("prune human exit %d, stderr: %s", code, stderr)
	}
	if stdout != "unknown path="+sibling+"\npruned: 0 sagas, 0 broker handoffs\n" {
		t.Fatalf("prune human output = %q", stdout)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Fatalf("prune touched the unknown sibling: %v", err)
	}
	if info, err := os.Lstat(target); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("prune changed the recorded symlink: %v %v", info, err)
	}
	if _, err := os.Stat(bound); err != nil {
		t.Fatalf("prune touched the alias-recorded workspace: %v", err)
	}
}

func TestWorktreeCLIRefusals(t *testing.T) {
	dir := chdirWorktreeRepo(t)

	// No database: exactly one diagnostic line, stdout empty.
	code, stdout, stderr := runCmd(cmdWorktree, "list")
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: project is not initialized; run noise tasks setup\n" {
		t.Fatalf("no-store exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	seedWorktreeStore(t, dir, "t")

	// --file is never accepted.
	code, _, stderr = runCmd(cmdWorktree, "list", "--file", "x.json")
	if code != 2 || !strings.Contains(stderr, "--file is not accepted for worktree list") {
		t.Fatalf("--file exit %d, stderr %q", code, stderr)
	}

	// The saga table spans all tags; --tag selects nothing.
	code, _, stderr = runCmd(cmdWorktree, "list", "--tag", "main")
	if code != 2 || !strings.Contains(stderr, "--tag is not accepted for worktree list") {
		t.Fatalf("--tag exit %d, stderr %q", code, stderr)
	}

	// Positional arguments are rejected.
	code, _, stderr = runCmd(cmdWorktree, "list", "1")
	if code != 2 || !strings.Contains(stderr, "unexpected argument") {
		t.Fatalf("argument exit %d, stderr %q", code, stderr)
	}
}

func TestWorktreeUnknownSubcommand(t *testing.T) {
	code, _, stderr := runCmd(cmdWorktree)
	if code != 2 || !strings.Contains(stderr, "usage: noise-tasks worktree <provision|list|remove|recover|prune>") {
		t.Fatalf("no-subcommand exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runCmd(cmdWorktree, "frobnicate")
	if code != 2 || !strings.Contains(stderr, `unknown subcommand "frobnicate"`) {
		t.Fatalf("unknown-subcommand exit %d, stderr %q", code, stderr)
	}
}
