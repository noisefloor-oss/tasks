package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// recoverArgs builds one `worktree recover` CLI invocation.
func recoverArgs(sagaID, sagaToken int64, requestID string, extra ...string) []string {
	args := []string{"recover", strconv.FormatInt(sagaID, 10),
		"--saga-token", strconv.FormatInt(sagaToken, 10),
		"--request-id", requestID}
	return append(args, extra...)
}

// recoverIDForFamily derives the §9.4 deterministic advertised recovery
// request id — `recover-` plus the lowercase SHA-256 over the domain string,
// family id, saga id, and generation — independently of the store's own
// derivation, so CLI tests cross-check the store.
func recoverIDForFamily(familyID string, sagaID, generation int64) string {
	sum := sha256.Sum256([]byte("noise-tasks-worktree-recover-v1\x00" + familyID + "\x00" +
		strconv.FormatInt(sagaID, 10) + "\x00" + strconv.FormatInt(generation, 10)))
	return "recover-" + hex.EncodeToString(sum[:])
}

// advertisedRecoverID derives the advertised request id for one saga's next
// generation from the store's family identity.
func advertisedRecoverID(t *testing.T, dir string, sagaID, generation int64) string {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	familyID, _, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return recoverIDForFamily(familyID, sagaID, generation)
}

// recoverPlanRemoval plans one pending removal saga against the bound
// provision and live run seedRemovalCLIRun created, modeling a crash between
// the planning transaction and the effect.
func recoverPlanRemoval(t *testing.T, dir string, fixture removalCLIRun, requestID string) *store.WorktreeRemovePlan {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	plan, err := db.PlanWorktreeRemoval(store.WorktreeRemoveOptions{
		RunID: fixture.runID, Owner: fixture.owner, ClaimToken: fixture.claimToken,
		Attempt: fixture.attempt, AttemptToken: fixture.attemptToken, RequestID: requestID})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// recoverSagaRow reads one saga row's status and generation.
func recoverSagaRow(t *testing.T, dir string, sagaID int64) (string, int64) {
	t.Helper()
	var status string
	var generation int64
	provisionScan(t, dir, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, sagaID)
	provisionScan(t, dir, `SELECT recovery_generation FROM worktree_sagas WHERE saga_id=?`, &generation, sagaID)
	return status, generation
}

// recoverJSON mirrors the CLI's recover projection: the provision/remove
// shape with sagaToken omitted entirely.
type recoverJSON struct {
	SagaID             int64                        `json:"sagaId"`
	Operation          string                       `json:"operation"`
	Status             string                       `json:"status"`
	RunID              *int64                       `json:"runId"`
	OwnerBoundary      string                       `json:"ownerBoundary"`
	Path               string                       `json:"path"`
	Branch             string                       `json:"branch"`
	BaseCommit         *string                      `json:"baseCommit"`
	RecoveryGeneration int64                        `json:"recoveryGeneration"`
	RecoverRequestID   *string                      `json:"recoverRequestId"`
	BrokerRequest      *store.WorktreeBrokerRequest `json:"brokerRequest"`
}

func decodeRecoverJSON(t *testing.T, stdout string) recoverJSON {
	t.Helper()
	var got recoverJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("recover --json output not JSON: %v\n%s", err, stdout)
	}
	return got
}

// TestWorktreeRecoverRequestIDSpecVector pins the §9.4 advertised-id formula
// against the spec's own test vector before any test derives live ids. For
// family 123e4567-e89b-42d3-a456-426614174000, saga 1, and next generation 1
// the id is recover-b9b99427… (SPEC §9.4).
func TestWorktreeRecoverRequestIDSpecVector(t *testing.T) {
	got := recoverIDForFamily("123e4567-e89b-42d3-a456-426614174000", 1, 1)
	want := "recover-b9b9942729a6879f2266171a38aee8bd1180e34262584040608bc5464eff150c"
	if got != want {
		t.Fatalf("advertised id = %s, want %s", got, want)
	}
}

