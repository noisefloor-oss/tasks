package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// claimToken runs a plain JSON claim and returns the fencing token.
func claimToken(t *testing.T, args ...string) int64 {
	t.Helper()
	code, stdout, _ := runCmd(cmdClaim, append([]string{"--json"}, args...)...)
	if code != 0 {
		t.Fatalf("claim %v exit %d", args, code)
	}
	var out struct {
		Token int64 `json:"token"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.Token == 0 {
		t.Fatalf("claim %v output = %q (%v)", args, stdout, err)
	}
	return out.Token
}

// showClaim reports the task's claim state via show --json.
func showClaim(t *testing.T, id string) (claimed bool, revision int) {
	t.Helper()
	code, stdout, stderr := runCmd(cmdShow, "--json", id)
	if code != 0 {
		t.Fatalf("show %s exit %d, stderr: %s", id, code, stderr)
	}
	var out struct {
		Revision int `json:"revision"`
		Claim    *struct {
			Owner string `json:"owner"`
		} `json:"claim"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("show %s output not JSON: %v\n%s", id, err, stdout)
	}
	return out.Claim != nil, out.Revision
}

func TestClaimWorktreeRemovedCLI(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")

	// §9.4 v5 cutover: claim --worktree is a usage error with the pinned
	// guidance, and no claim is acquired.
	want := "noise-tasks claim: --worktree was removed by the v5 cutover; acquire a claim, start a run, and use worktree provision\n"
	code, stdout, stderr := runCmd(cmdClaim, "--owner", "agent-a", "--worktree", "1")
	if code != 2 || stdout != "" || stderr != want {
		t.Fatalf("claim --worktree exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if claimed, _ := showClaim(t, "1"); claimed {
		t.Fatal("removed claim --worktree still acquired a claim")
	}
	if _, err := os.Lstat(canonTestPath(t, dir, "main-1")); !os.IsNotExist(err) {
		t.Fatal("removed claim --worktree still created a worktree")
	}

	// --file keeps its own pinned refusal.
	code, _, stderr = runCmd(cmdClaim, "--owner", "a", "--worktree", "--file", "x.json", "1")
	if code != 2 || !strings.Contains(stderr, "--file is not accepted for claim") {
		t.Fatalf("claim --file exit %d, stderr %q", code, stderr)
	}

	// The removed surface refuses before any renew combination is judged.
	code, _, stderr = runCmd(cmdClaim, "--owner", "a", "--worktree", "--renew", "--token", "1", "1")
	if code != 2 || stderr != want {
		t.Fatalf("claim --worktree --renew exit %d, stderr %q", code, stderr)
	}
}

func TestKeepWorktreeRemovedCLI(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	token := claimToken(t, "--owner", "agent-a", "--request-id", "req-keep-1", "1")
	_, revision := showClaim(t, "1")

	wantRelease := "noise-tasks release: --keep-worktree was removed by the v5 cutover; acquire a claim, start a run, and use worktree provision\n"
	code, stdout, stderr := runCmd(cmdRelease, "--owner", "agent-a", "--token", strconv.FormatInt(token, 10), "--keep-worktree", "1")
	if code != 2 || stdout != "" || stderr != wantRelease {
		t.Fatalf("release --keep-worktree exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	wantStatus := "noise-tasks set-status: --keep-worktree was removed by the v5 cutover; acquire a claim, start a run, and use worktree provision\n"
	code, stdout, stderr = runCmd(cmdSetStatus, "--revision", strconv.Itoa(revision),
		"--owner", "agent-a", "--token", strconv.FormatInt(token, 10), "--keep-worktree", "1", "in-progress")
	if code != 2 || stdout != "" || stderr != wantStatus {
		t.Fatalf("set-status --keep-worktree exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A --file fixture invocation still hits the removed surface first:
	// the cutover guidance is the pinned diagnostic for the flag itself.
	code, _, stderr = runCmd(cmdSetStatus, "--file", "x.json", "--keep-worktree", "1", "done")
	if code != 2 || stderr != wantStatus {
		t.Fatalf("set-status --file --keep-worktree exit %d, stderr %q", code, stderr)
	}
}

// TestReleaseAndDoneDoNotFoldWorktreeCLI pins the §9.4 v5 semantics that
// replace the keep flags: release and set-status done never fold a worktree
// removal — the scheduled cleanup saga owns the worktree, and trusted prune
// completes it. Branch retention applies throughout.
func TestReleaseAndDoneDoNotFoldWorktreeCLI(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimTokenValue, attemptToken, attempt := provisionCLISeed(t, dir, "no fold", "wt.nofold")
	wt := filepath.Join(dir, ".worktrees", "main-1")

	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimTokenValue, attemptToken, attempt, "wt.nofold")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	code, _, stderr = runCmd(cmdRun, "finish", strconv.FormatInt(runID, 10),
		"--owner", "agent-a", "--claim-token", strconv.FormatInt(claimTokenValue, 10),
		"--attempt", strconv.FormatInt(attempt, 10), "--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--outcome", "succeeded", "--request-id", "finish.wt.nofold")
	if code != 0 {
		t.Fatalf("run finish exit %d, stderr: %s", code, stderr)
	}

	// set-status done succeeds and leaves the worktree directory in place:
	// the cleanup saga scheduled by the finished run owns the removal.
	_, revision := showClaim(t, "1")
	code, _, stderr = runCmd(cmdSetStatus, "--revision", strconv.Itoa(revision),
		"--owner", "agent-a", "--token", strconv.FormatInt(claimTokenValue, 10), "1", "done")
	if code != 0 {
		t.Fatalf("set-status done exit %d, stderr: %s", code, stderr)
	}
	if _, err := os.Lstat(wt); err != nil {
		t.Fatalf("set-status done folded a worktree removal: %v", err)
	}

	// Trusted prune completes the scheduled cleanup; the branch is retained.
	code, _, stderr = runCmd(cmdWorktree, "prune")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists after prune: %v", err)
	}
	if !worktreeBranchPresent(t, dir, "tasks/main/1") {
		t.Fatal("cleanup deleted the retained branch")
	}
}
