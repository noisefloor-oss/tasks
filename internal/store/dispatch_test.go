package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The §9.3B lineage-proof regressions: each test tampers one committed
// record into a shape the common envelope still accepts, so only the
// complete per-kind receipt contract the proof must enforce can refuse it.

// dispatchLineageFixture is one seeded §9.3B record set: a claim acquisition
// under the derived claim id, one run start under the derived start id
// against that acquisition, one running attempt, and durable expiries pinned
// far in the future. The saga row is inserted per test.
type dispatchLineageFixture struct {
	db                            *DB
	boundary                      string
	claimID, startID, provisionID string
	runID, sagaID                 int64
	claimToken, attempt           int64
	attemptToken                  int64
}

// seedDispatchLineage commits the claim and start steps. plainClaim models a
// lineage whose claim step committed a plain `claim` acquisition under the
// derived claim id instead of the claim-next selection §9.3B composes.
func seedDispatchLineage(t *testing.T, plainClaim bool) *dispatchLineageFixture {
	t.Helper()
	dir := t.TempDir()
	boundary := filepath.Join(dir, "family")
	db, err := CreateWithOwner(filepath.Join(dir, "tasks.sqlite"), boundary)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTask("main", &Task{Title: "dispatch lineage", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	f := &dispatchLineageFixture{db: db, boundary: boundary,
		claimID: "dp1.claim.lineage", startID: "dp1.start.lineage", provisionID: "dp1.provision.lineage"}
	var claim *Claim
	if plainClaim {
		var revision int
		claim, revision, _, err = db.AcquireClaim("main", "1", "agent-a", 900, f.claimID)
		if err != nil || claim == nil {
			t.Fatalf("plain claim = %v %v (revision %d)", claim, err, revision)
		}
	} else {
		var task *Task
		task, claim, _, err = db.ClaimNext("main", "agent-a", 900, "", "", f.claimID)
		if err != nil || task == nil || claim == nil {
			t.Fatalf("claim-next = %v %v %v", task, claim, err)
		}
	}
	run, _, err := db.StartRun("main", "1", "agent-a", claim.Token, f.startID, 900)
	if err != nil {
		t.Fatal(err)
	}
	f.runID, f.attempt = run.RunID, run.Attempt
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`UPDATE tasks SET claim_expires_at='2999-01-01T00:00:00Z' WHERE tag='main' AND id='1'`)
	exec(`UPDATE attempts SET expires_at='2999-01-01T00:00:00Z' WHERE run_id=? AND attempt_no=?`, f.runID, f.attempt)
	if err := db.db.QueryRow(`SELECT claim_token FROM tasks WHERE tag='main' AND id='1'`).Scan(&f.claimToken); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT attempt_token FROM attempts WHERE run_id=? AND attempt_no=?`,
		f.runID, f.attempt).Scan(&f.attemptToken); err != nil {
		t.Fatal(err)
	}
	return f
}

// saga inserts the provision saga row reserving the derived provision id for
// the fixture's run and returns its id.
func (f *dispatchLineageFixture) saga(t *testing.T, status string, active bool, generation int64) int64 {
	t.Helper()
	fence := handoffFence(f.db)
	var started string
	if err := f.db.db.QueryRow(`SELECT started_at FROM runs WHERE run_id=?`, f.runID).Scan(&started); err != nil {
		t.Fatal(err)
	}
	activeInt := int64(0)
	if active {
		activeInt = 1
	}
	res, err := f.db.db.Exec(`INSERT INTO worktree_sagas(run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra)
		VALUES (?,NULL,'main','1','provision',?,?,?,?,?,?,?,?,?,?,0,?,?,NULL)`,
		f.runID, status, activeInt, fence, f.provisionID, generation, f.boundary,
		"/git-admin/worktrees", filepath.Join(f.boundary, ".worktrees", "main-1"), "tasks/main/1",
		handoffBase, started, started)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	f.sagaID = id
	return id
}

// prove runs the §9.3B lineage proof for the fixture's live controller.
func (f *dispatchLineageFixture) prove() (*DispatchSaga, error) {
	return f.db.ProveDispatchLineage(f.claimID, f.startID, f.provisionID, f.runID, f.sagaID,
		"agent-a", f.claimToken, f.attempt, f.attemptToken)
}

// tamperDispatchReceipt rewrites one stored receipt with mutate applied and
// the envelope digest recomputed, so the common envelope stays self-consistent
// and only the per-kind semantic validator can refuse the result.
func tamperDispatchReceipt(t *testing.T, db *DB, kind, requestID string, mutate func(map[string]any)) {
	t.Helper()
	var body string
	if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`, kind, requestID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeJSONObject([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	mutate(decoded)
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
	if _, err := db.db.Exec(`UPDATE receipts SET body=?, digest=? WHERE kind=? AND request_id=?`,
		string(canonical), digest, kind, requestID); err != nil {
		t.Fatal(err)
	}
}

// resignStartResultDigest recomputes the start receipt's result digest from
// its own — possibly already mutated — members, so a tampered result stays
// internally consistent for every member-level check.
func resignStartResultDigest(t *testing.T, r map[string]any) {
	t.Helper()
	attempt, _ := receiptInt64(r["attempt"])
	runID, _ := receiptInt64(r["runId"])
	generation, _ := receiptInt64(r["leaseGeneration"])
	taskRevision, _ := receiptInt64(r["taskRevision"])
	digest, err := runResultDigest(runResultValue{
		Attempt: attempt, AttemptID: r["attemptAcquisitionId"].(string),
		AttemptExpires: r["attemptExpiresAt"].(string), AttemptStatus: "running",
		ClaimID: r["claimAcquisitionId"].(string), ClaimExpires: r["claimExpiresAt"].(string),
		CommittedAt: r["committedAt"].(string), LeaseGeneration: generation,
		Operation: "start", Owner: r["owner"].(string), RunID: runID,
		RunStatus: "running", Tag: r["tag"].(string), Task: r["task"].(string),
		TaskRevision: int(taskRevision), TaskStatus: r["resultTaskStatus"].(string),
	})
	if err != nil {
		t.Fatal(err)
	}
	r["resultDigest"] = digest
}

// TestProveDispatchLineageRequiresClaimNext pins §9.3B's one-claim-next
// lineage: a plain `claim` acquisition committed under the derived claim id
// cannot prove the composed lineage even when every other link — owner, tag,
// task, acquisition, tokens, and saga row — matches exactly.
func TestProveDispatchLineageRequiresClaimNext(t *testing.T) {
	f := seedDispatchLineage(t, true)
	f.saga(t, "pending", false, 0)
	_, err := f.prove()
	var conflict *DispatchConflict
	if !errors.As(err, &conflict) || !strings.Contains(conflict.Reason, "claim-next") {
		t.Fatalf("plain-claim lineage proof = %v (%T), want the typed claim-next conflict", err, err)
	}
}

// TestFindDispatchStartReceiptVerifiesCompleteStartContract pins the prepare
// resume read: a fully re-signed start receipt that redirects the proven run
// keeps every envelope, input-digest, and shallow-field invariant — runId is
// a result member — so only the complete §9.3 start-receipt contract, here
// the started event's citation, refuses it.
func TestFindDispatchStartReceiptVerifiesCompleteStartContract(t *testing.T) {
	f := seedDispatchLineage(t, false)
	found, err := f.db.FindDispatchStartReceipt(f.startID, "main", "1", "agent-a", 900)
	if err != nil || found == nil || found.RunID != f.runID {
		t.Fatalf("genuine start receipt = %v %v, want run %d", found, err, f.runID)
	}
	tamperDispatchReceipt(t, f.db, ReceiptKindRun, f.startID, func(r map[string]any) {
		r["runId"] = json.Number(strconv.FormatInt(f.runID+1, 10))
		resignStartResultDigest(t, r)
	})
	_, err = f.db.FindDispatchStartReceipt(f.startID, "main", "1", "agent-a", 900)
	var integrity *ReceiptIntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("redirected start receipt = %v (%T), want the integrity refusal", err, err)
	}
}

// TestProveDispatchLineageVerifiesCompleteStartContract pins the reconcile
// proof: a re-signed start receipt whose recorded attempt lease no longer
// satisfies the committedAt+ttl arithmetic passes every member the shallow
// proof compared, and only the complete §9.3 start-receipt contract refuses
// it.
func TestProveDispatchLineageVerifiesCompleteStartContract(t *testing.T) {
	f := seedDispatchLineage(t, false)
	f.saga(t, "pending", false, 0)
	if saga, err := f.prove(); err != nil || saga == nil || saga.Status != "pending" {
		t.Fatalf("genuine lineage proof = %v %v", saga, err)
	}
	tamperDispatchReceipt(t, f.db, ReceiptKindRun, f.startID, func(r map[string]any) {
		expires, _ := r["attemptExpiresAt"].(string)
		when, err := time.Parse(time.RFC3339, expires)
		if err != nil {
			t.Fatalf("stored attempt expiry %q: %v", expires, err)
		}
		r["attemptExpiresAt"] = when.Add(time.Second).UTC().Format(time.RFC3339)
		resignStartResultDigest(t, r)
	})
	_, err := f.prove()
	var integrity *ReceiptIntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("altered start result = %v (%T), want the integrity refusal", err, err)
	}
}

// TestProveDispatchLineageVerifiesTerminalOriginalReceipt pins the §9.3B
// terminal-replay precondition: a terminal saga's original provision receipt
// is verified against the saga row by the §9.4 original-receipt contract —
// the same validation the provision and recover primitives enforce — not
// merely envelope-checked, so a re-signed receipt naming another workspace
// cannot carry a terminal replay to a leaf brief.
func TestProveDispatchLineageVerifiesTerminalOriginalReceipt(t *testing.T) {
	f := seedDispatchLineage(t, false)
	f.saga(t, "bound", true, 0)
	insertTestWorktreeReceipt(t, f.db, f.sagaID, 0, "bound", true)
	if saga, err := f.prove(); err != nil || saga == nil || saga.Status != "bound" {
		t.Fatalf("genuine terminal lineage proof = %v %v", saga, err)
	}
	tamperDispatchReceipt(t, f.db, ReceiptKindWorktree, f.provisionID, func(r map[string]any) {
		if path, ok := r["path"].(string); ok {
			r["path"] = path + ".redirect"
		}
	})
	_, err := f.prove()
	var integrity *ReceiptIntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("altered original receipt = %v (%T), want the integrity refusal", err, err)
	}
}
