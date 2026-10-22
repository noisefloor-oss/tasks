package main

import (
	"crypto/sha256"
	"encoding/hex"
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

// finishProvisionedRun provisions task 1's worktree through the live §9.4
// surface and finishes its run as succeeded, which schedules the linked
// automatic cleanup saga against the bound provision.
func finishProvisionedRun(t *testing.T, dir, requestID string) (runID int64, path string) {
	t.Helper()
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "prune cli", requestID)
	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, requestID)...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	code, _, stderr = runCmd(cmdRun, "finish", strconv.FormatInt(runID, 10),
		"--owner", "agent-a", "--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10), "--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--outcome", "succeeded", "--request-id", "finish."+requestID)
	if code != 0 {
		t.Fatalf("run finish exit %d, stderr: %s", code, stderr)
	}
	return runID, filepath.Join(dir, ".worktrees", "main-1")
}

func TestWorktreePruneCLIDrivesScheduledCleanup(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	_, path := finishProvisionedRun(t, dir, "wt.prune")

	// Unknown filesystem entries — a directory and a file, whatever their
	// shape — are reported and never touched: shape alone is never
	// ownership proof (SPEC §9.4).
	notes := filepath.Join(dir, ".worktrees", "notes")
	if err := os.MkdirAll(notes, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, ".worktrees", "zz.txt")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var cleanupSaga int64
	provisionScan(t, dir, `SELECT saga_id FROM worktree_sagas WHERE operation='remove' AND status='pending'`, &cleanupSaga)

	code, stdout, stderr := runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	// Exact output contract: exactly results, brokerRequests, and unknown;
	// results ordered by saga id with exactly sagaId and committed status,
	// unknown as bytewise-sorted objects containing exactly path
	// (SPEC §9.4).
	want := fmt.Sprintf("{\n  \"results\": [\n    {\n      \"sagaId\": %d,\n      \"status\": \"removed\"\n    }\n  ],\n  \"brokerRequests\": [],\n  \"unknown\": [\n    {\n      \"path\": %q\n    },\n    {\n      \"path\": %q\n    }\n  ]\n}\n",
		cleanupSaga, notes, stray)
	if stdout != want {
		t.Fatalf("prune JSON =\n%s\nwant\n%s", stdout, want)
	}
	// The recovery removed the worktree and its registration; the branch is
	// retained and the unknown entries survive.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree directory still exists: %v", err)
	}
	if gitWorktreeRegistered(t, dir, path) {
		t.Fatal("worktree registration survived prune")
	}
	if !worktreeBranchPresent(t, dir, "tasks/main/1") {
		t.Fatal("prune deleted the retained branch")
	}
	if _, err := os.Stat(notes); err != nil {
		t.Fatalf("unknown directory was removed: %v", err)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("unknown file was removed: %v", err)
	}
	var sourceActive int
	provisionScan(t, dir, `SELECT binding_active FROM worktree_sagas WHERE operation='provision'`, &sourceActive)
	if sourceActive != 0 {
		t.Fatal("removed cleanup left the source provision active")
	}

	// Human rerun is idempotent: nothing is eligible, and no saga token or
	// mint binding is ever printed.
	code, stdout, stderr = runCmd(cmdWorktree, "prune")
	if code != 0 {
		t.Fatalf("prune rerun exit %d, stderr: %s", code, stderr)
	}
	if stdout != "unknown path="+notes+"\nunknown path="+stray+"\npruned: 0 sagas, 0 broker handoffs\n" {
		t.Fatalf("prune rerun human output = %q", stdout)
	}
}

// TestWorktreePruneCLIVerifyAfterAutomaticCleanup pins the writer/validator
// agreement on the automatic cleanup receipt end to end: provision, run
// finish succeeded scheduling the run-caused cleanup, prune driving it to
// removed, and verify staying green over the stored original receipt whose
// input digest carries no owner.
func TestWorktreePruneCLIVerifyAfterAutomaticCleanup(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	finishProvisionedRun(t, dir, "wt.verify")
	code, _, stderr := runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	code, _, stderr = runCmd(verifyMain)
	if code != 0 {
		t.Fatalf("verify exit %d, stderr: %s", code, stderr)
	}
}

