package store

import (
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestRunCancelInputDigestVectors(t *testing.T) {
	tests := []struct {
		r    runRequest
		want string
	}{
		{runRequest{operation: "cancel", requestID: "req.cancel", owner: "agent-a", runID: 1, attempt: 2},
			"sha256:8e69b9f9c7c16b7ae72aee6f2457f9699f84bd5cf41deada5e73872d4fd92550"},
		{runRequest{operation: "cancel", requestID: "req.cancel.force", runID: 1, force: true},
			"sha256:367b0a09ccaa54544f7084c01bb704d282b8d6652a75eb654914439fe08198aa"},
	}
	for _, tt := range tests {
		got, err := runInputDigest(tt.r)
		if err != nil || got != tt.want {
			t.Fatalf("cancel digest = %q, %v; want %q", got, err, tt.want)
		}
	}
}

func TestRunCancelOrdinaryRunningEffectReceiptAndReplay(t *testing.T) {
	db, id, claim, run := startRunFixture(t, 300)
	result, replayed, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, false, "cancel.running")
	if err != nil || replayed {
		t.Fatalf("cancel = %+v replayed=%v err=%v", result, replayed, err)
	}
	if result.RunStatus != "cancelled" || result.Attempt != 1 || result.AttemptStatus != "cancelled" || result.Forced {
		t.Fatalf("cancel result = %+v", result)
	}
	var runStatus, attemptStatus, runUpdated, runTerminal, attemptUpdated, attemptTerminal string
	var runDigest, attemptDigest sql.NullString
	var forceFence sql.NullInt64
	if err := db.db.QueryRow(`SELECT r.status,a.status,r.updated_at,r.terminal_at,a.updated_at,a.terminal_at,
		r.terminal_result_digest,a.terminal_result_digest,r.force_cancel_fence FROM runs r JOIN attempts a
		ON a.run_id=r.run_id AND a.attempt_no=1 WHERE r.run_id=?`, run.RunID).Scan(&runStatus, &attemptStatus,
		&runUpdated, &runTerminal, &attemptUpdated, &attemptTerminal, &runDigest, &attemptDigest, &forceFence); err != nil {
		t.Fatal(err)
	}
	if runStatus != "cancelled" || attemptStatus != "cancelled" || runUpdated != runTerminal ||
		runUpdated != attemptUpdated || runUpdated != attemptTerminal || runDigest.Valid || attemptDigest.Valid || forceFence.Valid {
		t.Fatalf("stored cancellation mismatch")
	}
	var claimOwner string
	if err := db.db.QueryRow(`SELECT claim_owner FROM tasks WHERE tag='main' AND id=?`, id).Scan(&claimOwner); err != nil || claimOwner != "agent-a" {
		t.Fatalf("claim after cancel = %q, %v", claimOwner, err)
	}
	receipt, err := db.FindReceipt(ReceiptKindRun, "cancel.running")
	if err != nil || receipt == nil || receipt["operation"] != "cancel" || receipt["force"] != false ||
		receipt["claimAcquisitionId"] == "" || receipt["attemptAcquisitionId"] == "" {
		t.Fatalf("ordinary cancel receipt = %v, %v", receipt, err)
	}
	body, _ := canonicalJSON(receipt)
	if strings.Contains(string(body), "attemptToken") || strings.Contains(string(body), "claimToken") {
		t.Fatalf("cancel receipt leaked capability: %s", body)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "run" || last.Action != "cancelled" || last.Task != id || last.Owner != "agent-a" {
		t.Fatalf("cancel event = %+v", last)
	}
	again, replayed, err := db.CancelRun(run.RunID, "agent-a", claim.Token+99, 1, run.AttemptToken+99, false, "cancel.running")
	if err != nil || !replayed || !reflect.DeepEqual(again, result) {
		t.Fatalf("cancel replay = %+v replayed=%v err=%v; want %+v", again, replayed, err, result)
	}
	if _, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, false, "cancel.running.changed"); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("fresh terminal cancel = %v", err)
	}
}

