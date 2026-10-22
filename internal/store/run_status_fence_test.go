package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func liveSubtaskStatusFixture(t *testing.T) (*DB, string, *ClaimGate) {
	t.Helper()
	db, _ := newDB(t)
	subtasks := json.RawMessage(`[{"id":1,"title":"leaf","status":"pending","dependencies":[],"parentTaskId":"1"}]`)
	id, err := db.AddTask("main", &Task{Title: "parent", Subtasks: subtasks})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim.status-subtask")
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := db.StartRun("main", id, "agent-a", claim.Token, "run.status-subtask", 300)
	if err != nil {
		t.Fatal(err)
	}
	return db, id, &ClaimGate{Owner: "agent-a", Token: claim.Token, RunID: run.RunID,
		Attempt: run.Attempt, AttemptToken: run.AttemptToken}
}

func TestSetStatusRequiresExactLiveRunGate(t *testing.T) {
	tests := []struct {
		name    string
		fixture func(*testing.T) (*DB, string, *ClaimGate)
		apply   func(*DB, string, *ClaimGate) (string, error)
	}{
		{
			name: "top level",
			fixture: func(t *testing.T) (*DB, string, *ClaimGate) {
				db, id, _, _, gate := liveTaskMutationFixture(t)
				return db, id, gate
			},
			apply: func(db *DB, id string, gate *ClaimGate) (string, error) {
				return db.SetStatus("main", id, "in-progress", 1, gate)
			},
		},
		{
			name:    "subtask",
			fixture: liveSubtaskStatusFixture,
			apply: func(db *DB, id string, gate *ClaimGate) (string, error) {
				return db.SetStatus("main", id+".1", "done", 1, gate)
			},
		},
		{
			name: "worktree top level",
			fixture: func(t *testing.T) (*DB, string, *ClaimGate) {
				db, id, _, _, gate := liveTaskMutationFixture(t)
				return db, id, gate
			},
			apply: func(db *DB, id string, gate *ClaimGate) (string, error) {
				previous, _, err := db.SetStatusWorktree("", "main", id, "in-progress", 1, gate, false)
				return previous, err
			},
		},
		{
			name:    "worktree subtask",
			fixture: liveSubtaskStatusFixture,
			apply: func(db *DB, id string, gate *ClaimGate) (string, error) {
				previous, _, err := db.SetStatusWorktree("", "main", id+".1", "done", 1, gate, false)
				return previous, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, id, exact := tt.fixture(t)
			previous, err := tt.apply(db, id, nil)
			if previous != "" || !errors.As(err, new(*RunConflict)) {
				t.Fatalf("ungated status = %q, %v", previous, err)
			}
			previous, err = tt.apply(db, id, exact)
			if previous != "pending" || err != nil {
				t.Fatalf("exactly gated status = %q, %v", previous, err)
			}
		})
	}
}

func TestSetStatusLiveRunNoOpAndDoneFloor(t *testing.T) {
	db, id, _, _, exact := liveTaskMutationFixture(t)
	if previous, err := db.SetStatus("main", id, "pending", 1, nil); previous != "" || !errors.As(err, new(*RunConflict)) {
		t.Fatalf("ungated no-op = %q, %v", previous, err)
	}
	if previous, err := db.SetStatus("main", id, "pending", 1, exact); previous != "pending" || err != nil {
		t.Fatalf("gated no-op = %q, %v", previous, err)
	}
	if previous, err := db.SetStatus("main", id, "done", 1, exact); previous != "pending" || err != nil {
		t.Fatalf("gated done = %q, %v", previous, err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" || task.Revision != 2 || task.Claim != nil {
		t.Fatalf("done task = %+v", task)
	}
	if _, err := db.SetStatus("main", id, "review", 2, exact); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("invalidated run gate = %v", err)
	}
}

func TestSetStatusStructuralValidationPrecedesRunFence(t *testing.T) {
	db, id, _, _, _ := liveTaskMutationFixture(t)
	_, err := db.SetStatus("main", id+".99", "done", 1, nil)
	if err == nil || errors.As(err, new(*RunConflict)) {
		t.Fatalf("missing subtask precedence = %v", err)
	}
}

func TestSetStatusConditionalSQLRetainsRunFence(t *testing.T) {
	db, id, _, run, exact := liveTaskMutationFixture(t)
	oldHook := taskMutationFaultHook
	taskMutationFaultHook = func(tx *tx, point string) error {
		_, err := tx.Exec(`UPDATE attempts SET attempt_token=attempt_token+1 WHERE run_id=? AND attempt_no=1`, run.RunID)
		return err
	}
	t.Cleanup(func() { taskMutationFaultHook = oldHook })
	previous, err := db.SetStatus("main", id, "review", 1, exact)
	if previous != "pending" || !errors.As(err, new(*RunConflict)) || strings.Contains(err.Error(), "token") {
		t.Fatalf("raced status = %q, %v", previous, err)
	}
	task, _ := db.GetTask("main", id)
	if task.Status != "pending" || task.Revision != 1 {
		t.Fatalf("raced status persisted = %+v", task)
	}
}