// TestWorktreeRecoverCLIBoundJSONExactOutput pins the fresh bound recovery
// and its exact receipt replay: the §9.4 recover projection — the
// provision/remove shape with sagaToken omitted entirely — the real bind
// effect, and the identical historical result with no second effect.
func TestWorktreeRecoverCLIBoundJSONExactOutput(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, _ := provisionCLISeed(t, dir, "recover bound", "wt.rec.bound")
	base := gitRevParseMain(t, dir)
	// Crash between the planning transaction and the effect: the saga rests
	// pending at generation zero.
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.rec.bound")
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)

	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 {
		t.Fatalf("recover exit %d, stderr: %s", code, stderr)
	}
	wantPath := filepath.Join(dir, ".worktrees", "main-1")
	got := decodeRecoverJSON(t, stdout)
	if got.SagaID != plan.SagaID || got.Operation != "provision" || got.Status != "bound" ||
		got.RunID == nil || *got.RunID != runID || got.OwnerBoundary != dir ||
		got.Path != wantPath || got.Branch != "tasks/main/1" || got.BaseCommit == nil ||
		*got.BaseCommit != base || got.RecoveryGeneration != 1 ||
		got.RecoverRequestID != nil || got.BrokerRequest != nil {
		t.Fatalf("bound recover result = %+v", got)
	}
	// Exact output contract: the §9.4 recover projection and nothing else,
	// in order, with no sagaToken member.
	want := fmt.Sprintf("{\n  \"sagaId\": %d,\n  \"operation\": \"provision\",\n  \"status\": \"bound\",\n  \"runId\": %d,\n  \"ownerBoundary\": %q,\n  \"path\": %q,\n  \"branch\": \"tasks/main/1\",\n  \"baseCommit\": %q,\n  \"recoveryGeneration\": 1,\n  \"recoverRequestId\": null,\n  \"brokerRequest\": null\n}\n",
		plan.SagaID, runID, dir, wantPath, base)
	if stdout != want {
		t.Fatalf("bound recover JSON =\n%s\nwant\n%s", stdout, want)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &fields); err != nil || len(fields) != 11 {
		t.Fatalf("recover JSON fields = %d, %v", len(fields), err)
	}
	if _, ok := fields["sagaToken"]; ok {
		t.Fatal("recover JSON carries sagaToken")
	}
	// The real effect happened exactly once.
	if !gitWorktreeRegistered(t, dir, wantPath) {
		t.Fatal("recovery did not bind the worktree")
	}
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "bound" || generation != 1 {
		t.Fatalf("row after recovery = %s/%d", status, generation)
	}

	// Exact receipt replay: the same historical result, no second effect.
	code, replay, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 || replay != stdout {
		t.Fatalf("replay exit %d\n%s\nstderr %s\nwant\n%s", code, replay, stderr, stdout)
	}
	if !gitWorktreeRegistered(t, dir, wantPath) {
		t.Fatal("replay lost the bound worktree")
	}
}

// TestWorktreeRecoverCLIBoundHuman pins the human bound recovery and its
// replay: the non-secret fields, neither a saga token nor a broker request.
func TestWorktreeRecoverCLIBoundHuman(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, _ := provisionCLISeed(t, dir, "recover human", "wt.rec.human")
	base := gitRevParseMain(t, dir)
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.rec.human")
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)

	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 0 {
		t.Fatalf("recover exit %d, stderr: %s", code, stderr)
	}
	want := fmt.Sprintf("sagaId=%d operation=provision status=bound runId=%d ownerBoundary=%s path=%s branch=tasks/main/1 baseCommit=%s recoveryGeneration=1\n",
		plan.SagaID, runID, dir, filepath.Join(dir, ".worktrees", "main-1"), base)
	if stdout != want {
		t.Fatalf("human recover = %q, want %q", stdout, want)
	}
	if fields := strings.Fields(strings.TrimSpace(stdout)); len(fields) != 9 {
		t.Fatalf("human fields = %d: %q", len(fields), stdout)
	}
	if strings.Contains(stdout, "sagaToken") || strings.Contains(stdout, "brokerRequest") ||
		strings.Contains(stdout, "recoverRequestId") {
		t.Fatalf("human recover leaked a non-authorized field: %q", stdout)
	}

	// Replay keeps the same non-secret human result.
	code, replay, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 0 || replay != stdout {
		t.Fatalf("human replay exit %d stdout %q stderr %s", code, replay, stderr)
	}
}

// TestWorktreeRecoverCLIRemoved pins the removal recovery through the full
// CLI drive: a pending removal saga (crash between plan and effect) recovers
// to removed with the target gone, the retained branch intact, and the exact
// receipt replay returning the identical historical result.
func TestWorktreeRecoverCLIRemoved(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)
	plan := recoverPlanRemoval(t, dir, fixture, "wt.rec.remove")
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)

	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 {
		t.Fatalf("recover exit %d, stderr: %s", code, stderr)
	}
	got := decodeRecoverJSON(t, stdout)
	if got.Operation != "remove" || got.Status != "removed" || got.SagaID != plan.SagaID ||
		got.RunID == nil || *got.RunID != fixture.runID || got.Path != fixture.path ||
		got.Branch != fixture.branch || got.BaseCommit == nil || got.RecoveryGeneration != 1 ||
		got.RecoverRequestID != nil || got.BrokerRequest != nil {
		t.Fatalf("removed recover result = %+v", got)
	}
	if strings.Contains(stdout, "sagaToken") {
		t.Fatal("recover JSON carries sagaToken")
	}
	if _, err := os.Stat(fixture.path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	if !worktreeBranchPresent(t, dir, fixture.branch) {
		t.Fatal("recovery deleted the retained branch")
	}

	// Human mode renders the non-secret fields and exits 0.
	code, human, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 0 || !strings.Contains(human, "operation=remove status=removed") ||
		!strings.Contains(human, "recoveryGeneration=1") || strings.Contains(human, "sagaToken") {
		t.Fatalf("human removed = exit %d stdout %q stderr %q", code, human, stderr)
	}

	// Exact receipt replay: identical JSON, no effect.
	code, replay, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 || replay != stdout {
		t.Fatalf("removed replay exit %d\n%s\nstderr %s", code, replay, stderr)
	}
	if !worktreeBranchPresent(t, dir, fixture.branch) {
		t.Fatal("replay deleted the retained branch")
	}
}

