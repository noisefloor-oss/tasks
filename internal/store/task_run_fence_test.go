package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func liveTaskMutationFixture(t *testing.T) (*DB, string, *Claim, *RunStartResult, *ClaimGate) {
	t.Helper()
	db, id, claim, run := startRunFixture(t, 300)
	gate := &ClaimGate{Owner: "agent-a", Token: claim.Token, RunID: run.RunID,
		Attempt: run.Attempt, AttemptToken: run.AttemptToken}
	return db, id, claim, run, gate
}

func updateTitle(t *testing.T, db *DB, id, title string, revision int, gate *ClaimGate) error {
	t.Helper()
	return db.UpdateTask("main", id, revision, gate, func(task *Task) error {
		task.Title = title
		return nil
	})
}

func TestUpdateTaskRequiresExactLiveRunGate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ClaimGate)
	}{
		{"missing run", func(g *ClaimGate) { g.RunID, g.Attempt, g.AttemptToken = 0, 0, 0 }},
		{"wrong run", func(g *ClaimGate) { g.RunID++ }},
		{"wrong attempt", func(g *ClaimGate) { g.Attempt++ }},
		{"wrong attempt token", func(g *ClaimGate) { g.AttemptToken++ }},
		{"wrong owner", func(g *ClaimGate) { g.Owner = "agent-b" }},
		{"wrong claim token", func(g *ClaimGate) { g.Token++ }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, id, _, _, exact := liveTaskMutationFixture(t)
			gate := *exact
			tt.mutate(&gate)
			err := updateTitle(t, db, id, "rejected", 1, &gate)
			var conflict *RunConflict
			if !errors.As(err, &conflict) || strings.Contains(err.Error(), "token") ||
				strings.Contains(err.Error(), "owner") || strings.Contains(err.Error(), "expiry") ||
				strings.Contains(err.Error(), "status") {
				t.Fatalf("run-gated update = %v", err)
			}
			task, _ := db.GetTask("main", id)
			if task.Title == "rejected" || task.Revision != 1 {
				t.Fatalf("rejected update persisted: %+v", task)
			}
		})
	}
	db, id, _, _, exact := liveTaskMutationFixture(t)
	if err := updateTitle(t, db, id, "published", 1, exact); err != nil {
		t.Fatal(err)
	}
	task, _ := db.GetTask("main", id)
	if task.Title != "published" || task.Revision != 2 {
		t.Fatalf("published task = %+v", task)
	}
}

