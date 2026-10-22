package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

func TestAddTaskAllocatesSequentialIDsAndEvents(t *testing.T) {
	db, _ := newDB(t)
	id1 := addTask(t, db, "main", "first")
	id2 := addTask(t, db, "main", "second")
	if id1 != "1" || id2 != "2" {
		t.Fatalf("ids = %s, %s; want 1, 2", id1, id2)
	}
	info, err := db.Tag("main")
	if err != nil {
		t.Fatal(err)
	}
	if info.NextID != 3 || info.TaskCount != 2 || info.CompletedCount != 0 {
		t.Errorf("tag info = %+v", info)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Events) != 2 || feed.Events[0].Kind != "add" || feed.Events[0].Task != "1" {
		t.Errorf("feed = %+v", feed.Events)
	}
	if feed.OldestRetainedSeq != 1 {
		t.Errorf("oldestRetainedSeq = %d, want 1", feed.OldestRetainedSeq)
	}
}

func TestAddTaskRefusesMissingDependency(t *testing.T) {
	db, _ := newDB(t)
	_, err := db.AddTask("main", &Task{Title: "dangling", Dependencies: []string{"9"}})
	if err == nil {
		t.Fatal("add with a dangling dependency succeeded")
	}
}

// TestAddTaskValidatesMilestoneInTransaction pins §8.2: the milestone
// existence check is part of AddTask's immediate transaction, so a concurrent
// milestone remove cannot strand the reference.
func TestAddTaskValidatesMilestoneInTransaction(t *testing.T) {
	db, _ := newDB(t)
	var notFound *backlog.ErrMilestoneNotFound
	if _, err := db.AddTask("main", &Task{Title: "x", Milestone: "v1"}); !errors.As(err, &notFound) {
		t.Fatalf("add with unknown milestone = %v, want ErrMilestoneNotFound", err)
	}
	if err := db.AddMilestone("main", "v1"); err != nil {
		t.Fatal(err)
	}
	id, err := db.AddTask("main", &Task{Title: "x", Milestone: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil || task == nil || task.Milestone != "v1" {
		t.Fatalf("task = %+v, %v", task, err)
	}
	// A milestone removed out from under the name now refuses the add.
	if _, err := db.RemoveMilestone("main", "v1", true); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTask("main", &Task{Title: "y", Milestone: "v1"}); !errors.As(err, &notFound) {
		t.Fatalf("add after milestone removal = %v, want ErrMilestoneNotFound", err)
	}
}

func TestAddTasksBatchSummaryEvent(t *testing.T) {
	db, _ := newDB(t)
	ids, err := db.AddTasks("main", []*Task{{Title: "a"}, {Title: "b"}, {Title: "c"}})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids) != "[1 2 3]" {
		t.Fatalf("ids = %v", ids)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Events) != 1 || feed.Events[0].Kind != "add" || feed.Events[0].Count != 3 {
		t.Errorf("feed = %+v", feed.Events)
	}
}

func TestUpdateTaskCAS(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "cas")
	err := db.UpdateTask("main", id, 1, nil, func(task *Task) error {
		task.Title = "updated"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "updated" || task.Revision != 2 {
		t.Errorf("task = %+v", task)
	}
	// Stale revision: typed conflict carrying current state.
	err = db.UpdateTask("main", id, 1, nil, func(task *Task) error { return nil })
	var stale *StaleWriteConflict
	if !errors.As(err, &stale) {
		t.Fatalf("stale update: %v, want StaleWriteConflict", err)
	}
	if stale.Supplied != 1 || stale.Current != 2 || stale.Status != "pending" {
		t.Errorf("conflict = %+v", stale)
	}
	// The update event carries the new revision.
	feed, err := db.Feed(1)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Events) != 1 || feed.Events[0].Kind != "update" || feed.Events[0].Revision != 2 {
		t.Errorf("feed = %+v", feed.Events)
	}
}

func TestUpdateTaskNotFound(t *testing.T) {
	db, _ := newDB(t)
	err := db.UpdateTask("main", "9", 1, nil, func(task *Task) error { return nil })
	var notFound *TaskNotFound
	if !errors.As(err, &notFound) {
		t.Fatalf("update missing task: %v, want TaskNotFound", err)
	}
}

func TestSetStatusTopLevelAndDoneFloor(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "lifecycle")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 0, "req-life-1")
	if err != nil {
		t.Fatal(err)
	}
	// A claimed task refuses an ungated transition.
	_, err = db.SetStatus("main", id, "in-progress", 1, nil)
	var cc *ClaimConflict
	if !errors.As(err, &cc) || cc.Kind != "status" {
		t.Fatalf("ungated status: %v, want status ClaimConflict", err)
	}
	// The holder transitions with the gate.
	prev, err := db.SetStatus("main", id, "in-progress", 1, &ClaimGate{Owner: "agent-a", Token: claim.Token})
	if err != nil || prev != "pending" {
		t.Fatalf("gated status: prev=%q err=%v", prev, err)
	}
	// Done releases the claim (the durable done floor) with a release event.
	if _, err := db.SetStatus("main", id, "done", 2, &ClaimGate{Owner: "agent-a", Token: claim.Token}); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" || task.Claim != nil {
		t.Errorf("task = %+v claim=%+v", task, task.Claim)
	}
	info, err := db.Tag("main")
	if err != nil {
		t.Fatal(err)
	}
	if info.CompletedCount != 1 {
		t.Errorf("completedCount = %d, want 1", info.CompletedCount)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range feed.Events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"add", "claim", "status", "status", "release"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("feed kinds = %v, want %v", kinds, want)
	}
}