// TestWorktreeRecoverCLIFailedFreshAndReplay pins the §9.4 failed row
// through recover: a pending provision whose run terminalized before §9.4's
// parent flip compensates to proven absence and terminalizes failed — fresh
// and replay, both modes exit 1 with empty stdout and the stderr-only
// diagnostic.
func TestWorktreeRecoverCLIFailedFreshAndReplay(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, _ := provisionCLISeed(t, dir, "recover failed", "wt.rec.fail")
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.rec.fail")
	provisionTerminalizeRun(t, dir, runID)
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)
	absent := "noise-tasks: worktree recovery failed; the requested effect is proven absent\n"

	for _, jsonOut := range []bool{false, true} {
		extra := []string{}
		if jsonOut {
			extra = []string{"--json"}
		}
		code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, extra...)...)
		if code != 1 || stdout != "" || stderr != absent {
			t.Fatalf("failed fresh jsonOut=%v = exit %d stdout %q stderr %q", jsonOut, code, stdout, stderr)
		}
		// The exact receipt replay keeps the mode-specific class: exit 1,
		// empty stdout, diagnostic stderr-only.
		code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, extra...)...)
		if code != 1 || stdout != "" || stderr != absent {
			t.Fatalf("failed replay jsonOut=%v = exit %d stdout %q stderr %q", jsonOut, code, stdout, stderr)
		}
	}
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "failed" || generation != 1 {
		t.Fatalf("row after failed recovery = %s/%d", status, generation)
	}
}

