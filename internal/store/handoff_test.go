package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newHandoffDB builds one coherent §9.3A controller state: a claimed task
// with its acquisition receipt, one running run on attempt 1, and durable
// lease expiries pinned far in the future so liveness judgments are stable.
func newHandoffDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	boundary := filepath.Join(dir, "family")
	db, err := CreateWithOwner(filepath.Join(dir, "tasks.sqlite"), boundary)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMilestone("main", "v2.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTask("main", &Task{Title: "Implement layer", Status: "pending",
		Priority: "high", Milestone: "v2.5"}); err != nil {
		t.Fatal(err)
	}
	claim, _, _, err := db.AcquireClaim("main", "1", "agent-a", 900, "claim.handoff")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.StartRun("main", "1", "agent-a", claim.Token, "run.handoff", 900); err != nil {
		t.Fatal(err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE tasks SET claim_expires_at='2999-01-01T00:00:00Z' WHERE tag='main' AND id='1'`)
	exec(`UPDATE attempts SET expires_at='2999-01-01T00:00:00Z' WHERE run_id=1 AND attempt_no=1`)
	exec(`UPDATE meta SET value='100' WHERE key='fencing'`)
	return db, boundary
}

// handoffFence reserves distinct saga fences after the mutation fences.
func handoffFence(db *DB) int64 {
	if _, err := db.db.Exec(`UPDATE meta SET value=CAST(value AS INTEGER)+1 WHERE key='fencing'`); err != nil {
		return 0
	}
	var fence int64
	if err := db.db.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='fencing'`).Scan(&fence); err != nil {
		return 0
	}
	return fence
}

const handoffBase = "0123456789abcdef0123456789abcdef01234567"

// insertHandoffSaga inserts one run-1 provision saga in the given state and
// returns its id.
func insertHandoffSaga(t *testing.T, db *DB, boundary, status string, active bool, generation int64) int64 {
	t.Helper()
	fence := handoffFence(db)
	var started string
	if err := db.db.QueryRow(`SELECT started_at FROM runs WHERE run_id=1`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	activeInt := int64(0)
	if active {
		activeInt = 1
	}
	res, err := db.db.Exec(`INSERT INTO worktree_sagas(run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra)
		VALUES (1,NULL,'main','1','provision',?,?,?,?,?,?,?,?,?,?,0,?,?,NULL)`,
		status, activeInt, fence, fmt.Sprintf("wt.%s.%d", status, fence), generation, boundary,
		"/git-admin/worktrees", filepath.Join(boundary, ".worktrees", "main-1"), "tasks/main/1",
		handoffBase, started, started)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func handoffControllerNumbers(t *testing.T, db *DB) (claimToken, attemptToken int64, updatedAt string) {
	t.Helper()
	if err := db.db.QueryRow(`SELECT claim_token FROM tasks WHERE tag='main' AND id='1'`).Scan(&claimToken); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT attempt_token FROM attempts WHERE run_id=1 AND attempt_no=1`).Scan(&attemptToken); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT updated_at FROM tasks WHERE tag='main' AND id='1'`).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	return claimToken, attemptToken, updatedAt
}

func handoffFamilyID(t *testing.T, db *DB) string {
	t.Helper()
	var familyID string
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&familyID); err != nil {
		t.Fatal(err)
	}
	return familyID
}

// requireHandoffConflict asserts the error is the typed §9.3A refusal naming
// the wanted reason fragment.
func requireHandoffConflict(t *testing.T, err error, sagaID int64, want string) {
	t.Helper()
	var conflict *HandoffConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("handoff error = %v, want HandoffConflict", err)
	}
	if conflict.SagaID != sagaID {
		t.Fatalf("conflict saga = %d, want %d", conflict.SagaID, sagaID)
	}
	if !strings.Contains(conflict.Reason, want) {
		t.Fatalf("conflict reason = %q, want it to contain %q", conflict.Reason, want)
	}
}

