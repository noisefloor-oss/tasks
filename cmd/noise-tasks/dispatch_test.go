package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// dispatchPrepareArgs builds one `dispatch prepare` invocation with the
// default normalized inputs the seed helpers commit under the derived ids.
func dispatchPrepareArgs(root string, extra ...string) []string {
	args := []string{"prepare", "--owner", "agent-a", "--request-id", root, "--json"}
	return append(args, extra...)
}

// dispatchReconcileArgs builds one `dispatch reconcile` invocation from the
// live controller triple and the provisioning saga identity.
func dispatchReconcileArgs(root string, runID, claimToken, attempt, attemptToken, sagaID, sagaToken int64,
	recoverID string) []string {
	return []string{"reconcile", strconv.FormatInt(runID, 10),
		"--owner", "agent-a",
		"--claim-token", strconv.FormatInt(claimToken, 10),
		"--attempt", strconv.FormatInt(attempt, 10),
		"--attempt-token", strconv.FormatInt(attemptToken, 10),
		"--saga", strconv.FormatInt(sagaID, 10),
		"--saga-token", strconv.FormatInt(sagaToken, 10),
		"--recover-request-id", recoverID,
		"--root-request-id", root, "--json"}
}

// dispatchOverrideFlag replaces one flag's value in a built argv.
func dispatchOverrideFlag(args []string, name, value string) []string {
	out := make([]string, 0, len(args)+2)
	replaced := false
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			out = append(out, name, value)
			i++
			replaced = true
			continue
		}
		out = append(out, args[i])
	}
	if !replaced {
		out = append(out, name, value)
	}
	return out
}

// dispatchKeysInOrder returns the object keys of one JSON document in
// encoded order, so a test can pin the exact §9.3B member order.
func dispatchKeysInOrder(t *testing.T, raw string) []string {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("JSON is not an object: %v\n%s", err, raw)
	}
	var keys []string
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			t.Fatalf("read JSON key: %v", err)
		}
		key, _ := keyTok.(string)
		keys = append(keys, key)
		var value any
		if err := dec.Decode(&value); err != nil {
			t.Fatalf("skip JSON value for %s: %v", key, err)
		}
	}
	return keys
}

// dispatchMember returns one member's raw JSON for nested order checks.
func dispatchMember(t *testing.T, raw, key string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	value, ok := fields[key]
	if !ok {
		t.Fatalf("JSON member %s missing from %s", key, raw)
	}
	return string(value)
}

func dispatchEqualKeys(t *testing.T, raw string, want []string, where string) {
	t.Helper()
	got := dispatchKeysInOrder(t, raw)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s keys = %v, want %v\n%s", where, got, want, raw)
	}
}

// dispatchSeedClaim models a §9.3B crash right after the claim step
// committed under the derived claim request id. When claimRequestID is
// empty it is the derived id; a different value models a lineage the
// derived ids cannot prove.
func dispatchSeedClaim(t *testing.T, dir, root, claimRequestID string) (*store.Claim, dispatchStepIDs) {
	t.Helper()
	seedWorktreeStore(t, dir, "dispatch resume")
	bumpProvisionFencing(t, dir)
	steps, err := dispatchStepRequestIDs(root)
	if err != nil {
		t.Fatal(err)
	}
	if claimRequestID == "" {
		claimRequestID = steps.Claim
	}
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, claim, _, err := db.ClaimNext("main", "agent-a", 0, "", "", claimRequestID)
	if err != nil || task == nil || claim == nil {
		t.Fatalf("seeded claim-next = %v %v %v", task, claim, err)
	}
	return claim, steps
}

// dispatchSeedSteps models a crash after the claim and run start steps
// committed under the derived request ids: prepare must resume from the
// missing provision leg and never claim a second task, start a second run,
// or reserve a second lineage for the root.
func dispatchSeedSteps(t *testing.T, dir, root string) (*store.Claim, *store.RunStartResult, dispatchStepIDs) {
	t.Helper()
	claim, steps := dispatchSeedClaim(t, dir, root, "")
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var taskID string
	provisionScan(t, dir, `SELECT id FROM tasks WHERE tag='main' AND claim_owner='agent-a'`, &taskID)
	run, _, err := db.StartRun("main", taskID, "agent-a", claim.Token, steps.Start, 0)
	if err != nil {
		t.Fatal(err)
	}
	return claim, run, steps
}

