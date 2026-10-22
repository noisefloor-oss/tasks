package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// §9.4 lifecycle qualification: complete worktree lifecycles driven through
// the live CLI surface with real Git worktrees, pinning the cross-slice
// invariants no single-slice test can observe — attempt retry reusing the
// run's workspace (§9.4 "Retrying an attempt in the same run reuses the
// workspace"), every terminal cleanup authority scheduling its saga, and a
// later run reprovisioning only after the operator releases the retained
// branch — with `verify` green across every stage.

// lifecycleRunJSON is the run start/retry mutation result the lifecycle
// walks decode for the next attempt's capabilities.
type lifecycleRunJSON struct {
	RunID        int64 `json:"runId"`
	Attempt      int64 `json:"attempt"`
	AttemptToken int64 `json:"attemptToken"`
}

// lifecyclePruneJSON is the §9.4 prune result surface the walks decode.
type lifecyclePruneJSON struct {
	Results []struct {
		SagaID int64  `json:"sagaId"`
		Status string `json:"status"`
	} `json:"results"`
	BrokerRequests []json.RawMessage `json:"brokerRequests"`
	Unknown        []struct {
		Path string `json:"path"`
	} `json:"unknown"`
}

// lifecycleFinishRun finishes the seeded run's current attempt and returns
// nothing; the outcome selects the run's terminal shape.
func lifecycleFinishRun(t *testing.T, runID, claimToken, attemptToken, attempt int64, outcome, requestID string) {
	t.Helper()
	code, _, stderr := runCmd(cmdRun, "finish", strconv.FormatInt(runID, 10),
		"--owner", "agent-a", "--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10), "--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--outcome", outcome, "--request-id", requestID)
	if code != 0 {
		t.Fatalf("run finish %s exit %d, stderr: %s", outcome, code, stderr)
	}
}

// lifecycleSagaCount counts saga rows for one shape of question.
func lifecycleSagaCount(t *testing.T, dir, where string) int64 {
	t.Helper()
	var count int64
	provisionScan(t, dir, `SELECT COUNT(*) FROM worktree_sagas `+where, &count)
	return count
}

// lifecycleScheduledCleanup reads the one scheduled cleanup row's identity.
func lifecycleScheduledCleanup(t *testing.T, dir string) (request string, sagaID, runID int64) {
	t.Helper()
	var run sql.NullInt64
	lifecycleQueryRow(t, dir, `SELECT request_id,saga_id,run_id FROM worktree_sagas WHERE operation='remove'`,
		&request, &sagaID, &run)
	if run.Valid {
		runID = run.Int64
	}
	return request, sagaID, runID
}

// lifecycleQueryRow answers one direct-SQL question with multiple columns.
func lifecycleQueryRow(t *testing.T, dir, query string, dest ...any) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := sqlDB.QueryRow(query).Scan(dest...); err != nil {
		t.Fatal(err)
	}
}