// pruneFamilyID reads the store's family identity for golden expectations.
func pruneFamilyID(t *testing.T, dir string) string {
	t.Helper()
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	familyID, _, err := db.Identity()
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	return familyID
}

// pruneRecoverID derives the §9.4 deterministic recovery request id the
// same way the store does, so a golden can pin the advertised generation.
func pruneRecoverID(t *testing.T, dir string, sagaID, generation int64) string {
	t.Helper()
	sum := sha256.Sum256([]byte("noise-tasks-worktree-recover-v1\x00" + pruneFamilyID(t, dir) + "\x00" +
		strconv.FormatInt(sagaID, 10) + "\x00" + strconv.FormatInt(generation, 10)))
	return "recover-" + hex.EncodeToString(sum[:])
}

// TestWorktreePruneCLIBrokerHandoff pins the nonempty brokerRequests
// surface: a permission-confined registration write commits needs-broker,
// and the controller-only JSON handoff carries the exact flattened field
// order — sagaId, sagaToken, then the §9.4 broker request fields in order —
// under the currently advertised generation. The saga token appears only
// here: human prune and every list surface omit it (SPEC §9.4).
func TestWorktreePruneCLIBrokerHandoff(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	_, path := finishProvisionedRun(t, dir, "wt.prune.broker")
	base := gitRevParseMain(t, dir)

	var cleanupSaga, sagaToken int64
	var registrationRoot string
	provisionScan(t, dir, `SELECT saga_id FROM worktree_sagas WHERE operation='remove' AND status='pending'`, &cleanupSaga)
	provisionScan(t, dir, `SELECT saga_fence FROM worktree_sagas WHERE saga_id=?`, &sagaToken, cleanupSaga)
	provisionScan(t, dir, `SELECT registration_root FROM worktree_sagas WHERE saga_id=?`, &registrationRoot, cleanupSaga)
	token := strconv.FormatInt(sagaToken, 10)

	confineCLIRegistration(t, dir)

	code, stdout, stderr := runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	// The prune committed generation 1, so the handoff advertises the
	// deterministic id for generation 2.
	want := fmt.Sprintf("{\n  \"results\": [\n    {\n      \"sagaId\": %d,\n      \"status\": \"needs-broker\"\n    }\n  ],\n  \"brokerRequests\": [\n    {\n      \"sagaId\": %d,\n      \"sagaToken\": %d,\n      \"familyId\": %q,\n      \"verb\": \"remove\",\n      \"registrationRoot\": %q,\n      \"ownerBoundary\": %q,\n      \"targetPath\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": %q,\n      \"force\": false,\n      \"recoverRequestId\": %q\n    }\n  ],\n  \"unknown\": []\n}\n",
		cleanupSaga, cleanupSaga, sagaToken, pruneFamilyID(t, dir), registrationRoot, dir, path, base,
		pruneRecoverID(t, dir, cleanupSaga, 2))
	if stdout != want {
		t.Fatalf("prune JSON =\n%s\nwant\n%s", stdout, want)
	}
	var status string
	var generation int64
	provisionScan(t, dir, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, cleanupSaga)
	provisionScan(t, dir, `SELECT recovery_generation FROM worktree_sagas WHERE saga_id=?`, &generation, cleanupSaga)
	if status != "needs-broker" || generation != 1 {
		t.Fatalf("committed saga = %s generation %d; the advertised id must be generation 2", status, generation)
	}
	// The confined effect restored the staged worktree and kept the branch.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("confined prune lost the worktree: %v", err)
	}
	if !worktreeBranchPresent(t, dir, "tasks/main/1") {
		t.Fatal("confined prune deleted the retained branch")
	}

	// Human prune reruns the now needs-broker row without a broker
	// capability: the reconciliation is refused (stderr-only), the summary
	// counts the re-emitted handoff, and stdout never prints a saga token
	// or mint binding.
	code, stdout, stderr = runCmd(cmdWorktree, "prune")
	if code != 0 {
		t.Fatalf("human prune exit %d, stderr: %s", code, stderr)
	}
	wantHuman := fmt.Sprintf("sagaId=%d status=needs-broker\npruned: 1 sagas, 1 broker handoffs\n", cleanupSaga)
	if stdout != wantHuman {
		t.Fatalf("human prune output = %q, want %q", stdout, wantHuman)
	}
	if strings.Contains(stdout, token) || strings.Contains(stdout, "sagaToken") ||
		strings.Contains(stdout, "recoverRequestId") {
		t.Fatalf("human prune leaked a token or mint binding: %q", stdout)
	}

	// List never exposes the fence, in either mode.
	code, stdout, stderr = runCmd(cmdWorktree, "list", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, token) || strings.Contains(stdout, "sagaToken") {
		t.Fatalf("list --json leaked the saga token: %s", stdout)
	}
	code, stdout, stderr = runCmd(cmdWorktree, "list")
	if code != 0 {
		t.Fatalf("list human exit %d, stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, token) {
		t.Fatalf("list human leaked the saga token: %q", stdout)
	}
}

