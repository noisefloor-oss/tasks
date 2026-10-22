package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestRunStartDigestVectors(t *testing.T) {
	r := runRequest{operation: "start", requestID: "req.start", tag: "main", task: "7", owner: "agent-a", ttl: 900}
	input, err := runInputDigest(r)
	if err != nil || input != "sha256:fe37acb4ec67410fdf246deecda135cd1484d90b46399c6c4f441d098e5e3ed5" {
		t.Fatalf("input digest = %q, %v", input, err)
	}
	if got := runReceiptID(r.requestID, input); got != "run-9a5a925849e5b65b1f0507cb457237bdacc62e44b85422f6b448cac819110dcc" {
		t.Fatalf("receipt id = %q", got)
	}
	result, err := runResultDigest(runResultValue{
		Attempt: 1, AttemptID: "11223344556677889900aabbccddeeff",
		AttemptExpires: "2026-08-28T18:00:00Z", AttemptStatus: "running",
		ClaimID: "00112233445566778899aabbccddeeff", ClaimExpires: "2026-08-28T18:00:00Z",
		CommittedAt: "2026-08-28T17:45:00Z", LeaseGeneration: 1, Operation: "start",
		Owner: "agent-a", RunID: 1, RunStatus: "running", Tag: "main", Task: "7",
		TaskRevision: 3, TaskStatus: "pending",
	})
	if err != nil || result != "sha256:bc41786d858cd06eb463a4d25a4f7a5f59c241790f59f524cca83ddd50374c98" {
		t.Fatalf("result digest = %q, %v", result, err)
	}
	receipt := map[string]any{
		"attempt": int64(1), "attemptAcquisitionId": "11223344556677889900aabbccddeeff",
		"attemptExpiresAt": "2026-08-28T18:00:00Z", "attemptStatus": "running",
		"claimAcquisitionId": "00112233445566778899aabbccddeeff", "claimExpiresAt": "2026-08-28T18:00:00Z",
		"committedAt": "2026-08-28T17:45:00Z", "eventSeq": map[string]any{"first": int64(11), "last": int64(11)},
		"familyId": "123e4567-e89b-42d3-a456-426614174000", "force": false, "inputDigest": input,
		"inventory": map[string]any{"assignments": int64(0), "attempts": int64(1), "bindings": int64(0), "claims": int64(1), "dependencies": int64(0), "events": int64(1), "milestones": int64(0), "planMappings": int64(0), "plans": int64(0), "receipts": int64(1), "runs": int64(1), "subtasks": int64(0), "tags": int64(1), "tasks": int64(1), "worktreeSagas": int64(0)},
		"kind":      "run", "leaseGeneration": int64(1), "operation": "start", "outcome": "", "owner": "agent-a",
		"receiptId": runReceiptID(r.requestID, input), "requestId": "req.start", "resultDigest": result,
		"resultTaskStatus": "pending", "revision": int64(0), "runId": int64(1), "runStatus": "running",
		"schemaVersion": int64(1), "sourceAttempt": int64(0), "tag": "main", "task": "7",
		"taskRevision": int64(3), "taskStatus": "", "terminalResultDigest": "", "ttlSeconds": int64(900),
	}
	digest, err := receiptDigest(receipt)
	if err != nil || digest != "sha256:26595634d1c83b5dce5a1af1ea51cce81543277229756ce752696a360b2aa676" {
		t.Fatalf("receipt digest = %q, %v", digest, err)
	}
}

func startRunFixture(t *testing.T, ttl int) (*DB, string, *Claim, *RunStartResult) {
	t.Helper()
	db, _ := newDB(t)
	id := addTask(t, db, "main", "run")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.start")
	if err != nil {
		t.Fatal(err)
	}
	result, replayed, err := db.StartRun("main", id, "agent-a", claim.Token, "run.start", ttl)
	if err != nil || replayed {
		t.Fatalf("start = %+v replayed=%v err=%v", result, replayed, err)
	}
	return db, id, claim, result
}