// TestWorktreeLifecycleCLIAttemptRetryReusesWorkspace walks the §9.4
// retry lifecycle end to end: a worktree provisioned under attempt 1 stays
// bound to its run across a failed attempt and a retry, attempt 2 reuses the
// exact workspace, and only the run's success schedules its cleanup (SPEC
// §9.4; §9.6 "saga planning on one attempt followed by retry in the same
// run").
func TestWorktreeLifecycleCLIAttemptRetryReusesWorkspace(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "retry lifecycle", "wt.retry")
	path := canonTestPath(t, dir, "main-1")

	// Retry needs a terminal attempt: the live attempt 1 refuses.
	code, stdout, stderr := runCmd(cmdRun, "retry", strconv.FormatInt(runID, 10),
		"--owner", "agent-a", "--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10), "--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--request-id", "req.retry.early")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "run retry conflict") {
		t.Fatalf("early retry: exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Provision binds attempt 1's workspace to the run.
	code, _, stderr = runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.retry")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("provisioned workspace missing: %v", err)
	}
	if !gitWorktreeRegistered(t, dir, path) || !worktreeBranchPresent(t, dir, "tasks/main/1") {
		t.Fatal("provisioned registration or branch missing")
	}
	if code, _, stderr := runCmd(verifyMain); code != 0 {
		t.Fatalf("verify after provision: exit %d, stderr: %s", code, stderr)
	}

	// A failed attempt leaves the run retryable: nothing schedules cleanup,
	// and the bound workspace survives for the retry (§9.4).
	lifecycleFinishRun(t, runID, claimToken, attemptToken, attempt, "failed", "req.retry.fail1")
	if n := lifecycleSagaCount(t, dir, `WHERE operation='remove'`); n != 0 {
		t.Fatalf("failed attempt scheduled %d removals", n)
	}
	if n := lifecycleSagaCount(t, dir, ``); n != 1 {
		t.Fatalf("failed attempt changed the saga table; rows = %d", n)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("workspace removed after failed attempt: %v", err)
	}

	// The retry starts attempt 2 in the same run; the workspace, its
	// registration, its branch, and its single bound saga are all reused.
	var retried lifecycleRunJSON
	code, stdout, stderr = runCmd(cmdRun, "retry", strconv.FormatInt(runID, 10),
		"--owner", "agent-a", "--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10), "--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--request-id", "req.retry", "--json")
	if code != 0 {
		t.Fatalf("run retry exit %d, stderr: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &retried); err != nil {
		t.Fatalf("retry JSON: %v\n%s", err, stdout)
	}
	if retried.Attempt != attempt+1 || retried.AttemptToken == attemptToken || retried.RunID != runID {
		t.Fatalf("retry = %+v (from attempt %d token %d run %d)", retried, attempt, attemptToken, runID)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("workspace removed by retry: %v", err)
	}
	if !gitWorktreeRegistered(t, dir, path) || !worktreeBranchPresent(t, dir, "tasks/main/1") {
		t.Fatal("retry lost the registration or branch")
	}
	if n := lifecycleSagaCount(t, dir, ``); n != 1 {
		t.Fatalf("retry reallocated the saga; rows = %d", n)
	}
	var boundStatus string
	var boundActive int
	lifecycleQueryRow(t, dir, `SELECT status,binding_active FROM worktree_sagas`, &boundStatus, &boundActive)
	if boundStatus != "bound" || boundActive != 1 {
		t.Fatalf("saga after retry = %q/%d", boundStatus, boundActive)
	}

	// The stale attempt-1 capability no longer authorizes even disclosure.
	code, stdout, stderr = runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.retry")...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "run provision conflict") {
		t.Fatalf("stale-attempt replay: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The current controller — same owner, current attempt — still replays
	// the completed original request and gets its saga token (§9.4: the
	// current attempt may differ from the one that planned the saga).
	var replayed provisionJSON
	code, stdout, stderr = runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, retried.AttemptToken, retried.Attempt, "wt.retry"), "--json")...)
	if code != 0 {
		t.Fatalf("current-attempt replay exit %d, stderr: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &replayed); err != nil {
		t.Fatalf("replay JSON: %v\n%s", err, stdout)
	}
	if replayed.Status != "bound" || replayed.SagaToken <= 0 || replayed.RunID == nil || *replayed.RunID != runID {
		t.Fatalf("current-attempt replay = %+v", replayed)
	}
	// A distinct request cannot allocate a second workspace for the run.
	code, stdout, stderr = runCmd(cmdWorktree, provisionArgs(runID, claimToken, retried.AttemptToken, retried.Attempt, "wt.retry.second")...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "worktree path exists: "+path) {
		t.Fatalf("second workspace request: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if n := lifecycleSagaCount(t, dir, ``); n != 1 {
		t.Fatalf("second workspace request allocated a saga; rows = %d", n)
	}

	// Attempt 2 succeeds: the run's terminalization — not the failed
	// attempt — schedules exactly one unforced run-caused cleanup.
	lifecycleFinishRun(t, runID, claimToken, retried.AttemptToken, retried.Attempt, "succeeded", "req.retry.ok")
	request, cleanupSaga, cleanupRun := lifecycleScheduledCleanup(t, dir)
	if !strings.HasPrefix(request, "auto-") || cleanupRun != runID || cleanupSaga <= 0 {
		t.Fatalf("scheduled cleanup = %q saga %d run %d", request, cleanupSaga, cleanupRun)
	}
	var sourceActive int
	provisionScan(t, dir, `SELECT binding_active FROM worktree_sagas WHERE operation='provision'`, &sourceActive)
	if sourceActive != 1 {
		t.Fatal("scheduled cleanup deactivated its source early")
	}
	if code, _, stderr := runCmd(verifyMain); code != 0 {
		t.Fatalf("verify after finish: exit %d, stderr: %s", code, stderr)
	}

	// Prune drives the cleanup to removed: workspace and registration gone,
	// branch retained, source deactivated, store still verified.
	var pruned lifecyclePruneJSON
	code, stdout, stderr = runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &pruned); err != nil {
		t.Fatalf("prune JSON: %v\n%s", err, stdout)
	}
	if len(pruned.Results) != 1 || pruned.Results[0].SagaID != cleanupSaga || pruned.Results[0].Status != "removed" ||
		len(pruned.BrokerRequests) != 0 || len(pruned.Unknown) != 0 {
		t.Fatalf("prune results = %+v broker=%d unknown=%d", pruned.Results, len(pruned.BrokerRequests), len(pruned.Unknown))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace survived prune: %v", err)
	}
	if gitWorktreeRegistered(t, dir, path) {
		t.Fatal("registration survived prune")
	}
	if !worktreeBranchPresent(t, dir, "tasks/main/1") {
		t.Fatal("prune deleted the retained branch")
	}
	provisionScan(t, dir, `SELECT binding_active FROM worktree_sagas WHERE operation='provision'`, &sourceActive)
	if sourceActive != 0 {
		t.Fatal("removed cleanup left its source active")
	}
	if code, _, stderr := runCmd(verifyMain); code != 0 {
		t.Fatalf("verify after prune: exit %d, stderr: %s", code, stderr)
	}
}

// TestWorktreeLifecycleCLITerminalCleanupAuthorities walks each §9.4 cleanup
// authority that establishes cleanup in its own transaction — run cancel,
// force release, and direct task done — from one bound provision through
// prune to a verified store. Run-terminal parents reference the source run;
// the direct task-done row is runless (SPEC §9.4).
func TestWorktreeLifecycleCLITerminalCleanupAuthorities(t *testing.T) {
	cases := []struct {
		name      string
		parent    func(t *testing.T, dir string, runID, claimToken, attemptToken, attempt int64)
		runCaused bool
	}{
		{"run cancel", func(t *testing.T, dir string, runID, claimToken, attemptToken, attempt int64) {
			code, _, stderr := runCmd(cmdRun, "cancel", strconv.FormatInt(runID, 10),
				"--owner", "agent-a", "--claim-token", strconv.FormatInt(claimToken, 10),
				"--attempt", strconv.FormatInt(attempt, 10), "--attempt-token", strconv.FormatInt(attemptToken, 10),
				"--request-id", "req.cancel")
			if code != 0 {
				t.Fatalf("run cancel exit %d, stderr: %s", code, stderr)
			}
		}, true},
		{"force release", func(t *testing.T, dir string, runID, claimToken, attemptToken, attempt int64) {
			code, _, stderr := runCmd(cmdRelease, "--force", "--owner", "agent-a",
				"--token", strconv.FormatInt(claimToken, 10), "1")
			if code != 0 {
				t.Fatalf("force release exit %d, stderr: %s", code, stderr)
			}
		}, true},
		{"task done", func(t *testing.T, dir string, runID, claimToken, attemptToken, attempt int64) {
			db, err := store.Open(projectStorePath(dir))
			if err != nil {
				t.Fatal(err)
			}
			task, err := db.GetTask("main", "1")
			db.Close()
			if err != nil || task == nil {
				t.Fatalf("read task: %v", err)
			}
			code, _, stderr := runCmd(cmdSetStatus, "--owner", "agent-a",
				"--token", strconv.FormatInt(claimToken, 10),
				"--run", strconv.FormatInt(runID, 10),
				"--attempt", strconv.FormatInt(attempt, 10),
				"--attempt-token", strconv.FormatInt(attemptToken, 10),
				"--revision", strconv.Itoa(task.Revision), "1", "done")
			if code != 0 {
				t.Fatalf("set-status done exit %d, stderr: %s", code, stderr)
			}
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := chdirWorktreeRepo(t)
			path := canonTestPath(t, dir, "main-1")
			runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "authority "+tc.name, "wt.auth")
			code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.auth")...)
			if code != 0 {
				t.Fatalf("provision exit %d, stderr: %s", code, stderr)
			}

			tc.parent(t, dir, runID, claimToken, attemptToken, attempt)

			// Exactly one unforced automatic cleanup exists, caused by this
			// authority: run-referencing for terminal parents, runless for
			// direct task done.
			request, cleanupSaga, cleanupRun := lifecycleScheduledCleanup(t, dir)
			if !strings.HasPrefix(request, "auto-") || cleanupSaga <= 0 {
				t.Fatalf("scheduled cleanup = %q saga %d", request, cleanupSaga)
			}
			if tc.runCaused && cleanupRun != runID {
				t.Fatalf("run-caused cleanup references run %d, want %d", cleanupRun, runID)
			}
			if !tc.runCaused && cleanupRun != 0 {
				t.Fatalf("task-done cleanup references run %d, want runless", cleanupRun)
			}
			var force int
			provisionScan(t, dir, `SELECT force FROM worktree_sagas WHERE operation='remove'`, &force)
			if force != 0 {
				t.Fatal("automatic cleanup set force")
			}
			if code, _, stderr := runCmd(verifyMain); code != 0 {
				t.Fatalf("verify after %s: exit %d, stderr: %s", tc.name, code, stderr)
			}

			// Prune completes the lifecycle and the store still verifies.
			var pruned lifecyclePruneJSON
			code, stdout, stderr := runCmd(cmdWorktree, "prune", "--json")
			if code != 0 {
				t.Fatalf("prune exit %d, stderr: %s", code, stderr)
			}
			if err := json.Unmarshal([]byte(stdout), &pruned); err != nil {
				t.Fatalf("prune JSON: %v\n%s", err, stdout)
			}
			if len(pruned.Results) != 1 || pruned.Results[0].SagaID != cleanupSaga || pruned.Results[0].Status != "removed" {
				t.Fatalf("prune results = %+v", pruned.Results)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("workspace survived prune: %v", err)
			}
			if gitWorktreeRegistered(t, dir, path) {
				t.Fatal("registration survived prune")
			}
			if !worktreeBranchPresent(t, dir, "tasks/main/1") {
				t.Fatal("prune deleted the retained branch")
			}
			if code, _, stderr := runCmd(verifyMain); code != 0 {
				t.Fatalf("verify after prune: exit %d, stderr: %s", code, stderr)
			}
		})
	}
}

