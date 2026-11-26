package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runHygiene(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cmdHygiene(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// decodeHygiene strictly decodes the §8.9 JSON report, worktree classes
// included.
func decodeHygiene(t *testing.T, stdout string) *hygieneReportOut {
	t.Helper()
	var got hygieneReportOut
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode hygiene JSON: %v\n%s", err, stdout)
	}
	return &got
}

func TestHygieneCommandEmptyAndUsage(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main", "hygiene empty")

	code, stdout, stderr := runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeHygiene(t, stdout)
	if got.SchemaVersion != 2 || got.Now == "" ||
		got.StaleRuns == nil || got.ExpiredClaims == nil || got.ClaimlessInProgress == nil ||
		got.RetryableRuns == nil || got.Sagas == nil ||
		len(got.StaleRuns) != 0 || len(got.ExpiredClaims) != 0 || len(got.ClaimlessInProgress) != 0 ||
		len(got.RetryableRuns) != 0 || len(got.Sagas) != 0 {
		t.Fatalf("hygiene = %+v", got)
	}

	code, stdout, stderr = runHygiene()
	if code != 0 || stderr != "" || stdout != "no lifecycle residue found\n" {
		t.Fatalf("human hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}

	for _, args := range [][]string{{"--file", "fixture.json", "--json"}, {"--tag", "main", "--json"}, {"--json", "extra"}} {
		code, stdout, _ := runHygiene(args...)
		if code != 2 || stdout != "" {
			t.Errorf("args %q = code %d stdout %q", args, code, stdout)
		}
	}
}

func TestHygieneCommandReportsResidue(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main", "hygiene run", "hygiene drift", "hygiene claim")

	// A claimless in-progress task.
	code, stdout, stderr := runCmd(cmdSetStatus, "--revision", "1", "--json", "2", "in-progress")
	if code != 0 || stderr != "" {
		t.Fatalf("set-status = code %d stdout %q stderr %q", code, stdout, stderr)
	}

	// An expired claim on task 3.
	code, stdout, stderr = runCmd(cmdClaim, "--json", "--owner", "hygiene-probe",
		"--ttl", "1", "--request-id", "hygiene.expired.claim", "3")
	if code != 0 || stderr != "" {
		t.Fatalf("claim = code %d stdout %q stderr %q", code, stdout, stderr)
	}

	// A running run on task 1 whose attempt lease expires immediately.
	claimToken := claimTokenForRun(t)
	start := startRunCLITTL(t, claimToken, "hygiene.stale.run", 1)

	time.Sleep(1100 * time.Millisecond)

	code, stdout, stderr = runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeHygiene(t, stdout)

	if len(got.StaleRuns) != 1 {
		t.Fatalf("stale runs = %+v", got.StaleRuns)
	}
	run := got.StaleRuns[0]
	if run.RunID != start.RunID || run.Tag != "main" || run.Task != "1" ||
		run.Owner != "agent-a" || run.Attempt != start.Attempt ||
		run.TTLSeconds != 1 || run.AttemptExpiresAt == "" {
		t.Fatalf("stale run = %+v", run)
	}
	if len(got.ExpiredClaims) != 1 || got.ExpiredClaims[0].Task != "3" ||
		got.ExpiredClaims[0].Owner != "hygiene-probe" || got.ExpiredClaims[0].ExpiredAt == "" {
		t.Fatalf("expired claims = %+v", got.ExpiredClaims)
	}
	if len(got.ClaimlessInProgress) != 1 || got.ClaimlessInProgress[0].Task != "2" {
		t.Fatalf("claimless in-progress = %+v", got.ClaimlessInProgress)
	}
	if strings.Contains(stdout, "token") {
		t.Fatalf("hygiene output mentions tokens: %s", stdout)
	}

	code, stdout, _ = runHygiene()
	if code != 0 || !strings.Contains(stdout, "stale run") ||
		!strings.Contains(stdout, "expired claim") ||
		!strings.Contains(stdout, "in-progress without live claim") ||
		!strings.Contains(stdout, "3 lifecycle finding(s): 1 stale run(s), 1 expired claim(s), 1 in-progress without live claim, 0 retryable run(s), 0 stuck worktree saga(s), 0 unowned registered worktree(s), 0 primary checkout finding(s)") {
		t.Fatalf("human hygiene = code %d stdout %q", code, stdout)
	}
}

// startRunCLITTL is startRunCLI with an explicit TTL so leases can be
// expired deterministically in tests.
func startRunCLITTL(t *testing.T, claimToken int64, requestID string, ttl int64) runStartCLIOut {
	t.Helper()
	code, stdout, stderr := runCmd(cmdRun, "start", "1", "--json", "--owner", "agent-a",
		"--claim-token", fmt.Sprint(claimToken), "--request-id", requestID, "--ttl", fmt.Sprint(ttl))
	if code != 0 || stderr != "" {
		t.Fatalf("run start exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var out runStartCLIOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil || out.RunID <= 0 || out.AttemptToken <= 0 {
		t.Fatalf("run start output = %q, %v", stdout, err)
	}
	return out
}

func TestHygieneRetryableRunIsNotStale(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main", "hygiene retryable")
	claimToken := claimTokenForRun(t)
	start := startRunCLITTL(t, claimToken, "hygiene.retryable", 2)

	// A failed finish terminalizes the attempt and leaves the run
	// retryable: an explicit retry-or-cancel decision, never lease-stale
	// residue (§8.9) — including after the original lease moment has
	// passed.
	code, stdout, stderr := runCmd(cmdRun, "finish", fmt.Sprint(start.RunID), "--json", "--owner", "agent-a",
		"--claim-token", fmt.Sprint(claimToken), "--attempt", fmt.Sprint(start.Attempt),
		"--attempt-token", fmt.Sprint(start.AttemptToken), "--outcome", "failed",
		"--request-id", "hygiene.retryable.finish")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"runStatus": "retryable"`) {
		t.Fatalf("failed finish exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	time.Sleep(2200 * time.Millisecond)

	code, stdout, stderr = runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeHygiene(t, stdout)
	if len(got.StaleRuns) != 0 {
		t.Fatalf("retryable run reported stale after original expiry: %+v", got.StaleRuns)
	}
	// The same run is reported as its own class: an explicit
	// retry-or-cancel decision the orchestrator owns, never lease-stale.
	if len(got.RetryableRuns) != 1 {
		t.Fatalf("retryable run not reported as its own class: %+v", got.RetryableRuns)
	}
	run := got.RetryableRuns[0]
	if run.RunID != start.RunID || run.Tag != "main" || run.Task != "1" ||
		run.Owner != "agent-a" || run.Attempt != start.Attempt || run.UpdatedAt == "" {
		t.Fatalf("retryable run = %+v", run)
	}
}

func TestHygieneReportsStuckSagasWithPresence(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "hygiene saga", "wt.hygiene.saga")
	path := canonTestPath(t, dir, "main-1")
	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.hygiene.saga.provision")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	// A cancelled run schedules the bound workspace's removal: a pending
	// removal saga, reported with its path and branch presence (§8.9).
	code, _, stderr = runCmd(cmdRun, "cancel", "--force", "--request-id", "wt.hygiene.saga.cancel", strconv.FormatInt(runID, 10))
	if code != 0 {
		t.Fatalf("cancel exit %d, stderr: %s", code, stderr)
	}

	code, stdout, stderr := runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeHygiene(t, stdout)
	if len(got.Sagas) != 1 {
		t.Fatalf("stuck sagas = %+v", got.Sagas)
	}
	saga := got.Sagas[0]
	if saga.Operation != "remove" || saga.Status != "pending" || saga.Tag != "main" || saga.Task != "1" ||
		saga.RunID == nil || *saga.RunID != runID ||
		saga.Path != path || !saga.PathExists || saga.Branch != "tasks/main/1" || !saga.BranchExists || saga.UpdatedAt == "" {
		t.Fatalf("stuck saga = %+v", saga)
	}
	if strings.Contains(stdout, "sagaToken") || strings.Contains(stdout, "sagaFence") {
		t.Fatalf("hygiene output mentions saga tokens: %s", stdout)
	}

	// A branch deleted out-of-band is reported missing explicitly: it is
	// the fact that blocks the §9.4 removal proof, never a silence.
	gitCmd(t, dir, "update-ref", "-d", "refs/heads/tasks/main/1")
	code, stdout, stderr = runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got = decodeHygiene(t, stdout)
	if len(got.Sagas) != 1 || got.Sagas[0].BranchExists || !got.Sagas[0].PathExists {
		t.Fatalf("stuck saga after out-of-band branch deletion = %+v", got.Sagas)
	}

	code, stdout, _ = runHygiene()
	if code != 0 || !strings.Contains(stdout, "stuck worktree saga") ||
		!strings.Contains(stdout, "branch tasks/main/1 missing") ||
		!strings.Contains(stdout, "1 stuck worktree saga(s)") {
		t.Fatalf("human hygiene = code %d stdout %q", code, stdout)
	}

	// A dangling symlink at the recorded path is presence, not absence:
	// Stat follows the link and reports it gone, while recovery's Lstat
	// refuses the same residual link, so cleanup is still blocked.
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "nowhere"), path); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got = decodeHygiene(t, stdout)
	if len(got.Sagas) != 1 || !got.Sagas[0].PathExists || got.Sagas[0].BranchExists {
		t.Fatalf("a dangling symlink at the saga path is presence: %+v", got.Sagas)
	}
}