func TestWorktreeHandoffExactControllerProjectionAndLeafBrief(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	claimToken, attemptToken, updatedAt := handoffControllerNumbers(t, db)

	projection, brief, err := db.WorktreeHandoff(sagaID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	wantProjection := fmt.Sprintf(`{"owner":"agent-a","claim":{"token":%d,"expiresAt":"2999-01-01T00:00:00Z","revision":1},`+
		`"run":{"runId":1,"attempt":1,"leaseGeneration":1,"attemptToken":%d,"expiresAt":"2999-01-01T00:00:00Z"},`+
		`"workspace":{"sagaId":%d,"sagaToken":101,"status":"bound","path":%q,"ownerBoundary":%q,"branch":"tasks/main/1",`+
		`"baseCommit":"%s","recoveryGeneration":0,"recoverRequestId":null,"brokerRequest":null}}`,
		claimToken, attemptToken, sagaID, filepath.Join(boundary, ".worktrees", "main-1"), boundary, handoffBase)
	if string(raw) != wantProjection {
		t.Fatalf("controller projection =\n%s\nwant\n%s", raw, wantProjection)
	}

	raw, err = json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	wantBrief := fmt.Sprintf(`{"schemaVersion":1,"tag":"main","task":{"id":"1","title":"Implement layer","description":"",`+
		`"details":"","status":"pending","priority":"high","milestone":"v2.5","dependencies":[],"subtasks":[],"updatedAt":%q},`+
		`"run":{"runId":1,"attempt":1},"workspace":{"path":".","branch":"tasks/main/1","baseCommit":"%s"}}`,
		updatedAt, handoffBase)
	if string(raw) != wantBrief {
		t.Fatalf("leaf brief =\n%s\nwant\n%s", raw, wantBrief)
	}
}

func TestLeafBriefTaskFollowsShowJSONValueRules(t *testing.T) {
	// The §1 absent-priority default and the never-null array members mirror
	// show --json exactly; revision and claim are the brief's only omissions.
	if got := leafBriefPriority(""); got != "medium" {
		t.Errorf("absent priority = %q, want medium", got)
	}
	if got := leafBriefPriority("low"); got != "low" {
		t.Errorf("explicit priority = %q, want low", got)
	}
	task := &Task{ID: "7", Title: "Implement layer", Status: "pending"}
	saga := &worktreeSagaState{tag: "main", task: "7", branch: "tasks/main/7",
		base: sql.NullString{String: handoffBase, Valid: true}, run: sql.NullInt64{Int64: 1, Valid: true}}
	brief, err := saga.leafBrief(task, 1)
	if err != nil {
		t.Fatal(err)
	}
	if brief.Task.Priority != "medium" || brief.Task.Milestone != "" ||
		brief.Task.Dependencies == nil || string(brief.Task.Subtasks) != `[]` {
		t.Fatalf("brief task normalization = %+v", brief.Task)
	}
}

func TestWorktreeHandoffLeafBriefOmitsEveryControllerSecret(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	// Distinctive fencing values keep the omission assertions meaningful
	// when ordinary small ids (task, run, attempt, schema) legitimately
	// appear in the brief.
	const (
		secretClaim   = int64(987654321)
		secretAttempt = int64(987654322)
		secretFence   = int64(987654323)
	)
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE tasks SET claim_token=? WHERE tag='main' AND id='1'`, secretClaim)
	exec(`UPDATE runs SET claim_token=? WHERE run_id=1`, secretClaim)
	exec(`UPDATE attempts SET attempt_token=? WHERE run_id=1 AND attempt_no=1`, secretAttempt)
	exec(`UPDATE worktree_sagas SET saga_fence=? WHERE saga_id=?`, secretFence, sagaID)
	var claimAcquisition string
	if err := db.db.QueryRow(`SELECT claim_acquisition_id FROM runs WHERE run_id=1`).Scan(&claimAcquisition); err != nil {
		t.Fatal(err)
	}
	familyID := handoffFamilyID(t, db)

	_, brief, err := db.WorktreeHandoff(sagaID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(brief)
	if err != nil {
		t.Fatal(err)
	}
	forbiddenKeys := map[string]bool{
		"owner": true, "revision": true, "token": true, "claim": true, "leaseGeneration": true,
		"attemptToken": true, "acquisitionId": true, "sagaId": true, "sagaToken": true,
		"recoveryGeneration": true, "recoverRequestId": true, "brokerRequest": true,
		"requestId": true, "receiptId": true, "familyId": true, "registrationRoot": true,
		"ownerBoundary": true,
	}
	var walk func(v any)
	walk = func(v any) {
		switch value := v.(type) {
		case map[string]any:
			for key, child := range value {
				if forbiddenKeys[key] {
					t.Errorf("leaf brief leaked forbidden key %q", key)
				}
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	walk(decoded)

	document := string(raw)
	for _, secret := range []string{
		fmt.Sprint(secretClaim), fmt.Sprint(secretAttempt), fmt.Sprint(secretFence),
		claimAcquisition, familyID, boundary, "/git-admin/worktrees", "wt.bound",
		"claim.handoff", "run.handoff",
	} {
		if strings.Contains(document, secret) {
			t.Errorf("leaf brief leaked controller value %q", secret)
		}
	}
	if brief.Workspace.Path != "." {
		t.Errorf("leaf workspace path = %q, want \".\"", brief.Workspace.Path)
	}
}

func TestWorktreeHandoffTaskRevisionDriftRefusesWithoutBrief(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	if _, err := db.db.Exec(`UPDATE tasks SET revision=2 WHERE tag='main' AND id='1'`); err != nil {
		t.Fatal(err)
	}
	projection, brief, err := db.WorktreeHandoff(sagaID)
	if err == nil {
		t.Fatal("revision drift yielded a handoff")
	}
	if projection != nil || brief != nil {
		t.Fatalf("drift returned projection=%v brief=%v", projection, brief)
	}
	requireHandoffConflict(t, err, sagaID, "revision")
}

func TestWorktreeHandoffClaimAndAttemptLivenessRefuse(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)

	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at='2020-01-01T00:00:00Z' WHERE tag='main' AND id='1'`); err != nil {
		t.Fatal(err)
	}
	_, _, err := db.WorktreeHandoff(sagaID)
	requireHandoffConflict(t, err, sagaID, "claim is not live")

	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at='2999-01-01T00:00:00Z' WHERE tag='main' AND id='1'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE attempts SET expires_at='2020-01-01T00:00:00Z' WHERE run_id=1 AND attempt_no=1`); err != nil {
		t.Fatal(err)
	}
	_, _, err = db.WorktreeHandoff(sagaID)
	requireHandoffConflict(t, err, sagaID, "attempt lease is not live")

	// Equality with the clock counts as expired.
	if _, err := db.db.Exec(`UPDATE attempts SET expires_at=? WHERE run_id=1 AND attempt_no=1`,
		time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	_, _, err = db.WorktreeHandoff(sagaID)
	requireHandoffConflict(t, err, sagaID, "attempt lease is not live")
}

func TestWorktreeHandoffRunAttemptPairMustBeRunningRunning(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	at := "2026-08-28T19:00:00Z"
	if _, err := db.db.Exec(`UPDATE runs SET status='succeeded', terminal_at=?, updated_at=? WHERE run_id=1`, at, at); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE attempts SET status='succeeded', terminal_at=?, updated_at=? WHERE run_id=1 AND attempt_no=1`, at, at); err != nil {
		t.Fatal(err)
	}
	projection, brief, err := db.WorktreeHandoff(sagaID)
	if projection != nil || brief != nil || err == nil {
		t.Fatalf("terminal pair handed off: %v %v %v", projection, brief, err)
	}
	requireHandoffConflict(t, err, sagaID, "run/attempt pair is succeeded/succeeded")

	// A retryable/failed pair is equally refusing.
	if _, err := db.db.Exec(`UPDATE runs SET status='retryable', terminal_at=NULL WHERE run_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE attempts SET status='failed' WHERE run_id=1 AND attempt_no=1`); err != nil {
		t.Fatal(err)
	}
	_, _, err = db.WorktreeHandoff(sagaID)
	requireHandoffConflict(t, err, sagaID, "run/attempt pair is retryable/failed")
}

