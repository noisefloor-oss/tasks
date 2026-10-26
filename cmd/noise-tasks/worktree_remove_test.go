package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// removalCLIFixture seeds one task, a live claim/run, a committed bound
// provision saga, and the exact on-disk worktree a provision effect would
// have created. It returns the run capabilities for `worktree remove`.
type removalCLIRun struct {
	runID        int64
	owner        string
	claimToken   int64
	attempt      int64
	attemptToken int64
	path         string
	branch       string
}

func seedRemovalCLIRun(t *testing.T, dir string) removalCLIRun {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	claim, _, _, err := db.AcquireClaim("main", "1", "agent-a", 3600, "claim.remove.cli")
	if err != nil {
		t.Fatal(err)
	}
	run, replayed, err := db.StartRun("main", "1", "agent-a", claim.Token, "run.remove.cli", 3600)
	if err != nil || replayed {
		t.Fatalf("start run = %+v replayed=%v err=%v", run, replayed, err)
	}
	boundary, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	common, ok := backlog.GitCommonDir(boundary)
	if !ok {
		t.Fatal("fixture boundary has no Git common directory")
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	fixture := removalCLIRun{
		runID: run.RunID, owner: "agent-a", claimToken: claim.Token,
		attempt: run.Attempt, attemptToken: run.AttemptToken,
		path:   canonTestPath(t, dir, "main-1"),
		branch: "tasks/main/1",
	}
	provision := store.WorktreeProvisionOptions{
		RunID: run.RunID, Owner: fixture.owner, ClaimToken: claim.Token,
		Attempt: run.Attempt, AttemptToken: run.AttemptToken, RequestID: "wt.provision.remove.cli",
		OwnerBoundary:    boundary,
		RegistrationRoot: filepath.Join(common, "worktrees"),
		TargetPath:       fixture.path,
		Branch:           fixture.branch, BaseCommit: strings.TrimSpace(string(out)),
	}
	plan, err := db.PlanWorktreeProvision(provision)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd := exec.Command("git", "-C", dir, "worktree", "add", "-b", fixture.branch, fixture.path, provision.BaseCommit)
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	if _, err := db.FinalizeWorktreeProvision(plan, store.WorktreeProvisionBound); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (r removalCLIRun) args(requestID string, extra ...string) []string {
	args := []string{"remove", strconv.FormatInt(r.runID, 10),
		"--owner", r.owner,
		"--claim-token", strconv.FormatInt(r.claimToken, 10),
		"--attempt", strconv.FormatInt(r.attempt, 10),
		"--attempt-token", strconv.FormatInt(r.attemptToken, 10),
		"--request-id", requestID}
	return append(args, extra...)
}

func worktreeBranchPresent(t *testing.T, dir, branch string) bool {
	t.Helper()
	return exec.Command("git", "-C", dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func TestWorktreeRemoveCLIRemovedHumanAndReplay(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)

	code, stdout, stderr := runCmd(cmdWorktree, fixture.args("wt.remove.cli")...)
	if code != 0 {
		t.Fatalf("remove exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{"operation=remove", "status=removed", "path=" + fixture.path,
		"branch=" + fixture.branch, "sagaToken=", "recoveryGeneration=0", "recoverRequestId=null"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("removed human output missing %q: %q", want, stdout)
		}
	}
	if _, err := os.Stat(fixture.path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	// Branch retention: removal never deletes the derived branch.
	if !worktreeBranchPresent(t, dir, fixture.branch) {
		t.Fatal("removal deleted the retained branch")
	}

	// Exact request-id replay returns the committed receipt with no effect.
	code, replay, stderr := runCmd(cmdWorktree, fixture.args("wt.remove.cli")...)
	if code != 0 || replay != stdout {
		t.Fatalf("replay exit %d stdout %q (first %q), stderr: %s", code, replay, stdout, stderr)
	}
}

func TestWorktreeRemoveCLIRemovedJSON(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)

	code, stdout, stderr := runCmd(cmdWorktree, fixture.args("wt.remove.cli.json", "--json")...)
	if code != 0 {
		t.Fatalf("remove --json exit %d, stderr: %s", code, stderr)
	}
	var result store.WorktreeMutationResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("remove --json output not JSON: %v\n%s", err, stdout)
	}
	if result.Operation != "remove" || result.Status != "removed" || result.SagaID <= 0 ||
		result.SagaToken <= 0 || result.RunID == nil || *result.RunID != fixture.runID ||
		result.Path != fixture.path || result.Branch != fixture.branch || result.BaseCommit == nil ||
		result.RecoveryGeneration != 0 || result.RecoverRequestID != nil || result.BrokerRequest != nil {
		t.Fatalf("removed JSON = %+v", result)
	}
	if _, err := os.Stat(fixture.path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	if !worktreeBranchPresent(t, dir, fixture.branch) {
		t.Fatal("removal deleted the retained branch")
	}
}

func TestWorktreeRemoveCLIUsageAndConflicts(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t", "u")
	fixture := seedRemovalCLIRun(t, dir)

	// Missing positional run and missing capabilities are usage errors.
	code, stdout, stderr := runCmd(cmdWorktree, "remove")
	if code != 2 || !strings.Contains(stderr, "usage: noise-tasks worktree remove <run>") {
		t.Fatalf("no-run exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runCmd(cmdWorktree, "remove", strconv.FormatInt(fixture.runID, 10))
	if code != 2 || !strings.Contains(stderr, "noise-tasks worktree remove: --owner is required") {
		t.Fatalf("missing-owner exit %d, stderr %q", code, stderr)
	}
	code, _, stderr = runCmd(cmdWorktree, "remove", "abc",
		"--owner", "agent-a", "--claim-token", "1", "--attempt", "1", "--attempt-token", "1",
		"--request-id", "wt.remove.bad-run")
	if code != 2 || !strings.Contains(stderr, "noise-tasks worktree remove: run id must be a positive integer") {
		t.Fatalf("bad-run exit %d, stderr %q", code, stderr)
	}

	// --file is never accepted, with the family-pinned refusal.
	code, _, stderr = runCmd(cmdWorktree, "remove", "--file", "x.json", strconv.FormatInt(fixture.runID, 10))
	if code != 2 || !strings.Contains(stderr, "--file is not accepted for worktree remove") {
		t.Fatalf("--file exit %d, stderr %q", code, stderr)
	}

	// Reserved request-id prefixes refuse before opening the store.
	for _, prefix := range []string{"auto-", "recover-"} {
		code, stdout, stderr := runCmd(cmdWorktree, fixture.args(prefix+"reserved")...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, "--request-id uses a reserved prefix") {
			t.Fatalf("reserved %q exit %d stdout %q stderr %q", prefix, code, stdout, stderr)
		}
	}

	// An unknown run is a typed exit-3 run conflict.
	args := fixture.args("wt.remove.unknown-run")
	args[1] = "99"
	code, stdout, _ = runCmd(cmdWorktree, args...)
	if code != 3 || stdout != "" {
		t.Fatalf("unknown-run exit %d stdout %q", code, stdout)
	}

	// A live run whose task has no active worktree is a typed exit-3 conflict.
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	claim, _, _, err := db.AcquireClaim("main", "2", "agent-a", 3600, "claim.remove.cli.absent")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.StartRun("main", "2", "agent-a", claim.Token, "run.remove.cli.absent", 3600)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	absent := removalCLIRun{runID: run.RunID, owner: "agent-a", claimToken: claim.Token,
		attempt: run.Attempt, attemptToken: run.AttemptToken}
	code, _, stderr = runCmd(cmdWorktree, absent.args("wt.remove.absent")...)
	if code != 3 || !strings.Contains(stderr, "task 2 has no active worktree") {
		t.Fatalf("absent exit %d, stderr %q", code, stderr)
	}

	// A request-id replay with a changed force bit is the exit-3 collision.
	code, _, stderr = runCmd(cmdWorktree, fixture.args("wt.remove.collision")...)
	if code != 0 {
		t.Fatalf("first remove exit %d, stderr: %s", code, stderr)
	}
	code, stdout, stderr = runCmd(cmdWorktree, fixture.args("wt.remove.collision", "--force")...)
	if code != 3 || stdout != "" {
		t.Fatalf("force collision exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestWorktreeRemoveCLIDirtyRefusalAndForce(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)
	if err := os.WriteFile(filepath.Join(fixture.path, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Trusted preflight refuses the dirty worktree before any saga exists.
	code, stdout, stderr := runCmd(cmdWorktree, fixture.args("wt.remove.dirty")...)
	if code != 3 || stdout != "" ||
		!strings.Contains(stderr, "worktree has uncommitted changes: "+fixture.path) {
		t.Fatalf("dirty exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := db.FindReceipt("worktree", "wt.remove.dirty"); err != nil || receipt != nil {
		t.Fatalf("dirty refusal receipt = %v, %v", receipt, err)
	}
	db.Close()

	// --force is part of the immutable identity, so it needs a fresh request id.
	code, stdout, stderr = runCmd(cmdWorktree, fixture.args("wt.remove.dirty.force", "--force")...)
	if code != 0 || !strings.Contains(stdout, "status=removed") {
		t.Fatalf("force remove exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(fixture.path); !os.IsNotExist(err) {
		t.Fatalf("dirty worktree directory still exists: %v", err)
	}
	if !worktreeBranchPresent(t, dir, fixture.branch) {
		t.Fatal("force removal deleted the retained branch")
	}
}

// TestWriteWorktreeRemoveResultFailedContract pins the §9.4 failed row: every
// committed failed outcome — fresh or replay — is exit 1 with empty stdout in
// both modes, even when the effect cause is a typed conflict (a worktree that
// became dirty between preflight and effect); the cause is a bounded
// stderr-only diagnostic and never a typed exit-3 conflict. The store-level
// dirty-between-preflight-and-effect path that produces this result is pinned
// by TestPerformWorktreeRemovalEffectDirtyRefusalAndForce.
func TestWriteWorktreeRemoveResultFailedContract(t *testing.T) {
	runID := int64(7)
	base := strings.Repeat("a", 40)
	result := &store.WorktreeMutationResult{SagaID: 3, Operation: "remove",
		Status: "failed", RunID: &runID, OwnerBoundary: "/repo",
		Path: "/repo/.worktrees/main-1", Branch: "tasks/main/1", BaseCommit: &base,
		SagaToken: 44}
	dirty := &store.WorktreeConflict{Kind: "dirty", Tag: "main", ID: "1",
		Path: "/repo/.worktrees/main-1", Branch: "tasks/main/1"}
	for _, jsonOut := range []bool{false, true} {
		var stdout, stderr strings.Builder
		code := writeWorktreeRemoveResult(&stdout, &stderr, jsonOut, result, dirty)
		if code != 1 || stdout.String() != "" {
			t.Fatalf("failed jsonOut=%v = exit %d stdout %q", jsonOut, code, stdout.String())
		}
		if stderr.String() != "noise-tasks: worktree has uncommitted changes: /repo/.worktrees/main-1\n" {
			t.Fatalf("failed jsonOut=%v stderr = %q", jsonOut, stderr.String())
		}
	}
	// A nil cause still produces the exit-1 empty-stdout contract.
	var stdout, stderr strings.Builder
	if code := writeWorktreeRemoveResult(&stdout, &stderr, false, result, nil); code != 1 ||
		stdout.String() != "" || stderr.String() != "noise-tasks: worktree removal failed\n" {
		t.Fatalf("failed nil cause = exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

// TestWriteWorktreeRemoveResultCleanupRequiredContract pins the §9.4
// five-column matrix row for cleanup-required at the one surface both fresh
// finalization and exact receipt replay pass through: exit-0 mutation result
// under --json with the bounded observation cause on stderr; exit 1, empty
// stdout, exactly the pinned line on stderr for human invocation.
func TestWriteWorktreeRemoveResultCleanupRequiredContract(t *testing.T) {
	recoverID := "recover-" + strings.Repeat("c", 64)
	runID := int64(7)
	base := strings.Repeat("a", 40)
	result := &store.WorktreeMutationResult{SagaID: 3, Operation: "remove",
		Status: "cleanup-required", RunID: &runID, OwnerBoundary: "/repo",
		Path: "/repo/.worktrees/main-1", Branch: "tasks/main/1", BaseCommit: &base,
		SagaToken: 44, RecoveryGeneration: 0, RecoverRequestID: &recoverID}

	var stdout, stderr strings.Builder
	code := writeWorktreeRemoveResult(&stdout, &stderr, true, result, errors.New("unproved"))
	if code != 0 || stderr.String() != "noise-tasks: unproved\n" {
		t.Fatalf("cleanup-required --json = exit %d stderr %q", code, stderr.String())
	}
	var decoded store.WorktreeMutationResult
	if err := json.Unmarshal([]byte(stdout.String()), &decoded); err != nil {
		t.Fatalf("cleanup-required --json output not JSON: %v\n%s", err, stdout.String())
	}
	if decoded.Status != "cleanup-required" || decoded.SagaToken != 44 ||
		decoded.RecoverRequestID == nil || *decoded.RecoverRequestID != recoverID ||
		decoded.BrokerRequest != nil {
		t.Fatalf("cleanup-required --json result = %+v", decoded)
	}

	stdout.Reset()
	stderr.Reset()
	code = writeWorktreeRemoveResult(&stdout, &stderr, false, result, errors.New("unproved"))
	if code != 1 || stdout.String() != "" ||
		stderr.String() != "noise-tasks: worktree cleanup is required\n" {
		t.Fatalf("cleanup-required human = exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

// TestWorktreeRemoveCLIConfinement pins §9.5 through the CLI: the first direct
// attempt records needs-broker without invoking any broker; human output is
// the pinned exit-1 line with empty stdout, while --json returns the exit-0
// saga mutation result with the non-secret broker mint bindings.
func TestWorktreeRemoveCLIConfinement(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)
	confineCLIRegistration(t, dir)
	stageCLIBroker(t, "") // valid root, no broker entry

	code, stdout, stderr := runCmd(cmdWorktree, fixture.args("wt.remove.confined")...)
	if code != 1 || stdout != "" {
		t.Fatalf("confined human exit %d stdout %q", code, stdout)
	}
	if want := "noise-tasks: worktree registration is confined and no broker is available\n"; stderr != want {
		t.Fatalf("confined human stderr = %q, want %q", stderr, want)
	}
	// The staged target was restored and the branch retained.
	if !worktreeBranchPresent(t, dir, fixture.branch) {
		t.Fatal("confined removal deleted the branch")
	}
	if _, err := os.Stat(filepath.Join(fixture.path, ".git")); err != nil {
		t.Fatalf("staged target was not restored: %v", err)
	}

	// --json replay of the committed handoff is exit 0 with the saga mutation
	// result on stdout; the diagnostic stays on stderr.
	code, stdout, stderr = runCmd(cmdWorktree, fixture.args("wt.remove.confined", "--json")...)
	if code != 0 {
		t.Fatalf("confined --json exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "worktree registration is confined and no broker is available") {
		t.Fatalf("confined --json stderr = %q", stderr)
	}
	var result store.WorktreeMutationResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatalf("confined --json output not JSON: %v\n%s", err, stdout)
	}
	if result.Status != "needs-broker" || result.SagaToken <= 0 || result.RecoverRequestID == nil ||
		result.BrokerRequest == nil || result.BrokerRequest.Verb != "remove" ||
		result.BrokerRequest.RegistrationRoot == "" || result.BrokerRequest.TargetPath != fixture.path ||
		result.BrokerRequest.Branch != fixture.branch || result.BrokerRequest.Force ||
		result.BrokerRequest.RecoverRequestID != *result.RecoverRequestID {
		t.Fatalf("confined --json result = %+v", result)
	}
	encoded, _ := json.Marshal(result.BrokerRequest)
	// Numeric token redaction with distinguishable fencing values is pinned by
	// the store-level tests; here the broker request must not name capabilities.
	for _, secret := range []string{"claimToken", "attemptToken", "sagaToken"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("broker request leaked %q: %s", secret, encoded)
		}
	}
}