func TestRunStartEffectReceiptAndReplay(t *testing.T) {
	db, id, claim, first := startRunFixture(t, 300)
	if first.RunID <= 0 || first.Attempt != 1 || first.LeaseGeneration != 1 || first.AttemptToken <= claim.Token {
		t.Fatalf("start result = %+v after claim %+v", first, claim)
	}
	var runs, attempts int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || attempts != 1 {
		t.Fatalf("rows = %d runs, %d attempts", runs, attempts)
	}
	receipt, err := db.FindReceipt(ReceiptKindRun, "run.start")
	if err != nil || receipt == nil {
		t.Fatalf("receipt = %v, %v", receipt, err)
	}
	body, err := canonicalJSON(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "attemptToken") || strings.Contains(string(body), "claimToken") || strings.Contains(string(body), "fencingToken") {
		t.Fatalf("receipt leaked a capability: %s", body)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != ReceiptKindRun || last.Action != "started" || last.Task != id {
		t.Fatalf("run event = %+v", last)
	}
	var eventReceipt string
	if err := json.Unmarshal(last.Extra["receiptId"], &eventReceipt); err != nil || eventReceipt != receipt["receiptId"] {
		t.Fatalf("event receipt = %q, %v", eventReceipt, err)
	}
	fence, _ := db.Fencing()
	again, replayed, err := db.StartRun("main", id, "agent-a", claim.Token+999, "run.start", 300)
	if err != nil || !replayed || *again != *first {
		t.Fatalf("replay = %+v replayed=%v err=%v; want %+v", again, replayed, err, first)
	}
	afterFence, _ := db.Fencing()
	if afterFence != fence {
		t.Fatalf("replay advanced fence %d -> %d", fence, afterFence)
	}
	if _, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.start", 301); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("divergent replay = %v", err)
	}
}

func TestRunStartRefusalsAndRollback(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "run")
	if _, _, err := db.StartRun("main", id, "agent-a", 1, "run.none", 300); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("unclaimed start = %v", err)
	}
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.rollback")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.StartRun("main", id+".1", "agent-a", claim.Token, "run.subtask", 300); err == nil {
		t.Fatal("subtask start succeeded")
	}
	if _, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.badttl", MaxRunTTL+1); err == nil {
		t.Fatal("oversize ttl succeeded")
	}
	oldHook := runFaultHook
	runFaultHook = func(point string) error {
		if point == "before-commit" {
			return fmt.Errorf("injected crash")
		}
		return nil
	}
	t.Cleanup(func() { runFaultHook = oldHook })
	if _, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.rollback", 300); err == nil {
		t.Fatal("faulted start succeeded")
	}
	for table, want := range map[string]int{"runs": 0, "attempts": 0} {
		var got int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s after rollback = %d, %v", table, got, err)
		}
	}
	if receipt, err := db.FindReceipt(ReceiptKindRun, "run.rollback"); err != nil || receipt != nil {
		t.Fatalf("rollback receipt = %v, %v", receipt, err)
	}
	runFaultHook = oldHook
	if _, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.rollback", 300); err != nil {
		t.Fatalf("retry after rollback = %v", err)
	}
	if _, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.second", 300); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("second nonterminal start = %v", err)
	}
}

func TestConcurrentRunStartOneEffectAndReplay(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "run")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.concurrent")
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result   *RunStartResult
		replayed bool
		err      error
	}
	start := make(chan struct{})
	out := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, replayed, err := db.StartRun("main", id, "agent-a", claim.Token, "run.concurrent", 300)
			out <- outcome{result, replayed, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var fresh, replay int
	var want *RunStartResult
	for got := range out {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if want == nil {
			want = got.result
		} else if *want != *got.result {
			t.Fatalf("results differ: %+v / %+v", want, got.result)
		}
		if got.replayed {
			replay++
		} else {
			fresh++
		}
	}
	if fresh != 1 || replay != 1 {
		t.Fatalf("fresh=%d replay=%d", fresh, replay)
	}
}