func TestUpdateTaskRejectsMalformedFenceTimestamps(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"claim", `UPDATE tasks SET claim_expires_at='not-a-time' WHERE tag='main' AND id='1'`},
		{"attempt", `UPDATE attempts SET expires_at='not-a-time' WHERE run_id=1 AND attempt_no=1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, id, _, _, exact := liveTaskMutationFixture(t)
			if _, err := db.db.Exec(tt.sql); err != nil {
				t.Fatal(err)
			}
			err := updateTitle(t, db, id, "must-not-persist", 1, exact)
			if err == nil || !strings.Contains(err.Error(), "malformed") {
				t.Fatalf("malformed %s expiry = %v", tt.name, err)
			}
		})
	}
}

func TestUpdateTaskExpiredAttemptCannotFallThroughWithStaleGate(t *testing.T) {
	db, id, claim, run, exact := liveTaskMutationFixture(t)
	if _, err := db.db.Exec(`UPDATE attempts SET expires_at='2000-01-01T00:00:00Z' WHERE run_id=? AND attempt_no=1`, run.RunID); err != nil {
		t.Fatal(err)
	}
	if err := updateTitle(t, db, id, "stale", 1, exact); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("expired attempt update = %v", err)
	}
	claimOnly := &ClaimGate{Owner: "agent-a", Token: claim.Token}
	if err := updateTitle(t, db, id, "orchestrator", 1, claimOnly); err != nil {
		t.Fatalf("claim-only update after attempt expiry = %v", err)
	}
}

func TestUpdateTaskValidationPrecedesRunFence(t *testing.T) {
	db, id, _, _, _ := liveTaskMutationFixture(t)
	err := db.UpdateTask("main", id, 1, nil, func(task *Task) error {
		task.Status = "not-a-status"
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "invalid status") || errors.As(err, new(*RunConflict)) {
		t.Fatalf("invalid proposal precedence = %v", err)
	}
	if err := updateTitle(t, db, id, "stale", 99, nil); !errors.As(err, new(*StaleWriteConflict)) {
		t.Fatalf("stale revision precedence = %v", err)
	}
	want := errors.New("callback refused")
	err = db.UpdateTask("main", id, 1, nil, func(*Task) error { return want })
	if !errors.Is(err, want) || errors.As(err, new(*RunConflict)) {
		t.Fatalf("callback error precedence = %v", err)
	}
}

func TestUpdateTaskCallbackCannotRetargetFencedWrite(t *testing.T) {
	db, _ := newDB(t)
	source := addTask(t, db, "main", "source")
	target := addTask(t, db, "main", "target")
	claim, _, _, err := db.AcquireClaim("main", target, "agent-a", 3600, "claim.retarget")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.StartRun("main", target, "agent-a", claim.Token, "run.retarget", 300)
	if err != nil {
		t.Fatal(err)
	}
	err = db.UpdateTask("main", source, 1, nil, func(task *Task) error {
		task.Tag = "other"
		task.ID = target
		task.Revision = 99
		task.Title = "source-only"
		task.OuterExtra = map[string]json.RawMessage{"injected": json.RawMessage(`true`)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	gotSource, _ := db.GetTask("main", source)
	gotTarget, _ := db.GetTask("main", target)
	if gotSource.Title != "source-only" || gotSource.Revision != 2 || gotSource.Tag != "main" ||
		gotSource.ID != source || gotSource.OuterExtra != nil {
		t.Fatalf("source identity state = %+v", gotSource)
	}
	if gotTarget.Title != "target" || gotTarget.Revision != 1 || gotTarget.Claim == nil || gotTarget.Claim.Token != claim.Token {
		t.Fatalf("retargeted task changed = %+v", gotTarget)
	}
	record, err := db.GetRun(run.RunID)
	if err != nil || record == nil || record.Task != target || record.Status != "running" {
		t.Fatalf("target run = %+v, %v", record, err)
	}
}

func TestUpdateTaskRejectsIncoherentOrSupersededAttemptState(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(*testing.T, *DB, *RunStartResult)
	}{
		{
			name: "incoherent status pair",
			corrupt: func(t *testing.T, db *DB, run *RunStartResult) {
				if _, err := db.db.Exec(`UPDATE attempts SET status='failed',terminal_at=updated_at WHERE run_id=? AND attempt_no=1`, run.RunID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "superseded current attempt",
			corrupt: func(t *testing.T, db *DB, run *RunStartResult) {
				_, err := db.db.Exec(`INSERT INTO attempts(run_id,attempt_no,acquisition_id,attempt_token,status,
					ttl_seconds,lease_generation,created_request_id,started_at,expires_at,updated_at,terminal_at)
					SELECT run_id,2,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',attempt_token+1000,'lost',ttl_seconds,1,
					'corrupt.superseded',started_at,expires_at,updated_at,updated_at
					FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID)
				if err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, id, _, run, exact := liveTaskMutationFixture(t)
			tt.corrupt(t, db, run)
			err := updateTitle(t, db, id, "must-not-persist", 1, exact)
			if err == nil || !strings.Contains(err.Error(), "run state invariant failed") || errors.As(err, new(*RunConflict)) {
				t.Fatalf("corrupt run update = %v", err)
			}
			task, _ := db.GetTask("main", id)
			if task.Title == "must-not-persist" || task.Revision != 1 {
				t.Fatalf("corrupt run update persisted = %+v", task)
			}
		})
	}
}

func TestUpdateTaskConditionalSQLMatchesEveryRunFence(t *testing.T) {
	tests := []struct {
		name string
		sql  string
	}{
		{"task claim owner", `UPDATE tasks SET claim_owner='agent-b' WHERE tag='main' AND id='1'`},
		{"task claim token", `UPDATE tasks SET claim_token=claim_token+100 WHERE tag='main' AND id='1'`},
		{"task claim expiry", `UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id='1'`},
		{"task claim acquisition", `UPDATE tasks SET claim_extra='{"acquisitionId":"ffffffffffffffffffffffffffffffff"}' WHERE tag='main' AND id='1'`},
		{"run owner", `UPDATE runs SET owner='agent-b' WHERE run_id=1`},
		{"run claim token", `UPDATE runs SET claim_token=claim_token+100 WHERE run_id=1`},
		{"run claim acquisition", `UPDATE runs SET claim_acquisition_id='eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee' WHERE run_id=1`},
		{"run status", `UPDATE runs SET status='retryable' WHERE run_id=1`},
		{"run current attempt", `UPDATE runs SET current_attempt=2 WHERE run_id=1`},
		{"attempt token", `UPDATE attempts SET attempt_token=attempt_token+100 WHERE run_id=1 AND attempt_no=1`},
		{"attempt status", `UPDATE attempts SET status='cancelled',terminal_at=updated_at WHERE run_id=1 AND attempt_no=1`},
		{"attempt expiry", `UPDATE attempts SET expires_at='2000-01-01T00:00:00Z' WHERE run_id=1 AND attempt_no=1`},
		{"newer attempt", `INSERT INTO attempts(run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,
			lease_generation,created_request_id,started_at,expires_at,updated_at,terminal_at)
			SELECT run_id,2,'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb',attempt_token+1000,'lost',ttl_seconds,1,
			'fault.newer',started_at,expires_at,updated_at,updated_at FROM attempts WHERE run_id=1 AND attempt_no=1`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, id, claim, run, exact := liveTaskMutationFixture(t)
			oldHook := taskMutationFaultHook
			taskMutationFaultHook = func(tx *tx, point string) error {
				if point != "before-write" {
					return fmt.Errorf("unexpected hook %q", point)
				}
				_, err := tx.Exec(tt.sql)
				return err
			}
			t.Cleanup(func() { taskMutationFaultHook = oldHook })
			err := updateTitle(t, db, id, "must-not-persist", 1, exact)
			if !errors.As(err, new(*RunConflict)) {
				t.Fatalf("fence mutation %s = %v", tt.name, err)
			}
			task, _ := db.GetTask("main", id)
			if task.Title == "must-not-persist" || task.Revision != 1 || task.Claim == nil || task.Claim.Token != claim.Token {
				t.Fatalf("fence rollback task = %+v", task)
			}
			var runStatus, attemptStatus string
			var runClaimToken, attemptToken int64
			if err := db.db.QueryRow(`SELECT status,claim_token FROM runs WHERE run_id=?`, run.RunID).Scan(&runStatus, &runClaimToken); err != nil {
				t.Fatal(err)
			}
			if err := db.db.QueryRow(`SELECT status,attempt_token FROM attempts WHERE run_id=? AND attempt_no=1`, run.RunID).
				Scan(&attemptStatus, &attemptToken); err != nil {
				t.Fatal(err)
			}
			if runStatus != "running" || attemptStatus != "running" || runClaimToken != claim.Token || attemptToken != run.AttemptToken {
				t.Fatalf("fence hook did not roll back run=%s/%d attempt=%s/%d", runStatus, runClaimToken, attemptStatus, attemptToken)
			}
		})
	}
}

