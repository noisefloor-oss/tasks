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

const testResultDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestRunFinishInputDigestVectors(t *testing.T) {
	tests := []struct {
		r    runRequest
		want string
	}{
		{runRequest{operation: "finish", requestID: "req.finish", owner: "agent-a", runID: 1,
			attempt: 2, outcome: "succeeded", revision: 3, taskStatus: "done", terminalResultDigest: testResultDigest},
			"sha256:21d37a732659cdc08323b6e541a9d647ea230e0068390492488193cecc041a2b"},
		{runRequest{operation: "finish", requestID: "req.fail", owner: "agent-a", runID: 1,
			attempt: 2, outcome: "failed"},
			"sha256:fddfb13fd392b56b65b3e9de32cdaf986a5cb4a1e074bf6c82a5df85b4246d49"},
	}
	for _, tt := range tests {
		got, err := runInputDigest(tt.r)
		if err != nil || got != tt.want {
			t.Fatalf("finish digest = %q, %v; want %q", got, err, tt.want)
		}
	}
}

func TestRunFinishSucceededPublishesTaskAndReplays(t *testing.T) {
	db, id, claim, run := startRunFixture(t, 300)
	result, replayed, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.success", "done", 1, testResultDigest)
	if err != nil || replayed {
		t.Fatalf("finish = %+v replayed=%v err=%v", result, replayed, err)
	}
	if result.RunStatus != "succeeded" || result.AttemptStatus != "succeeded" ||
		result.Task == nil || result.Task.ID != id || result.Task.Status != "done" || result.Task.Revision != 2 ||
		result.TerminalResultDigest == nil || *result.TerminalResultDigest != testResultDigest {
		t.Fatalf("finish result = %+v", result)
	}
	var runStatus, attemptStatus, runAt, attemptAt, runDigest, attemptDigest string
	if err := db.db.QueryRow(`SELECT r.status,a.status,r.terminal_at,a.terminal_at,
		r.terminal_result_digest,a.terminal_result_digest FROM runs r JOIN attempts a
		ON a.run_id=r.run_id AND a.attempt_no=1 WHERE r.run_id=?`, run.RunID).
		Scan(&runStatus, &attemptStatus, &runAt, &attemptAt, &runDigest, &attemptDigest); err != nil {
		t.Fatal(err)
	}
	if runStatus != "succeeded" || attemptStatus != "succeeded" || runAt != attemptAt ||
		runDigest != testResultDigest || attemptDigest != testResultDigest {
		t.Fatalf("stored finish = %s/%s at %s/%s digest %s/%s", runStatus, attemptStatus, runAt, attemptAt, runDigest, attemptDigest)
	}
	var claimOwner sql.NullString
	var taskStatus string
	var revision int
	if err := db.db.QueryRow(`SELECT status,revision,claim_owner FROM tasks WHERE tag='main' AND id=?`, id).
		Scan(&taskStatus, &revision, &claimOwner); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "done" || revision != 2 || claimOwner.Valid {
		t.Fatalf("task after finish = %s r%d claim=%v", taskStatus, revision, claimOwner)
	}
	receipt, err := db.FindReceipt(ReceiptKindRun, "finish.success")
	if err != nil || receipt == nil || receipt["operation"] != "finish" || receipt["taskStatus"] != "done" ||
		receipt["resultTaskStatus"] != "done" || receipt["claimAcquisitionId"] == "" {
		t.Fatalf("finish receipt = %v, %v", receipt, err)
	}
	body, _ := canonicalJSON(receipt)
	if strings.Contains(string(body), "attemptToken") || strings.Contains(string(body), "claimToken") {
		t.Fatalf("finish receipt leaked capability: %s", body)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "run" || last.Action != "finished" || last.Task != id {
		t.Fatalf("finish event = %+v", last)
	}
	for _, event := range feed.Events {
		if event.Kind == "status" || event.Kind == "release" {
			t.Fatalf("finish emitted extra task event: %+v", event)
		}
	}
	again, replayed, err := db.FinishRun(run.RunID, "agent-a", claim.Token+99, 1, run.AttemptToken+99,
		"succeeded", "finish.success", "done", 1, testResultDigest)
	if err != nil || !replayed || !reflect.DeepEqual(again, result) {
		t.Fatalf("finish replay = %+v replayed=%v err=%v; want %+v", again, replayed, err, result)
	}
	if _, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.success", "done", 1, ""); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("divergent finish replay = %v", err)
	}
}

