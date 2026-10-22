package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestRunRetryInputDigestVector(t *testing.T) {
	got, err := runInputDigest(runRequest{operation: "retry", requestID: "req.retry", owner: "agent-a", runID: 1, attempt: 1, ttl: 900})
	want := "sha256:7f7762b22dbecee5f1578355a6a0073837c1b047d2584e9a60b92801a3afdeaa"
	if err != nil || got != want {
		t.Fatalf("retry digest = %q, %v; want %q", got, err, want)
	}
}

func expiredRunAttempt(t *testing.T) (*DB, string, *Claim, *RunStartResult) {
	t.Helper()
	db, id, claim, run := startRunFixture(t, 300)
	if _, err := db.db.Exec(`UPDATE attempts SET expires_at='2000-01-01T00:00:00Z' WHERE run_id=? AND attempt_no=1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	return db, id, claim, run
}

func terminalRetrySource(t *testing.T, status string) (*DB, string, *Claim, *RunStartResult, string) {
	t.Helper()
	db, id, claim, run := startRunFixture(t, 300)
	at := "2026-08-28T18:00:00Z"
	digest := any(nil)
	if status == "failed" {
		digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	}
	if _, err := db.db.Exec(`UPDATE attempts SET status=?,updated_at=?,terminal_at=?,terminal_result_digest=?
		WHERE run_id=? AND attempt_no=1`, status, at, at, digest, run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE runs SET status='retryable',updated_at=? WHERE run_id=?`, at, run.RunID); err != nil {
		t.Fatal(err)
	}
	return db, id, claim, run, at
}