func TestWorktreeHandoffNonBoundProjectionsAreRecoveryMaterialOnly(t *testing.T) {
	db, boundary := newHandoffDB(t)
	drop := func(id int64) {
		t.Helper()
		if _, err := db.db.Exec(`DELETE FROM worktree_sagas WHERE saga_id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	familyID := handoffFamilyID(t, db)

	pending := insertHandoffSaga(t, db, boundary, "pending", false, 0)
	projection, brief, err := db.WorktreeHandoff(pending)
	if err != nil {
		t.Fatal(err)
	}
	if brief != nil {
		t.Fatalf("pending saga yielded a leaf brief: %v", brief)
	}
	next, err := worktreeRecoveryRequestID(familyID, pending, 1)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Workspace.RecoverRequestID == nil || *projection.Workspace.RecoverRequestID != next {
		t.Fatalf("pending recoverRequestId = %v, want %s", projection.Workspace.RecoverRequestID, next)
	}
	if projection.Workspace.BrokerRequest != nil {
		t.Fatalf("pending brokerRequest = %v, want null", projection.Workspace.BrokerRequest)
	}
	drop(pending)

	broker := insertHandoffSaga(t, db, boundary, "needs-broker", false, 2)
	projection, brief, err = db.WorktreeHandoff(broker)
	if err != nil {
		t.Fatal(err)
	}
	if brief != nil {
		t.Fatalf("needs-broker saga yielded a leaf brief: %v", brief)
	}
	next, err = worktreeRecoveryRequestID(familyID, broker, 3)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(projection.Workspace.BrokerRequest)
	if err != nil {
		t.Fatal(err)
	}
	wantBroker := fmt.Sprintf(`{"familyId":%q,"verb":"create","registrationRoot":"/git-admin/worktrees","ownerBoundary":%q,`+
		`"targetPath":%q,"branch":"tasks/main/1","baseCommit":"%s","force":false,"recoverRequestId":%q}`,
		familyID, boundary, filepath.Join(boundary, ".worktrees", "main-1"), handoffBase, next)
	if string(raw) != wantBroker {
		t.Fatalf("broker request =\n%s\nwant\n%s", raw, wantBroker)
	}
	if projection.Workspace.RecoverRequestID == nil || *projection.Workspace.RecoverRequestID != next {
		t.Fatalf("needs-broker recoverRequestId = %v, want %s", projection.Workspace.RecoverRequestID, next)
	}
	drop(broker)

	cleanup := insertHandoffSaga(t, db, boundary, "cleanup-required", false, 1)
	projection, brief, err = db.WorktreeHandoff(cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if brief != nil || projection.Workspace.BrokerRequest != nil || projection.Workspace.RecoverRequestID == nil {
		t.Fatalf("cleanup-required projection = %+v brief = %v", projection.Workspace, brief)
	}

	failed := insertHandoffSaga(t, db, boundary, "failed", false, 0)
	projection, brief, err = db.WorktreeHandoff(failed)
	if err != nil {
		t.Fatal(err)
	}
	if brief != nil || projection.Workspace.RecoverRequestID != nil || projection.Workspace.BrokerRequest != nil {
		t.Fatalf("terminal projection = %+v brief = %v", projection.Workspace, brief)
	}
}

func TestWorktreeHandoffLinkedRemovalBrokerVerb(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	source := insertHandoffSaga(t, db, boundary, "bound", true, 0)

	// A live linked removal against the bound source: the source itself must
	// refuse (a live saga exists), while the removal renders as recovery
	// material with the remove broker verb.
	fence := handoffFence(db)
	var started string
	if err := db.db.QueryRow(`SELECT started_at FROM runs WHERE run_id=1`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	res, err := db.db.Exec(`INSERT INTO worktree_sagas(run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra)
		VALUES (1,?,'main','1','remove','needs-broker',0,?,?,0,?,?,?,?,?,0,?,?,NULL)`,
		source, fence, fmt.Sprintf("wt.remove.%d", fence), boundary, "/git-admin/worktrees",
		filepath.Join(boundary, ".worktrees", "main-1"), "tasks/main/1", handoffBase, started, started)
	if err != nil {
		t.Fatal(err)
	}
	removal, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = db.WorktreeHandoff(source)
	requireHandoffConflict(t, err, source, "live worktree saga exists")

	projection, brief, err := db.WorktreeHandoff(removal)
	if err != nil {
		t.Fatal(err)
	}
	if brief != nil {
		t.Fatalf("removal saga yielded a leaf brief: %v", brief)
	}
	if projection.Workspace.BrokerRequest == nil || projection.Workspace.BrokerRequest.Verb != "remove" {
		t.Fatalf("removal broker request = %+v, want verb remove", projection.Workspace.BrokerRequest)
	}
	if projection.Workspace.BrokerRequest.BaseCommit == nil || *projection.Workspace.BrokerRequest.BaseCommit != handoffBase {
		t.Fatalf("removal broker base = %v, want the copied source base", projection.Workspace.BrokerRequest.BaseCommit)
	}
}

func TestWorktreeHandoffRecoveryGenerationExhaustion(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()

	exhausted := insertHandoffSaga(t, db, boundary, "needs-broker", false, MaxWorktreeRecoveryGeneration)
	projection, brief, err := db.WorktreeHandoff(exhausted)
	if projection != nil || brief != nil || !errors.Is(err, ErrWorktreeRecoveryExhausted) {
		t.Fatalf("exhausted nonterminal handoff = %v %v %v", projection, brief, err)
	}
	if err == nil || err.Error() != "worktree recovery generation exhausted" {
		t.Fatalf("exhaustion diagnostic = %v", err)
	}

	// A terminal saga at the maximum generation derives no next id and its
	// projection remains available.
	terminal := insertHandoffSaga(t, db, boundary, "failed", false, MaxWorktreeRecoveryGeneration)
	projection, brief, err = db.WorktreeHandoff(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if brief != nil || projection.Workspace.RecoverRequestID != nil {
		t.Fatalf("terminal exhausted projection = %+v brief = %v", projection.Workspace, brief)
	}
}

func TestWorktreeHandoffSelectionMismatchesRefuse(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()

	if _, _, err := db.WorktreeHandoff(99); err == nil || strings.Contains(err.Error(), "does not yield") {
		t.Fatalf("missing saga error = %v", err)
	} else if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing saga error = %v", err)
	}

	// A runless saga has no referenced run to project an owner from.
	var started string
	if err := db.db.QueryRow(`SELECT started_at FROM runs WHERE run_id=1`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	fence := handoffFence(db)
	res, err := db.db.Exec(`INSERT INTO worktree_sagas(run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra)
		VALUES (NULL,NULL,'main','1','remove','cleanup-required',0,?,?,0,?,NULL,?,?,NULL,0,?,?,NULL)`,
		fence, fmt.Sprintf("wt.runless.%d", fence), boundary,
		filepath.Join(boundary, ".worktrees", "main-1"), "tasks/main/1", started, started)
	if err != nil {
		t.Fatal(err)
	}
	runless, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = db.WorktreeHandoff(runless)
	requireHandoffConflict(t, err, runless, "saga names no run")

	// The saga must name the run's own tag and task.
	if _, err := db.AddTask("main", &Task{Title: "other", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	fence = handoffFence(db)
	res, err = db.db.Exec(`INSERT INTO worktree_sagas(run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra)
		VALUES (1,NULL,'main','2','provision','pending',0,?,?,0,?,?,?,?,?,0,?,?,NULL)`,
		fence, fmt.Sprintf("wt.mismatch.%d", fence), boundary, "/git-admin/worktrees",
		filepath.Join(boundary, ".worktrees", "main-2"), "tasks/main/2", handoffBase, started, started)
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = db.WorktreeHandoff(mismatch)
	requireHandoffConflict(t, err, mismatch, "does not name run 1's tag and task")

	// A bound row whose linked removal committed is no longer handoff material.
	inactive := insertHandoffSaga(t, db, boundary, "bound", false, 0)
	_, _, err = db.WorktreeHandoff(inactive)
	requireHandoffConflict(t, err, inactive, "no longer the active binding")

	// The claim members must be the run's own captured acquisition.
	drifted := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	if _, err := db.db.Exec(`UPDATE tasks SET claim_token=claim_token+1 WHERE tag='main' AND id='1'`); err != nil {
		t.Fatal(err)
	}
	_, _, err = db.WorktreeHandoff(drifted)
	requireHandoffConflict(t, err, drifted, "does not match run 1's captured acquisition")
}

func TestWorktreeHandoffMissingAcquisitionReceiptFailsClosed(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	if _, err := db.db.Exec(`DELETE FROM receipts WHERE kind='claim'`); err != nil {
		t.Fatal(err)
	}
	var conflict *HandoffConflict
	_, _, err := db.WorktreeHandoff(sagaID)
	if err == nil || errors.As(err, &conflict) {
		t.Fatalf("missing acquisition receipt = %v, want fail-closed error", err)
	}
	if !strings.Contains(err.Error(), "no recorded receipt") {
		t.Fatalf("missing receipt error = %v", err)
	}
}

func TestWorktreeHandoffStaleNonBoundAndTerminalRefusals(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}

	// A stale controller never discloses recovery material: a needs-broker
	// projection refuses once its claim expires.
	broker := insertHandoffSaga(t, db, boundary, "needs-broker", false, 0)
	exec(`UPDATE tasks SET claim_expires_at='2020-01-01T00:00:00Z' WHERE tag='main' AND id='1'`)
	projection, brief, err := db.WorktreeHandoff(broker)
	if projection != nil || brief != nil || err == nil {
		t.Fatalf("stale needs-broker handoff = %v %v %v", projection, brief, err)
	}
	requireHandoffConflict(t, err, broker, "claim is not live")
	exec(`UPDATE tasks SET claim_expires_at='2999-01-01T00:00:00Z' WHERE tag='main' AND id='1'`)

	exec(`DELETE FROM worktree_sagas WHERE saga_id=?`, broker)

	// A pending projection refuses when the current attempt's lease is dead.
	pending := insertHandoffSaga(t, db, boundary, "pending", false, 0)
	exec(`UPDATE attempts SET expires_at='2020-01-01T00:00:00Z' WHERE run_id=1 AND attempt_no=1`)
	_, _, err = db.WorktreeHandoff(pending)
	requireHandoffConflict(t, err, pending, "attempt lease is not live")
	exec(`UPDATE attempts SET expires_at='2999-01-01T00:00:00Z' WHERE run_id=1 AND attempt_no=1`)
	exec(`DELETE FROM worktree_sagas WHERE saga_id=?`, pending)

	// A terminal controller refuses for terminal saga statuses too.
	at := "2026-08-28T19:00:00Z"
	exec(`UPDATE runs SET status='succeeded', terminal_at=?, updated_at=? WHERE run_id=1`, at, at)
	exec(`UPDATE attempts SET status='succeeded', terminal_at=?, updated_at=? WHERE run_id=1 AND attempt_no=1`, at, at)
	failed := insertHandoffSaga(t, db, boundary, "failed", false, 0)
	projection, brief, err = db.WorktreeHandoff(failed)
	if projection != nil || brief != nil || err == nil {
		t.Fatalf("terminal controller disclosed a failed saga's projection: %v %v %v", projection, brief, err)
	}
	requireHandoffConflict(t, err, failed, "run/attempt pair is succeeded/succeeded")
}

func TestWorktreeHandoffFollowsRenewalAndCurrentAttempt(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	claimToken, attemptToken, _ := handoffControllerNumbers(t, db)

	// Renewal rotates both capabilities and the projection follows the
	// current live controller, not the tokens that planned the saga.
	renew, _, err := db.RenewRun(1, "agent-a", claimToken, 1, attemptToken, "run.renew-handoff", 900)
	if err != nil {
		t.Fatal(err)
	}
	projection, brief, err := db.WorktreeHandoff(sagaID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Claim.Token != renew.ClaimToken || projection.Claim.Token == claimToken ||
		projection.Claim.ExpiresAt != renew.ClaimExpiresAt ||
		projection.Run.AttemptToken != renew.AttemptToken || projection.Run.AttemptToken == attemptToken ||
		projection.Run.LeaseGeneration != renew.LeaseGeneration || projection.Run.LeaseGeneration != 2 {
		t.Fatalf("post-renewal projection = %+v (renew %+v)", projection, renew)
	}
	if brief == nil || brief.Run.Attempt != 1 {
		t.Fatalf("post-renewal brief = %+v", brief)
	}

	// A retry makes attempt 2 current; the saga planned on attempt 1 still
	// renders, disclosing only the current attempt's capability.
	if _, err := db.db.Exec(`UPDATE attempts SET expires_at='2020-01-01T00:00:00Z' WHERE run_id=1 AND attempt_no=1`); err != nil {
		t.Fatal(err)
	}
	retry, _, err := db.RetryRun(1, "agent-a", renew.ClaimToken, 1, renew.AttemptToken, "run.retry-handoff", 900)
	if err != nil {
		t.Fatal(err)
	}
	projection, brief, err = db.WorktreeHandoff(sagaID)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Run.Attempt != 2 || projection.Run.AttemptToken != retry.AttemptToken ||
		projection.Run.AttemptToken == renew.AttemptToken || projection.Run.LeaseGeneration != 1 ||
		projection.Claim.Token != renew.ClaimToken {
		t.Fatalf("post-retry projection = %+v (retry %+v)", projection, retry)
	}
	if brief == nil || brief.Run.Attempt != 2 {
		t.Fatalf("post-retry brief = %+v", brief)
	}
}

// tamperClaimReceipt rewrites the fixture's claim receipt with mutate applied
// and every digest recomputed, so the stored envelope stays self-consistent
// and only the semantic validator can refuse the result.
func tamperClaimReceipt(t *testing.T, db *DB, mutate func(r map[string]any)) {
	t.Helper()
	decoded := readClaimReceipt(t, db)
	mutate(decoded)
	persistClaimReceipt(t, db, decoded)
}

func readClaimReceipt(t *testing.T, db *DB) map[string]any {
	t.Helper()
	var body string
	if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind='claim'`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeJSONObject([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func persistClaimReceipt(t *testing.T, db *DB, decoded map[string]any) {
	t.Helper()
	delete(decoded, "receiptDigest")
	digest, err := receiptDigest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded["receiptDigest"] = digest
	canonical, err := canonicalJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE receipts SET body=?, digest=?, receipt_id=?, created_at=? WHERE kind='claim' AND request_id=?`,
		string(canonical), digest, decoded["receiptId"], decoded["committedAt"], decoded["requestId"]); err != nil {
		t.Fatal(err)
	}
}

// rewriteClaimReceipt mutates the fixture's claim receipt and recomputes every
// deterministic identity — input digest, receipt id, result digest, embedded
// receipt digest, canonical body, and the row's receipt id and digest — so
// the stored envelope and the semantic validator both stay satisfied and only
// the controller-linkage checks can refuse the result. The fixture's
// acquisitions are plain claims, whose request inputs are fully
// reconstructible from the receipt.
func rewriteClaimReceipt(t *testing.T, db *DB, mutate func(r map[string]any)) {
	t.Helper()
	decoded := readClaimReceipt(t, db)
	mutate(decoded)
	ttl, _ := receiptInt64(decoded["ttlSeconds"])
	revision, revisionOK := receiptInt64(decoded["revision"])
	r := claimRequest{operation: decoded["operation"].(string), requestID: decoded["requestId"].(string),
		tag: decoded["tag"].(string), task: decoded["task"].(string), owner: decoded["owner"].(string), ttl: int(ttl)}
	input, err := claimInputDigest(r)
	if err != nil {
		t.Fatal(err)
	}
	decoded["inputDigest"] = input
	decoded["receiptId"] = claimReceiptID(r.requestID, input)
	if revisionOK {
		if result, err := claimResultDigestValues(r.task, revision, r.owner,
			decoded["acquisitionId"].(string), decoded["acquiredAt"].(string), decoded["expiresAt"].(string)); err == nil {
			decoded["resultDigest"] = result
		}
	}
	persistClaimReceipt(t, db, decoded)
}

func TestLoadClaimAcquisitionRevisionValidator(t *testing.T) {
	valid := func(t *testing.T) (*DB, string) {
		db, boundary := newHandoffDB(t)
		t.Cleanup(func() { db.Close() })
		return db, boundary
	}

	t.Run("zero revision", func(t *testing.T) {
		db, _ := valid(t)
		tamperClaimReceipt(t, db, func(r map[string]any) {
			r["revision"] = json.Number("0")
			taskID, owner, acquisition, acquired, expires := r["task"].(string), r["owner"].(string),
				r["acquisitionId"].(string), r["acquiredAt"].(string), r["expiresAt"].(string)
			if result, err := claimResultDigestValues(taskID, 0, owner, acquisition, acquired, expires); err == nil {
				r["resultDigest"] = result
			}
		})
		_, _, err := db.WorktreeHandoff(insertHandoffSaga(t, db, "", "bound", true, 0))
		if err == nil || !strings.Contains(err.Error(), "claim receipt") {
			t.Fatalf("zero revision = %v", err)
		}
	})

	t.Run("overflow revision", func(t *testing.T) {
		db, _ := valid(t)
		tamperClaimReceipt(t, db, func(r map[string]any) {
			r["revision"] = json.Number("9223372036854775808")
		})
		_, _, err := db.WorktreeHandoff(insertHandoffSaga(t, db, "", "bound", true, 0))
		if err == nil || !strings.Contains(err.Error(), "claim receipt") {
			t.Fatalf("overflow revision = %v", err)
		}
	})

	t.Run("re-digested malformed timestamps", func(t *testing.T) {
		db, _ := valid(t)
		tamperClaimReceipt(t, db, func(r map[string]any) {
			r["expiresAt"] = "2999-02-01T00:00:00Z"
		})
		_, _, err := db.WorktreeHandoff(insertHandoffSaga(t, db, "", "bound", true, 0))
		if err == nil || !strings.Contains(err.Error(), "claim receipt") {
			t.Fatalf("malformed timestamps = %v", err)
		}
	})

	t.Run("duplicate acquisition id", func(t *testing.T) {
		db, _ := valid(t)
		var body string
		if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind='claim'`).Scan(&body); err != nil {
			t.Fatal(err)
		}
		decoded, err := decodeJSONObject([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		r := claimRequest{operation: "claim", requestID: "claim.duplicate",
			tag: decoded["tag"].(string), task: decoded["task"].(string),
			owner: decoded["owner"].(string), ttl: 900}
		input, err := claimInputDigest(r)
		if err != nil {
			t.Fatal(err)
		}
		decoded["requestId"] = "claim.duplicate"
		decoded["inputDigest"] = input
		decoded["receiptId"] = claimReceiptID("claim.duplicate", input)
		delete(decoded, "receiptDigest")
		digest, err := receiptDigest(decoded)
		if err != nil {
			t.Fatal(err)
		}
		decoded["receiptDigest"] = digest
		canonical, err := canonicalJSON(decoded)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`INSERT INTO receipts(kind,receipt_id,request_id,body,digest,created_at)
			VALUES (?,?,?,?,?,?)`, ReceiptKindClaim, decoded["receiptId"], "claim.duplicate",
			string(canonical), digest, decoded["committedAt"]); err != nil {
			t.Fatal(err)
		}
		_, _, err = db.WorktreeHandoff(insertHandoffSaga(t, db, "", "bound", true, 0))
		if err == nil || !strings.Contains(err.Error(), "recorded by 2 receipts") {
			t.Fatalf("duplicate acquisition = %v", err)
		}
	})
}

func TestWorktreeHandoffMalformedClaimExpiryRefuses(t *testing.T) {
	db, boundary := newHandoffDB(t)
	defer db.Close()
	malformed := `UPDATE tasks SET claim_expires_at='not-a-timestamp' WHERE tag='main' AND id='1'`

	// An active bound provision never hands off over a malformed expiry.
	bound := insertHandoffSaga(t, db, boundary, "bound", true, 0)
	if _, err := db.db.Exec(malformed); err != nil {
		t.Fatal(err)
	}
	projection, brief, err := db.WorktreeHandoff(bound)
	if projection != nil || brief != nil || err == nil {
		t.Fatalf("malformed claim expiry handed off: %v %v %v", projection, brief, err)
	}
	requireHandoffConflict(t, err, bound, "claim is not live")

	// A non-bound projection is equally refused: recovery material is never
	// disclosed without a provably live controller.
	if _, err := db.db.Exec(`DELETE FROM worktree_sagas WHERE saga_id=?`, bound); err != nil {
		t.Fatal(err)
	}
	broker := insertHandoffSaga(t, db, boundary, "needs-broker", false, 0)
	projection, brief, err = db.WorktreeHandoff(broker)
	if projection != nil || brief != nil || err == nil {
		t.Fatalf("malformed claim expiry disclosed recovery material: %v %v %v", projection, brief, err)
	}
	requireHandoffConflict(t, err, broker, "claim is not live")
}

func TestWorktreeHandoffAcquisitionIdentityDriftRefuses(t *testing.T) {
	t.Run("owner drift", func(t *testing.T) {
		db, boundary := newHandoffDB(t)
		defer db.Close()
		sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
		rewriteClaimReceipt(t, db, func(r map[string]any) { r["owner"] = "agent-b" })
		projection, brief, err := db.WorktreeHandoff(sagaID)
		if projection != nil || brief != nil || err == nil {
			t.Fatalf("owner-drifted receipt handed off: %v %v %v", projection, brief, err)
		}
		requireHandoffConflict(t, err, sagaID, "claim acquisition owner does not match the run controller")
	})

	t.Run("task drift", func(t *testing.T) {
		db, boundary := newHandoffDB(t)
		defer db.Close()
		sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
		rewriteClaimReceipt(t, db, func(r map[string]any) { r["task"] = "2" })
		projection, brief, err := db.WorktreeHandoff(sagaID)
		if projection != nil || brief != nil || err == nil {
			t.Fatalf("task-drifted receipt handed off: %v %v %v", projection, brief, err)
		}
		requireHandoffConflict(t, err, sagaID, "claim acquisition record does not name the saga's tag and task")
	})

	t.Run("acquiredAt drift", func(t *testing.T) {
		db, boundary := newHandoffDB(t)
		defer db.Close()
		sagaID := insertHandoffSaga(t, db, boundary, "bound", true, 0)
		rewriteClaimReceipt(t, db, func(r map[string]any) {
			// Shift the whole acquisition window one hour later so the
			// expiry arithmetic stays coherent; only the linkage to the
			// live claim's preserved acquisition time can refuse it.
			acquired, expires := r["acquiredAt"].(string), r["expiresAt"].(string)
			r["acquiredAt"] = shiftTestTime(t, acquired, time.Hour)
			r["committedAt"] = r["acquiredAt"]
			r["expiresAt"] = shiftTestTime(t, expires, time.Hour)
		})
		projection, brief, err := db.WorktreeHandoff(sagaID)
		if projection != nil || brief != nil || err == nil {
			t.Fatalf("acquiredAt-drifted receipt handed off: %v %v %v", projection, brief, err)
		}
		requireHandoffConflict(t, err, sagaID, "claim acquisition time does not match the live claim")
	})
}

func shiftTestTime(t *testing.T, value string, by time.Duration) string {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed.Add(by).Format(time.RFC3339)
}