func TestRunFinishFailedRemainsRetryableAndReplaySurvivesRetry(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 300)
	result, replayed, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
		"failed", "finish.failed", "", 0, testResultDigest)
	if err != nil || replayed || result.RunStatus != "retryable" || result.AttemptStatus != "failed" ||
		result.Task != nil || result.TerminalResultDigest == nil {
		t.Fatalf("failed finish = %+v replayed=%v err=%v", result, replayed, err)
	}
	var runDigest sql.NullString
	var runTerminal sql.NullString
	if err := db.db.QueryRow(`SELECT terminal_at,terminal_result_digest FROM runs WHERE run_id=?`, run.RunID).
		Scan(&runTerminal, &runDigest); err != nil {
		t.Fatal(err)
	}
	if runTerminal.Valid || runDigest.Valid {
		t.Fatalf("retryable run has terminal data: at=%v digest=%v", runTerminal, runDigest)
	}
	next, _, err := db.RetryRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken, "retry.after.finish", 300)
	if err != nil || next.Attempt != 2 {
		t.Fatalf("retry after failed finish = %+v, %v", next, err)
	}
	again, replayed, err := db.FinishRun(run.RunID, "agent-a", 999, 1, 999,
		"failed", "finish.failed", "", 0, testResultDigest)
	if err != nil || !replayed || !reflect.DeepEqual(again, result) {
		t.Fatalf("historical failed replay = %+v replayed=%v err=%v; want %+v", again, replayed, err, result)
	}
}

func TestRunFinishFailedReplaySurvivesSameSecondCancellation(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 300)
	result, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
		"failed", "finish.failed.cancelled", "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var at string
	if err := db.db.QueryRow(`SELECT updated_at FROM runs WHERE run_id=?`, run.RunID).Scan(&at); err != nil {
		t.Fatal(err)
	}
	// Cancellation can legitimately observe the same RFC3339 second. Preserve
	// the failed attempt evidence and shape the later terminal run transition.
	if _, err := db.db.Exec(`UPDATE runs SET status='cancelled',updated_at=?,terminal_at=? WHERE run_id=?`,
		at, at, run.RunID); err != nil {
		t.Fatal(err)
	}
	again, replayed, err := db.FinishRun(run.RunID, "agent-a", 999, 1, 999,
		"failed", "finish.failed.cancelled", "", 0, "")
	if err != nil || !replayed || !reflect.DeepEqual(again, result) {
		t.Fatalf("same-second cancelled replay = %+v replayed=%v err=%v; want %+v", again, replayed, err, result)
	}
}

func TestRunFinishNoOpTaskStatusStillPublishesRevision(t *testing.T) {
	db, id, claim, run := startRunFixture(t, 300)
	result, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.noop", "pending", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Task == nil || result.Task.Revision != 2 || result.Task.Status != "pending" || result.TerminalResultDigest != nil {
		t.Fatalf("no-op task result = %+v", result)
	}
	var revision int
	var claimOwner string
	if err := db.db.QueryRow(`SELECT revision,claim_owner FROM tasks WHERE tag='main' AND id=?`, id).
		Scan(&revision, &claimOwner); err != nil || revision != 2 || claimOwner != "agent-a" {
		t.Fatalf("revision/claim = %d/%q, %v", revision, claimOwner, err)
	}
	var runDigest, attemptDigest sql.NullString
	if err := db.db.QueryRow(`SELECT r.terminal_result_digest,a.terminal_result_digest FROM runs r
		JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=1 WHERE r.run_id=?`, run.RunID).
		Scan(&runDigest, &attemptDigest); err != nil || runDigest.Valid || attemptDigest.Valid {
		t.Fatalf("omitted digests = run %v attempt %v, %v", runDigest, attemptDigest, err)
	}
}

func TestRunFinishSucceededWithoutTaskUpdateRetainsClaim(t *testing.T) {
	db, id, claim, run := startRunFixture(t, 300)
	result, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.no-task", "", 0, "")
	if err != nil || result.Task != nil || result.TerminalResultDigest != nil {
		t.Fatalf("finish without task update = %+v, %v", result, err)
	}
	var claimOwner string
	var revision int
	if err := db.db.QueryRow(`SELECT claim_owner,revision FROM tasks WHERE tag='main' AND id=?`, id).
		Scan(&claimOwner, &revision); err != nil || claimOwner != "agent-a" || revision != 1 {
		t.Fatalf("retained claim/revision = %q/%d, %v", claimOwner, revision, err)
	}
}