func TestSetStatusSubtaskKeepsParentFields(t *testing.T) {
	db, _ := newDB(t)
	sub := json.RawMessage(`[{"id":1,"title":"child","status":"pending","dependencies":[],"parentTaskId":"1","customSubField":{"x":1}}]`)
	id, err := db.AddTask("main", &Task{Title: "parent", Subtasks: sub, Details: "keep me"})
	if err != nil {
		t.Fatal(err)
	}
	prev, err := db.SetStatus("main", id+".1", "done", 1, nil)
	if err != nil || prev != "pending" {
		t.Fatalf("subtask status: prev=%q err=%v", prev, err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Revision != 2 || task.Details != "keep me" {
		t.Errorf("task = %+v", task)
	}
	var subs []map[string]json.RawMessage
	if err := json.Unmarshal(task.Subtasks, &subs); err != nil {
		t.Fatal(err)
	}
	if string(subs[0]["status"]) != `"done"` {
		t.Errorf("subtask status = %s", subs[0]["status"])
	}
	if string(subs[0]["customSubField"]) != `{"x":1}` {
		t.Errorf("subtask unknown field not preserved: %s", task.Subtasks)
	}
}

func TestRemoveTaskDependentsAndForce(t *testing.T) {
	db, _ := newDB(t)
	id1 := addTask(t, db, "main", "base")
	id2, err := db.AddTask("main", &Task{Title: "dep", Dependencies: []string{id1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveTask("main", id1, 1, false, nil); err == nil {
		t.Fatal("remove with dependents succeeded without force")
	}
	stripped, err := db.RemoveTask("main", id1, 1, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(stripped) != "["+id2+"]" {
		t.Errorf("stripped = %v, want [%s]", stripped, id2)
	}
	dep, err := db.GetTask("main", id2)
	if err != nil {
		t.Fatal(err)
	}
	if len(dep.Dependencies) != 0 || dep.Revision != 2 {
		t.Errorf("dependent = %+v", dep)
	}
	task, err := db.GetTask("main", id1)
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Error("removed task still readable")
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range feed.Events {
		kinds = append(kinds, e.Kind)
	}
	want := []string{"add", "add", "update", "remove"}
	if fmt.Sprint(kinds) != fmt.Sprint(want) {
		t.Errorf("feed kinds = %v, want %v", kinds, want)
	}
}

func TestRemoveSubtask(t *testing.T) {
	db, _ := newDB(t)
	sub := json.RawMessage(`[{"id":1,"title":"child","status":"pending","dependencies":[],"parentTaskId":"1"}]`)
	id, err := db.AddTask("main", &Task{Title: "parent", Subtasks: sub})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveTask("main", id+".1", 1, false, nil); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if string(task.Subtasks) != `[]` || task.Revision != 2 {
		t.Errorf("task = %+v subtasks=%s", task, task.Subtasks)
	}
}

func TestAppendDetails(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "notes")
	stamp, err := db.AppendDetails("main", id, "first note", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	want := "\n<info added " + stamp + ">\nfirst note\n</info>"
	if task.Details != want {
		t.Errorf("details = %q, want %q", task.Details, want)
	}
}

func TestClaimsConflictRenewReleaseExpiry(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "claimable")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-acq-1")
	if err != nil {
		t.Fatal(err)
	}
	if claim.Token != 1 {
		t.Errorf("first fencing token = %d, want 1", claim.Token)
	}
	// A second acquire conflicts with a typed error.
	_, _, _, err = db.AcquireClaim("main", id, "agent-b", 300, "req-acq-2")
	var cc *ClaimConflict
	if !errors.As(err, &cc) || cc.Kind != "claim" || cc.Owner != "agent-a" {
		t.Fatalf("second acquire: %v, want claim ClaimConflict", err)
	}
	// Renew with the wrong token conflicts.
	if _, _, err := db.RenewClaim("main", id, "agent-a", claim.Token+99, 0); !errors.As(err, &cc) || cc.Kind != "renew" {
		t.Fatalf("renew wrong token: %v, want renew ClaimConflict", err)
	}
	renewed, _, err := db.RenewClaim("main", id, "agent-a", claim.Token, 600)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Token <= claim.Token {
		t.Errorf("renew token = %d, want > %d", renewed.Token, claim.Token)
	}
	if renewed.TTL != 600 {
		t.Errorf("renew ttl = %d, want 600", renewed.TTL)
	}
	// Release with the old token conflicts.
	if err := db.ReleaseClaim("main", id, "agent-a", claim.Token, false); !errors.As(err, &cc) || cc.Kind != "release" {
		t.Fatalf("release old token: %v, want release ClaimConflict", err)
	}
	if err := db.ReleaseClaim("main", id, "agent-a", renewed.Token, false); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Claim != nil {
		t.Errorf("claim still present: %+v", task.Claim)
	}
	// Expired claims are treated as absent: acquire again after forcing the
	// expiry into the past.
	if _, _, _, err := db.AcquireClaim("main", id, "agent-b", 300, "req-acq-3"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE tasks SET claim_expires_at='2000-01-01T00:00:00Z' WHERE tag='main' AND id=?`, id); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.AcquireClaim("main", id, "agent-c", 300, "req-acq-4"); err != nil {
		t.Fatalf("acquire after expiry: %v", err)
	}
	// Force release by any owner, recorded on the feed.
	if err := db.ReleaseClaim("main", id, "operator", 0, true); err != nil {
		t.Fatal(err)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "release" || last.Owner != "agent-c" {
		t.Errorf("force release event = %+v", last)
	}
}

func TestBindingsDoNotTouchRevision(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "bindable")
	binding := &WorktreeBinding{
		Path: "/tmp/wt", Boundary: "/tmp/repo", Branch: "tasks/main/1",
		CreatedAt: "2026-08-22T00:00:00Z",
		Extra:     map[string]json.RawMessage{"future": json.RawMessage(`true`)},
	}
	if err := db.SetBinding("main", id, binding); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Revision != 1 {
		t.Errorf("revision = %d, want 1 (binding set must not touch revision)", task.Revision)
	}
	if task.Binding == nil || task.Binding.Path != "/tmp/wt" || task.Binding.Keep {
		t.Errorf("binding = %+v", task.Binding)
	}
	if string(task.Binding.Extra["future"]) != `true` {
		t.Errorf("binding unknown field not preserved: %+v", task.Binding.Extra)
	}
	if err := db.ClearBinding("main", id); err != nil {
		t.Fatal(err)
	}
	task, err = db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Binding != nil || task.Revision != 1 {
		t.Errorf("after clear: binding=%+v revision=%d", task.Binding, task.Revision)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range feed.Events {
		if e.Kind == "worktree" {
			actions = append(actions, e.Action)
		}
	}
	if fmt.Sprint(actions) != "[created removed]" {
		t.Errorf("worktree event actions = %v", actions)
	}
}

func TestMilestoneRegistryReadWrite(t *testing.T) {
	db, _ := newDB(t)
	raw, err := db.Milestones("main")
	if err != nil || raw != nil {
		t.Fatalf("empty registry = %s, %v", raw, err)
	}
	registry := json.RawMessage(`[{"name":"v1","status":"open","note":"kept"}]`)
	if err := db.SetMilestones("main", registry); err != nil {
		t.Fatal(err)
	}
	got, err := db.Milestones("main")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(registry) {
		t.Errorf("registry = %s, want %s (byte-faithful)", got, registry)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "milestone" || last.Count != 1 {
		t.Errorf("milestone event = %+v", last)
	}
}

func TestResolveTagDefault(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.ResolveTag(""); err == nil {
		t.Fatal("ResolveTag with no tags succeeded")
	}
	addTask(t, db, "main", "one")
	tag, err := db.ResolveTag("")
	if err != nil || tag != "main" {
		t.Errorf("ResolveTag = %q, %v", tag, err)
	}
	if _, err := db.ResolveTag("nope"); err == nil {
		t.Fatal("ResolveTag with unknown tag succeeded")
	}
}

func TestFeedRetentionCompactionInsideMutationTx(t *testing.T) {
	db, _ := newDB(t)
	addTask(t, db, "main", "seed")
	// Append MaxRetainedEvents+50 events through one mutation-scoped append
	// path (appendEvent compacts inside the same transaction).
	err := db.immediate(func(tx *tx) error {
		for i := 0; i < MaxRetainedEvents+50; i++ {
			if err := tx.appendEvent(Event{Tag: "main", Kind: "update", At: nowUTC()}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(feed.Events) != MaxRetainedEvents {
		t.Fatalf("retained = %d, want %d", len(feed.Events), MaxRetainedEvents)
	}
	// Sequence numbers stay monotonic across the retention boundary: the seed
	// add took seq 1, so 10051 events were appended and the retained floor is
	// 52.
	if feed.OldestRetainedSeq != 52 {
		t.Errorf("oldestRetainedSeq = %d, want 52", feed.OldestRetainedSeq)
	}
	truncated, err := db.Feed(10)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated.Truncated {
		t.Error("since below the retention floor did not report truncation")
	}
	// New events keep drawing higher seqs after compaction.
	addTask(t, db, "main", "after")
	feed, err = db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Seq != MaxRetainedEvents+52 {
		t.Errorf("post-compaction seq = %d, want %d", last.Seq, MaxRetainedEvents+52)
	}
}

func TestBackupConsistency(t *testing.T) {
	db, path := newDB(t)
	for i := 0; i < 5; i++ {
		addTask(t, db, "main", fmt.Sprintf("task-%d", i))
	}
	srcSnap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	srcDigest, err := srcSnap.Digest()
	if err != nil {
		t.Fatal(err)
	}
	// A writer exists mid-transaction on a second handle while the backup is
	// taken; the backup must still be a consistent copy.
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- db2.immediate(func(tx *tx) error {
			if err := tx.insertTask(&Task{Tag: "main", ID: "99", Title: "uncommitted", Status: "pending",
				Dependencies: []string{}, Subtasks: json.RawMessage(`[]`), Revision: 1, UpdatedAt: nowUTC()}); err != nil {
				return err
			}
			<-release
			return nil
		})
	}()
	// Give the writer a moment to take the write lock, then back up.
	backupPath := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := db.BackupTo(backupPath); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	backup, err := Open(backupPath)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.IntegrityCheck(); err != nil {
		t.Fatal(err)
	}
	tasks, err := backup.ListTasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 5 {
		t.Errorf("backup holds %d tasks, want 5 (the uncommitted write must not leak)", len(tasks))
	}
	dstSnap, err := backup.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	dstDigest, err := dstSnap.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if srcDigest != dstDigest {
		t.Errorf("backup digest %s != pre-writer source digest %s", dstDigest, srcDigest)
	}
}
