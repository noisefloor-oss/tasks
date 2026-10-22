package store

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunRenewInputDigestVector(t *testing.T) {
	got, err := runInputDigest(runRequest{operation: "renew", requestID: "req.renew", owner: "agent-a", runID: 1, attempt: 2, ttl: 300})
	want := "sha256:8077bf4524f111a24b25623c71197e3c69c77114c9644a04aab1cb03dd6d54ec"
	if err != nil || got != want {
		t.Fatalf("renew digest = %q, %v; want %q", got, err, want)
	}
}

func TestRunRenewRotatesBothLeasesAndReplays(t *testing.T) {
	db, id, claim, run := startRunFixture(t, 900)
	var claimAcquired, claimExtra, attemptID, attemptStarted, createdRequest string
	if err := db.db.QueryRow(`SELECT claim_acquired_at,COALESCE(claim_extra,'') FROM tasks WHERE tag='main' AND id=?`, id).
		Scan(&claimAcquired, &claimExtra); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT acquisition_id,started_at,created_request_id FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).
		Scan(&attemptID, &attemptStarted, &createdRequest); err != nil {
		t.Fatal(err)
	}
	beforeFence, _ := db.Fencing()
	result, replayed, err := db.RenewRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "run.renew", 600)
	if err != nil || replayed {
		t.Fatalf("renew = %+v replayed=%v err=%v", result, replayed, err)
	}
	if result.LeaseGeneration != 2 || result.ClaimToken != beforeFence+1 || result.AttemptToken != beforeFence+2 ||
		result.ClaimExpiresAt != result.AttemptExpiresAt {
		t.Fatalf("renew result = %+v after fence %d", result, beforeFence)
	}
	var storedClaimToken, storedAttemptToken, generation int64
	var storedClaimAcquired, storedClaimExtra, storedAttemptID, storedAttemptStarted, storedRequest string
	var claimExpires, attemptExpires, runUpdated, attemptUpdated string
	if err := db.db.QueryRow(`SELECT claim_token,claim_acquired_at,claim_expires_at,COALESCE(claim_extra,'') FROM tasks WHERE tag='main' AND id=?`, id).
		Scan(&storedClaimToken, &storedClaimAcquired, &claimExpires, &storedClaimExtra); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT attempt_token,lease_generation,acquisition_id,started_at,created_request_id,expires_at,updated_at
		FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).
		Scan(&storedAttemptToken, &generation, &storedAttemptID, &storedAttemptStarted, &storedRequest, &attemptExpires, &attemptUpdated); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT updated_at FROM runs WHERE run_id=?`, run.RunID).Scan(&runUpdated); err != nil {
		t.Fatal(err)
	}
	if storedClaimToken != result.ClaimToken || storedAttemptToken != result.AttemptToken || generation != 2 ||
		claimExpires != attemptExpires || attemptUpdated != runUpdated || storedClaimAcquired != claimAcquired ||
		storedClaimExtra != claimExtra || storedAttemptID != attemptID || storedAttemptStarted != attemptStarted || storedRequest != createdRequest {
		t.Fatalf("renew persistence/preservation mismatch")
	}
	receipt, err := db.FindReceipt(ReceiptKindRun, "run.renew")
	if err != nil || receipt == nil || receipt["operation"] != "renew" {
		t.Fatalf("renew receipt = %v, %v", receipt, err)
	}
	body, _ := canonicalJSON(receipt)
	if strings.Contains(string(body), "attemptToken") || strings.Contains(string(body), "claimToken") {
		t.Fatalf("renew receipt leaked capability: %s", body)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "attempt" || last.Action != "renewed" {
		t.Fatalf("renew event = %+v", last)
	}
	fence, _ := db.Fencing()
	again, replayed, err := db.RenewRun(run.RunID, "agent-a", claim.Token+99, 1, run.AttemptToken+99, "run.renew", 600)
	if err != nil || !replayed || *again != *result {
		t.Fatalf("renew replay = %+v replayed=%v err=%v; want %+v", again, replayed, err, result)
	}
	afterFence, _ := db.Fencing()
	if afterFence != fence {
		t.Fatalf("renew replay advanced fence %d -> %d", fence, afterFence)
	}
	if _, _, err := db.RenewRun(run.RunID, "agent-a", result.ClaimToken, 1, result.AttemptToken, "run.renew", 601); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("divergent renew replay = %v", err)
	}
}

func TestRunRenewMayShortenAndSupersedesReplay(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 900)
	first, _, err := db.RenewRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "renew.long", 600)
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := db.RenewRun(run.RunID, "agent-a", first.ClaimToken, 1, first.AttemptToken, "renew.short", 60)
	if err != nil {
		t.Fatal(err)
	}
	longExpiry, _ := time.Parse(time.RFC3339, first.AttemptExpiresAt)
	shortExpiry, _ := time.Parse(time.RFC3339, second.AttemptExpiresAt)
	if !shortExpiry.Before(longExpiry) || second.LeaseGeneration != 3 {
		t.Fatalf("short renew = %+v after %+v", second, first)
	}
	if _, _, err := db.RenewRun(run.RunID, "agent-a", first.ClaimToken, 1, first.AttemptToken, "renew.long", 600); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("superseded replay = %v", err)
	}
}

func TestRunRenewRefusalsAndAtomicRollback(t *testing.T) {
	t.Run("expired attempt", func(t *testing.T) {
		db, _, claim, run := expiredRunAttempt(t)
		if _, _, err := db.RenewRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "renew.expired", 300); !errors.As(err, new(*RunConflict)) {
			t.Fatalf("expired renew = %v", err)
		}
	})
	t.Run("second fence exhaustion", func(t *testing.T) {
		db, id, claim, run := startRunFixture(t, 900)
		if _, err := db.db.Exec(`UPDATE meta SET value='9223372036854775806' WHERE key='fencing'`); err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.RenewRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "renew.exhausted", 300); err == nil {
			t.Fatal("renew with exhausted second fence succeeded")
		}
		var fence string
		var claimToken, attemptToken, generation int64
		if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='fencing'`).Scan(&fence); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT claim_token FROM tasks WHERE tag='main' AND id=?`, id).Scan(&claimToken); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT attempt_token,lease_generation FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).
			Scan(&attemptToken, &generation); err != nil {
			t.Fatal(err)
		}
		if fence != "9223372036854775806" || claimToken != claim.Token || attemptToken != run.AttemptToken || generation != 1 {
			t.Fatalf("failed renew persisted: fence=%s claim=%d attempt=%d generation=%d", fence, claimToken, attemptToken, generation)
		}
	})
	t.Run("fault rollback", func(t *testing.T) {
		db, id, claim, run := startRunFixture(t, 900)
		oldHook := runFaultHook
		runFaultHook = func(point string) error {
			if point == "before-commit" {
				return fmt.Errorf("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { runFaultHook = oldHook })
		if _, _, err := db.RenewRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "renew.rollback", 300); err == nil {
			t.Fatal("faulted renew succeeded")
		}
		var claimToken, attemptToken, generation int64
		if err := db.db.QueryRow(`SELECT claim_token FROM tasks WHERE tag='main' AND id=?`, id).Scan(&claimToken); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT attempt_token,lease_generation FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).
			Scan(&attemptToken, &generation); err != nil {
			t.Fatal(err)
		}
		if claimToken != claim.Token || attemptToken != run.AttemptToken || generation != 1 {
			t.Fatalf("faulted renew persisted claim=%d attempt=%d generation=%d", claimToken, attemptToken, generation)
		}
	})
}

func TestConcurrentIdenticalRunRenewOneEffectAndReplay(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 900)
	type outcome struct {
		result   *RunRenewResult
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
			result, replayed, err := db.RenewRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "renew.concurrent", 300)
			out <- outcome{result, replayed, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var fresh, replay int
	var first *RunRenewResult
	for got := range out {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first == nil {
			first = got.result
		} else if *first != *got.result {
			t.Fatalf("renew results differ: %+v / %+v", first, got.result)
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