// TestWorktreeRecoverCLIFailedDirtyCause pins the bounded §9.5 diagnostic of
// a committed failed removal recovery: a target that became dirty between
// planning and recovery commits failed with the observation cause on stderr;
// the replay keeps the exit-1 empty-stdout contract with the pinned
// nil-cause line.
func TestWorktreeRecoverCLIFailedDirtyCause(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)
	plan := recoverPlanRemoval(t, dir, fixture, "wt.rec.dirty")
	if err := os.WriteFile(filepath.Join(fixture.path, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)

	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: worktree has uncommitted changes: "+fixture.path+"\n" {
		t.Fatalf("dirty fresh = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(fixture.path, "dirty.txt")); err != nil {
		t.Fatalf("dirty target disturbed by the refused recovery: %v", err)
	}
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "failed" || generation != 1 {
		t.Fatalf("row after dirty recovery = %s/%d", status, generation)
	}

	// The replay has no observation cause: the pinned failed line instead.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: worktree recovery failed; the requested effect is proven absent\n" {
		t.Fatalf("dirty replay = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestWorktreeRecoverCLICleanupRequired pins the committed cleanup-required
// recovery and its replay: an operator commit left the effect unproved, so
// the compensation commits cleanup-required — exit-0 mutation result with a
// non-null recoverRequestId and null brokerRequest under --json, the pinned
// exit-1 line with empty stdout for human invocation.
func TestWorktreeRecoverCLICleanupRequired(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, _ := provisionCLISeed(t, dir, "recover cleanup", "wt.rec.clean")
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.rec.clean")
	if outcome, err := store.EffectWorktreeProvision(dir, dir, plan); err != nil || outcome != store.WorktreeProvisionBound {
		t.Fatalf("effect = %v, %v", outcome, err)
	}
	// Operator work inside the worktree moves its HEAD off the immutable
	// base, so the compensation can never prove exact absence.
	gitCLIOutput(t, plan.TargetPath, "-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "operator work", "--no-gpg-sign")
	provisionTerminalizeRun(t, dir, runID)
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)
	next := advertisedRecoverID(t, dir, plan.SagaID, 2)

	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 || stderr != "" {
		t.Fatalf("cleanup fresh --json = exit %d stderr %q", code, stderr)
	}
	got := decodeRecoverJSON(t, stdout)
	if got.Status != "cleanup-required" || got.RecoveryGeneration != 1 ||
		got.RecoverRequestID == nil || *got.RecoverRequestID != next || got.BrokerRequest != nil {
		t.Fatalf("cleanup fresh result = %+v", got)
	}
	// The unproved residue is the branch the operator moved off the
	// immutable base: it survives, still not at the base.
	if !worktreeBranchPresent(t, dir, plan.Branch) {
		t.Fatal("unproved compensation deleted the moved branch")
	}
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "cleanup-required" || generation != 1 {
		t.Fatalf("row after cleanup recovery = %s/%d", status, generation)
	}

	// Human invocation: exit 1, empty stdout, exactly the pinned line.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 1 || stdout != "" || stderr != "noise-tasks: worktree cleanup is required\n" {
		t.Fatalf("cleanup fresh human = exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Exact receipt replay returns the historical result at its recorded
	// generation with the same class per mode.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 {
		t.Fatalf("cleanup replay --json exit %d stderr %q", code, stderr)
	}
	got = decodeRecoverJSON(t, stdout)
	if got.Status != "cleanup-required" || got.RecoveryGeneration != 1 ||
		got.RecoverRequestID == nil || *got.RecoverRequestID != next || got.BrokerRequest != nil {
		t.Fatalf("cleanup replay result = %+v", got)
	}
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 1 || stdout != "" || stderr != "noise-tasks: worktree cleanup is required\n" {
		t.Fatalf("cleanup replay human = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestWorktreeRecoverCLICleanupUnprovedNoBurn pins §9.4's no-burn exception
// through the CLI: an unproved cleanup-required provision attempt commits
// nothing — no CAS, receipt, event, or generation — and returns the
// unchanged handoff still advertising the identical request id.
func TestWorktreeRecoverCLICleanupUnprovedNoBurn(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "noburn", "wt.rec.noburn")
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.rec.noburn")
	// A branch pointing off the immutable base is no longer uniquely
	// attributable, so absence can never be proved. Advancing main after
	// planning leaves the saga's stored base one commit behind.
	gitCLIOutput(t, dir, "-c", "user.email=test@example.invalid", "-c", "user.name=test",
		"commit", "--allow-empty", "-m", "advance main", "--no-gpg-sign")
	gitCLIOutput(t, dir, "branch", plan.Branch, "main")
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// Run cancel applies §9.4's automatic cleanup: the pending provision
	// flips to cleanup-required at generation zero.
	if _, _, err := db.CancelRun(runID, "agent-a", claimToken, attempt, attemptToken, false, "run.cancel.rec.noburn"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "cleanup-required" || generation != 0 {
		t.Fatalf("row after cancel = %s/%d", status, generation)
	}
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)

	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 {
		t.Fatalf("noburn --json exit %d stderr %q", code, stderr)
	}
	got := decodeRecoverJSON(t, stdout)
	if got.Status != "cleanup-required" || got.RecoveryGeneration != 0 ||
		got.RecoverRequestID == nil || *got.RecoverRequestID != request || got.BrokerRequest != nil {
		t.Fatalf("noburn handoff = %+v", got)
	}
	// Nothing committed: the row and the absent receipt prove no burn.
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "cleanup-required" || generation != 0 {
		t.Fatalf("row after noburn = %s/%d", status, generation)
	}
	db, err = store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if receipt, err := db.FindReceipt(store.ReceiptKindWorktree, request); err != nil || receipt != nil {
		t.Fatalf("noburn receipt = %v, %v", receipt, err)
	}
	// Human invocation keeps the pinned cleanup line; the identical --json
	// request returns the same unchanged handoff again.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 1 || stdout != "" || stderr != "noise-tasks: worktree cleanup is required\n" {
		t.Fatalf("noburn human = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 {
		t.Fatalf("noburn retry exit %d stderr %q", code, stderr)
	}
	if again := decodeRecoverJSON(t, stdout); again.RecoveryGeneration != 0 ||
		again.RecoverRequestID == nil || *again.RecoverRequestID != request {
		t.Fatalf("noburn retry handoff = %+v", again)
	}
}

// functionalRecoverBroker is the §9.5 fake broker the subprocess test stages:
// it validates its single verb argument, reads the minted capability from its
// inherited fd 3, and performs the registration effect itself.
const functionalRecoverBroker = `#!/bin/sh
if [ "$#" -ne 1 ]; then echo bad-argv >&2; exit 7; fi
if ! IFS= read -r target <&3; then echo missing-fd3 >&2; exit 8; fi
IFS= read -r branch <&3 || exit 8
IFS= read -r ref <&3 || exit 8
exec 3<&-
reg="$(git rev-parse --git-common-dir)/worktrees"
chmod u+w "$reg"
if git show-ref --verify --quiet "refs/heads/$branch"; then
  git worktree add -- "$target" "$branch"
else
  git worktree add -b "$branch" -- "$target" "$ref"
fi
status=$?
chmod u-w "$reg"
exit "$status"
`

// buildRecoverCLI links the real CLI against one fake libexec root, so the
// staged noise-worktree-broker is exactly what the strict §5.2 resolution
// finds in the subprocess.
func buildRecoverCLI(t *testing.T, libexecRoot string) string {
	t.Helper()
	_, thisFile, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(thisFile)
	bin := filepath.Join(t.TempDir(), "noise-tasks")
	build := exec.Command("go", "build", "-o", bin,
		"-ldflags", "-X github.com/noisefloor-oss/tasks/internal/libexec.Root="+libexecRoot, ".")
	build.Dir = pkgDir
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}
	return bin
}

// runRecoverCLI runs the built binary once, optionally placing one minted
// capability document on inherited fd 3.
func runRecoverCLI(t *testing.T, bin, dir string, capability string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if capability != "" {
		capPath := filepath.Join(t.TempDir(), "capability")
		if err := os.WriteFile(capPath, []byte(capability), 0o600); err != nil {
			t.Fatal(err)
		}
		cap, err := os.Open(capPath)
		if err != nil {
			t.Fatal(err)
		}
		defer cap.Close()
		// ExtraFiles maps the first entry onto the child's fd 3 (§9.5).
		cmd.ExtraFiles = []*os.File{cap}
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	err := cmd.Run()
	code := 0
	if exit, ok := err.(*exec.ExitError); ok {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatalf("run CLI: %v\n%s", err, errBuf.String())
	}
	return code, out.String(), errBuf.String()
}

// TestWorktreeRecoverCLINeedsBrokerFakeBroker pins the full §9.4/§9.5
// needs-broker flow through the real binary: the controller mints the
// capability, places it only on fd 3, and the delegated create binds the
// saga; the exact receipt replay needs no capability and returns the
// identical historical result, whose human form renders no token.
func TestWorktreeRecoverCLINeedsBrokerFakeBroker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("confined registration requires an unprivileged uid")
	}
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "recover fakebroker", "wt.rec.fake")
	base := gitRevParseMain(t, dir)
	confineCLIRegistration(t, dir)

	code, stdout, stderr := runCmd(cmdWorktree, append(provisionArgs(runID, claimToken, attemptToken, attempt, "wt.rec.fake"), "--json")...)
	if code != 0 {
		t.Fatalf("confined provision exit %d, stderr: %s", code, stderr)
	}
	var handoff struct {
		recoverJSON
		SagaToken int64 `json:"sagaToken"`
	}
	if err := json.Unmarshal([]byte(stdout), &handoff); err != nil {
		t.Fatalf("confined provision result not JSON: %v\n%s", err, stdout)
	}
	if handoff.Status != "needs-broker" || handoff.SagaToken <= 0 ||
		handoff.RecoverRequestID == nil || handoff.BrokerRequest == nil {
		t.Fatalf("confined provision handoff = %+v", handoff)
	}
	request := *handoff.RecoverRequestID

	// The staged broker under a linked libexec root.
	libexecRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(libexecRoot, "noise-worktree-broker"),
		[]byte(functionalRecoverBroker), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := buildRecoverCLI(t, libexecRoot)

	// With no minted capability document reachable on fd 3, the delegated
	// attempt reaches no reconciled outcome: the operational §9.5 refusal,
	// exit 1 with empty stdout, commits nothing, and the identical request
	// may be retried after minting.
	code, out, errOut := runRecoverCLI(t, bin, dir, "",
		"worktree", "recover", strconv.FormatInt(handoff.SagaID, 10),
		"--saga-token", strconv.FormatInt(handoff.SagaToken, 10),
		"--request-id", request, "--json")
	if code != 1 || out != "" || errOut != "noise-tasks: worktree broker refused: broker exited 8\n" {
		t.Fatalf("no-capability recover = exit %d stdout %q stderr %q", code, out, errOut)
	}
	if status, generation := recoverSagaRow(t, dir, handoff.SagaID); status != "needs-broker" || generation != 0 {
		t.Fatalf("row after no-capability = %s/%d", status, generation)
	}
	// The §9.5 capability document: target, branch, and base ref.
	capability := handoff.BrokerRequest.TargetPath + "\n" +
		handoff.BrokerRequest.Branch + "\n" + *handoff.BrokerRequest.BaseCommit + "\n"

	code, out, errOut = runRecoverCLI(t, bin, dir, capability,
		"worktree", "recover", strconv.FormatInt(handoff.SagaID, 10),
		"--saga-token", strconv.FormatInt(handoff.SagaToken, 10),
		"--request-id", request, "--json")
	if code != 0 {
		t.Fatalf("broker recover exit %d, stderr: %s", code, errOut)
	}
	got := decodeRecoverJSON(t, out)
	if got.Status != "bound" || got.RecoveryGeneration != 1 ||
		got.RecoverRequestID != nil || got.BrokerRequest != nil {
		t.Fatalf("broker-bound recover = %s", out)
	}
	if strings.Contains(out, "sagaToken") {
		t.Fatalf("broker-bound recover leaked the saga token: %s", out)
	}
	if !gitWorktreeRegistered(t, dir, handoff.BrokerRequest.TargetPath) {
		t.Fatal("broker create did not register the worktree")
	}
	if head := strings.TrimSpace(gitCLIOutput(t, dir, "rev-parse", handoff.BrokerRequest.Branch)); head != base {
		t.Fatalf("broker-created branch = %s, want the immutable base %s", head, base)
	}

	// Exact receipt replay through the same binary without fd 3: no effect,
	// the identical historical result.
	code, replay, errOut := runRecoverCLI(t, bin, dir, "",
		"worktree", "recover", strconv.FormatInt(handoff.SagaID, 10),
		"--saga-token", strconv.FormatInt(handoff.SagaToken, 10),
		"--request-id", request, "--json")
	if code != 0 || replay != out {
		t.Fatalf("broker replay = exit %d\n%s\nstderr %s", code, replay, errOut)
	}

	// The human replay needs no capability either: non-secret fields only.
	code, human, stderr := runCmd(cmdWorktree, recoverArgs(handoff.SagaID, handoff.SagaToken, request)...)
	if code != 0 || !strings.Contains(human, "status=bound") ||
		strings.Contains(human, "sagaToken") || strings.Contains(human, "brokerRequest") {
		t.Fatalf("human broker replay = exit %d stdout %q stderr %q", code, human, stderr)
	}
}

// TestWorktreeRecoverCLIExhaustion pins the §9.4 generation bound: a
// nonterminal saga at SQLite's maximum signed recovery generation refuses a
// new raw recovery with the exhaustion diagnostic before any effect, in both
// output modes.
func TestWorktreeRecoverCLIExhaustion(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, _ := provisionCLISeed(t, dir, "exhausted", "wt.rec.exh")
	plan := provisionHostPlan(t, dir, runID, claimToken, attemptToken, "wt.rec.exh")
	provisionExec(t, dir, `UPDATE worktree_sagas SET recovery_generation=9223372036854775807 WHERE saga_id=?`, plan.SagaID)
	want := "noise-tasks: worktree recovery generation exhausted\n"

	for _, extra := range [][]string{{}, {"--json"}} {
		code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, "recover-"+strings.Repeat("a", 64), extra...)...)
		if code != 1 || stdout != "" || stderr != want {
			t.Fatalf("exhausted %v = exit %d stdout %q stderr %q", extra, code, stdout, stderr)
		}
	}
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "pending" || generation != 9223372036854775807 {
		t.Fatalf("row after exhaustion = %s/%d", status, generation)
	}
}