func TestRunFinishValidationConflictsAndRollback(t *testing.T) {
	t.Run("local combinations", func(t *testing.T) {
		db, _, claim, run := startRunFixture(t, 300)
		bad := []struct {
			outcome, status, digest string
			revision                int
		}{
			{"unknown", "", "", 0},
			{"failed", "done", "", 1},
			{"succeeded", "done", "", 0},
			{"succeeded", "", "bad", 0},
		}
		for i, tt := range bad {
			if _, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
				tt.outcome, fmt.Sprintf("finish.bad.%d", i), tt.status, tt.revision, tt.digest); err == nil {
				t.Fatalf("invalid finish %d succeeded", i)
			}
		}
	})
	t.Run("expired and malformed leases", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			query string
		}{
			{"attempt expired", `UPDATE attempts SET expires_at='2000-01-01T00:00:00Z' WHERE run_id=? AND attempt_no=1`},
			{"claim expired", `UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id=(SELECT task_id FROM runs WHERE run_id=?)`},
			{"claim malformed", `UPDATE tasks SET claim_expires_at='not-a-time' WHERE tag='main' AND id=(SELECT task_id FROM runs WHERE run_id=?)`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				db, _, claim, run := startRunFixture(t, 300)
				if _, err := db.db.Exec(tc.query, run.RunID); err != nil {
					t.Fatal(err)
				}
				if _, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
					"succeeded", "finish.lease", "", 0, ""); err == nil {
					t.Fatal("finish with invalid lease succeeded")
				}
			})
		}
	})
	t.Run("revision before fence", func(t *testing.T) {
		db, _, claim, run := startRunFixture(t, 300)
		_, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token+1, 1, run.AttemptToken+1,
			"succeeded", "finish.stale", "done", 2, "")
		var conflict *RunConflict
		if !errors.As(err, &conflict) || strings.Contains(err.Error(), "revision") || strings.Contains(err.Error(), "token") {
			t.Fatalf("stale finish = %v", err)
		}
	})
	t.Run("fresh capability conflicts are secret free", func(t *testing.T) {
		variants := []struct {
			name                  string
			owner                 string
			claim, attempt, token int64
		}{
			{"owner", "agent-b", 0, 1, 0},
			{"claim", "agent-a", 1, 1, 0},
			{"attempt number", "agent-a", 0, 2, 0},
			{"attempt token", "agent-a", 0, 1, 1},
		}
		for _, tt := range variants {
			t.Run(tt.name, func(t *testing.T) {
				db, _, claim, run := startRunFixture(t, 300)
				claimToken := claim.Token + tt.claim
				attemptToken := run.AttemptToken + tt.token
				_, _, err := db.FinishRun(run.RunID, tt.owner, claimToken, tt.attempt, attemptToken,
					"succeeded", "finish.capability", "", 0, "")
				var conflict *RunConflict
				if !errors.As(err, &conflict) || strings.Contains(err.Error(), "agent-") ||
					strings.Contains(err.Error(), "token") || strings.Contains(err.Error(), "expiry") {
					t.Fatalf("capability conflict = %v", err)
				}
				var status string
				if err := db.db.QueryRow(`SELECT status FROM runs WHERE run_id=?`, run.RunID).Scan(&status); err != nil || status != "running" {
					t.Fatalf("run after conflict = %s, %v", status, err)
				}
			})
		}
	})
	t.Run("fault rollback", func(t *testing.T) {
		db, id, claim, run := startRunFixture(t, 300)
		oldHook := runFaultHook
		runFaultHook = func(point string) error {
			if point == "before-commit" {
				return fmt.Errorf("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { runFaultHook = oldHook })
		if _, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
			"succeeded", "finish.rollback", "done", 1, testResultDigest); err == nil {
			t.Fatal("faulted finish succeeded")
		}
		var runStatus, attemptStatus, taskStatus, claimOwner string
		var revision int
		if err := db.db.QueryRow(`SELECT status FROM runs WHERE run_id=?`, run.RunID).Scan(&runStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT status FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).Scan(&attemptStatus); err != nil {
			t.Fatal(err)
		}
		if err := db.db.QueryRow(`SELECT status,revision,claim_owner FROM tasks WHERE tag='main' AND id=?`, id).
			Scan(&taskStatus, &revision, &claimOwner); err != nil {
			t.Fatal(err)
		}
		var completed int
		if err := db.db.QueryRow(`SELECT completed_count FROM tags WHERE name='main'`).Scan(&completed); err != nil {
			t.Fatal(err)
		}
		if runStatus != "running" || attemptStatus != "running" || taskStatus != "pending" || revision != 1 ||
			claimOwner != "agent-a" || completed != 0 {
			t.Fatalf("rollback state = run %s attempt %s task %s r%d claim=%s completed=%d",
				runStatus, attemptStatus, taskStatus, revision, claimOwner, completed)
		}
	})
}

func TestConcurrentIdenticalRunFinishOneEffectAndReplay(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 300)
	type outcome struct {
		result   *RunFinishResult
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
			result, replayed, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
				"succeeded", "finish.concurrent", "", 0, "")
			out <- outcome{result, replayed, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var fresh, replay int
	var first *RunFinishResult
	for got := range out {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if first == nil {
			first = got.result
		} else if !reflect.DeepEqual(first, got.result) {
			t.Fatalf("finish results differ: %+v / %+v", first, got.result)
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