func retryableCancelFixture(t *testing.T, status string) (*DB, string, *Claim, *RunStartResult) {
	t.Helper()
	db, id, claim, run := startRunFixture(t, 300)
	at := "2026-08-28T18:00:00Z"
	digest := any(nil)
	if status == "failed" {
		digest = testResultDigest
	}
	if _, err := db.db.Exec(`UPDATE attempts SET status=?,updated_at=?,terminal_at=?,terminal_result_digest=?
		WHERE run_id=? AND attempt_no=1`, status, at, at, digest, run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE runs SET status='retryable',updated_at=? WHERE run_id=?`, at, run.RunID); err != nil {
		t.Fatal(err)
	}
	return db, id, claim, run
}

func TestRunCancelOrdinaryPreservesFailedAndLostEvidence(t *testing.T) {
	for _, status := range []string{"failed", "lost"} {
		t.Run(status, func(t *testing.T) {
			db, _, claim, run := retryableCancelFixture(t, status)
			var beforeUpdated, beforeTerminal string
			var beforeDigest sql.NullString
			if err := db.db.QueryRow(`SELECT updated_at,terminal_at,terminal_result_digest FROM attempts
				WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&beforeUpdated, &beforeTerminal, &beforeDigest); err != nil {
				t.Fatal(err)
			}
			result, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, false, "cancel."+status)
			if err != nil || result.AttemptStatus != status {
				t.Fatalf("cancel %s = %+v, %v", status, result, err)
			}
			var afterStatus, afterUpdated, afterTerminal string
			var afterDigest sql.NullString
			if err := db.db.QueryRow(`SELECT status,updated_at,terminal_at,terminal_result_digest FROM attempts
				WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&afterStatus, &afterUpdated, &afterTerminal, &afterDigest); err != nil {
				t.Fatal(err)
			}
			if afterStatus != status || beforeUpdated != afterUpdated || beforeTerminal != afterTerminal || beforeDigest != afterDigest {
				t.Fatalf("%s evidence changed", status)
			}
		})
	}
}

func TestRunCancelReplayVerifiesPreservedAttemptEvidence(t *testing.T) {
	t.Run("failed binds prior finish receipt", func(t *testing.T) {
		db, _, claim, run := startRunFixture(t, 300)
		if _, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
			"failed", "finish.before.cancel", "", 0, testResultDigest); err != nil {
			t.Fatal(err)
		}
		result, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
			false, "cancel.failed.verified")
		if err != nil {
			t.Fatal(err)
		}
		again, replayed, err := db.CancelRun(run.RunID, "agent-a", 999, 1, 999,
			false, "cancel.failed.verified")
		if err != nil || !replayed || !reflect.DeepEqual(again, result) {
			t.Fatalf("failed cancel replay = %+v replayed=%v err=%v", again, replayed, err)
		}
		if _, err := db.db.Exec(`UPDATE attempts SET updated_at='2001-01-01T00:00:00Z',terminal_at='2001-01-01T00:00:00Z'
			WHERE run_id=? AND attempt_no=1`, run.RunID); err != nil {
			t.Fatal(err)
		}
		_, _, err = db.CancelRun(run.RunID, "agent-a", 999, 1, 999, false, "cancel.failed.verified")
		if !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("mutated failed evidence replay = %v", err)
		}
	})
	t.Run("lost enforces terminal invariants", func(t *testing.T) {
		db, _, claim, run := retryableCancelFixture(t, "lost")
		result, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
			false, "cancel.lost.verified")
		if err != nil {
			t.Fatal(err)
		}
		again, replayed, err := db.CancelRun(run.RunID, "agent-a", 999, 1, 999,
			false, "cancel.lost.verified")
		if err != nil || !replayed || !reflect.DeepEqual(again, result) {
			t.Fatalf("lost cancel replay = %+v replayed=%v err=%v", again, replayed, err)
		}
		if _, err := db.db.Exec(`UPDATE attempts SET updated_at='2001-01-01T00:00:00Z'
			WHERE run_id=? AND attempt_no=1`, run.RunID); err != nil {
			t.Fatal(err)
		}
		_, _, err = db.CancelRun(run.RunID, "agent-a", 999, 1, 999, false, "cancel.lost.verified")
		if !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("mutated lost evidence replay = %v", err)
		}
	})
}

func TestRunCancelForceFencesWithoutLeakingLineage(t *testing.T) {
	db, id, _, run := startRunFixture(t, 300)
	if _, err := db.db.Exec(`UPDATE attempts SET expires_at='2000-01-01T00:00:00Z' WHERE run_id=? AND attempt_no=1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	beforeFence, _ := db.Fencing()
	result, replayed, err := db.CancelRun(run.RunID, "", 0, 0, 0, true, "cancel.force")
	if err != nil || replayed || !result.Forced || result.Attempt != 1 || result.AttemptStatus != "cancelled" {
		t.Fatalf("force cancel = %+v replayed=%v err=%v", result, replayed, err)
	}
	afterFence, _ := db.Fencing()
	if afterFence != beforeFence+1 {
		t.Fatalf("force fence = %d -> %d", beforeFence, afterFence)
	}
	var storedFence int64
	if err := db.db.QueryRow(`SELECT force_cancel_fence FROM runs WHERE run_id=?`, run.RunID).Scan(&storedFence); err != nil || storedFence != afterFence {
		t.Fatalf("stored force fence = %d, %v", storedFence, err)
	}
	receipt, err := db.FindReceipt(ReceiptKindRun, "cancel.force")
	if err != nil || receipt == nil {
		t.Fatalf("force receipt = %v, %v", receipt, err)
	}
	generation, generationOK := receiptInt64(receipt["leaseGeneration"])
	if receipt["force"] != true || receipt["owner"] != "" ||
		receipt["claimAcquisitionId"] != "" || receipt["attemptAcquisitionId"] != "" ||
		receipt["claimExpiresAt"] != "" || receipt["attemptExpiresAt"] != "" || !generationOK || generation != 0 {
		t.Fatalf("force receipt = %v, %v", receipt, err)
	}
	feed, _ := db.Feed(0)
	last := feed.Events[len(feed.Events)-1]
	if last.Owner != "" || last.Task != id {
		t.Fatalf("force event leaked owner: %+v", last)
	}
	again, replayed, err := db.CancelRun(run.RunID, "", 0, 0, 0, true, "cancel.force")
	if err != nil || !replayed || !reflect.DeepEqual(again, result) {
		t.Fatalf("force replay = %+v replayed=%v err=%v", again, replayed, err)
	}
	finalFence, _ := db.Fencing()
	if finalFence != afterFence {
		t.Fatalf("force replay advanced fence %d -> %d", afterFence, finalFence)
	}
}