// dispatchSeedPlan additionally commits the provision planning transaction
// under the derived provision request id, modeling a crash before the
// §9.4 effect.
func dispatchSeedPlan(t *testing.T, dir, root string) (*store.WorktreeProvisionPlan, *store.Claim, *store.RunStartResult, dispatchStepIDs) {
	t.Helper()
	claim, run, steps := dispatchSeedSteps(t, dir, root)
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	record, err := db.GetRun(run.RunID)
	if err != nil || record == nil {
		t.Fatalf("seeded run %d = %v %v", run.RunID, record, err)
	}
	identity, err := store.PreflightWorktreeProvision(dir, dir, record.Tag, record.Task, false, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := db.PlanWorktreeProvision(store.WorktreeProvisionOptions{
		RunID: run.RunID, Owner: "agent-a", ClaimToken: claim.Token,
		Attempt: record.CurrentAttempt, AttemptToken: run.AttemptToken, RequestID: steps.Provision,
		OwnerBoundary: identity.OwnerBoundary, RegistrationRoot: identity.RegistrationRoot,
		TargetPath: identity.TargetPath, Branch: identity.Branch, BaseCommit: identity.BaseCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan, claim, run, steps
}

// dispatchCountReceipts answers how many receipts exist for one request id.
func dispatchCountReceipts(t *testing.T, dir, requestID string) int {
	t.Helper()
	var count int
	provisionScan(t, dir, `SELECT COUNT(*) FROM receipts WHERE request_id=?`, &count, requestID)
	return count
}

// dispatchAssertSuccess pins the shared §9.3B success invariants: one
// lineage, a bound controller workspace, and a leaf brief whose workspace
// path is the literal dot.
func dispatchAssertSuccess(t *testing.T, dir, root, stdout string) dispatchSuccess {
	t.Helper()
	var got dispatchSuccess
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("success JSON: %v\n%s", err, stdout)
	}
	if got.RootRequestID != root || got.Task != "1" || got.Leaf == nil ||
		got.Leaf.Workspace.Path != "." || got.Controller.Workspace.Status != "bound" ||
		got.Controller.Workspace.RecoverRequestID != nil {
		t.Fatalf("success = %+v", got)
	}
	var runs, sagas int
	provisionScan(t, dir, `SELECT COUNT(*) FROM runs`, &runs)
	provisionScan(t, dir, `SELECT COUNT(*) FROM worktree_sagas`, &sagas)
	if runs != 1 || sagas != 1 {
		t.Fatalf("resume invented a second lineage: runs=%d sagas=%d", runs, sagas)
	}
	return got
}

func TestDispatchStepRequestIDGolden(t *testing.T) {
	// The exact SPEC §9.3B vectors for root request id release-25.
	ids, err := dispatchStepRequestIDs("release-25")
	if err != nil {
		t.Fatal(err)
	}
	want := dispatchStepIDs{
		Claim:     "dp1.claim.a724c9d7d6f921ce1f587b374e557912ed15360a7c392218e628b6b706a8a67e",
		Start:     "dp1.start.2373de635e80051c8615ed6bcefce2c7164fc8aa14dbcb33151956944c52c58b",
		Provision: "dp1.provision.9daa26cc30b6cb1b65b4ddf70934472b93aec2e005b63e94350deacdafa7361c",
	}
	if ids != want {
		t.Fatalf("derived ids = %+v, want %+v", ids, want)
	}
	// The ids stay inside the local-key grammar and well below its bound.
	for _, id := range []string{ids.Claim, ids.Start, ids.Provision} {
		if !store.ValidLocalKey(id) || len(id) > 128 {
			t.Fatalf("derived id %q is not a bounded local key", id)
		}
	}
	// A different root derives entirely different ids.
	other, _ := dispatchStepRequestIDs("release-26")
	if other.Claim == ids.Claim || other.Start == ids.Start || other.Provision == ids.Provision {
		t.Fatalf("root change did not change the derivation: %+v", other)
	}
}

func TestDispatchPrepareFreshSuccessAndBoundReplay(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "dispatch first", "dispatch second")
	bumpProvisionFencing(t, dir)
	base := gitRevParseMain(t, dir)
	steps, err := dispatchStepRequestIDs("release-25")
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("release-25", "--claim-ttl", "1200", "--run-ttl", "1800")...)
	if code != 0 {
		t.Fatalf("prepare exit %d, stderr: %s", code, stderr)
	}
	// Exact member order for the §9.3B success object and both §9.3A
	// projections.
	dispatchEqualKeys(t, stdout, []string{"rootRequestId", "task", "stepRequestIds", "controller", "leaf"}, "success")
	dispatchEqualKeys(t, dispatchMember(t, stdout, "stepRequestIds"),
		[]string{"claim", "start", "provision"}, "stepRequestIds")
	dispatchEqualKeys(t, dispatchMember(t, stdout, "controller"),
		[]string{"owner", "claim", "run", "workspace"}, "controller")
	dispatchEqualKeys(t, dispatchMember(t, dispatchMember(t, stdout, "controller"), "claim"),
		[]string{"token", "expiresAt", "revision"}, "controller.claim")
	dispatchEqualKeys(t, dispatchMember(t, dispatchMember(t, stdout, "controller"), "run"),
		[]string{"runId", "attempt", "leaseGeneration", "attemptToken", "expiresAt"}, "controller.run")
	dispatchEqualKeys(t, dispatchMember(t, dispatchMember(t, stdout, "controller"), "workspace"),
		[]string{"sagaId", "sagaToken", "status", "path", "ownerBoundary", "branch", "baseCommit",
			"recoveryGeneration", "recoverRequestId", "brokerRequest"}, "controller.workspace")
	dispatchEqualKeys(t, dispatchMember(t, stdout, "leaf"),
		[]string{"schemaVersion", "tag", "task", "run", "workspace"}, "leaf")
	dispatchEqualKeys(t, dispatchMember(t, dispatchMember(t, stdout, "leaf"), "task"),
		[]string{"id", "title", "description", "details", "status", "priority", "milestone",
			"dependencies", "subtasks", "updatedAt"}, "leaf.task")
	dispatchEqualKeys(t, dispatchMember(t, dispatchMember(t, stdout, "leaf"), "workspace"),
		[]string{"path", "branch", "baseCommit"}, "leaf.workspace")

	got := dispatchAssertSuccess(t, dir, "release-25", stdout)
	if got.StepRequestIDs != steps {
		t.Fatalf("step request ids = %+v, want %+v", got.StepRequestIDs, steps)
	}
	var claimToken int64
	provisionScan(t, dir, `SELECT claim_token FROM tasks WHERE tag='main' AND id='1'`, &claimToken)
	wantPath := filepath.Join(dir, ".worktrees", "main-1")
	if got.Controller.Owner != "agent-a" || got.Controller.Claim.Token != claimToken ||
		got.Controller.Claim.Revision != 1 || got.Controller.Claim.ExpiresAt == "" {
		t.Fatalf("controller claim = %+v", got.Controller.Claim)
	}
	if got.Controller.Run.RunID != 1 || got.Controller.Run.Attempt != 1 ||
		got.Controller.Run.LeaseGeneration != 1 || got.Controller.Run.AttemptToken <= claimToken {
		t.Fatalf("controller run = %+v", got.Controller.Run)
	}
	ws := got.Controller.Workspace
	if ws.SagaID != 1 || ws.SagaToken <= 0 || ws.Path != wantPath ||
		ws.OwnerBoundary != dir || ws.Branch != "tasks/main/1" || ws.BaseCommit == nil ||
		*ws.BaseCommit != base || ws.RecoveryGeneration != 0 || ws.BrokerRequest != nil {
		t.Fatalf("controller workspace = %+v", ws)
	}
	leafRaw := dispatchMember(t, stdout, "leaf")
	leaf := got.Leaf
	if leaf.SchemaVersion != 1 || leaf.Tag != "main" || leaf.Task.ID != "1" ||
		leaf.Task.Title != "dispatch first" || leaf.Task.Status != "pending" ||
		leaf.Task.Priority != "medium" || leaf.Task.Dependencies == nil ||
		leaf.Run.RunID != 1 || leaf.Run.Attempt != 1 ||
		leaf.Workspace.Branch != "tasks/main/1" || leaf.Workspace.BaseCommit != base {
		t.Fatalf("leaf = %+v", leaf)
	}
	for _, forbidden := range []string{"token", "sagaToken", "\"owner\"", "ownerBoundary", "familyId"} {
		if strings.Contains(leafRaw, forbidden) {
			t.Fatalf("leaf leaked %q: %s", forbidden, leafRaw)
		}
	}
	if !gitWorktreeRegistered(t, dir, wantPath) {
		t.Fatal("prepare did not create a registered worktree")
	}

	// One lineage: exactly one claim, one run, one saga, and one receipt for
	// each derived step request id.
	if dispatchCountReceipts(t, dir, steps.Claim) != 1 ||
		dispatchCountReceipts(t, dir, steps.Start) != 1 || dispatchCountReceipts(t, dir, steps.Provision) != 1 {
		t.Fatal("step receipts are not exactly one each")
	}

	// Bound replay: the identical success object, byte for byte, with no
	// second effect or lineage.
	code, replay, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("release-25", "--claim-ttl", "1200", "--run-ttl", "1800")...)
	if code != 0 || replay != stdout {
		t.Fatalf("bound replay = exit %d\n%s\nstderr %s\nwant\n%s", code, replay, stderr, stdout)
	}
}

