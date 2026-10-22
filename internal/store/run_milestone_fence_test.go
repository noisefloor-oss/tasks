package store

import (
	"errors"
	"strings"
	"testing"
)

func TestSetTaskMilestoneRequiresExactLiveRunGate(t *testing.T) {
	db, id, _, _, exact := liveTaskMutationFixture(t)
	if err := db.AddMilestone("main", "M1"); err != nil {
		t.Fatal(err)
	}
	previous, err := db.SetTaskMilestone("main", id, "M1", 1, nil)
	if previous != "" || !errors.As(err, new(*RunConflict)) {
		t.Fatalf("ungated milestone = %q, %v", previous, err)
	}
	previous, err = db.SetTaskMilestone("main", id, "M1", 1, exact)
	if previous != "" || err != nil {
		t.Fatalf("gated milestone = %q, %v", previous, err)
	}
	task, _ := db.GetTask("main", id)
	if task.Milestone != "M1" || task.Revision != 2 {
		t.Fatalf("milestoned task = %+v", task)
	}
}

func TestSetTaskMilestoneNoOpAndValidationStillFenceCorrectly(t *testing.T) {
	db, id, _, _, exact := liveTaskMutationFixture(t)
	if err := db.AddMilestone("main", "M1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetTaskMilestone("main", id, "unknown", 1, nil); err == nil || errors.As(err, new(*RunConflict)) {
		t.Fatalf("unknown milestone precedence = %v", err)
	}
	if previous, err := db.SetTaskMilestone("main", id, "", 1, nil); previous != "" || !errors.As(err, new(*RunConflict)) {
		t.Fatalf("ungated no-op = %q, %v", previous, err)
	}
	if previous, err := db.SetTaskMilestone("main", id, "", 1, exact); previous != "" || err != nil {
		t.Fatalf("gated no-op = %q, %v", previous, err)
	}
}

func TestSetTaskMilestoneConditionalSQLRetainsRunFence(t *testing.T) {
	db, id, _, run, exact := liveTaskMutationFixture(t)
	if err := db.AddMilestone("main", "M1"); err != nil {
		t.Fatal(err)
	}
	oldHook := taskMutationFaultHook
	taskMutationFaultHook = func(tx *tx, point string) error {
		_, err := tx.Exec(`UPDATE runs SET status='retryable' WHERE run_id=?`, run.RunID)
		return err
	}
	t.Cleanup(func() { taskMutationFaultHook = oldHook })
	previous, err := db.SetTaskMilestone("main", id, "M1", 1, exact)
	if previous != "" || !errors.As(err, new(*RunConflict)) || strings.Contains(err.Error(), "status") {
		t.Fatalf("raced milestone = %q, %v", previous, err)
	}
	task, _ := db.GetTask("main", id)
	if task.Milestone != "" || task.Revision != 1 {
		t.Fatalf("raced milestone persisted = %+v", task)
	}
}