// TestWorktreePruneCLIReemitsBrokerHandoff pins §9.4's handoff durability:
// once a saga is committed needs-broker, a separate later prune --json that
// cannot itself reconcile the broker step — what a human prune or a lost
// first response leaves behind — still re-emits the exact token-bearing
// handoff at the currently advertised generation; emission never depends on
// a successful reconciliation (SPEC §9.4).
func TestWorktreePruneCLIReemitsBrokerHandoff(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	_, path := finishProvisionedRun(t, dir, "wt.prune.reemit")
	base := gitRevParseMain(t, dir)

	var cleanupSaga, sagaToken int64
	var registrationRoot string
	provisionScan(t, dir, `SELECT saga_id FROM worktree_sagas WHERE operation='remove' AND status='pending'`, &cleanupSaga)
	provisionScan(t, dir, `SELECT saga_fence FROM worktree_sagas WHERE saga_id=?`, &sagaToken, cleanupSaga)
	provisionScan(t, dir, `SELECT registration_root FROM worktree_sagas WHERE saga_id=?`, &registrationRoot, cleanupSaga)

	confineCLIRegistration(t, dir)

	// First run: the confined registration write commits needs-broker at
	// generation 1 and hands off the mint advertising generation 2.
	code, stdout, stderr := runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	handoff := fmt.Sprintf("{\n  \"results\": [\n    {\n      \"sagaId\": %d,\n      \"status\": \"needs-broker\"\n    }\n  ],\n  \"brokerRequests\": [\n    {\n      \"sagaId\": %d,\n      \"sagaToken\": %d,\n      \"familyId\": %q,\n      \"verb\": \"remove\",\n      \"registrationRoot\": %q,\n      \"ownerBoundary\": %q,\n      \"targetPath\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": %q,\n      \"force\": false,\n      \"recoverRequestId\": %q\n    }\n  ],\n  \"unknown\": []\n}\n",
		cleanupSaga, cleanupSaga, sagaToken, pruneFamilyID(t, dir), registrationRoot, dir, path, base,
		pruneRecoverID(t, dir, cleanupSaga, 2))
	if stdout != handoff {
		t.Fatalf("first prune JSON =\n%s\nwant\n%s", stdout, handoff)
	}

	// Separate later run without the fd-3 capability: the reconciliation
	// is refused and commits nothing, but the handoff is re-emitted
	// byte-identically at the still-advertised generation.
	code, stdout, stderr = runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune rerun exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "prune recovery") {
		t.Fatalf("rerun stderr = %q, want the refused-recovery diagnostic", stderr)
	}
	if stdout != handoff {
		t.Fatalf("prune rerun JSON =\n%s\nwant the same handoff\n%s", stdout, handoff)
	}
	var status string
	var generation int64
	provisionScan(t, dir, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, cleanupSaga)
	provisionScan(t, dir, `SELECT recovery_generation FROM worktree_sagas WHERE saga_id=?`, &generation, cleanupSaga)
	if status != "needs-broker" || generation != 1 {
		t.Fatalf("refused rerun committed %s generation %d; nothing may change", status, generation)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("refused rerun lost the worktree: %v", err)
	}
}