func TestRunCancelForcePreservesRetryableEvidence(t *testing.T) {
	for _, status := range []string{"failed", "lost"} {
		t.Run(status, func(t *testing.T) {
			db, _, _, run := retryableCancelFixture(t, status)
			var beforeUpdated, beforeTerminal string
			var beforeDigest sql.NullString
			if err := db.db.QueryRow(`SELECT updated_at,terminal_at,terminal_result_digest FROM attempts
				WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&beforeUpdated, &beforeTerminal, &beforeDigest); err != nil {
				t.Fatal(err)
			}
			beforeFence, _ := db.Fencing()
			result, _, err := db.CancelRun(run.RunID, "", 0, 0, 0, true, "cancel.force."+status)
			if err != nil || result.AttemptStatus != status || !result.Forced {
				t.Fatalf("force %s = %+v, %v", status, result, err)
			}
			afterFence, _ := db.Fencing()
			if afterFence != beforeFence+1 {
				t.Fatalf("force %s fence = %d -> %d", status, beforeFence, afterFence)
			}
			var afterStatus, afterUpdated, afterTerminal string
			var afterDigest sql.NullString
			if err := db.db.QueryRow(`SELECT status,updated_at,terminal_at,terminal_result_digest FROM attempts
				WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&afterStatus, &afterUpdated, &afterTerminal, &afterDigest); err != nil {
				t.Fatal(err)
			}
			if afterStatus != status || beforeUpdated != afterUpdated || beforeTerminal != afterTerminal || beforeDigest != afterDigest {
				t.Fatalf("force %s changed attempt evidence", status)
			}
		})
	}
}