func TestConcurrentDistinctRunStartsHaveOneWinner(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "run")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.distinct")
	if err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		result *RunStartResult
		err    error
	}
	start := make(chan struct{})
	out := make(chan outcome, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			result, _, err := db.StartRun("main", id, "agent-a", claim.Token, fmt.Sprintf("run.distinct.%d", n), 300)
			out <- outcome{result, err}
		}(i)
	}
	close(start)
	wg.Wait()
	close(out)
	var successes, conflicts int
	for got := range out {
		switch {
		case got.err == nil && got.result != nil:
			successes++
		case errors.As(got.err, new(*RunConflict)):
			conflicts++
		default:
			t.Fatalf("unexpected result %+v, err=%v", got.result, got.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	var runs, attempts, receipts int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM attempts`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM receipts WHERE kind='run'`).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if runs != 1 || attempts != 1 || receipts != 1 {
		t.Fatalf("effects = %d runs, %d attempts, %d receipts", runs, attempts, receipts)
	}
}

func TestRunStartReplayRequiresCurrentLiveLineage(t *testing.T) {
	t.Run("attempt generation", func(t *testing.T) {
		db, id, claim, _ := startRunFixture(t, 300)
		if _, err := db.db.Exec(`UPDATE attempts SET lease_generation=2 WHERE run_id=1 AND attempt_no=1`); err != nil {
			t.Fatal(err)
		}
		_, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.start", 300)
		if !errors.As(err, new(*RunConflict)) {
			t.Fatalf("superseded replay = %v", err)
		}
	})
	t.Run("claim expiry", func(t *testing.T) {
		db, id, claim, _ := startRunFixture(t, 300)
		if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id=?`, id); err != nil {
			t.Fatal(err)
		}
		_, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.start", 300)
		if !errors.As(err, new(*RunConflict)) {
			t.Fatalf("expired-claim replay = %v", err)
		}
	})
	t.Run("durable linkage", func(t *testing.T) {
		db, id, claim, _ := startRunFixture(t, 300)
		if _, err := db.db.Exec(`UPDATE runs SET start_request_id='different' WHERE run_id=1`); err != nil {
			t.Fatal(err)
		}
		_, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.start", 300)
		if !errors.As(err, new(*RunConflict)) {
			t.Fatalf("altered linkage replay = %v", err)
		}
	})
}

func TestRunStartFencesClaimAcquisitionAfterClaimExpiry(t *testing.T) {
	db, id, _, _ := startRunFixture(t, 300)
	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id=?`, id); err != nil {
		t.Fatal(err)
	}
	claim, _, replayed, err := db.AcquireClaim("main", id, "agent-b", 300, "claim.after-run")
	var conflict *ClaimConflict
	if claim != nil || replayed || !errors.As(err, &conflict) {
		t.Fatalf("claim across nonterminal run = %+v replayed=%v err=%v", claim, replayed, err)
	}
	if conflict.Owner != "" || conflict.Expiry != "" || conflict.FencingToken != 0 {
		t.Fatalf("run-fenced claim disclosed stale capability lineage: %+v", conflict)
	}
	task, claim, replayed, err := db.ClaimNext("main", "agent-b", 300, "", "", "claim-next.after-run")
	if err != nil || replayed || task != nil || claim != nil {
		t.Fatalf("claim-next across nonterminal run = task %+v claim %+v replayed=%v err=%v", task, claim, replayed, err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindClaim, "claim-next.after-run"); err != nil || receipt != nil {
		t.Fatalf("empty claim-next receipt = %v, %v", receipt, err)
	}
}

func TestNonterminalRunFencesBareClaimRenewAndRelease(t *testing.T) {
	t.Run("renew", func(t *testing.T) {
		db, id, claim, _ := startRunFixture(t, 300)
		before, _ := db.Fencing()
		if _, _, err := db.RenewClaim("main", id, "agent-a", claim.Token, 600); !errors.As(err, new(*ClaimConflict)) {
			t.Fatalf("bare renew = %v", err)
		}
		after, _ := db.Fencing()
		if after != before {
			t.Fatalf("refused renew advanced fence %d -> %d", before, after)
		}
	})
	for _, tc := range []struct {
		name    string
		release func(*DB, string, *Claim) error
	}{
		{"store", func(db *DB, id string, claim *Claim) error {
			return db.ReleaseClaim("main", id, "agent-a", claim.Token, false)
		}},
		{"worktree", func(db *DB, id string, claim *Claim) error {
			_, err := db.ReleaseClaimWorktree("", "main", id, "agent-a", claim.Token, false, false)
			return err
		}},
	} {
		t.Run("release-"+tc.name, func(t *testing.T) {
			db, id, claim, _ := startRunFixture(t, 300)
			if err := tc.release(db, id, claim); !errors.As(err, new(*ClaimConflict)) {
				t.Fatalf("ordinary release = %v", err)
			}
			task, err := db.GetTask("main", id)
			if err != nil || task.Claim == nil || task.Claim.Token != claim.Token {
				t.Fatalf("claim after refused release = %+v, %v", task, err)
			}
		})
	}
}

func TestForceReleaseCancelsRunAndAttemptAtomically(t *testing.T) {
	for _, tc := range []struct {
		name    string
		release func(*DB, string, *Claim) error
	}{
		{"store", func(db *DB, id string, claim *Claim) error {
			return db.ReleaseClaim("main", id, "operator", 0, true)
		}},
		{"worktree", func(db *DB, id string, claim *Claim) error {
			_, err := db.ReleaseClaimWorktree("", "main", id, "operator", 0, true, false)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, id, claim, run := startRunFixture(t, 300)
			if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id=?`, id); err != nil {
				t.Fatal(err)
			}
			before, _ := db.Fencing()
			if err := tc.release(db, id, claim); err != nil {
				t.Fatal(err)
			}
			after, _ := db.Fencing()
			if after != before+1 {
				t.Fatalf("force-release fence %d -> %d", before, after)
			}
			var runStatus, attemptStatus, runTerminal, attemptTerminal string
			var forceFence int64
			if err := db.db.QueryRow(`SELECT r.status,r.force_cancel_fence,r.terminal_at,a.status,a.terminal_at
				FROM runs r JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
				WHERE r.run_id=?`, run.RunID).Scan(&runStatus, &forceFence, &runTerminal, &attemptStatus, &attemptTerminal); err != nil {
				t.Fatal(err)
			}
			if runStatus != "cancelled" || attemptStatus != "cancelled" || forceFence != after ||
				runTerminal == "" || attemptTerminal != runTerminal {
				t.Fatalf("force release state = run %s fence %d terminal %q, attempt %s terminal %q",
					runStatus, forceFence, runTerminal, attemptStatus, attemptTerminal)
			}
			task, err := db.GetTask("main", id)
			if err != nil || task.Claim != nil {
				t.Fatalf("claim after force release = %+v, %v", task, err)
			}
			var runReceipts int
			if err := db.db.QueryRow(`SELECT COUNT(*) FROM receipts WHERE kind='run'`).Scan(&runReceipts); err != nil || runReceipts != 1 {
				t.Fatalf("force release run receipts = %d, %v; want start receipt only", runReceipts, err)
			}
		})
	}
}

func TestForceReleaseFenceFailureRollsBackCancellation(t *testing.T) {
	db, id, claim, run := startRunFixture(t, 300)
	if _, err := db.db.Exec(`UPDATE meta SET value='9223372036854775807' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	if err := db.ReleaseClaim("main", id, "operator", 0, true); err == nil {
		t.Fatal("force release with exhausted fence succeeded")
	}
	var runStatus, attemptStatus string
	if err := db.db.QueryRow(`SELECT r.status,a.status FROM runs r
		JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
		WHERE r.run_id=?`, run.RunID).Scan(&runStatus, &attemptStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" || attemptStatus != "running" {
		t.Fatalf("failed force release persisted cancellation: run=%s attempt=%s", runStatus, attemptStatus)
	}
	task, err := db.GetTask("main", id)
	if err != nil || task.Claim == nil || task.Claim.Token != claim.Token {
		t.Fatalf("failed force release cleared claim: %+v, %v", task, err)
	}
}
