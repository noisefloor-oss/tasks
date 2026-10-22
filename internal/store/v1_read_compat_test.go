package store

import (
	"path/filepath"
	"testing"
)

// makeV1StoreWithTask creates a store carrying one task, then rewinds it to a
// released v1 shape (pre-record_extra, pre-receipts, pre-identity,
// user_version 1) so reads can be exercised against the legacy schema.
func makeV1StoreWithTask(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "noise-tasks", "tasks.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTask("main", &Task{Title: "legacy", Description: "v1 era"}); err != nil {
		t.Fatal(err)
	}
	rewindStoreToV2(t, db)
	for _, q := range []string{
		`DROP INDEX receipts_request_id`,
		`DROP TABLE receipts`,
		`ALTER TABLE tasks DROP COLUMN record_extra`,
		`DELETE FROM meta WHERE key IN ('familyId','ownerBoundary')`,
		`PRAGMA user_version=1`,
	} {
		if _, err := db.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	return path
}

// Read-only paths on an untouched v1 store must work without mutating it:
// direct task reads, tag listings, the whole-store Snapshot, and the
// digest/verify pass all previously failed with "no such column:
// record_extra".
func TestV1StoreReadsBeforeUpgrade(t *testing.T) {
	path := makeV1StoreWithTask(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	task, err := db.GetTask("main", "1")
	if err != nil {
		t.Fatalf("GetTask on v1: %v", err)
	}
	if task == nil || task.Title != "legacy" {
		t.Fatalf("GetTask on v1 = %+v, want the legacy task", task)
	}
	if task.OuterExtra != nil {
		t.Fatalf("OuterExtra on v1 = %v, want nil", task.OuterExtra)
	}

	tasks, err := db.ListTasks("main")
	if err != nil {
		t.Fatalf("ListTasks on v1: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Title != "legacy" {
		t.Fatalf("ListTasks on v1 = %d tasks, want 1 legacy task", len(tasks))
	}

	snap, err := db.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot on v1: %v", err)
	}
	tag := snap.Tags["main"]
	if tag == nil || len(tag.Tasks) != 1 || tag.Tasks[0].Title != "legacy" {
		t.Fatalf("Snapshot on v1 main tag = %+v, want one legacy task", snap.Tags["main"])
	}

	if _, _, err := db.MilestoneCountsForList("main"); err != nil {
		t.Fatalf("milestone rollup on v1: %v", err)
	}
	if err := db.ValidateStore(); err != nil {
		t.Fatalf("ValidateStore on v1: %v", err)
	}

	// Reads never upgrade: the store still reports the released v1 schema.
	if v, verr := db.userVersion(); verr != nil || v != 1 {
		t.Fatalf("user_version %d (%v) after reads, want 1", v, verr)
	}
}

// After a v1 read, the first mutation atomically traverses v1→v2→v3
// upgrade and the task survives intact, with reads now hitting the real
// record_extra column.
func TestV1ReadsThenUpgradeOnFirstMutation(t *testing.T) {
	path := makeV1StoreWithTask(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	owner := filepath.Join(filepath.Dir(filepath.Dir(path)), "family")
	if err := db.SetOwnerBoundary(owner); err != nil {
		t.Fatal(err)
	}

	if _, err := db.GetTask("main", "1"); err != nil {
		t.Fatalf("GetTask on v1 before upgrade: %v", err)
	}
	if _, err := db.AddTask("main", &Task{Title: "post-upgrade"}); err != nil {
		t.Fatalf("first mutation on v1: %v", err)
	}
	if v, verr := db.userVersion(); verr != nil || v != SchemaVersion {
		t.Fatalf("user_version %d (%v) after first mutation, want %d", v, verr, SchemaVersion)
	}

	tasks, err := db.ListTasks("main")
	if err != nil {
		t.Fatalf("ListTasks after upgrade: %v", err)
	}
	if len(tasks) != 2 || tasks[0].Title != "legacy" || tasks[1].Title != "post-upgrade" {
		var titles []string
		for _, tk := range tasks {
			titles = append(titles, tk.Title)
		}
		t.Fatalf("tasks after upgrade = %v, want [legacy post-upgrade]", titles)
	}
	for _, tk := range tasks {
		if tk.OuterExtra != nil {
			t.Fatalf("task %s OuterExtra = %v after upgrade, want nil", tk.ID, tk.OuterExtra)
		}
	}
	if _, _, inv, err := db.ClassDigests(); err != nil {
		t.Fatalf("ClassDigests after upgrade: %v", err)
	} else if inv.Tasks != 2 {
		t.Fatalf("ClassDigests inventory tasks after upgrade = %d, want 2", inv.Tasks)
	}
}
