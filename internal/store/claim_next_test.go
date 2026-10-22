package store

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// claimNextReqSeq hands every helper acquisition a distinct request id so
// each call performs a real acquisition; replay tests call ClaimNext
// directly with explicit ids.
var claimNextReqSeq atomic.Int64

func claimNextTask(t *testing.T, db *DB, owner string, ttl int, milestone, assignment string) (*Task, *Claim) {
	t.Helper()
	task, claim, _, err := db.ClaimNext("main", owner, ttl, milestone, assignment,
		fmt.Sprintf("req-cn-%d", claimNextReqSeq.Add(1)))
	if err != nil {
		t.Fatalf("claim-next: %v", err)
	}
	return task, claim
}

func addFullTask(t *testing.T, db *DB, title, priority, milestone string, deps ...string) string {
	t.Helper()
	id, err := db.AddTask("main", &Task{Title: title, Status: "pending", Priority: priority, Milestone: milestone, Dependencies: deps})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func setTaskStatus(t *testing.T, db *DB, id, status string, revision int, gate *ClaimGate) {
	t.Helper()
	if _, err := db.SetStatus("main", id, status, revision, gate); err != nil {
		t.Fatal(err)
	}
}

func startRunWithExpiredClaim(t *testing.T, db *DB, taskID, requestSuffix string) (*Claim, *RunStartResult) {
	t.Helper()
	claim, _, replayed, err := db.AcquireClaim("main", taskID, "run-owner", 3600, "claim.run-exclusion."+requestSuffix)
	if err != nil || replayed {
		t.Fatalf("acquire run claim: claim=%+v replayed=%v err=%v", claim, replayed, err)
	}
	run, replayed, err := db.StartRun("main", taskID, "run-owner", claim.Token, "run.exclusion."+requestSuffix, 300)
	if err != nil || replayed {
		t.Fatalf("start run: run=%+v replayed=%v err=%v", run, replayed, err)
	}
	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at=? WHERE tag='main' AND id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), taskID); err != nil {
		t.Fatal(err)
	}
	return claim, run
}

