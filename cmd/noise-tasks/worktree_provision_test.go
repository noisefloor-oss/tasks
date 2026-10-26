package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// bumpProvisionFencing pushes the fencing counter high through the same
// direct-SQL fixture surface other CLI tests use, so claim and attempt
// capabilities become long unique digit strings that cannot leak by
// coincidence.
func bumpProvisionFencing(t *testing.T, dir string) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(`UPDATE meta SET value='900000000000000000' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
}

// provisionCLISeed seeds one claimed, running task in the current worktree
// repo and returns its live capability triple.
func provisionCLISeed(t *testing.T, dir, title, requestID string) (runID, claimToken, attemptToken, attempt int64) {
	t.Helper()
	seedProjectStore(t, dir, "main", title)
	bumpProvisionFencing(t, dir)
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	claim, _, _, err := db.AcquireClaim("main", "1", "agent-a", 3600, "claim."+requestID)
	if err != nil {
		t.Fatal(err)
	}
	run, replayed, err := db.StartRun("main", "1", "agent-a", claim.Token, "run."+requestID, 3600)
	if err != nil || replayed {
		t.Fatalf("start run = %+v replayed=%v err=%v", run, replayed, err)
	}
	return run.RunID, claim.Token, run.AttemptToken, run.Attempt
}

// provisionScan answers one direct-SQL question about the store fixture.
func provisionScan(t *testing.T, dir, query string, dest any, args ...any) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if err := sqlDB.QueryRow(query, args...).Scan(dest); err != nil {
		t.Fatal(err)
	}
}

// provisionTerminalizeRun models a store whose run went terminal through a
// path that carries no §9.4 automatic scheduling — an older binary's store —
// so the worktree saga rows stay exactly as that binary left them and the
// finalize-time compensation protocol still owns the observed effect.
func provisionTerminalizeRun(t *testing.T, dir string, runID int64) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(`UPDATE runs SET status='cancelled',terminal_at=updated_at WHERE run_id=?`, runID); err != nil {
		t.Fatal(err)
	}
}

// provisionHostPlan replicates the CLI's derivation and planning phase
// directly, to pin the §9.4 crash boundaries around the CLI's effect and
// finalization phases.
func provisionHostPlan(t *testing.T, dir string, runID, claimToken, attemptToken int64, requestID string) *store.WorktreeProvisionPlan {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	record, err := db.GetRun(runID)
	if err != nil || record == nil {
		t.Fatalf("run %d = %+v, %v", runID, record, err)
	}
	identity, err := store.PreflightWorktreeProvision(dir, dir, record.Tag, record.Task, false, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := db.PlanWorktreeProvision(store.WorktreeProvisionOptions{
		RunID: runID, Owner: "agent-a", ClaimToken: claimToken,
		Attempt: record.CurrentAttempt, AttemptToken: attemptToken, RequestID: requestID,
		OwnerBoundary: identity.OwnerBoundary, RegistrationRoot: identity.RegistrationRoot,
		TargetPath: identity.TargetPath, Branch: identity.Branch, BaseCommit: identity.BaseCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func provisionArgs(runID, claimToken, attemptToken, attempt int64, requestID string) []string {
	return []string{"provision", strconv.FormatInt(runID, 10), "--owner", "agent-a",
		"--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10),
		"--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--request-id", requestID}
}

type provisionJSON struct {
	SagaID             int64                        `json:"sagaId"`
	Operation          string                       `json:"operation"`
	Status             string                       `json:"status"`
	RunID              *int64                       `json:"runId"`
	OwnerBoundary      string                       `json:"ownerBoundary"`
	Path               string                       `json:"path"`
	Branch             string                       `json:"branch"`
	BaseCommit         *string                      `json:"baseCommit"`
	SagaToken          int64                        `json:"sagaToken"`
	RecoveryGeneration int64                        `json:"recoveryGeneration"`
	RecoverRequestID   *string                      `json:"recoverRequestId"`
	BrokerRequest      *store.WorktreeBrokerRequest `json:"brokerRequest"`
}

func gitCLIOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func gitRevParseMain(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(gitCLIOutput(t, dir, "rev-parse", "main"))
}

func gitWorktreeRegistered(t *testing.T, dir, target string) bool {
	t.Helper()
	for _, line := range strings.Split(gitCLIOutput(t, dir, "worktree", "list", "--porcelain"), "\n") {
		if strings.HasPrefix(line, "worktree ") && strings.TrimSpace(strings.TrimPrefix(line, "worktree ")) == target {
			return true
		}
	}
	return false
}

func TestWorktreeProvisionCLIBoundExactOutput(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "provision cli", "wt.cli")
	base := gitRevParseMain(t, dir)

	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.cli"), "--json")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	var got provisionJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("provision --json not JSON: %v\n%s", err, stdout)
	}
	wantPath := filepath.Join(dir, ".worktrees", "main-1")
	if got.SagaID != 1 || got.Operation != "provision" || got.Status != "bound" || got.RunID == nil ||
		*got.RunID != runID || got.OwnerBoundary != dir || got.Path != wantPath || got.Branch != "tasks/main/1" ||
		got.BaseCommit == nil || *got.BaseCommit != base || got.SagaToken <= 0 || got.RecoveryGeneration != 0 ||
		got.RecoverRequestID != nil || got.BrokerRequest != nil {
		t.Fatalf("bound result = %+v", got)
	}
	// Exact output contract: the §9.4 projection and nothing else, in order.
	want := fmt.Sprintf("{\n  \"sagaId\": 1,\n  \"operation\": \"provision\",\n  \"status\": \"bound\",\n  \"runId\": %d,\n  \"ownerBoundary\": %q,\n  \"path\": %q,\n  \"branch\": \"tasks/main/1\",\n  \"baseCommit\": %q,\n  \"sagaToken\": %d,\n  \"recoveryGeneration\": 0,\n  \"recoverRequestId\": null,\n  \"brokerRequest\": null\n}\n",
		runID, dir, wantPath, base, got.SagaToken)
	if stdout != want {
		t.Fatalf("bound JSON =\n%s\nwant\n%s", stdout, want)
	}
	// The real effect happened: a registered worktree on the exact branch.
	if !gitWorktreeRegistered(t, dir, wantPath) {
		t.Fatal("provision did not create a registered worktree")
	}
	// Capability secrecy: exactly the authorized fields, and no capability
	// digit string anywhere in the output.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &fields); err != nil || len(fields) != 12 {
		t.Fatalf("bound JSON fields = %d, %v", len(fields), err)
	}
	for _, secret := range []string{strconv.FormatInt(claimToken, 10), strconv.FormatInt(attemptToken, 10), "claimToken", "attemptToken"} {
		if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
			t.Fatalf("output leaked capability %q:\n%s\n%s", secret, stdout, stderr)
		}
	}

	// Completed replay: the same receipt result, no second effect or saga.
	code, stdout, stderr = runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.cli"), "--json")...)
	if code != 0 || stdout != want {
		t.Fatalf("replay exit %d\n%s\nstderr %s\nwant\n%s", code, stdout, stderr, want)
	}
}

func TestWorktreeProvisionCLIHumanOutput(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "provision human", "wt.human")
	base := gitRevParseMain(t, dir)

	code, stdout, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.human")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	// The human surface renders exactly the non-secret fields and, for manual
	// provision, the saga token.
	want := fmt.Sprintf("sagaId=1 operation=provision status=bound runId=%d ownerBoundary=%s path=%s branch=tasks/main/1 baseCommit=%s recoveryGeneration=0 sagaToken=",
		runID, dir, filepath.Join(dir, ".worktrees", "main-1"), base)
	if !strings.HasPrefix(stdout, want) || !strings.HasSuffix(stdout, "\n") {
		t.Fatalf("human output = %q, want prefix %q", stdout, want)
	}
	if fields := strings.Fields(strings.TrimSpace(stdout)); len(fields) != 10 {
		t.Fatalf("human fields = %d: %q", len(fields), stdout)
	}
	if strings.Contains(stdout, "recoverRequestId") || strings.Contains(stdout, strconv.FormatInt(claimToken, 10)) {
		t.Fatalf("human output leaked a non-authorized field: %q", stdout)
	}
}

func TestWorktreeProvisionCLICrashBoundaries(t *testing.T) {
	t.Run("planned before effect", func(t *testing.T) {
		dir := chdirWorktreeRepo(t)
		runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "crash plan", "wt.crash1")
		crashed := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.crash1")
		if !crashed.EffectRequired {
			t.Fatal("crashed plan must require the effect")
		}
		code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.crash1"), "--json")...)
		if code != 0 {
			t.Fatalf("resume exit %d, stderr: %s", code, stderr)
		}
		var got provisionJSON
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status != "bound" || got.SagaID != crashed.SagaID || got.SagaToken != crashed.SagaToken {
			t.Fatalf("resumed result = %+v; crashed plan = %+v", got, crashed)
		}
		var sagas int
		provisionScan(t, dir, `SELECT COUNT(*) FROM worktree_sagas`, &sagas)
		if sagas != 1 {
			t.Fatalf("resume allocated %d sagas", sagas)
		}
	})
	t.Run("effect before finalization", func(t *testing.T) {
		dir := chdirWorktreeRepo(t)
		runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "crash effect", "wt.crash2")
		plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.crash2")
		if outcome, err := store.EffectWorktreeProvision(dir, dir, plan); err != nil || outcome != store.WorktreeProvisionBound {
			t.Fatalf("crashed effect = %v, %v", outcome, err)
		}
		// Operator content appears inside the crashed worktree; the resume
		// must observe the intact effect, never re-create it.
		marker := filepath.Join(plan.TargetPath, "operator-note")
		if err := os.WriteFile(marker, []byte("keep me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.crash2"), "--json")...)
		if code != 0 {
			t.Fatalf("resume exit %d, stderr: %s", code, stderr)
		}
		var got provisionJSON
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatal(err)
		}
		if got.Status != "bound" || got.SagaID != plan.SagaID {
			t.Fatalf("resumed result = %+v", got)
		}
		if data, err := os.ReadFile(marker); err != nil || string(data) != "keep me\n" {
			t.Fatalf("resume repeated the effect; marker = %q, %v", data, err)
		}
		var status string
		provisionScan(t, dir, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, plan.SagaID)
		if status != "bound" {
			t.Fatalf("resumed saga = %q", status)
		}
	})
}

func TestWorktreeProvisionCLICompensationOrchestration(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, _ := provisionCLISeed(t, dir, "compensate", "wt.comp")
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.comp")
	if outcome, err := store.EffectWorktreeProvision(dir, dir, plan); err != nil || outcome != store.WorktreeProvisionBound {
		t.Fatalf("effect = %v, %v", outcome, err)
	}
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The run terminalizes after the create was observed — through a path
	// predating §9.4's parent flip, so the saga is still pending: the
	// finalize loop must close SQLite, compensate the exact effect, and
	// record failed only because absence is proved.
	provisionTerminalizeRun(t, dir, runID)
	ps := &projectStore{db: db, path: projectStorePath(dir), boundary: dir, owner: dir}
	result, ferr := finalizeWorktreeProvisionWithCompensation(ps, plan, store.WorktreeProvisionBound)
	if ferr != nil || result.Status != "failed" {
		t.Fatalf("compensated finalize = %+v, %v", result, ferr)
	}
	var outBuf, errBuf strings.Builder
	if code := writeWorktreeProvisionResult(&outBuf, &errBuf, false, result); code != 1 ||
		outBuf.String() != "" || !strings.Contains(errBuf.String(), "proven absent") {
		t.Fatalf("failed render = %d, stdout %q stderr %q", code, outBuf.String(), errBuf.String())
	}
	var status string
	provisionScan(t, dir, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, plan.SagaID)
	if status != "failed" {
		t.Fatalf("compensated saga = %q", status)
	}
	if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("compensated target = %v", err)
	}
	after, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer after.Close()
	if receipt, err := after.FindReceipt(store.ReceiptKindWorktree, "wt.comp"); err != nil || receipt == nil {
		t.Fatalf("failed receipt = %v, %v", receipt, err)
	}
	// Unproved compensation renders the cleanup-required handoff instead: an
	// operator commit moves the branch off the immutable base first.
	dir2 := chdirWorktreeRepo(t)
	runID2, claimToken2, attemptToken2, _ := provisionCLISeed(t, dir2, "cleanup", "wt.clean")
	plan2 := provisionHostPlan(t, dir2, runID2, claimToken2, attemptToken2, "wt.clean")
	if outcome, err := store.EffectWorktreeProvision(dir2, dir2, plan2); err != nil || outcome != store.WorktreeProvisionBound {
		t.Fatalf("effect = %v, %v", outcome, err)
	}
	gitCLIOutput(t, plan2.TargetPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "operator work", "--no-gpg-sign")
	db2, err := store.Open(projectStorePath(dir2))
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	// Same pre-§9.4 terminalization: the saga is still pending when the
	// unproved compensation must render the cleanup-required handoff.
	provisionTerminalizeRun(t, dir2, runID2)
	ps2 := &projectStore{db: db2, path: projectStorePath(dir2), boundary: dir2, owner: dir2}
	result2, ferr := finalizeWorktreeProvisionWithCompensation(ps2, plan2, store.WorktreeProvisionBound)
	if ferr != nil || result2.Status != "cleanup-required" || result2.RecoverRequestID == nil ||
		!strings.HasPrefix(*result2.RecoverRequestID, "recover-") {
		t.Fatalf("cleanup finalize = %+v, %v", result2, ferr)
	}
	var jout, jerr strings.Builder
	if code := writeWorktreeProvisionResult(&jout, &jerr, true, result2); code != 0 || jerr.String() != "" {
		t.Fatalf("cleanup JSON render = %d, stderr %q", code, jerr.String())
	}
	var cleanup provisionJSON
	if err := json.Unmarshal([]byte(jout.String()), &cleanup); err != nil || cleanup.Status != "cleanup-required" ||
		cleanup.RecoverRequestID == nil || cleanup.BrokerRequest != nil {
		t.Fatalf("cleanup JSON = %s, %v", jout.String(), err)
	}
	var hout, herr strings.Builder
	if code := writeWorktreeProvisionResult(&hout, &herr, false, result2); code != 1 || hout.String() != "" ||
		herr.String() != "noise-tasks: worktree cleanup is required\n" {
		t.Fatalf("cleanup human render = %d, stdout %q stderr %q", code, hout.String(), herr.String())
	}
}

func TestWorktreeProvisionCLITerminalRunReplayRefuses(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "terminal replay", "wt.term")
	provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.term")
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, err := db.CancelRun(runID, "agent-a", claimToken, attempt, attemptToken, false, "run.cancel.term"); err != nil {
		t.Fatal(err)
	}
	// A pending saga whose run went terminal has no live controller: the
	// original replay refuses and recovery owns the saga (§9.4).
	code, stdout, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.term")...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "provision conflict") {
		t.Fatalf("terminal replay exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestWorktreeProvisionCLIConfinement(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("confined registration requires an unprivileged uid")
	}
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "confined", "wt.confine")
	reg := filepath.Join(dir, ".git", "worktrees")
	if err := os.MkdirAll(reg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(reg, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(reg, 0o755) })

	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.confine"), "--json")...)
	if code != 0 {
		t.Fatalf("confined provision exit %d, stderr: %s", code, stderr)
	}
	var got provisionJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	familyID, _, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "needs-broker" || got.RecoverRequestID == nil || got.BrokerRequest == nil {
		t.Fatalf("confined result = %+v", got)
	}
	br := got.BrokerRequest
	if br.FamilyID != familyID || br.Verb != "create" ||
		br.RegistrationRoot != filepath.Join(dir, ".git", "worktrees") ||
		br.OwnerBoundary != dir || br.TargetPath != filepath.Join(dir, ".worktrees", "main-1") ||
		br.Branch != "tasks/main/1" || br.BaseCommit == nil || br.Force ||
		br.RecoverRequestID != *got.RecoverRequestID || !strings.HasPrefix(*got.RecoverRequestID, "recover-") {
		t.Fatalf("broker request = %+v", br)
	}
	for _, secret := range []string{"uid", "gid", "expiry", "auth", strconv.FormatInt(claimToken, 10), strconv.FormatInt(attemptToken, 10)} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("broker request leaked %q: %s", secret, stdout)
		}
	}
	// The handoff replay returns the same mint bindings, still recoverable.
	code, stdout, stderr = runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.confine"), "--json")...)
	if code != 0 {
		t.Fatalf("confined replay exit %d, stderr: %s", code, stderr)
	}
	var replay provisionJSON
	if err := json.Unmarshal([]byte(stdout), &replay); err != nil || replay.Status != "needs-broker" ||
		replay.BrokerRequest == nil || replay.SagaID != got.SagaID {
		t.Fatalf("confined replay = %s, %v", stdout, err)
	}

	// Human mode keeps the pinned §9.4 confinement diagnostic: exit 1,
	// empty stdout.
	dir2 := chdirWorktreeRepo(t)
	runID2, claimToken2, attemptToken2, attempt2 := provisionCLISeed(t, dir2, "confined human", "wt.confine2")
	reg2 := filepath.Join(dir2, ".git", "worktrees")
	if err := os.MkdirAll(reg2, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(reg2, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(reg2, 0o755) })
	code, stdout, stderr = runCmd(cmdWorktree, provisionArgs(runID2, claimToken2, attemptToken2, attempt2, "wt.confine2")...)
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: worktree registration is confined and no broker is available\n" {
		t.Fatalf("confined human exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The replay keeps the same pinned diagnostic in human mode.
	code, stdout, stderr = runCmd(cmdWorktree, provisionArgs(runID2, claimToken2, attemptToken2, attempt2, "wt.confine2")...)
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: worktree registration is confined and no broker is available\n" {
		t.Fatalf("confined human replay exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestWorktreeProvisionCLIGrammarAndRefusals(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "grammar", "wt.ok")
	run := strconv.FormatInt(runID, 10)
	base := []string{"provision", run, "--owner", "agent-a",
		"--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10),
		"--attempt-token", strconv.FormatInt(attemptToken, 10)}
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"missing owner", []string{"provision", run, "--claim-token", "1", "--attempt", "1", "--attempt-token", "1", "--request-id", "wt.x"}, 2, "--owner is required"},
		{"zero token", []string{"provision", run, "--owner", "agent-a", "--claim-token", "0", "--attempt", "1", "--attempt-token", "1", "--request-id", "wt.x"}, 2, "must be positive integers"},
		{"auto prefix", append(append([]string(nil), base...), "--request-id", "auto-1"), 2, `cannot begin with "auto-"`},
		{"recover prefix", append(append([]string(nil), base...), "--request-id", "recover-1"), 2, `cannot begin with "auto-" or "recover-"`},
		{"bad request grammar", append(append([]string(nil), base...), "--request-id", "bad id!"), 2, "--request-id must match"},
		{"file refused", append(append([]string(nil), base...), "--request-id", "wt.x", "--file", "x.json"), 2, "--file is not accepted"},
		{"tag refused", append(append([]string(nil), base...), "--request-id", "wt.x", "--tag", "main"), 2, "--tag is not accepted"},
		{"extra positional", append(append([]string(nil), base...), "--request-id", "wt.x", "extra"), 2, "usage:"},
		{"unknown run", []string{"provision", "999", "--owner", "agent-a", "--claim-token", "1", "--attempt", "1", "--attempt-token", "1", "--request-id", "wt.x"}, 3, "provision conflict"},
		{"wrong owner", []string{"provision", run, "--owner", "agent-b", "--claim-token", "1", "--attempt", "1", "--attempt-token", "1", "--request-id", "wt.x"}, 3, "provision conflict"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCmd(cmdWorktree, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.want) || stdout != "" {
			t.Fatalf("%s: exit %d stdout %q stderr %q (want code %d with %q)", tc.name, code, stdout, stderr, tc.code, tc.want)
		}
	}

	// Collision refusals with the exact §9.4 diagnostics.
	if err := os.MkdirAll(filepath.Join(dir, ".worktrees", "main-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCmd(cmdWorktree, append(append([]string(nil), base...), "--request-id", "wt.collide1")...)
	if code != 3 || !strings.Contains(stderr, "worktree path exists: "+filepath.Join(dir, ".worktrees", "main-1")) {
		t.Fatalf("existing target: exit %d stderr %q", code, stderr)
	}
	if err := os.Remove(filepath.Join(dir, ".worktrees", "main-1")); err != nil {
		t.Fatal(err)
	}
	gitCLIOutput(t, dir, "branch", "tasks/main/1")
	code, _, stderr = runCmd(cmdWorktree, append(append([]string(nil), base...), "--request-id", "wt.collide2")...)
	if code != 3 || !strings.Contains(stderr, "branch tasks/main/1 already exists") {
		t.Fatalf("existing branch: exit %d stderr %q", code, stderr)
	}
}

// TestWorktreeProvisionCLITaskIDArgumentHint pins the §9.4 task-id-mistake
// diagnostic: when the positional argument names no run but does name a task,
// the run-conflict refusal (exit 3, empty stdout) additionally names the
// mistake and the task's current run id — scoped per tag, since several tags
// reuse one bare id — and only then. When the number is both a valid run id
// and a valid task id, the run wins and no hint appears.
func TestWorktreeProvisionCLITaskIDArgumentHint(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	// Tag main carries tasks 1–9 and tag other reuses ids 1–6, so ids 1–6 are
	// ambiguous across tags while 7–9 are main-only. Runs 1–3 exist (main 9,
	// main 6, other 6), so no argument below ever names a run.
	seedProjectStore(t, dir, "main", "a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9")
	bumpProvisionFencing(t, dir)
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.EnsureTag("other"); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 6; i++ {
		if _, err := db.AddTask("other", &store.Task{Title: fmt.Sprintf("b%d", i), Status: "pending"}); err != nil {
			t.Fatal(err)
		}
	}
	startRun := func(tag, task, req string) (claimToken, attemptToken int64) {
		t.Helper()
		claim, _, _, err := db.AcquireClaim(tag, task, "agent-a", 3600, "claim.wt."+req)
		if err != nil {
			t.Fatal(err)
		}
		run, replayed, err := db.StartRun(tag, task, "agent-a", claim.Token, "run.wt."+req, 3600)
		if err != nil || replayed {
			t.Fatalf("start run %s/%s = %+v replayed=%v err=%v", tag, task, run, replayed, err)
		}
		return claim.Token, run.AttemptToken
	}
	claim9, attempt9 := startRun("main", "9", "hint9") // run 1
	startRun("main", "6", "hint6m")                    // run 2

	args := provisionArgs(0, 1, 1, 1, "wt.hint")
	cases := []struct {
		name      string
		arg       string
		wantCode  int
		wantIn    []string
		wantNotIn []string
	}{
		{"single-tag id with live run", "9", 3, []string{
			"run provision conflict: run 9",
			"hint: 9 is a task id, not a run id; task 9's current run id is 1",
		}, nil},
		{"single-tag id with no run", "8", 3, []string{
			"run provision conflict: run 8",
			"hint: 8 is a task id, not a run id; task 8 has no current run",
		}, []string{"current run id is"}},
		{"reused id, live run in one tag", "6", 3, []string{
			"run provision conflict: run 6",
			"hint: 6 is a task id in tags main, other; task 6 in tag main has current run id 2; task 6 in tag other has no current run",
		}, nil},
		{"neither run nor task", "99", 3, []string{
			"run provision conflict: run 99",
		}, []string{"is a task id"}},
	}
	for _, tc := range cases {
		tcArgs := append([]string(nil), args...)
		tcArgs[1] = tc.arg
		code, stdout, stderr := runCmd(cmdWorktree, tcArgs...)
		if code != tc.wantCode || stdout != "" {
			t.Fatalf("%s: exit %d stdout %q stderr %q (want code %d, empty stdout)", tc.name, code, stdout, stderr, tc.wantCode)
		}
		for _, want := range tc.wantIn {
			if !strings.Contains(stderr, want) {
				t.Fatalf("%s: stderr %q missing %q", tc.name, stderr, want)
			}
		}
		for _, banned := range tc.wantNotIn {
			if strings.Contains(stderr, banned) {
				t.Fatalf("%s: stderr %q must not contain %q", tc.name, stderr, banned)
			}
		}
	}

	// The second tag's task 6 goes live as run 3: the reused-id hint now names
	// a live run for every tag.
	startRun("other", "6", "hint6o")
	hint6Args := []string{"provision", "6", "--owner", "agent-a", "--claim-token", "1",
		"--attempt", "1", "--attempt-token", "1", "--request-id", "wt.hint6"}
	code, stdout, stderr := runCmd(cmdWorktree, hint6Args...)
	if code != 3 || stdout != "" {
		t.Fatalf("reused id both live: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	want := "hint: 6 is a task id in tags main, other; " +
		"task 6 in tag main has current run id 2; task 6 in tag other has current run id 3"
	if !strings.Contains(stderr, want) {
		t.Fatalf("reused id both live: stderr %q missing %q", stderr, want)
	}

	// --json carries the refusal and hint on stderr only.
	code, stdout, stderr = runCmd(cmdWorktree, append(append([]string(nil), hint6Args...), "--json")...)
	if code != 3 || stdout != "" {
		t.Fatalf("json refusal: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var conflict struct {
		Kind      string `json:"kind"`
		Operation string `json:"operation"`
		RunID     int64  `json:"runId"`
		Hint      string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(stderr), &conflict); err != nil {
		t.Fatalf("json refusal stderr %q: %v", stderr, err)
	}
	if conflict.Kind != "run-conflict" || conflict.Operation != "provision" || conflict.RunID != 6 ||
		!strings.Contains(conflict.Hint, "task 6 in tag other has current run id 3") {
		t.Fatalf("json refusal = %+v stderr %q", conflict, stderr)
	}

	// Both-valid: argument 1 names run 1 and task 1 in both tags; the run
	// wins, provision proceeds for that run, and no hint appears anywhere.
	code, stdout, stderr = runCmd(cmdWorktree, append(provisionArgs(1, claim9, attempt9, 1, "wt.both"), "--json")...)
	if code != 0 || stderr != "" {
		t.Fatalf("both-valid: exit %d stdout %q stderr %q (want bound success)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"status": "bound"`) || strings.Contains(stdout, "task id") {
		t.Fatalf("both-valid stdout %q", stdout)
	}

	// A hint-lookup failure must not soften the refusal: with the tasks
	// table unreadable, the same argument keeps today's bare run conflict.
	provisionExec(t, dir, `ALTER TABLE tasks RENAME TO tasks_unreadable`)
	code, stdout, stderr = runCmd(cmdWorktree, append(append([]string(nil), hint6Args...), "--request-id", "wt.hintfail")...)
	if code != 3 || stdout != "" {
		t.Fatalf("hint lookup failure: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "run provision conflict: run 6") || strings.Contains(stderr, "is a task id") {
		t.Fatalf("hint lookup failure: stderr %q", stderr)
	}
}