// TestWorktreePruneCLILegacyConfinedHandoff pins the legacy cleanup's
// prune-registration verb: the runless row can store no registration root,
// so the mint binding carries the root derived at effect time — and only
// there, never persisted into the row (SPEC §9.4).
func TestWorktreePruneCLILegacyConfinedHandoff(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	sagaID, path := seedLegacyCleanupSaga(t, dir)
	confineCLIRegistration(t, dir)
	reg := filepath.Join(dir, ".git", "worktrees")

	var sagaToken int64
	provisionScan(t, dir, `SELECT saga_fence FROM worktree_sagas WHERE saga_id=?`, &sagaToken, sagaID)

	code, stdout, stderr := runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	want := fmt.Sprintf("{\n  \"results\": [\n    {\n      \"sagaId\": %d,\n      \"status\": \"needs-broker\"\n    }\n  ],\n  \"brokerRequests\": [\n    {\n      \"sagaId\": %d,\n      \"sagaToken\": %d,\n      \"familyId\": %q,\n      \"verb\": \"prune-registration\",\n      \"registrationRoot\": %q,\n      \"ownerBoundary\": %q,\n      \"targetPath\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": null,\n      \"force\": false,\n      \"recoverRequestId\": %q\n    }\n  ],\n  \"unknown\": []\n}\n",
		sagaID, sagaID, sagaToken, pruneFamilyID(t, dir), reg, dir, path,
		pruneRecoverID(t, dir, sagaID, 2))
	if stdout != want {
		t.Fatalf("prune JSON =\n%s\nwant\n%s", stdout, want)
	}
	// The effect-time-derived root is mint-binding-only: the row keeps
	// its stored NULL registration root.
	var stored int
	provisionScan(t, dir, `SELECT COUNT(*) FROM worktree_sagas WHERE saga_id=? AND registration_root IS NOT NULL`,
		&stored, sagaID)
	if stored != 0 {
		t.Fatal("legacy confined handoff persisted the derived registration root")
	}
	// The confined effect restored the staged legacy target.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("confined legacy prune lost the target: %v", err)
	}
	// A separate rerun cannot reconcile the broker step: nothing commits,
	// the handoff is re-emitted with the registration root derived at
	// emission time — every emitted broker request carries the exact
	// effect-time proof, while the stored row keeps its NULL root — and
	// the unconfined target half of the staged legacy removal completes
	// inside the owner boundary before the capability refusal
	// (SPEC §9.4/§9.5).
	code, stdout, stderr = runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune rerun exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "prune recovery") {
		t.Fatalf("legacy rerun stderr = %q, want the refused-recovery diagnostic", stderr)
	}
	want = fmt.Sprintf("{\n  \"results\": [\n    {\n      \"sagaId\": %d,\n      \"status\": \"needs-broker\"\n    }\n  ],\n  \"brokerRequests\": [\n    {\n      \"sagaId\": %d,\n      \"sagaToken\": %d,\n      \"familyId\": %q,\n      \"verb\": \"prune-registration\",\n      \"registrationRoot\": %q,\n      \"ownerBoundary\": %q,\n      \"targetPath\": %q,\n      \"branch\": \"tasks/main/1\",\n      \"baseCommit\": null,\n      \"force\": false,\n      \"recoverRequestId\": %q\n    }\n  ],\n  \"unknown\": []\n}\n",
		sagaID, sagaID, sagaToken, pruneFamilyID(t, dir), reg, dir, path,
		pruneRecoverID(t, dir, sagaID, 2))
	if stdout != want {
		t.Fatalf("legacy rerun JSON =\n%s\nwant\n%s", stdout, want)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy rerun left the target behind: %v", err)
	}
	var generation int64
	provisionScan(t, dir, `SELECT recovery_generation FROM worktree_sagas WHERE saga_id=?`, &generation, sagaID)
	if generation != 1 {
		t.Fatalf("refused legacy rerun committed generation %d; nothing may change", generation)
	}
	// The re-emitted derivation is still mint-binding-only: the row keeps
	// its stored NULL registration root after the rerun too.
	if stored != 0 {
		t.Fatal("legacy rerun persisted the derived registration root")
	}
}