func TestRunCancelValidationAndRollback(t *testing.T) {
	t.Run("local force combination", func(t *testing.T) {
		db, _, _, run := startRunFixture(t, 300)
		if _, _, err := db.CancelRun(run.RunID, "agent-a", 1, 1, 1, true, "cancel.bad"); err == nil {
			t.Fatal("forced cancel accepted capabilities")
		}
	})
	t.Run("force conflict reveals only supplied run", func(t *testing.T) {
		db, _, _, run := startRunFixture(t, 300)
		if _, _, err := db.CancelRun(run.RunID, "", 0, 0, 0, true, "cancel.first"); err != nil {
			t.Fatal(err)
		}
		_, _, err := db.CancelRun(run.RunID, "", 0, 0, 0, true, "cancel.second")
		var conflict *RunConflict
		if !errors.As(err, &conflict) || strings.Contains(err.Error(), "attempt") ||
			!strings.Contains(err.Error(), fmt.Sprintf("run %d", run.RunID)) {
			t.Fatalf("force conflict = %v", err)
		}
	})
	t.Run("expired ordinary running attempt", func(t *testing.T) {
		db, _, claim, run := expiredRunAttempt(t)
		if _, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, false, "cancel.expired"); !errors.As(err, new(*RunConflict)) {
			t.Fatalf("expired cancel = %v", err)
		}
	})
	t.Run("fault rollback including fence", func(t *testing.T) {
		db, _, _, run := startRunFixture(t, 300)
		beforeFence, _ := db.Fencing()
		oldHook := runFaultHook
		runFaultHook = func(point string) error {
			if point == "before-commit" {
				return fmt.Errorf("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { runFaultHook = oldHook })
		if _, _, err := db.CancelRun(run.RunID, "", 0, 0, 0, true, "cancel.rollback"); err == nil {
			t.Fatal("faulted cancel succeeded")
		}
		var runStatus, attemptStatus string
		var forceFence sql.NullInt64
		if err := db.db.QueryRow(`SELECT status,force_cancel_fence FROM runs WHERE run_id=?`, run.RunID).Scan(&runStatus, &forceFence); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT status FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&attemptStatus); err != nil {
			t.Fatal(err)
		}
		afterFence, _ := db.Fencing()
		if runStatus != "running" || attemptStatus != "running" || forceFence.Valid || afterFence != beforeFence {
			t.Fatalf("rollback = run %s attempt %s force=%v fence=%d->%d", runStatus, attemptStatus, forceFence, beforeFence, afterFence)
		}
	})
}

func TestConcurrentIdenticalRunCancelOneEffectAndReplay(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 300)
	type outcome struct {
		result   *RunCancelResult
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
			result, replayed, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, false, "cancel.concurrent")
			out <- outcome{result, replayed, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var fresh, replay int
	var first *RunCancelResult
	for got := range out {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first == nil {
			first = got.result
		} else if !reflect.DeepEqual(first, got.result) {
			t.Fatalf("cancel results differ: %+v / %+v", first, got.result)
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

func TestConcurrentDistinctRunCancelOneWinner(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 300)
	start := make(chan struct{})
	errCh := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			_, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
				false, fmt.Sprintf("cancel.distinct.%d", n))
			errCh <- err
		}(i)
	}
	close(start)
	wg.Wait()
	close(errCh)
	var success, conflict int
	for err := range errCh {
		if err == nil {
			success++
		} else if errors.As(err, new(*RunConflict)) {
			conflict++
		} else {
			t.Fatalf("unexpected cancel error: %v", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}