func TestDispatchPrepareResumeBoundaries(t *testing.T) {
	t.Run("claim committed before run start", func(t *testing.T) {
		dir := chdirWorktreeRepo(t)
		_, steps := dispatchSeedClaim(t, dir, "root.resume.claim", "")
		code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.resume.claim")...)
		if code != 0 {
			t.Fatalf("resume exit %d, stderr: %s", code, stderr)
		}
		dispatchAssertSuccess(t, dir, "root.resume.claim", stdout)
		var startRequest string
		provisionScan(t, dir, `SELECT start_request_id FROM runs WHERE run_id=1`, &startRequest)
		if startRequest != steps.Start {
			t.Fatalf("run start request = %q, want the derived %q", startRequest, steps.Start)
		}
	})
	t.Run("run start committed before provision", func(t *testing.T) {
		dir := chdirWorktreeRepo(t)
		dispatchSeedSteps(t, dir, "root.resume.plan")
		code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.resume.plan")...)
		if code != 0 {
			t.Fatalf("resume exit %d, stderr: %s", code, stderr)
		}
		dispatchAssertSuccess(t, dir, "root.resume.plan", stdout)
	})
	t.Run("provision planned before effect", func(t *testing.T) {
		dir := chdirWorktreeRepo(t)
		plan, _, _, _ := dispatchSeedPlan(t, dir, "root.resume.effect")
		if !plan.EffectRequired {
			t.Fatal("seeded plan must require the effect")
		}
		code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.resume.effect")...)
		if code != 0 {
			t.Fatalf("resume exit %d, stderr: %s", code, stderr)
		}
		dispatchAssertSuccess(t, dir, "root.resume.effect", stdout)
		var status string
		provisionScan(t, dir, `SELECT status FROM worktree_sagas WHERE saga_id=?`, &status, plan.SagaID)
		if status != "bound" {
			t.Fatalf("resumed saga = %q", status)
		}
	})
	t.Run("provision effected before finalization", func(t *testing.T) {
		dir := chdirWorktreeRepo(t)
		plan, _, _, _ := dispatchSeedPlan(t, dir, "root.resume.finalize")
		if outcome, err := store.EffectWorktreeProvision(dir, dir, plan); err != nil || outcome != store.WorktreeProvisionBound {
			t.Fatalf("seeded effect = %v %v", outcome, err)
		}
		marker := filepath.Join(plan.TargetPath, "operator-note")
		if err := os.WriteFile(marker, []byte("keep me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.resume.finalize")...)
		if code != 0 {
			t.Fatalf("resume exit %d, stderr: %s", code, stderr)
		}
		dispatchAssertSuccess(t, dir, "root.resume.finalize", stdout)
		if data, err := os.ReadFile(marker); err != nil || string(data) != "keep me\n" {
			t.Fatalf("resume repeated the effect; marker = %q %v", data, err)
		}
	})
	t.Run("renewal after start uses live capabilities", func(t *testing.T) {
		dir := chdirWorktreeRepo(t)
		claim, run, _ := dispatchSeedSteps(t, dir, "root.resume.renew")
		db, err := store.Open(projectStorePath(dir))
		if err != nil {
			t.Fatal(err)
		}
		// A renewal rotates both capabilities after the start receipt
		// committed; prepare must read the live controller, never replay
		// the historical start tokens.
		if _, _, err := db.RenewRun(run.RunID, "agent-a", claim.Token, run.Attempt,
			run.AttemptToken, "run.renew.dispatch", 900); err != nil {
			t.Fatal(err)
		}
		db.Close()
		code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.resume.renew")...)
		if code != 0 {
			t.Fatalf("resume after renewal exit %d, stderr: %s", code, stderr)
		}
		got := dispatchAssertSuccess(t, dir, "root.resume.renew", stdout)
		var liveClaim int64
		provisionScan(t, dir, `SELECT claim_token FROM tasks WHERE tag='main' AND id='1'`, &liveClaim)
		if got.Controller.Claim.Token != liveClaim || got.Controller.Run.LeaseGeneration != 2 ||
			got.Controller.Run.AttemptToken == run.AttemptToken {
			t.Fatalf("controller after renewal = %+v / %+v", got.Controller.Claim, got.Controller.Run)
		}
	})
}