// TestWorktreePruneCLIKnownPathFromEverySagaRow pins prune's unknown
// judgment against every saga row's target path, terminal rows included:
// an active bound provision with no cleanup saga is known workspace — not
// prune-eligible, and never reported unknown (SPEC §9.4).
func TestWorktreePruneCLIKnownPathFromEverySagaRow(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	runID, claimToken, attemptToken, attempt := provisionCLISeed(t, dir, "prune known", "wt.prune.known")
	code, _, stderr := runCmd(cmdWorktree, provisionArgs(runID, claimToken, attemptToken, attempt, "wt.prune.known")...)
	if code != 0 {
		t.Fatalf("provision exit %d, stderr: %s", code, stderr)
	}
	path := filepath.Join(dir, ".worktrees", "main-1")

	code, stdout, stderr := runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	if stdout != "{\n  \"results\": [],\n  \"brokerRequests\": [],\n  \"unknown\": []\n}\n" {
		t.Fatalf("prune JSON =\n%s\nwant empty results and unknown\n", stdout)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("prune touched a known bound workspace: %v", err)
	}

	code, stdout, stderr = runCmd(cmdWorktree, "prune")
	if code != 0 {
		t.Fatalf("human prune exit %d, stderr: %s", code, stderr)
	}
	if stdout != "pruned: 0 sagas, 0 broker handoffs\n" {
		t.Fatalf("human prune output = %q", stdout)
	}
}

// seedLegacyCleanupSaga replicates the v4→v5 conversion's durable legacy
// evidence (SPEC §9.4): a task row keeping its wt_* binding columns beside a
// runless sourceless cleanup-required removal saga whose request id is the
// pinned legacy hash, plus the real registered worktree both name.
func seedLegacyCleanupSaga(t *testing.T, dir string) (sagaID int64, path string) {
	t.Helper()
	seedWorktreeStore(t, dir, "legacy")
	path = filepath.Join(dir, ".worktrees", "main-1")
	if err := pruneGitWorktreeAdd(dir, path); err != nil {
		t.Fatalf("git worktree add: %v", err)
	}
	created := "2026-08-28T00:00:00Z"
	provisionExec(t, dir, `UPDATE tasks SET wt_boundary=?, wt_path=?, wt_branch=?, wt_created_at=?, wt_keep=0
		WHERE tag='main' AND id='1'`, dir, path, "tasks/main/1", created)
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	familyID, _, err := db.Identity()
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	// SPEC §9.4: auto- plus lowercase SHA-256 over the legacy domain.
	sum := sha256.Sum256([]byte("noise-tasks-worktree-auto-v1\x00" + familyID + "\x00legacy\x00main\x001\x00" + path + "\x00tasks/main/1"))
	requestID := "auto-" + hex.EncodeToString(sum[:])
	bumpProvisionFencing(t, dir)
	provisionExec(t, dir, `INSERT INTO worktree_sagas(
		run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
		request_id,recovery_generation,owner_boundary,registration_root,target_path,
		branch,base_commit,force,created_at,updated_at,extra)
		VALUES (NULL,NULL,'main','1','remove','cleanup-required',0,900000000000000001,?,0,?,NULL,?,?,NULL,0,?,?,NULL)`,
		requestID, dir, path, "tasks/main/1", created, created)
	provisionScan(t, dir, `SELECT saga_id FROM worktree_sagas WHERE request_id=?`, &sagaID, requestID)
	return sagaID, path
}

// pruneGitWorktreeAdd registers the legacy fixture's worktree on the
// retained branch.
func pruneGitWorktreeAdd(dir, path string) error {
	cmd := exec.Command("git", "worktree", "add", "-b", "tasks/main/1", path, "main")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v\n%s", err, out)
	}
	return nil
}