func TestWorktreeProvisionCLIIgnoreAndBaseRefusals(t *testing.T) {
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })

	// A not-ignored .worktrees root refuses as a usage error before planning.
	dir := t.TempDir()
	gitCLIOutput(t, dir, "init", "-b", "main")
	gitCLIOutput(t, dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "init")
	seedProjectStore(t, dir, "main", "no ignore")
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	db := openProvisionStore(t, dir)
	claim, _, _, err := db.AcquireClaim("main", "1", "agent-a", 3600, "claim.wt.ign")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.StartRun("main", "1", "agent-a", claim.Token, "run.wt.ign", 3600)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdWorktree, provisionArgs(run.RunID, claim.Token, run.AttemptToken, run.Attempt, "wt.ign")...)
	if code != 2 || stdout != "" ||
		stderr != "noise-tasks: .worktrees is not git-ignored; add it to .gitignore or .git/info/exclude\n" {
		t.Fatalf("not-ignored exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var sagas int
	provisionScan(t, dir, `SELECT COUNT(*) FROM worktree_sagas`, &sagas)
	if sagas != 0 {
		t.Fatalf("refusal planned %d sagas", sagas)
	}

	// No origin/HEAD and no local main: the pinned base refusal.
	baseless := t.TempDir()
	gitCLIOutput(t, baseless, "init", "-b", "trunk")
	if err := os.WriteFile(filepath.Join(baseless, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(baseless); err != nil {
		t.Fatal(err)
	}
	seedProjectStore(t, baseless, "main", "baseless")
	db2 := openProvisionStore(t, baseless)
	claim2, _, _, err := db2.AcquireClaim("main", "1", "agent-a", 3600, "claim.wt.base")
	if err != nil {
		t.Fatal(err)
	}
	run2, _, err := db2.StartRun("main", "1", "agent-a", claim2.Token, "run.wt.base", 3600)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCmd(cmdWorktree, provisionArgs(run2.RunID, claim2.Token, run2.AttemptToken, run2.Attempt, "wt.base")...)
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: cannot resolve worktree base: origin/HEAD and main do not resolve to commits\n" {
		t.Fatalf("baseless exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// provisionExec mutates the store fixture through the direct-SQL surface
// other CLI tests use for setup-only state changes.
func provisionExec(t *testing.T, dir, query string, args ...any) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeProvisionCLIFailedReplay(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "failed replay", "wt.fail")
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.fail")
	if outcome, err := store.EffectWorktreeProvision(dir, dir, plan); err != nil || outcome != store.WorktreeProvisionBound {
		t.Fatalf("effect = %v, %v", outcome, err)
	}
	// The task completes while the controller stays live: finalization can
	// no longer bind, so it compensates and terminalizes the saga failed.
	provisionExec(t, dir, `UPDATE tasks SET status='done' WHERE tag='main' AND id='1'`)
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ps := &projectStore{db: db, path: projectStorePath(dir), boundary: dir, owner: dir}
	result, ferr := finalizeWorktreeProvisionWithCompensation(ps, plan, store.WorktreeProvisionBound)
	if ferr != nil || result.Status != "failed" {
		t.Fatalf("failed finalize = %+v, %v", result, ferr)
	}
	if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("compensated target = %v", err)
	}
	// §9.4: an exact replay of a failed result — like the fresh result — is
	// exit 1 with empty stdout in both modes; the diagnostic is
	// stderr-only.
	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.fail"), "--json")...)
	if code != 1 || stdout != "" {
		t.Fatalf("failed --json replay exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.fail")...)
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: worktree provision failed; the requested effect is proven absent\n" {
		t.Fatalf("failed human replay exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestWorktreeProvisionCLIReplayUsesStoredBase(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "stored base", "wt.stored")
	base := gitRevParseMain(t, dir)
	// Crash boundary: planning committed, the effect never ran.
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.stored")
	if plan.BaseCommit != base {
		t.Fatalf("planned base = %q, want %q", plan.BaseCommit, base)
	}
	// The default branch advances after planning.
	gitCLIOutput(t, dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "advance main", "--no-gpg-sign")
	moved := gitRevParseMain(t, dir)
	if moved == base {
		t.Fatal("main did not advance")
	}
	// §9.4 replay submits the saga's immutable stored base — never a
	// re-derived ref — so the resumed effect checks out the planned base.
	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.stored"), "--json")...)
	if code != 0 {
		t.Fatalf("moved-base replay exit %d, stderr: %s", code, stderr)
	}
	var got provisionJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "bound" || got.BaseCommit == nil || *got.BaseCommit != base {
		t.Fatalf("moved-base replay = %+v; want bound at %q", got, base)
	}
	if head := strings.TrimSpace(gitCLIOutput(t, dir, "rev-parse", plan.Branch)); head != base {
		t.Fatalf("saga branch = %s, want the stored base %s", head, base)
	}
	// The completed replay after another advance still returns the
	// historical result at the immutable base.
	gitCLIOutput(t, dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "advance again", "--no-gpg-sign")
	code, stdout, stderr = runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.stored"), "--json")...)
	if code != 0 {
		t.Fatalf("completed replay exit %d, stderr: %s", code, stderr)
	}
	var replay provisionJSON
	if err := json.Unmarshal([]byte(stdout), &replay); err != nil || replay.Status != "bound" ||
		replay.BaseCommit == nil || *replay.BaseCommit != base {
		t.Fatalf("completed replay = %s, %v", stdout, err)
	}
}

func TestWorktreeProvisionCLILinkedWorktreeInvocation(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	linked := filepath.Join(t.TempDir(), "linked")
	gitCLIOutput(t, dir, "worktree", "add", "-b", "linked-wt", linked)
	t.Cleanup(func() { os.Chdir(dir) })
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "linked invoke", "wt.linked")
	base := gitRevParseMain(t, dir)

	// §9.4: invocation from a linked worktree still provisions at the
	// family-owned root.
	if err := os.Chdir(linked); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.linked"), "--json")...)
	if code != 0 {
		t.Fatalf("linked provision exit %d, stderr: %s", code, stderr)
	}
	var got provisionJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, ".worktrees", "main-1")
	if got.Status != "bound" || got.OwnerBoundary != dir || got.Path != wantPath ||
		got.BaseCommit == nil || *got.BaseCommit != base {
		t.Fatalf("linked result = %+v", got)
	}
	if !gitWorktreeRegistered(t, dir, wantPath) {
		t.Fatal("linked invocation did not register the worktree at the family root")
	}
	if _, err := os.Lstat(filepath.Join(linked, ".worktrees")); !os.IsNotExist(err) {
		t.Fatalf("linked checkout grew a worktree: %v", err)
	}
}

func TestWorktreeProvisionCLICompensationIdentityFailsClosed(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, _ := provisionCLISeed(t, dir, "compensate identity", "wt.compid")
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.compid")
	if outcome, err := store.EffectWorktreeProvision(dir, dir, plan); err != nil || outcome != store.WorktreeProvisionBound {
		t.Fatalf("effect = %v, %v", outcome, err)
	}
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// The run terminalized through the same pre-§9.4 path, so the saga is
	// still pending when the compensation must judge identity.
	provisionTerminalizeRun(t, dir, runID)
	// The repository family physically relocates between the observed
	// effect and the compensation. The trusted host rederives the
	// registration root and owner boundary from current repository state —
	// never from the caller — and the no-longer-equal identity fails
	// closed: no effect, no row rewrite, and the saga stays pending for
	// replay.
	moved := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	ps := &projectStore{db: db, path: projectStorePath(dir), boundary: moved, owner: dir}
	if _, ferr := finalizeWorktreeProvisionWithCompensation(ps, plan, store.WorktreeProvisionBound); ferr == nil ||
		!strings.Contains(ferr.Error(), "does not match the saga registration root") {
		t.Fatalf("identity-refused compensation = %v", ferr)
	}
	// The refusal performed no effect: the created worktree survived at its
	// relocated path, still registered under the relocated family.
	if _, err := os.Lstat(filepath.Join(moved, ".worktrees", filepath.Base(plan.TargetPath))); err != nil {
		t.Fatalf("refused compensation still removed the target: %v", err)
	}
	if !gitWorktreeRegistered(t, moved, plan.TargetPath) {
		t.Fatal("refused compensation still unregistered the worktree")
	}
	var status string
	provisionScan(t, moved, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, plan.SagaID)
	if status != "pending" {
		t.Fatalf("row after refused compensation = %q", status)
	}
}

func TestWorktreeProvisionCLIReplayReprovesIgnore(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "reprove ignore", "wt.reignore")
	// Crash after planning, before the effect.
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.reignore")
	// The ignore rule then ceases to cover .worktrees. The reserved replay
	// skips preflight's ignore proof, so phase 2 must repeat it and fail
	// closed: the pinned usage refusal, no Git work, and a replayable saga.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("# nothing ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.reignore"), "--json")...)
	if code != 2 || stdout != "" ||
		!strings.Contains(stderr, ".worktrees is not git-ignored; add it to .gitignore or .git/info/exclude") {
		t.Fatalf("unignored replay exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("unignored replay created the target: %v", err)
	}
	if gitWorktreeRegistered(t, dir, plan.TargetPath) {
		t.Fatal("unignored replay registered a worktree")
	}
	if branches := gitCLIOutput(t, dir, "branch", "--list", plan.Branch); strings.TrimSpace(branches) != "" {
		t.Fatalf("unignored replay created the branch: %q", branches)
	}
	var status string
	provisionScan(t, dir, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, plan.SagaID)
	if status != "pending" {
		t.Fatalf("row after refused replay = %q", status)
	}
	// Restoring the rule lets the identical request proceed.
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".worktrees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.reignore"), "--json")...)
	if code != 0 {
		t.Fatalf("restored replay exit %d, stderr: %s", code, stderr)
	}
	var got provisionJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil || got.Status != "bound" || got.SagaID != plan.SagaID {
		t.Fatalf("restored replay = %s, %v", stdout, err)
	}
}
func openProvisionStore(t *testing.T, dir string) *store.DB {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