// TestWorktreeLifecycleCLIReprovisionAfterRetainedBranch walks §9.4's
// later-run rule end to end: after the first run's cleanup removed the
// workspace and retained its branch, a second run for the same task cannot
// adopt the old saga or workspace — reprovisioning refuses on the retained
// branch until the operator removes it explicitly, and only then a fresh
// saga binds the new run while the terminal history stays untouched (SPEC
// §9.4).
func TestWorktreeLifecycleCLIReprovisionAfterRetainedBranch(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	path := canonTestPath(t, dir, "main-1")
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "reprovision", "wt.repro")
	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.repro")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	lifecycleFinishRun(t, runID, claimToken, attemptToken, attempt, "succeeded", "req.repro.fin")
	if code, _, stderr := runCmd(cmdWorktree, "prune", "--json"); code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	// The terminal run leaves the claim releasable; a fresh controller takes
	// over and starts the second run.
	code, _, stderr = runCmd(cmdRelease, "--owner", "agent-a", "--token", strconv.FormatInt(claimToken, 10), "1")
	if code != 0 {
		t.Fatalf("release exit %d, stderr: %s", code, stderr)
	}
	secondRun, secondClaim, secondAttemptToken, secondAttempt := lifecycleSecondRun(t, dir, "agent-b", "wt.repro.two")
	if secondRun == runID {
		t.Fatalf("second run reused run %d", runID)
	}

	// The retained branch refuses the second run's provision without
	// allocating any saga.
	code, stdout, stderr := runCmd(cmdWorktree, lifecycleProvisionArgs("agent-b", secondRun, secondClaim, secondAttemptToken, secondAttempt, "wt.repro.second")...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "branch tasks/main/1 already exists") {
		t.Fatalf("reprovision over retained branch: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if n := lifecycleSagaCount(t, dir, ``); n != 2 {
		t.Fatalf("refused reprovision changed the saga table; rows = %d", n)
	}

	// The operator removes the retained branch; the same second run now
	// provisions a fresh saga and workspace at the family-owned root.
	gitCLIOutput(t, dir, "branch", "-D", "tasks/main/1")
	var provisioned provisionJSON
	code, stdout, stderr = runCmd(cmdWorktree, append(lifecycleProvisionArgs("agent-b", secondRun, secondClaim, secondAttemptToken, secondAttempt, "wt.repro.third"), "--json")...)
	if code != 0 {
		t.Fatalf("reprovision exit %d, stderr: %s", code, stderr)
	}
	if err := json.Unmarshal([]byte(stdout), &provisioned); err != nil {
		t.Fatalf("reprovision JSON: %v\n%s", err, stdout)
	}
	if provisioned.Status != "bound" || provisioned.RunID == nil || *provisioned.RunID != secondRun {
		t.Fatalf("reprovision = %+v", provisioned)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("reprovisioned workspace missing: %v", err)
	}
	if !gitWorktreeRegistered(t, dir, path) {
		t.Fatal("reprovisioned registration missing")
	}

	// The list projection reports the full history in saga order — the
	// first run's inactive provision, its removed cleanup, and the second
	// run's active binding — and the store still verifies.
	code, stdout, stderr = runCmd(cmdWorktree, "list", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var listed struct {
		Entries []struct {
			SagaID    int64  `json:"sagaId"`
			RunID     *int64 `json:"runId"`
			Operation string `json:"operation"`
			Status    string `json:"status"`
			Active    bool   `json:"active"`
		} `json:"entries"`
		Unknown []json.RawMessage `json:"unknown"`
	}
	if err := json.Unmarshal([]byte(stdout), &listed); err != nil {
		t.Fatalf("list JSON: %v\n%s", err, stdout)
	}
	if len(listed.Entries) != 3 || len(listed.Unknown) != 0 {
		t.Fatalf("list entries = %+v unknown = %d", listed.Entries, len(listed.Unknown))
	}
	want := []struct {
		operation string
		status    string
		active    bool
		run       int64
	}{
		{"provision", "bound", false, runID},
		{"remove", "removed", false, runID},
		{"provision", "bound", true, secondRun},
	}
	for i, e := range listed.Entries {
		w := want[i]
		runValue := int64(0)
		if e.RunID != nil {
			runValue = *e.RunID
		}
		if e.SagaID != int64(i+1) || e.Operation != w.operation || e.Status != w.status || e.Active != w.active || runValue != w.run {
			t.Fatalf("list entry %d = %+v, want %v", i, e, w)
		}
	}
	if code, _, stderr := runCmd(verifyMain); code != 0 {
		t.Fatalf("verify after reprovision: exit %d, stderr: %s", code, stderr)
	}
}

// lifecycleProvisionArgs builds the worktree provision argv for a given
// controller owner.
func lifecycleProvisionArgs(owner string, runID, claimToken, attemptToken, attempt int64, requestID string) []string {
	return []string{"provision", strconv.FormatInt(runID, 10), "--owner", owner,
		"--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10),
		"--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--request-id", requestID}
}

// lifecycleSecondRun hands a fresh controller the claim and run the
// reprovision walk needs, mirroring provisionCLISeed's seeding for a second
// owner after the first claim was released.
func lifecycleSecondRun(t *testing.T, dir, owner, requestID string) (runID, claimToken, attemptToken, attempt int64) {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	claim, _, _, err := db.AcquireClaim("main", "1", owner, 3600, "claim."+requestID)
	if err != nil {
		t.Fatal(err)
	}
	run, replayed, err := db.StartRun("main", "1", owner, claim.Token, "run."+requestID, 3600)
	if err != nil || replayed {
		t.Fatalf("second run = %+v replayed=%v err=%v", run, replayed, err)
	}
	return run.RunID, claim.Token, run.AttemptToken, run.Attempt
}