func TestUpdateTaskConditionalSQLProtectsClaimOnlyAndUnclaimedWrites(t *testing.T) {
	t.Run("claim with null metadata", func(t *testing.T) {
		db, _ := newDB(t)
		id := addTask(t, db, "main", "legacy claim")
		if _, err := db.db.Exec(`UPDATE tasks SET claim_owner='agent-a',claim_token=7,claim_ttl=300,
			claim_acquired_at='2099-01-01T00:00:00Z',claim_expires_at='2099-01-01T00:05:00Z'
			WHERE tag='main' AND id=?`, id); err != nil {
			t.Fatal(err)
		}
		if err := updateTitle(t, db, id, "published", 1, &ClaimGate{Owner: "agent-a", Token: 7}); err != nil {
			t.Fatalf("null-metadata claim update = %v", err)
		}
	})

	t.Run("claim only", func(t *testing.T) {
		db, id, claim, run, _ := liveTaskMutationFixture(t)
		if _, err := db.db.Exec(`UPDATE attempts SET expires_at='2000-01-01T00:00:00Z' WHERE run_id=?`, run.RunID); err != nil {
			t.Fatal(err)
		}
		oldHook := taskMutationFaultHook
		taskMutationFaultHook = func(tx *tx, _ string) error {
			_, err := tx.Exec(`UPDATE tasks SET claim_token=claim_token+1 WHERE tag='main' AND id=?`, id)
			return err
		}
		t.Cleanup(func() { taskMutationFaultHook = oldHook })
		err := updateTitle(t, db, id, "must-not-persist", 1, &ClaimGate{Owner: "agent-a", Token: claim.Token})
		if !errors.As(err, new(*ClaimConflict)) {
			t.Fatalf("claim-only raced update = %v", err)
		}
		task, _ := db.GetTask("main", id)
		if task.Title == "must-not-persist" || task.Claim == nil || task.Claim.Token != claim.Token {
			t.Fatalf("claim-only race persisted = %+v", task)
		}
	})

	t.Run("unclaimed", func(t *testing.T) {
		db, _ := newDB(t)
		id := addTask(t, db, "main", "unclaimed")
		oldHook := taskMutationFaultHook
		taskMutationFaultHook = func(tx *tx, _ string) error {
			_, err := tx.Exec(`UPDATE tasks SET claim_owner='agent-b',claim_token=99,claim_ttl=300,
				claim_acquired_at='2099-01-01T00:00:00Z',claim_expires_at='2099-01-01T00:05:00Z',
				claim_extra='{"acquisitionId":"cccccccccccccccccccccccccccccccc"}' WHERE tag='main' AND id=?`, id)
			return err
		}
		t.Cleanup(func() { taskMutationFaultHook = oldHook })
		err := updateTitle(t, db, id, "must-not-persist", 1, nil)
		if err == nil {
			t.Fatal("unclaimed raced update succeeded")
		}
		task, _ := db.GetTask("main", id)
		if task.Title == "must-not-persist" || task.Revision != 1 || task.Claim != nil {
			t.Fatalf("unclaimed race persisted = %+v", task)
		}
	})
}

func TestValidateTaskRunGateGrammar(t *testing.T) {
	bad := []*ClaimGate{
		{Owner: "agent-a"}, {Token: 1}, {Owner: " bad-owner", Token: 1},
		{RunID: 1}, {Owner: "agent-a", Token: 1, RunID: 1, Attempt: 1},
		{Owner: "agent-a", Token: 1, RunID: -1, Attempt: 1, AttemptToken: 1},
	}
	for i, gate := range bad {
		if err := ValidateTaskRunGate(gate); err == nil {
			t.Fatalf("bad gate %d accepted: %+v", i, gate)
		}
	}
}