// TestWorktreeRecoverCLIExhaustedNonterminalCommit pins the sole §9.4 null
// exception: a recovery from generation max-1 may commit the maximum, and a
// still-nonterminal outcome there carries null recoverRequestId and null
// brokerRequest — the exit-0 mutation result under --json, the pinned
// confinement line for human invocation — after which a new raw recovery
// refuses with the exhaustion diagnostic.
func TestWorktreeRecoverCLIExhaustedNonterminalCommit(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("confined registration requires an unprivileged uid")
	}
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)
	plan := recoverPlanRemoval(t, dir, fixture, "wt.rec.max")
	provisionExec(t, dir, `UPDATE worktree_sagas SET recovery_generation=9223372036854775806 WHERE saga_id=?`, plan.SagaID)
	request := advertisedRecoverID(t, dir, plan.SagaID, 9223372036854775807)
	confineCLIRegistration(t, dir)

	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 0 {
		t.Fatalf("max-commit --json exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "worktree registration is confined and no broker is available") {
		t.Fatalf("max-commit --json stderr = %q", stderr)
	}
	got := decodeRecoverJSON(t, stdout)
	boundary, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != "remove" || got.Status != "needs-broker" ||
		got.RunID == nil || *got.RunID != fixture.runID || got.OwnerBoundary != boundary ||
		got.Path != fixture.path || got.Branch != fixture.branch || got.BaseCommit == nil ||
		got.RecoveryGeneration != 9223372036854775807 ||
		got.RecoverRequestID != nil || got.BrokerRequest != nil {
		t.Fatalf("max-commit result = %+v", got)
	}
	// The sole §9.4 null exception renders literally: no wrapped or
	// saturated id is advertised, and the saga token stays omitted.
	if tail := "  \"recoveryGeneration\": 9223372036854775807,\n  \"recoverRequestId\": null,\n  \"brokerRequest\": null\n}\n"; !strings.HasSuffix(stdout, tail) {
		t.Fatalf("max-commit JSON tail =\n%s", stdout)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &fields); err != nil || len(fields) != 11 {
		t.Fatalf("max-commit JSON fields = %d, %v", len(fields), err)
	}
	if _, ok := fields["sagaToken"]; ok {
		t.Fatal("recover JSON carries sagaToken")
	}
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "needs-broker" || generation != 9223372036854775807 {
		t.Fatalf("row after max commit = %s/%d", status, generation)
	}
	// The staged target was restored: the confined attempt did not delete it.
	if _, err := os.Stat(filepath.Join(fixture.path, ".git")); err != nil {
		t.Fatalf("confined recovery lost the staged target: %v", err)
	}

	// Human invocation: the pinned confinement line, exit 1, empty stdout.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request)...)
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: worktree registration is confined and no broker is available\n" {
		t.Fatalf("max-commit human = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A new raw recovery at the maximum refuses before any effect.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, "wt.rec.aftermax")...)
	if code != 1 || stdout != "" || stderr != "noise-tasks: worktree recovery generation exhausted\n" {
		t.Fatalf("after-max = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestWorktreeRecoverCLIGrammarAndRefusals pins the local grammar: the
// command accepts exactly one saga positional with --saga-token and
// --request-id — every owner/run/attempt/path/branch/base/force override is
// an unknown-flag usage error, --file and extra positionals refuse, and an
// unknown saga is the operational not-found refusal.
func TestWorktreeRecoverCLIGrammarAndRefusals(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	base := []string{"recover", "1", "--saga-token", "5", "--request-id", "wt.rec.grammar"}
	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"no positional", []string{"recover", "--saga-token", "5", "--request-id", "wt.x"}, 2, "usage: noise-tasks worktree recover <saga>"},
		{"bad saga", []string{"recover", "abc", "--saga-token", "5", "--request-id", "wt.x"}, 2, "saga id must be a positive integer"},
		{"zero saga", []string{"recover", "0", "--saga-token", "5", "--request-id", "wt.x"}, 2, "saga id must be a positive integer"},
		{"missing token", []string{"recover", "1", "--request-id", "wt.x"}, 2, "--saga-token must be a positive integer"},
		{"zero token", []string{"recover", "1", "--saga-token", "0", "--request-id", "wt.x"}, 2, "--saga-token must be a positive integer"},
		{"missing request", []string{"recover", "1", "--saga-token", "5"}, 2, "--request-id must match"},
		{"bad request grammar", []string{"recover", "1", "--saga-token", "5", "--request-id", "bad id!"}, 2, "--request-id must match"},
		{"file refused", append(append([]string(nil), base...), "--file", "x.json"), 2, "--file is not accepted for worktree recover"},
		{"extra positional", append(append([]string(nil), base...), "extra"), 2, "usage:"},
		{"owner refused", append(append([]string(nil), base...), "--owner", "agent-a"), 2, "flag provided but not defined: -owner"},
		{"force refused", append(append([]string(nil), base...), "--force"), 2, "flag provided but not defined: -force"},
		{"attempt refused", append(append([]string(nil), base...), "--attempt", "1"), 2, "flag provided but not defined: -attempt"},
		{"claim-token refused", append(append([]string(nil), base...), "--claim-token", "1"), 2, "flag provided but not defined: -claim-token"},
		{"attempt-token refused", append(append([]string(nil), base...), "--attempt-token", "1"), 2, "flag provided but not defined: -attempt-token"},
		{"path refused", append(append([]string(nil), base...), "--path", "/x"), 2, "flag provided but not defined: -path"},
		{"branch refused", append(append([]string(nil), base...), "--branch", "b"), 2, "flag provided but not defined: -branch"},
		{"base refused", append(append([]string(nil), base...), "--base", "c"), 2, "flag provided but not defined: -base"},
		{"tag refused", append(append([]string(nil), base...), "--tag", "main"), 2, "flag provided but not defined: -tag"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCmd(cmdWorktree, tc.args...)
		if code != tc.code || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Fatalf("%s: exit %d stdout %q stderr %q (want code %d with %q)",
				tc.name, code, stdout, stderr, tc.code, tc.want)
		}
	}

	// An unknown saga is the operational not-found refusal.
	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(999, 5, "wt.rec.unknown")...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "worktree saga 999 not found") {
		t.Fatalf("unknown saga = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// Invalid authorization is refused before any row is read.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(1, 0, "wt.rec.badauth")...)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "--saga-token must be a positive integer") {
		t.Fatalf("bad auth = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestWorktreeRecoverCLIConflicts pins the typed exit-3 refusals: a wrong
// saga fence, a request id that is not the advertised one, a receipt-missing
// request against a terminal saga, and the common §8.4 collision of a
// receipt encoding a different saga's recovery.
func TestWorktreeRecoverCLIConflicts(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t", "u")
	fixture := seedRemovalCLIRun(t, dir)
	plan := recoverPlanRemoval(t, dir, fixture, "wt.rec.conflict")

	// Wrong fence: the typed recovery conflict, empty stdout, both modes.
	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken+1, "wt.rec.wrongfence", "--json")...)
	if code != 3 || stdout != "" {
		t.Fatalf("wrong fence --json = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	var conflict struct {
		Kind   string `json:"kind"`
		SagaID int64  `json:"sagaId"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stderr), &conflict); err != nil ||
		conflict.Kind != "worktree-recovery-conflict" || conflict.SagaID != plan.SagaID ||
		!strings.Contains(conflict.Reason, "saga token does not match the saga fence") {
		t.Fatalf("wrong fence conflict = %q, %v", stderr, err)
	}
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken+1, "wt.rec.wrongfence")...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "saga token does not match the saga fence") {
		t.Fatalf("wrong fence human = exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// A request id that is not the one advertised for the next generation.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, "wt.rec.notadvertised")...)
	if code != 3 || stdout != "" ||
		!strings.Contains(stderr, "request id is not the one advertised for the next recovery generation") {
		t.Fatalf("not advertised = exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Commit one recovery, terminalizing the removal saga.
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)
	if code, _, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...); code != 0 {
		t.Fatalf("terminalizing recover exit %d stderr %q", code, stderr)
	}

	// A receipt-missing request against the terminal saga is exit 3.
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, "wt.rec.terminalmissing")...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "has no receipt") {
		t.Fatalf("terminal missing = exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// A receipt under that request id encoding another saga's recovery is
	// the common typed §8.4 collision: a second, pending provision saga in
	// the same family recovers with the removal saga's committed id.
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	claim, _, _, err := db.AcquireClaim("main", "2", "agent-a", 3600, "claim.rec.collide")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.StartRun("main", "2", "agent-a", claim.Token, "run.rec.collide", 3600)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	other := provisionHostPlan(t, dir, run.RunID, claim.Token, run.AttemptToken, "wt.rec.collide")
	code, stdout, stderr = runCmd(cmdWorktree, recoverArgs(other.SagaID, other.SagaToken, request)...)
	if code != 3 || stdout != "" ||
		!strings.Contains(stderr, "was already used for a different worktree request") {
		t.Fatalf("cross-saga collision = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestWorktreeRecoverCLIReceiptCorruption pins the fail-closed corruption
// refusal: a stored recovery receipt whose body no longer matches its digest
// is an operational failure with empty stdout, never a replay.
func TestWorktreeRecoverCLIReceiptCorruption(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "t")
	fixture := seedRemovalCLIRun(t, dir)
	plan := recoverPlanRemoval(t, dir, fixture, "wt.rec.corrupt")
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)
	if code, _, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...); code != 0 {
		t.Fatalf("terminalizing recover exit %d stderr %q", code, stderr)
	}

	// An altered receipt body fails the stored-row integrity validation.
	provisionExec(t, dir, `UPDATE receipts SET body=replace(body, '"status":"removed"', '"status":"bound"')
		WHERE kind='worktree' AND request_id=?`, request)
	code, stdout, stderr := runCmd(cmdWorktree, recoverArgs(plan.SagaID, plan.SagaToken, request, "--json")...)
	if code != 1 || stdout != "" ||
		!strings.Contains(stderr, "stored worktree receipt failed integrity validation") {
		t.Fatalf("corrupt receipt = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestReportWorktreeRecoverErrorTaxonomy pins the §9.4/§9.5 exit mapping of
// the recovery error taxonomy at the CLI mapper: the typed capability
// conflict is exit 3; a broker refusal, an uncertain broker result, and the
// generation-exhaustion refusal are operational exit-1 failures with empty
// stdout and bounded stderr diagnostics.
func TestReportWorktreeRecoverErrorTaxonomy(t *testing.T) {
	capability := &store.WorktreeRecoveryCapabilityConflict{SagaID: 7}
	for _, jsonOut := range []bool{false, true} {
		var stderr strings.Builder
		code := reportWorktreeRecoverError(&stderr, jsonOut, capability)
		if code != 3 {
			t.Fatalf("capability conflict jsonOut=%v = exit %d", jsonOut, code)
		}
		if jsonOut {
			var conflict struct {
				Kind   string `json:"kind"`
				SagaID int64  `json:"sagaId"`
			}
			if err := json.Unmarshal([]byte(stderr.String()), &conflict); err != nil ||
				conflict.Kind != "worktree-recovery-capability-conflict" || conflict.SagaID != 7 {
				t.Fatalf("capability conflict JSON = %q, %v", stderr.String(), err)
			}
		} else if !strings.Contains(stderr.String(), "recovery requires the broker capability on fd 3") {
			t.Fatalf("capability conflict human = %q", stderr.String())
		}
	}
	for _, err := range []error{
		&store.WorktreeRecoveryBrokerUncertain{SagaID: 7, Verb: "create"},
		&store.WorktreeBrokerRefusal{Reason: "not today"},
		store.ErrWorktreeRecoveryExhausted,
	} {
		var stderr strings.Builder
		if code := reportWorktreeRecoverError(&stderr, false, err); code != 1 ||
			!strings.HasPrefix(stderr.String(), "noise-tasks: ") {
			t.Fatalf("%v = exit %d stderr %q", err, code, stderr.String())
		}
	}
	var stderr strings.Builder
	if code := reportWorktreeRecoverError(&stderr, false, fmt.Errorf("wrap: %w", store.ErrWorktreeRecoveryExhausted)); code != 1 ||
		stderr.String() != "noise-tasks: wrap: worktree recovery generation exhausted\n" {
		t.Fatalf("wrapped exhaustion = exit %d stderr %q", code, stderr.String())
	}
}