func TestRunRetryExpiredAttemptEffectReceiptAndReplay(t *testing.T) {
	db, id, claim, run := expiredRunAttempt(t)
	beforeFence, _ := db.Fencing()
	result, replayed, err := db.RetryRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "run.retry", 600)
	if err != nil || replayed {
		t.Fatalf("retry = %+v replayed=%v err=%v", result, replayed, err)
	}
	if result.Attempt != 2 || result.LeaseGeneration != 1 || result.AttemptToken != beforeFence+1 {
		t.Fatalf("retry result = %+v after fence %d", result, beforeFence)
	}
	var predecessorStatus, predecessorUpdated, predecessorTerminal string
	if err := db.db.QueryRow(`SELECT status,updated_at,terminal_at FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).
		Scan(&predecessorStatus, &predecessorUpdated, &predecessorTerminal); err != nil {
		t.Fatal(err)
	}
	if predecessorStatus != "lost" || predecessorUpdated == "" || predecessorTerminal != predecessorUpdated {
		t.Fatalf("predecessor = %s updated=%q terminal=%q", predecessorStatus, predecessorUpdated, predecessorTerminal)
	}
	var runStatus string
	var current int64
	if err := db.db.QueryRow(`SELECT status,current_attempt FROM runs WHERE run_id=?`, run.RunID).Scan(&runStatus, &current); err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" || current != 2 {
		t.Fatalf("run = %s attempt %d", runStatus, current)
	}
	receipt, err := db.FindReceipt(ReceiptKindRun, "run.retry")
	if err != nil || receipt == nil || receipt["operation"] != "retry" || receipt["tag"] != "main" || receipt["task"] != id {
		t.Fatalf("retry receipt = %v, %v", receipt, err)
	}
	body, _ := canonicalJSON(receipt)
	if strings.Contains(string(body), "attemptToken") || strings.Contains(string(body), "claimToken") {
		t.Fatalf("retry receipt leaked capability: %s", body)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "attempt" || last.Action != "retried" || last.Task != id {
		t.Fatalf("retry event = %+v", last)
	}
	fence, _ := db.Fencing()
	again, replayed, err := db.RetryRun(run.RunID, "agent-a", claim.Token+99, 1, run.AttemptToken+99, "run.retry", 600)
	if err != nil || !replayed || *again != *result {
		t.Fatalf("retry replay = %+v replayed=%v err=%v; want %+v", again, replayed, err, result)
	}
	after, _ := db.Fencing()
	if after != fence {
		t.Fatalf("retry replay advanced fence %d -> %d", fence, after)
	}
	if _, _, err := db.RetryRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "run.retry", 601); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("divergent retry replay = %v", err)
	}
}

func TestRunRetryPreservesTerminalPredecessor(t *testing.T) {
	for _, status := range []string{"failed", "lost"} {
		t.Run(status, func(t *testing.T) {
			db, _, claim, run, _ := terminalRetrySource(t, status)
			var beforeStatus, beforeUpdated, beforeTerminal string
			var beforeDigest any
			if err := db.db.QueryRow(`SELECT status,updated_at,terminal_at,terminal_result_digest FROM attempts
				WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&beforeStatus, &beforeUpdated, &beforeTerminal, &beforeDigest); err != nil {
				t.Fatal(err)
			}
			if _, _, err := db.RetryRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "retry."+status, 300); err != nil {
				t.Fatal(err)
			}
			var afterStatus, afterUpdated, afterTerminal string
			var afterDigest any
			if err := db.db.QueryRow(`SELECT status,updated_at,terminal_at,terminal_result_digest FROM attempts
				WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&afterStatus, &afterUpdated, &afterTerminal, &afterDigest); err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(beforeStatus, beforeUpdated, beforeTerminal, beforeDigest) != fmt.Sprint(afterStatus, afterUpdated, afterTerminal, afterDigest) {
				t.Fatalf("predecessor changed: %v/%v/%v/%v -> %v/%v/%v/%v",
					beforeStatus, beforeUpdated, beforeTerminal, beforeDigest, afterStatus, afterUpdated, afterTerminal, afterDigest)
			}
		})
	}
}

func TestRunRetryRefusalAndRollback(t *testing.T) {
	t.Run("live attempt", func(t *testing.T) {
		db, _, claim, run := startRunFixture(t, 300)
		if _, _, err := db.RetryRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "retry.live", 300); !errors.As(err, new(*RunConflict)) {
			t.Fatalf("live retry = %v", err)
		}
	})
	t.Run("rollback", func(t *testing.T) {
		db, _, claim, run := expiredRunAttempt(t)
		beforeFence, _ := db.Fencing()
		oldHook := runFaultHook
		runFaultHook = func(point string) error {
			if point == "before-commit" {
				return fmt.Errorf("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { runFaultHook = oldHook })
		if _, _, err := db.RetryRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "retry.rollback", 300); err == nil {
			t.Fatal("faulted retry succeeded")
		}
		var count int
		var runStatus, attemptStatus string
		var current int64
		if err := db.db.QueryRow(`SELECT status,current_attempt FROM runs WHERE run_id=?`, run.RunID).Scan(&runStatus, &current); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT COUNT(*),MIN(status) FROM attempts WHERE run_id=?`, run.RunID).Scan(&count, &attemptStatus); err != nil {
			t.Fatal(err)
		}
		afterFence, _ := db.Fencing()
		if runStatus != "running" || current != 1 || count != 1 || attemptStatus != "running" || afterFence != beforeFence {
			t.Fatalf("rollback state run=%s/%d attempts=%d/%s fence=%d->%d", runStatus, current, count, attemptStatus, beforeFence, afterFence)
		}
	})
}

func TestConcurrentIdenticalRunRetryOneEffectAndReplay(t *testing.T) {
	db, _, claim, run := expiredRunAttempt(t)
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
			result, replayed, err := db.RetryRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "retry.concurrent", 300)
			out <- outcome{result, replayed, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var fresh, replay int
	var first *RunStartResult
	for got := range out {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first == nil {
			first = got.result
		} else if *first != *got.result {
			t.Fatalf("retry results differ: %+v / %+v", first, got.result)
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
