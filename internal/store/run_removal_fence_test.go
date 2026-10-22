package store

import (
	"errors"
	"strings"
	"testing"
)

func TestRemoveTopLevelRefusesAnyRunHistoryEvenWithForce(t *testing.T) {
	tests := []struct {
		name     string
		terminal bool
	}{
		{"live", false},
		{"terminal", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, id, claim, run, exact := liveTaskMutationFixture(t)
			if tt.terminal {
				if _, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, run.Attempt,
					run.AttemptToken, "succeeded", "finish.remove-history", "", 0, ""); err != nil {
					t.Fatal(err)
				}
			}
			stripped, err := db.RemoveTask("main", id, 1, true, exact)
			var conflict *RunConflict
			if stripped != nil || !errors.As(err, &conflict) || conflict.Operation != "remove" ||
				conflict.Tag != "main" || conflict.Task != id || conflict.RunID != 0 || conflict.Attempt != 0 {
				t.Fatalf("run-history removal = %v, %+v", stripped, err)
			}
			if task, _ := db.GetTask("main", id); task == nil || task.Revision != 1 {
				t.Fatalf("run-history task removed = %+v", task)
			}
		})
	}
}

func TestRemoveSubtaskRequiresExactLiveRunGate(t *testing.T) {
	db, id, exact := liveSubtaskStatusFixture(t)
	if _, err := db.RemoveTask("main", id+".1", 1, false, nil); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("ungated subtask removal = %v", err)
	}
	if _, err := db.RemoveTask("main", id+".1", 1, false, exact); err != nil {
		t.Fatalf("gated subtask removal = %v", err)
	}
	task, _ := db.GetTask("main", id)
	if task.Revision != 2 || string(task.Subtasks) != "[]" {
		t.Fatalf("subtask removal task = %+v", task)
	}
}

func TestRemoveTaskDependencyValidationPrecedesRunHistoryFence(t *testing.T) {
	db, id, _, _, exact := liveTaskMutationFixture(t)
	if _, err := db.AddTask("main", &Task{Title: "dependent", Dependencies: []string{id}}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveTask("main", id, 1, false, exact); err == nil ||
		errors.As(err, new(*RunConflict)) || !strings.Contains(err.Error(), "is a dependency of") {
		t.Fatalf("dependency precedence = %v", err)
	}
	if _, err := db.RemoveTask("main", id, 1, true, exact); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("forced run-history removal = %v", err)
	}
}

func TestRemoveTaskRefusesCollateralLiveRun(t *testing.T) {
	db, _ := newDB(t)
	target := addTask(t, db, "main", "target")
	dependent, err := db.AddTask("main", &Task{Title: "dependent", Dependencies: []string{target}})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, _, err := db.AcquireClaim("main", dependent, "agent-b", 3600, "claim.collateral")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.StartRun("main", dependent, "agent-b", claim.Token, "run.collateral", 300); err != nil {
		t.Fatal(err)
	}
	_, err = db.RemoveTask("main", target, 1, true, nil)
	var conflict *RunConflict
	if !errors.As(err, &conflict) || conflict.Tag != "main" || conflict.Task != dependent || conflict.RunID != 0 {
		t.Fatalf("collateral live run = %v", err)
	}
	if task, _ := db.GetTask("main", target); task == nil {
		t.Fatal("target removed on collateral refusal")
	}
	if task, _ := db.GetTask("main", dependent); task == nil || len(task.Dependencies) != 1 {
		t.Fatalf("dependent rewritten on collateral refusal = %+v", task)
	}
}

func TestRemoveTaskConditionalSQLRetainsTargetAndCollateralFences(t *testing.T) {
	t.Run("target claim", func(t *testing.T) {
		db, _ := newDB(t)
		id := addTask(t, db, "main", "target")
		claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.remove-target")
		if err != nil {
			t.Fatal(err)
		}
		oldHook := taskMutationFaultHook
		taskMutationFaultHook = func(tx *tx, _ string) error {
			_, err := tx.Exec(`UPDATE tasks SET claim_token=claim_token+1 WHERE tag='main' AND id=?`, id)
			return err
		}
		t.Cleanup(func() { taskMutationFaultHook = oldHook })
		if _, err := db.RemoveTask("main", id, 1, false,
			&ClaimGate{Owner: "agent-a", Token: claim.Token}); !errors.As(err, new(*ClaimConflict)) {
			t.Fatalf("raced target removal = %v", err)
		}
		if task, _ := db.GetTask("main", id); task == nil || task.Claim == nil || task.Claim.Token != claim.Token {
			t.Fatalf("raced target removal persisted = %+v", task)
		}
	})

	t.Run("collateral claim", func(t *testing.T) {
		db, _ := newDB(t)
		target := addTask(t, db, "main", "target")
		dependent, err := db.AddTask("main", &Task{Title: "dependent", Dependencies: []string{target}})
		if err != nil {
			t.Fatal(err)
		}
		oldHook := taskMutationFaultHook
		taskMutationFaultHook = func(tx *tx, _ string) error {
			_, err := tx.Exec(`UPDATE tasks SET claim_owner='agent-b',claim_token=99,claim_ttl=300,
				claim_acquired_at='2099-01-01T00:00:00Z',claim_expires_at='2099-01-01T00:05:00Z',
				claim_extra='{"acquisitionId":"dddddddddddddddddddddddddddddddd"}'
				WHERE tag='main' AND id=?`, dependent)
			return err
		}
		t.Cleanup(func() { taskMutationFaultHook = oldHook })
		if _, err := db.RemoveTask("main", target, 1, true, nil); !errors.As(err, new(*ClaimConflict)) {
			t.Fatalf("raced collateral removal = %v", err)
		}
		if task, _ := db.GetTask("main", target); task == nil {
			t.Fatal("target removed on collateral race")
		}
		if task, _ := db.GetTask("main", dependent); task == nil || len(task.Dependencies) != 1 || task.Claim != nil {
			t.Fatalf("collateral race persisted = %+v", task)
		}
	})
}

func TestRemoveSubtaskConditionalSQLRetainsRunFence(t *testing.T) {
	db, id, exact := liveSubtaskStatusFixture(t)
	oldHook := taskMutationFaultHook
	taskMutationFaultHook = func(tx *tx, _ string) error {
		_, err := tx.Exec(`UPDATE attempts SET attempt_token=attempt_token+1 WHERE run_id=? AND attempt_no=?`,
			exact.RunID, exact.Attempt)
		return err
	}
	t.Cleanup(func() { taskMutationFaultHook = oldHook })
	if _, err := db.RemoveTask("main", id+".1", 1, false, exact); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("raced subtask removal = %v", err)
	}
	task, _ := db.GetTask("main", id)
	if task.Revision != 1 || string(task.Subtasks) == "[]" {
		t.Fatalf("raced subtask removal persisted = %+v", task)
	}
}