func TestWorktreePruneCLILegacyEvidence(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	sagaID, path := seedLegacyCleanupSaga(t, dir)

	// List renders the legacy row with its null runId and baseCommit
	// (SPEC §9.4).
	code, stdout, stderr := runCmd(cmdWorktree, "list", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var report struct {
		Entries []struct {
			SagaID     int64   `json:"sagaId"`
			RunID      *int64  `json:"runId"`
			Operation  string  `json:"operation"`
			Status     string  `json:"status"`
			BaseCommit *string `json:"baseCommit"`
			Force      bool    `json:"force"`
		} `json:"entries"`
	}
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		t.Fatalf("list --json not JSON: %v\n%s", err, stdout)
	}
	if len(report.Entries) != 1 || report.Entries[0].SagaID != sagaID ||
		report.Entries[0].RunID != nil || report.Entries[0].BaseCommit != nil ||
		report.Entries[0].Operation != "remove" || report.Entries[0].Status != "cleanup-required" ||
		report.Entries[0].Force {
		t.Fatalf("legacy entry = %+v", report.Entries)
	}

	// Prune drives the runless legacy cleanup through its prune-registration
	// recovery and clears the retained legacy binding columns.
	code, stdout, stderr = runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf(`"sagaId": %d`, sagaID)) ||
		!strings.Contains(stdout, `"status": "removed"`) {
		t.Fatalf("prune results = %s", stdout)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("legacy worktree directory still exists: %v", err)
	}
	var binding int
	provisionScan(t, dir, `SELECT COUNT(*) FROM tasks WHERE wt_path IS NOT NULL`, &binding)
	if binding != 0 {
		t.Fatal("legacy binding columns survived the removed cleanup")
	}
}

func TestWorktreePruneCLIExhaustedGeneration(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	_, path := finishProvisionedRun(t, dir, "wt.prune.exhausted")

	var cleanupSaga int64
	provisionScan(t, dir, `SELECT saga_id FROM worktree_sagas WHERE operation='remove' AND status='pending'`, &cleanupSaga)
	provisionExec(t, dir, `UPDATE worktree_sagas SET recovery_generation=? WHERE saga_id=?`,
		store.MaxWorktreeRecoveryGeneration, cleanupSaga)

	// The broker handoff refuses with the exhaustion diagnostic before any
	// effect; the result still reports the row's committed status (SPEC
	// §9.4).
	code, stdout, stderr := runCmd(cmdWorktree, "prune", "--json")
	if code != 0 {
		t.Fatalf("prune exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "noise-tasks: worktree recovery generation exhausted\n") {
		t.Fatalf("exhaustion stderr = %q", stderr)
	}
	if !strings.Contains(stdout, fmt.Sprintf(`"sagaId": %d`, cleanupSaga)) ||
		!strings.Contains(stdout, `"status": "pending"`) ||
		!strings.Contains(stdout, `"brokerRequests": []`) {
		t.Fatalf("exhausted prune output = %s", stdout)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("exhausted prune still ran an effect: %v", err)
	}
}

func TestWorktreePruneCLIRefusals(t *testing.T) {
	dir := chdirWorktreeRepo(t)

	// No database: exactly one diagnostic line, stdout empty.
	code, stdout, stderr := runCmd(cmdWorktree, "prune")
	if code != 1 || stdout != "" ||
		stderr != "noise-tasks: project is not initialized; run noise tasks setup\n" {
		t.Fatalf("no-live exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	seedWorktreeStore(t, dir, "t")

	// --file is never accepted.
	code, _, stderr = runCmd(cmdWorktree, "prune", "--file", "x.json")
	if code != 2 || !strings.Contains(stderr, "--file is not accepted for worktree prune") {
		t.Fatalf("--file exit %d, stderr %q", code, stderr)
	}

	// The saga table spans all tags; --tag scopes nothing.
	code, _, stderr = runCmd(cmdWorktree, "prune", "--tag", "main")
	if code != 2 || !strings.Contains(stderr, "--tag is not accepted for worktree prune") {
		t.Fatalf("--tag exit %d, stderr %q", code, stderr)
	}

	// Positional arguments are rejected.
	code, _, stderr = runCmd(cmdWorktree, "prune", "1")
	if code != 2 || !strings.Contains(stderr, "unexpected argument") {
		t.Fatalf("argument exit %d, stderr %q", code, stderr)
	}
}