func cancelRunForClaimExclusion(t *testing.T, db *DB, runID int64) {
	t.Helper()
	at := nowUTC()
	if err := db.transact("BEGIN IMMEDIATE", func(tx *tx) error {
		if _, err := tx.Exec(`UPDATE attempts SET status='cancelled', updated_at=?, terminal_at=?
			WHERE run_id=? AND status='running'`, at, at, runID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE runs SET status='cancelled', updated_at=?, terminal_at=? WHERE run_id=?`, at, at, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func makeRunRetryableForClaimExclusion(t *testing.T, db *DB, runID int64) {
	t.Helper()
	at := nowUTC()
	if err := db.transact("BEGIN IMMEDIATE", func(tx *tx) error {
		if _, err := tx.Exec(`UPDATE attempts SET status='failed', updated_at=?, terminal_at=?
			WHERE run_id=? AND status='running'`, at, at, runID); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE runs SET status='retryable', updated_at=? WHERE run_id=?`, at, runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireClaimExcludesNonterminalRunAfterClaimExpiry(t *testing.T) {
	db, _ := newDB(t)
	defer db.Close()
	id := addFullTask(t, db, "running", "", "")
	original, run := startRunWithExpiredClaim(t, db, id, "acquire")

	beforeFence, err := db.Fencing()
	if err != nil {
		t.Fatal(err)
	}
	var beforeEvents, beforeReceipts int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM receipts`).Scan(&beforeReceipts); err != nil {
		t.Fatal(err)
	}

	claim, _, replayed, err := db.AcquireClaim("main", id, "new-owner", 300, "claim.blocked-by-run")
	var conflict *ClaimConflict
	if claim != nil || replayed || !errors.As(err, &conflict) || conflict.Kind != "claim" {
		t.Fatalf("claim across running run = %+v replayed=%v err=%v", claim, replayed, err)
	}
	if conflict.Owner != "" || conflict.Expiry != "" || conflict.FencingToken != 0 {
		t.Fatalf("running-run conflict disclosed stale claim: %+v", conflict)
	}
	afterFence, _ := db.Fencing()
	var afterEvents, afterReceipts int
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM events`).Scan(&afterEvents)
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM receipts`).Scan(&afterReceipts)
	if afterFence != beforeFence || afterEvents != beforeEvents || afterReceipts != beforeReceipts {
		t.Fatalf("refusal changed effects: fence %d->%d events %d->%d receipts %d->%d",
			beforeFence, afterFence, beforeEvents, afterEvents, beforeReceipts, afterReceipts)
	}
	var physicalToken int64
	if err := db.db.QueryRow(`SELECT claim_token FROM tasks WHERE tag='main' AND id=?`, id).Scan(&physicalToken); err != nil || physicalToken != original.Token {
		t.Fatalf("physical claim token = %d, %v; want original %d", physicalToken, err, original.Token)
	}

	// A terminal run no longer excludes the task, and the failed request id
	// was not reserved: retrying that exact acquisition performs one effect.
	cancelRunForClaimExclusion(t, db, run.RunID)
	claim, _, replayed, err = db.AcquireClaim("main", id, "new-owner", 300, "claim.blocked-by-run")
	if err != nil || replayed || claim == nil || claim.Owner != "new-owner" || claim.Token <= original.Token {
		t.Fatalf("claim after terminal run = %+v replayed=%v err=%v", claim, replayed, err)
	}
}

func TestClaimNextExcludesNonterminalRunAfterClaimExpiry(t *testing.T) {
	db, _ := newDB(t)
	defer db.Close()
	held := addFullTask(t, db, "running-high", "high", "")
	free := addFullTask(t, db, "free-low", "low", "")
	_, run := startRunWithExpiredClaim(t, db, held, "claim-next")
	makeRunRetryableForClaimExclusion(t, db, run.RunID)

	task, claim, replayed, err := db.ClaimNext("main", "next-owner", 300, "", "", "claim-next.run-exclusion")
	if err != nil || replayed || task == nil || task.ID != free || claim == nil {
		t.Fatalf("claim-next = task %+v claim %+v replayed=%v err=%v; want free task %s", task, claim, replayed, err, free)
	}
	task, claim, replayed, err = db.ClaimNext("main", "other-owner", 300, "", "", "claim-next.only-run")
	if err != nil || replayed || task != nil || claim != nil {
		t.Fatalf("claim-next with only running task = task %+v claim %+v replayed=%v err=%v", task, claim, replayed, err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindClaim, "claim-next.only-run"); err != nil || receipt != nil {
		t.Fatalf("empty run-excluded claim-next receipt = %v, %v", receipt, err)
	}
}

func TestNonterminalRunTaskIDsLegacyCompatibility(t *testing.T) {
	for _, version := range []int{identitySchemaVersion, orchestratorSchemaVersion} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			db, _ := newDB(t)
			defer db.Close()
			addFullTask(t, db, "legacy", "", "")
			if version == identitySchemaVersion {
				rewindStoreToV2(t, db)
			} else {
				rewindStoreToV3(t, db)
			}
			ids, err := db.NonterminalRunTaskIDs("main")
			if err != nil || len(ids) != 0 {
				t.Fatalf("nonterminal ids = %v, %v; want empty", ids, err)
			}
			if got, err := db.userVersion(); err != nil || got != version {
				t.Fatalf("read changed schema = %d, %v; want %d", got, err, version)
			}
			task, claim, replayed, err := db.ClaimNext("main", "legacy-owner", 300, "", "",
				fmt.Sprintf("claim-next.legacy-v%d", version))
			if err != nil || replayed || task == nil || task.ID != "1" || claim == nil {
				t.Fatalf("claim-next after v%d read = task %+v claim %+v replayed=%v err=%v", version, task, claim, replayed, err)
			}
			if got, err := db.userVersion(); err != nil || got != SchemaVersion {
				t.Fatalf("mutation schema = %d, %v; want %d", got, err, SchemaVersion)
			}
		})
	}

	t.Run("malformed-v4", func(t *testing.T) {
		db, _ := newDB(t)
		defer db.Close()
		rewindStoreToV3(t, db)
		if _, err := db.db.Exec(`PRAGMA user_version=4`); err != nil {
			t.Fatal(err)
		}
		if ids, err := db.NonterminalRunTaskIDs("main"); err == nil {
			t.Fatalf("malformed v4 returned ids %v without error", ids)
		}
	})
}

// TestClaimNextOrderingAndDependencyReadiness pins §8.3: deterministic next
// ordering (priority high > medium (absent) > low, ties by lower id) over
// dependency-ready pending tasks only.
func TestClaimNextOrderingAndDependencyReadiness(t *testing.T) {
	db, _ := newDB(t)
	defer db.Close()
	low := addFullTask(t, db, "low", "low", "")
	medium := addFullTask(t, db, "medium", "", "")
	blocked := addFullTask(t, db, "high-blocked", "high", "", medium)
	done := addFullTask(t, db, "done-dep", "high", "", low)
	setTaskStatus(t, db, done, "done", 1, nil)

	// The blocked high-priority task gates on its pending dependency; the
	// done dependency satisfies, so the medium task wins over low.
	task, claim := claimNextTask(t, db, "agent-a", 300, "", "")
	if task == nil || task.ID != medium {
		t.Fatalf("claim-next = %v, want task %s", task, medium)
	}
	if claim.Owner != "agent-a" || claim.TTL != 300 || claim.Token == 0 {
		t.Fatalf("claim = %+v", claim)
	}

	// With medium cancelled by its holder (satisfying blocked's dependency)
	// the unblocked high task outranks low.
	setTaskStatus(t, db, medium, "cancelled", 1, &ClaimGate{Owner: "agent-a", Token: claim.Token})
	task, _ = claimNextTask(t, db, "agent-b", 0, "", "")
	if task == nil || task.ID != blocked {
		t.Fatalf("second claim-next = %v, want the unblocked task %s", task, blocked)
	}

	// Low is next; cancelling it by its holder satisfies done-dep.
	task, lowClaim := claimNextTask(t, db, "agent-a", 0, "", "")
	if task == nil || task.ID != low {
		t.Fatalf("third claim-next = %v, want task %s", task, low)
	}
	setTaskStatus(t, db, low, "cancelled", 1, &ClaimGate{Owner: "agent-a", Token: lowClaim.Token})

	// Everything is now claimed, satisfied, or no longer pending: the
	// successful empty result.
	if task, _ := claimNextTask(t, db, "agent-b", 0, "", ""); task != nil {
		t.Fatalf("claim-next on exhausted store = %v, want nil", task)
	}
}

// TestClaimNextExcludesClaimedExpiredAndClosedMilestone pins the two
// claim-specific exclusions and expired-lease reclaim.
func TestClaimNextExcludesClaimedExpiredAndClosedMilestone(t *testing.T) {
	db, _ := newDB(t)
	defer db.Close()
	if err := db.AddMilestone("main", "m1"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMilestone("main", "m2"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetMilestoneStatus("main", "m2", "closed"); err != nil {
		t.Fatal(err)
	}
	open1 := addFullTask(t, db, "open-1", "", "m1")
	closed := addFullTask(t, db, "closed", "high", "m2")
	unclaimed := addFullTask(t, db, "open-2", "low", "m1")

	// The closed-milestone high task is not a candidate; the open one is.
	if task, _ := claimNextTask(t, db, "agent-a", 0, "", ""); task == nil || task.ID != open1 {
		t.Fatalf("claim-next = %v, want %s", task, open1)
	}

	// An unexpired claim excludes open1; the next call takes the low task,
	// never the closed-milestone task.
	if task, _ := claimNextTask(t, db, "agent-b", 0, "", ""); task == nil || task.ID != unclaimed {
		t.Fatalf("claim-next = %v, want %s", task, unclaimed)
	}
	if task, _ := claimNextTask(t, db, "agent-c", 0, "", ""); task != nil && task.ID == closed {
		t.Fatalf("closed-milestone task %s selected", closed)
	}

	// Force open1's lease into the past: the expired lease is claimable in
	// the same transaction that judges it.
	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at=? WHERE tag='main' AND id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), open1); err != nil {
		t.Fatal(err)
	}
	task, claim := claimNextTask(t, db, "agent-c", 0, "", "")
	if task == nil || task.ID != open1 || claim.Owner != "agent-c" {
		t.Fatalf("expired-lease reclaim = %v %+v", task, claim)
	}
}

// TestClaimNextFilters pins the exact --milestone filter and the §8.3
// assignment-filter rule: only tasks durably assigned to the requested owner
// are candidates, and the filter never falls back to unassigned work.
func TestClaimNextFilters(t *testing.T) {
	db, _ := newDB(t)
	defer db.Close()
	if err := db.AddMilestone("main", "m1"); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMilestone("main", "m2"); err != nil {
		t.Fatal(err)
	}
	inM1 := addFullTask(t, db, "in-m1", "high", "m1")
	inM2 := addFullTask(t, db, "in-m2", "low", "m2")

	task, _ := claimNextTask(t, db, "agent-a", 0, "m2", "")
	if task == nil || task.ID != inM2 {
		t.Fatalf("milestone filter = %v, want %s", task, inM2)
	}
	task, inM1Claim := claimNextTask(t, db, "agent-a", 0, "m1", "")
	if task == nil || task.ID != inM1 {
		t.Fatalf("milestone filter = %v, want %s", task, inM1)
	}
	// A milestone removed after tasks referenced it clears their references;
	// the stale filter value then names no milestone and leaves no
	// candidates (the CLI checks knowledge earlier, §12.2). Clearing a
	// claimed task's field is fenced (§8.3), so release the claim first.
	if err := db.ReleaseClaim("main", inM1, "agent-a", inM1Claim.Token, false); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveMilestone("main", "m1", true); err != nil {
		t.Fatal(err)
	}
	if task, _ := claimNextTask(t, db, "agent-a", 0, "m1", ""); task != nil {
		t.Fatalf("removed-milestone filter = %v, want nil", task)
	}
	if task, _ := claimNextTask(t, db, "agent-a", 0, "", "orchestrator"); task != nil {
		t.Fatalf("assignment filter = %v, want nil before assignment", task)
	}

	plan := mustOrchestratorPlan(t, `{
		"schemaVersion":1,"planId":"claim-next-assignment","tag":"main",
		"milestones":[],
		"tasks":[{"key":"assigned","title":"assigned work","description":"","details":"","priority":"medium","dependencies":[]}],
		"assignments":[{"key":"primary","task":{"key":"assigned"},"owner":"orchestrator"}]
	}`)
	result, err := db.ApplyOrchestratorPlan(plan, plan.Digest, "claim-next-assignment")
	if err != nil {
		t.Fatal(err)
	}
	mappings := result.Receipt["taskMappings"].([]any)
	assignedID := mappings[0].(map[string]any)["id"].(string)
	if task, _ := claimNextTask(t, db, "agent-a", 0, "", "someone-else"); task != nil {
		t.Fatalf("wrong-owner assignment filter = %v, want nil", task)
	}
	if task, _ := claimNextTask(t, db, "agent-a", 0, "", "orchestrator"); task == nil || task.ID != assignedID {
		t.Fatalf("assignment filter = %v, want assigned task %s", task, assignedID)
	}
}

// TestClaimNextFencingContract pins the audit and CAS interaction: the task
// revision is untouched, the fencing counter advances store-wide, and one
// claim event names the owner without a token.
func TestClaimNextFencingContract(t *testing.T) {
	db, _ := newDB(t)
	defer db.Close()
	id := addFullTask(t, db, "fenced", "", "")
	before, err := db.Fencing()
	if err != nil {
		t.Fatal(err)
	}
	task, claim := claimNextTask(t, db, "agent-a", 120, "", "")
	if task == nil || task.ID != id {
		t.Fatalf("claim-next = %v", task)
	}
	if task.Revision != 1 {
		t.Fatalf("revision = %d, want unchanged 1", task.Revision)
	}
	after, err := db.Fencing()
	if err != nil {
		t.Fatal(err)
	}
	if after != claim.Token || after <= before {
		t.Fatalf("fencing %d -> %d, token %d", before, after, claim.Token)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range feed.Events {
		if e.Kind == "claim" && e.Task == id {
			found = true
			if e.Owner != "agent-a" {
				t.Fatalf("claim event owner = %q", e.Owner)
			}
			if strings.Contains(string(mustMarshalEvent(e)), `"token"`) {
				t.Fatalf("claim event leaked a token: %v", e)
			}
		}
	}
	if !found {
		t.Fatal("no claim event appended")
	}

	// Field mutations on the unexpired claim are gated (§8.3).
	err = db.UpdateTask("main", id, 1, nil, func(task *Task) error { return nil })
	var cc *ClaimConflict
	if !errors.As(err, &cc) || cc.Kind != "update" {
		t.Fatalf("ungated update: %v, want update ClaimConflict", err)
	}
	if _, err := db.AppendDetails("main", id, "note", 1, nil); !errors.As(err, &cc) {
		t.Fatalf("ungated append: %v, want ClaimConflict", err)
	}
	if err := db.UpdateTask("main", id, 1, &ClaimGate{Owner: "agent-a", Token: claim.Token}, func(task *Task) error {
		task.Title = "renamed"
		return nil
	}); err != nil {
		t.Fatalf("gated update: %v", err)
	}
	// The milestone field mutation is gated identically.
	if err := db.AddMilestone("main", "m"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetTaskMilestone("main", id, "m", 2, nil); !errors.As(err, &cc) {
		t.Fatalf("ungated set-milestone: %v, want ClaimConflict", err)
	}
	if _, err := db.SetTaskMilestone("main", id, "m", 2, &ClaimGate{Owner: "agent-a", Token: claim.Token}); err != nil {
		t.Fatalf("gated set-milestone: %v", err)
	}
	// An expired claim no longer gates: force expiry, then mutate ungated.
	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at=? WHERE tag='main' AND id=?`,
		time.Now().UTC().Add(-time.Minute).Format(time.RFC3339), id); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTask("main", id, 3, nil, func(task *Task) error { return nil }); err != nil {
		t.Fatalf("update after lease expiry: %v", err)
	}
}

func mustMarshalEvent(e Event) []byte {
	b, err := e.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return b
}