func TestDispatchPrepareEmptyAndRootReuse(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main")
	bumpProvisionFencing(t, dir)
	steps, err := dispatchStepRequestIDs("root.empty")
	if err != nil {
		t.Fatal(err)
	}

	// The successful empty claim-next passes through exactly: JSON null,
	// exit 0, and nothing reserved for the root or its step ids.
	code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.empty", "--claim-ttl", "300")...)
	if code != 0 || stdout != "null\n" {
		t.Fatalf("empty prepare = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	for _, id := range []string{steps.Claim, steps.Start, steps.Provision} {
		if dispatchCountReceipts(t, dir, id) != 0 {
			t.Fatalf("empty prepare reserved request %s", id)
		}
	}

	// The root stays unreserved: the same root with changed inputs may
	// later acquire work.
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTask("main", &store.Task{Title: "later work", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, stdout, stderr = runCmd(cmdDispatch, dispatchPrepareArgs("root.empty", "--claim-ttl", "900")...)
	if code != 0 {
		t.Fatalf("reused root prepare exit %d, stderr: %s", code, stderr)
	}
	dispatchAssertSuccess(t, dir, "root.empty", stdout)
}

func TestDispatchPrepareStaleReplayConflicts(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "conflict seed")
	bumpProvisionFencing(t, dir)
	code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.conflict")...)
	if code != 0 {
		t.Fatalf("prepare exit %d, stderr: %s", code, stderr)
	}

	// Reusing the committed root with different normalized inputs surfaces
	// the primitives' typed replay conflicts: exit 3, empty stdout.
	for _, extra := range [][]string{
		{"--claim-ttl", "777"},
		{"--run-ttl", "666"},
		{"--owner", "agent-b"},
	} {
		code, stdout, stderr = runCmd(cmdDispatch, dispatchPrepareArgs("root.conflict", extra...)...)
		if code != 3 || stdout != "" {
			t.Fatalf("changed-input replay %v = exit %d stdout %q stderr %q", extra, code, stdout, stderr)
		}
	}

	// A bound saga with a drifted task revision never yields a brief.
	provisionExec(t, dir, `UPDATE tasks SET revision=revision+1 WHERE tag='main' AND id='1'`)
	code, stdout, stderr = runCmd(cmdDispatch, dispatchPrepareArgs("root.conflict")...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "worktree-handoff-conflict") {
		t.Fatalf("stale-revision replay = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	provisionExec(t, dir, `UPDATE tasks SET revision=revision-1 WHERE tag='main' AND id='1'`)

	// An expired claim is a stale capability: the primitive exit 3, never a
	// blocked handoff.
	provisionExec(t, dir, `UPDATE tasks SET claim_expires_at='2020-01-01T00:00:00Z' WHERE tag='main' AND id='1'`)
	code, stdout, stderr = runCmd(cmdDispatch, dispatchPrepareArgs("root.conflict")...)
	if code != 3 || stdout != "" {
		t.Fatalf("expired-claim replay = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestDispatchPrepareBlockedNeedsBroker(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("confined registration requires an unprivileged uid")
	}
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "broker seed")
	bumpProvisionFencing(t, dir)
	confineCLIRegistration(t, dir)
	steps, err := dispatchStepRequestIDs("root.broker")
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.broker")...)
	if code != 0 {
		t.Fatalf("confined prepare exit %d, stderr: %s", code, stderr)
	}
	// The blocked result carries exactly the six §9.3B members in order and
	// no leaf: a nonterminal workspace is never handed to a leaf.
	dispatchEqualKeys(t, stdout, []string{"kind", "reason", "rootRequestId", "task",
		"stepRequestIds", "controller"}, "blocked")
	var blocked dispatchBlocked
	if err := json.Unmarshal([]byte(stdout), &blocked); err != nil {
		t.Fatalf("blocked JSON: %v\n%s", err, stdout)
	}
	if blocked.Kind != "dispatch-blocked" || blocked.Reason != "needs-broker" ||
		blocked.RootRequestID != "root.broker" || blocked.Task != "1" || blocked.StepRequestIDs != steps {
		t.Fatalf("blocked result = %+v", blocked)
	}
	ws := blocked.Controller.Workspace
	if ws.Status != "needs-broker" || ws.SagaID != 1 || ws.SagaToken <= 0 ||
		ws.RecoveryGeneration != 0 || ws.RecoverRequestID == nil || ws.BrokerRequest == nil {
		t.Fatalf("blocked workspace = %+v", ws)
	}
	if *ws.RecoverRequestID != ws.BrokerRequest.RecoverRequestID ||
		!strings.HasPrefix(*ws.RecoverRequestID, "recover-") ||
		ws.BrokerRequest.Verb != "create" ||
		ws.BrokerRequest.TargetPath != filepath.Join(dir, ".worktrees", "main-1") ||
		ws.BrokerRequest.Branch != "tasks/main/1" || ws.BrokerRequest.BaseCommit == nil ||
		ws.BrokerRequest.FamilyID == "" {
		t.Fatalf("blocked broker request = %+v", ws.BrokerRequest)
	}
	if strings.Contains(stdout, "\"leaf\"") || strings.Contains(stdout, "schemaVersion") {
		t.Fatalf("blocked result carries a leaf brief: %s", stdout)
	}
	if status, generation := recoverSagaRow(t, dir, 1); status != "needs-broker" || generation != 0 {
		t.Fatalf("confined row = %s/%d", status, generation)
	}
}

func TestDispatchPrepareBlockedNeedsRecovery(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	plan, claim, run, _ := dispatchSeedPlan(t, dir, "root.recover.blocked")

	// A pending saga past generation zero no longer re-effects: the
	// original replay returns the current handoff and prepare reports the
	// nonterminal workspace as needs-recovery.
	provisionExec(t, dir, `UPDATE worktree_sagas SET recovery_generation=1 WHERE saga_id=?`, plan.SagaID)
	advertised := advertisedRecoverID(t, dir, plan.SagaID, 2)
	code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.recover.blocked")...)
	if code != 0 {
		t.Fatalf("pending prepare exit %d, stderr: %s", code, stderr)
	}
	var blocked dispatchBlocked
	if err := json.Unmarshal([]byte(stdout), &blocked); err != nil {
		t.Fatalf("pending blocked JSON: %v\n%s", err, stdout)
	}
	if blocked.Kind != "dispatch-blocked" || blocked.Reason != "needs-recovery" ||
		blocked.Controller.Workspace.Status != "pending" ||
		blocked.Controller.Workspace.RecoveryGeneration != 1 ||
		blocked.Controller.Workspace.RecoverRequestID == nil ||
		*blocked.Controller.Workspace.RecoverRequestID != advertised ||
		blocked.Controller.Workspace.BrokerRequest != nil {
		t.Fatalf("pending blocked = %+v", blocked.Controller.Workspace)
	}
	if _, err := os.Stat(plan.TargetPath); !os.IsNotExist(err) {
		t.Fatalf("pending replay effected the worktree: %v", err)
	}

	// cleanup-required — like every otherwise-nonterminal workspace — is
	// needs-recovery too, never a brief.
	provisionExec(t, dir, `UPDATE worktree_sagas SET status='cleanup-required' WHERE saga_id=?`, plan.SagaID)
	code, stdout, stderr = runCmd(cmdDispatch, dispatchPrepareArgs("root.recover.blocked")...)
	if code != 0 {
		t.Fatalf("cleanup prepare exit %d, stderr: %s", code, stderr)
	}
	blocked = dispatchBlocked{}
	if err := json.Unmarshal([]byte(stdout), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Reason != "needs-recovery" || blocked.Controller.Workspace.Status != "cleanup-required" ||
		blocked.Controller.Workspace.RecoverRequestID == nil ||
		blocked.Controller.Workspace.BrokerRequest != nil {
		t.Fatalf("cleanup blocked = %+v", blocked.Controller.Workspace)
	}

	// A nonterminal saga at the maximum recovery generation fails closed in
	// both dispatch surfaces: the pinned §9.3A exhaustion diagnostic, exit
	// 1, empty stdout, and no wrapped or null id.
	provisionExec(t, dir, `UPDATE worktree_sagas SET recovery_generation=9223372036854775807 WHERE saga_id=?`, plan.SagaID)
	wantExhausted := "noise-tasks: worktree recovery generation exhausted\n"
	code, stdout, stderr = runCmd(cmdDispatch, dispatchPrepareArgs("root.recover.blocked")...)
	if code != 1 || stdout != "" || stderr != wantExhausted {
		t.Fatalf("exhausted prepare = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCmd(cmdDispatch, dispatchReconcileArgs("root.recover.blocked",
		plan.RunID, claim.Token, run.Attempt, run.AttemptToken, plan.SagaID, plan.SagaToken,
		"recover-"+strings.Repeat("c", 64))...)
	if code != 1 || stdout != "" || stderr != wantExhausted {
		t.Fatalf("exhausted reconcile = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestDispatchReconcileFreshRecovery(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	plan, claim, run, _ := dispatchSeedPlan(t, dir, "root.reconcile.fresh")
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)

	// A recovery request id that is not the advertised one is the typed
	// conflict even though raw §9.4 recover would accept its grammar.
	code, stdout, stderr := runCmd(cmdDispatch, dispatchReconcileArgs("root.reconcile.fresh",
		plan.RunID, claim.Token, run.Attempt, run.AttemptToken, plan.SagaID, plan.SagaToken,
		"recover-"+strings.Repeat("b", 64))...)
	if code != 3 || stdout != "" {
		t.Fatalf("wrong-id reconcile = exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	code, stdout, stderr = runCmd(cmdDispatch, dispatchReconcileArgs("root.reconcile.fresh",
		plan.RunID, claim.Token, run.Attempt, run.AttemptToken, plan.SagaID, plan.SagaToken, request)...)
	if code != 0 {
		t.Fatalf("fresh reconcile exit %d, stderr: %s", code, stderr)
	}
	got := dispatchAssertSuccess(t, dir, "root.reconcile.fresh", stdout)
	if got.Controller.Workspace.RecoveryGeneration != 1 || got.Controller.Workspace.BrokerRequest != nil {
		t.Fatalf("reconciled workspace = %+v", got.Controller.Workspace)
	}
	if !gitWorktreeRegistered(t, dir, plan.TargetPath) {
		t.Fatal("recovery did not create the registered worktree")
	}
}

func TestDispatchReconcileTerminalReplay(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "terminal replay")
	bumpProvisionFencing(t, dir)
	// A saga bound by its original provision has no recovery receipt: every
	// recovery id is exit 3, and prepare owns the bound replay.
	code, success, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.terminal.original")...)
	if code != 0 {
		t.Fatalf("seed prepare exit %d, stderr: %s", code, stderr)
	}
	var bound dispatchSuccess
	if err := json.Unmarshal([]byte(success), &bound); err != nil {
		t.Fatal(err)
	}
	anyID := advertisedRecoverID(t, dir, bound.Controller.Workspace.SagaID, 1)
	code, stdout, stderr := runCmd(cmdDispatch, dispatchReconcileArgs("root.terminal.original",
		bound.Controller.Run.RunID, bound.Controller.Claim.Token, bound.Controller.Run.Attempt,
		bound.Controller.Run.AttemptToken, bound.Controller.Workspace.SagaID,
		bound.Controller.Workspace.SagaToken, anyID)...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "no recovery receipt") {
		t.Fatalf("original-bound reconcile = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// TestDispatchReconcileTerminalFailedRequiresLiveController pins the §9.3B
// ordering against a terminalizing failed recovery receipt: the primitive
// exit-1 failure is reachable only from a proven-live controller, so a
// controller whose claim has expired replays the exact terminalizing receipt
// as the typed exit-3 conflict with empty stdout — never the primitive
// failure result.
func TestDispatchReconcileTerminalFailedRequiresLiveController(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	plan, claim, run, _ := dispatchSeedPlan(t, dir, "root.terminal.failed")
	// A cleanup-required provision whose recovery proves the worktree
	// absent terminalizes failed at generation one under the advertised id.
	provisionExec(t, dir, `UPDATE worktree_sagas SET status='cleanup-required' WHERE saga_id=?`, plan.SagaID)
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)
	code, stdout, stderr := runCmd(cmdDispatch, dispatchReconcileArgs("root.terminal.failed",
		plan.RunID, claim.Token, run.Attempt, run.AttemptToken, plan.SagaID, plan.SagaToken, request)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "proven absent") {
		t.Fatalf("terminalizing failed recovery = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if status, generation := recoverSagaRow(t, dir, plan.SagaID); status != "failed" || generation != 1 {
		t.Fatalf("terminalized saga row = %s/%d, want failed/1", status, generation)
	}

	// The stale-controller terminal replay: the exact terminalizing
	// receipt, replayed against an expired claim, is the typed conflict.
	provisionExec(t, dir, `UPDATE tasks SET claim_expires_at='2020-01-01T00:00:00Z' WHERE tag='main' AND id='1'`)
	code, stdout, stderr = runCmd(cmdDispatch, dispatchReconcileArgs("root.terminal.failed",
		plan.RunID, claim.Token, run.Attempt, run.AttemptToken, plan.SagaID, plan.SagaToken, request)...)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "worktree-handoff-conflict") {
		t.Fatalf("stale terminal failed replay = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestDispatchReconcileWrongControllerExit3(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	plan, claim, run, _ := dispatchSeedPlan(t, dir, "root.reconcile.wrong")
	request := advertisedRecoverID(t, dir, plan.SagaID, 1)
	base := dispatchReconcileArgs("root.reconcile.wrong", plan.RunID, claim.Token, run.Attempt,
		run.AttemptToken, plan.SagaID, plan.SagaToken, request)

	// The lineage proof: every supplied capability must name the saga's
	// live controller, and the derived ids must name the committed records.
	unknownRun := append([]string{}, base...)
	unknownRun[1] = "2"
	cases := []struct {
		name string
		argv []string
	}{
		{"wrong claim token", dispatchOverrideFlag(base, "--claim-token", "424242")},
		{"wrong attempt", dispatchOverrideFlag(base, "--attempt", "9")},
		{"wrong attempt token", dispatchOverrideFlag(base, "--attempt-token", "424242")},
		{"wrong saga fence", dispatchOverrideFlag(base, "--saga-token", "424242")},
		{"unknown saga", dispatchOverrideFlag(base, "--saga", "99")},
		{"unknown run", unknownRun},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCmd(cmdDispatch, tc.argv...)
		if code != 3 || stdout != "" {
			t.Fatalf("%s = exit %d stdout %q stderr %q", tc.name, code, stdout, stderr)
		}
	}

	// A lineage whose claim step was committed under a different request id
	// cannot be proven by the derived ids: typed conflict, never a fallback
	// to claim-next or a search for a convenient saga.
	other := chdirWorktreeRepo(t)
	plan2, claim2, run2, _ := dispatchSeedPlanWithClaimID(t, other, "root.reconcile.wrong", "other.claim.id")
	request2 := advertisedRecoverID(t, other, plan2.SagaID, 1)
	code, stdout, stderr := runCmd(cmdDispatch, dispatchReconcileArgs("root.reconcile.wrong",
		plan2.RunID, claim2.Token, run2.Attempt, run2.AttemptToken, plan2.SagaID, plan2.SagaToken, request2)...)
	if code != 3 || stdout != "" {
		t.Fatalf("foreign claim lineage = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// dispatchSeedPlanWithClaimID seeds a full step lineage whose claim step
// committed under claimRequestID instead of the derived claim id.
func dispatchSeedPlanWithClaimID(t *testing.T, dir, root, claimRequestID string) (*store.WorktreeProvisionPlan, *store.Claim, *store.RunStartResult, dispatchStepIDs) {
	t.Helper()
	claim, steps := dispatchSeedClaim(t, dir, root, claimRequestID)
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var taskID string
	provisionScan(t, dir, `SELECT id FROM tasks WHERE tag='main' AND claim_owner='agent-a'`, &taskID)
	run, _, err := db.StartRun("main", taskID, "agent-a", claim.Token, steps.Start, 0)
	if err != nil {
		t.Fatal(err)
	}
	record, err := db.GetRun(run.RunID)
	if err != nil || record == nil {
		t.Fatalf("seeded run %d = %v %v", run.RunID, record, err)
	}
	identity, err := store.PreflightWorktreeProvision(dir, dir, record.Tag, record.Task, false, "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := db.PlanWorktreeProvision(store.WorktreeProvisionOptions{
		RunID: run.RunID, Owner: "agent-a", ClaimToken: claim.Token,
		Attempt: record.CurrentAttempt, AttemptToken: run.AttemptToken, RequestID: steps.Provision,
		OwnerBoundary: identity.OwnerBoundary, RegistrationRoot: identity.RegistrationRoot,
		TargetPath: identity.TargetPath, Branch: identity.Branch, BaseCommit: identity.BaseCommit,
	})
	if err != nil {
		t.Fatal(err)
	}
	return plan, claim, run, steps
}

func TestDispatchUsageErrors(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main")

	cases := []struct {
		name string
		argv []string
		want string
	}{
		{"no subcommand", nil, "usage: noise-tasks dispatch"},
		{"unknown subcommand", []string{"bogus"}, "unknown subcommand"},
		{"prepare without json", []string{"prepare", "--owner", "agent-a", "--request-id", "r"}, "--json is required"},
		{"prepare with file", []string{"prepare", "--owner", "agent-a", "--request-id", "r", "--file", "x.json", "--json"}, "--file is not accepted"},
		{"prepare extra positional", []string{"prepare", "--owner", "agent-a", "--request-id", "r", "--json", "extra"}, "usage:"},
		{"prepare without owner", []string{"prepare", "--request-id", "r", "--json"}, "--owner is required"},
		{"prepare bad root", []string{"prepare", "--owner", "agent-a", "--request-id", "!bad", "--json"}, "must match"},
		{"reconcile without json", []string{"reconcile", "1", "--owner", "agent-a", "--claim-token", "1",
			"--attempt", "1", "--attempt-token", "1", "--saga", "1", "--saga-token", "1",
			"--recover-request-id", "recover-x", "--root-request-id", "r"}, "--json is required"},
		{"reconcile without positional", []string{"reconcile", "--owner", "agent-a", "--claim-token", "1",
			"--attempt", "1", "--attempt-token", "1", "--saga", "1", "--saga-token", "1",
			"--recover-request-id", "recover-x", "--root-request-id", "r", "--json"}, "usage:"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCmd(cmdDispatch, tc.argv...)
		if code != 2 || stdout != "" || !strings.Contains(stderr, tc.want) {
			t.Fatalf("%s = exit %d stdout %q stderr %q, want %q", tc.name, code, stdout, stderr, tc.want)
		}
	}
}

// TestDispatchReconcileNeedsBrokerFD3 pins the full §9.3B/§9.4/§9.5
// confined flow through the real binary: the controller mints the
// capability, places it only on fd 3, and dispatch reconcile inherits it
// unchanged to delegate the registration create; the saga binds, the exact
// terminalizing receipt replays without any capability, and every new id is
// the typed conflict.
func TestDispatchReconcileNeedsBrokerFD3(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("confined registration requires an unprivileged uid")
	}
	dir := chdirWorktreeRepo(t)
	seedWorktreeStore(t, dir, "fd3 seed")
	bumpProvisionFencing(t, dir)
	confineCLIRegistration(t, dir)

	code, stdout, stderr := runCmd(cmdDispatch, dispatchPrepareArgs("root.fd3")...)
	if code != 0 {
		t.Fatalf("confined prepare exit %d, stderr: %s", code, stderr)
	}
	var blocked dispatchBlocked
	if err := json.Unmarshal([]byte(stdout), &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Reason != "needs-broker" || blocked.Controller.Workspace.BrokerRequest == nil {
		t.Fatalf("confined prepare = %s", stdout)
	}
	ws := blocked.Controller.Workspace
	request := *ws.RecoverRequestID
	// The staged broker under a linked libexec root.
	libexecRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(libexecRoot, "noise-worktree-broker"),
		[]byte(functionalRecoverBroker), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := buildRecoverCLI(t, libexecRoot)
	argv := append([]string{"dispatch"}, dispatchReconcileArgs("root.fd3", blocked.Controller.Run.RunID,
		blocked.Controller.Claim.Token, blocked.Controller.Run.Attempt,
		blocked.Controller.Run.AttemptToken, ws.SagaID, ws.SagaToken, request)...)

	// With no minted capability document on fd 3, the delegated attempt
	// reaches no reconciled outcome: the operational §9.5 refusal keeps
	// exit 1 with empty stdout and commits nothing, so the identical
	// request may be retried after minting.
	code, out, errOut := runRecoverCLI(t, bin, dir, "", argv...)
	if code != 1 || out != "" || !strings.Contains(errOut, "broker") {
		t.Fatalf("no-capability reconcile = exit %d stdout %q stderr %q", code, out, errOut)
	}
	if status, generation := recoverSagaRow(t, dir, ws.SagaID); status != "needs-broker" || generation != 0 {
		t.Fatalf("row after no-capability = %s/%d", status, generation)
	}

	// The §9.5 capability document — target, branch, and base ref — on
	// fd 3 only: reconcile inherits it unchanged and the broker's create
	// binds the saga.
	capability := ws.BrokerRequest.TargetPath + "\n" +
		ws.BrokerRequest.Branch + "\n" + *ws.BrokerRequest.BaseCommit + "\n"
	code, out, errOut = runRecoverCLI(t, bin, dir, capability, argv...)
	if code != 0 {
		t.Fatalf("broker reconcile exit %d, stderr: %s", code, errOut)
	}
	bound := dispatchAssertSuccess(t, dir, "root.fd3", out)
	if bound.Controller.Workspace.Status != "bound" || bound.Controller.Workspace.RecoveryGeneration != 1 ||
		bound.Controller.Workspace.RecoverRequestID != nil {
		t.Fatalf("broker-bound result = %+v", bound.Controller.Workspace)
	}
	if !gitWorktreeRegistered(t, dir, ws.BrokerRequest.TargetPath) {
		t.Fatal("broker create did not register the worktree")
	}

	// Terminal replay of the exact terminalizing receipt needs no
	// capability: the identical success object, in-process.
	code, replay, stderr := runCmd(cmdDispatch, dispatchReconcileArgs("root.fd3",
		bound.Controller.Run.RunID, bound.Controller.Claim.Token, bound.Controller.Run.Attempt,
		bound.Controller.Run.AttemptToken, ws.SagaID, ws.SagaToken, request)...)
	if code != 0 || replay != out {
		t.Fatalf("terminal replay = exit %d\n%s\nstderr %s\nwant\n%s", code, replay, stderr, out)
	}

	// Every new id against the terminal saga is the typed conflict.
	newID := advertisedRecoverID(t, dir, ws.SagaID, 2)
	code, stdout, stderr = runCmd(cmdDispatch, dispatchReconcileArgs("root.fd3",
		bound.Controller.Run.RunID, bound.Controller.Claim.Token, bound.Controller.Run.Attempt,
		bound.Controller.Run.AttemptToken, ws.SagaID, ws.SagaToken, newID)...)
	if code != 3 || stdout != "" {
		t.Fatalf("new-id terminal reconcile = exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
