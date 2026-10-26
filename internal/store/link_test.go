package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

func linkOf(family, repo string, issue int64) *backlog.ExternalLink {
	return &backlog.ExternalLink{Family: family, Repository: repo, Issue: issue}
}

func TestSetLinkAssignsOnceAndBumpsRevision(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "linked")
	link := *linkOf("github", "acme/widgets", 214)
	if err := db.SetLink("main", id, link, 1, nil); err != nil {
		t.Fatalf("SetLink: %v", err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Link == nil || task.Link.String() != "github/acme/widgets#214" {
		t.Fatalf("link = %+v", task.Link)
	}
	if task.Revision != 2 {
		t.Fatalf("revision = %d; want 2", task.Revision)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	if feed.Events[len(feed.Events)-1].Kind != "update" || feed.Events[len(feed.Events)-1].Revision != 2 {
		t.Fatalf("events = %+v", feed.Events)
	}

	// Set-once: any second attempt is a typed refusal, even the same value.
	err = db.SetLink("main", id, link, 2, nil)
	var once *LinkSetConflict
	if !errors.As(err, &once) {
		t.Fatalf("second set-link = %v; want LinkSetConflict", err)
	}
	other := *linkOf("gitlab", "acme/tools", 9)
	err = db.SetLink("main", id, other, 2, nil)
	if !errors.As(err, &once) {
		t.Fatalf("changed set-link = %v; want LinkSetConflict", err)
	}

	// Stale revision is the ordinary §8.2 CAS conflict.
	err = db.SetLink("main", id, other, 1, nil)
	var stale *StaleWriteConflict
	if !errors.As(err, &stale) {
		t.Fatalf("stale set-link = %v; want StaleWriteConflict", err)
	}
}

func TestSetLinkDuplicateTripleRefuses(t *testing.T) {
	db, _ := newDB(t)
	first := addTask(t, db, "main", "first")
	second := addTask(t, db, "main", "second")
	if err := db.SetLink("main", first, *linkOf("github", "acme/widgets", 214), 1, nil); err != nil {
		t.Fatal(err)
	}
	err := db.SetLink("main", second, *linkOf("github", "acme/widgets", 214), 1, nil)
	var exists *LinkExistsConflict
	if !errors.As(err, &exists) || exists.ExistingID != first {
		t.Fatalf("duplicate set-link = %v; want LinkExistsConflict for %s", err, first)
	}
	task, _ := db.GetTask("main", second)
	if task.Link != nil || task.Revision != 1 {
		t.Fatalf("refused write persisted: %+v", task)
	}
}

func TestAddTaskLinkReplayReturnsExistingTask(t *testing.T) {
	db, _ := newDB(t)
	id, err := db.AddTask("main", &Task{Title: "from issue", Link: linkOf("github", "acme/widgets", 214)})
	if err != nil || id != "1" {
		t.Fatalf("first add = %s, %v", id, err)
	}
	feedAfterFirst, _ := db.Feed(0)
	replay, err := db.AddTask("main", &Task{Title: "different title", Link: linkOf("github", "acme/widgets", 214)})
	if err != nil {
		t.Fatalf("replay add: %v", err)
	}
	if replay != "1" {
		t.Fatalf("replay id = %s; want 1", replay)
	}
	info, _ := db.Tag("main")
	if info.TaskCount != 1 || info.NextID != 2 {
		t.Fatalf("replay allocated state: %+v", info)
	}
	feed, _ := db.Feed(0)
	if len(feed.Events) != len(feedAfterFirst.Events) {
		t.Fatalf("replay appended events: %+v", feed.Events)
	}
	task, _ := db.GetTask("main", "1")
	if task.Title != "from issue" || task.Revision != 1 {
		t.Fatalf("replay mutated task: %+v", task)
	}
	// A different triple still creates a task.
	other, err := db.AddTask("main", &Task{Title: "second", Link: linkOf("github", "acme/widgets", 215)})
	if err != nil || other != "2" {
		t.Fatalf("distinct add = %s, %v", other, err)
	}
	// An invalid link grammar refuses before any write.
	if _, err := db.AddTask("main", &Task{Title: "bad", Link: &backlog.ExternalLink{Family: "x", Repository: "", Issue: 1}}); err == nil {
		t.Fatal("invalid link add succeeded")
	}
}

func TestAddTaskLinkCrossTagReplayRefuses(t *testing.T) {
	db, _ := newDB(t)
	if _, err := db.AddTask("main", &Task{Title: "holder", Link: linkOf("github", "acme/widgets", 214)}); err != nil {
		t.Fatal(err)
	}
	// A holder in another tag is a typed conflict naming it: add prints
	// one id of its own tag, so a cross-tag replay must not be mistaken
	// for it (SPEC §4).
	if err := db.EnsureTag("other"); err != nil {
		t.Fatal(err)
	}
	_, err := db.AddTask("other", &Task{Title: "wrong tag", Link: linkOf("github", "acme/widgets", 214)})
	var exists *LinkExistsConflict
	if !errors.As(err, &exists) || exists.ExistingTag != "main" || exists.ExistingID != "1" {
		t.Fatalf("cross-tag add = %v; want LinkExistsConflict for main/1", err)
	}
	info, _ := db.Tag("other")
	if info.TaskCount != 0 {
		t.Fatalf("cross-tag add wrote state: %+v", info)
	}
}

func TestSetLinkRefusesNonConformingExtraMember(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "imported")
	// A row whose externalLink member failed §1 parsing keeps it in Extra
	// verbatim (taskFromFields); presence blocks set-link rather than
	// repairing it, or the field would render twice and collide in digests.
	err := db.UpdateTask("main", id, 1, nil, func(task *Task) error {
		if task.Extra == nil {
			task.Extra = map[string]json.RawMessage{}
		}
		task.Extra["externalLink"] = json.RawMessage(`{"family":"github"}`)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = db.SetLink("main", id, *linkOf("github", "acme/widgets", 214), 2, nil)
	var once *LinkSetConflict
	if !errors.As(err, &once) {
		t.Fatalf("set over Extra member = %v; want LinkSetConflict", err)
	}
	if !strings.Contains(err.Error(), "non-conforming") {
		t.Fatalf("conflict message = %q", err.Error())
	}
	current, err := db.GetTask("main", id)
	if err != nil || current.Link != nil || current.Revision != 2 {
		t.Fatalf("refused write persisted: %+v, %v", current, err)
	}
}

func TestLinkColumnsPreservedThroughOtherMutations(t *testing.T) {
	db, _ := newDB(t)
	id := addTask(t, db, "main", "linked")
	if err := db.SetLink("main", id, *linkOf("github", "acme/widgets", 214), 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetTaskMilestone("main", id, "", 2, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetStatus("main", id, "in-progress", 2, nil); err != nil {
		t.Fatal(err)
	}
	task, _ := db.GetTask("main", id)
	if task.Link == nil || task.Link.String() != "github/acme/widgets#214" {
		t.Fatalf("link not preserved: %+v", task.Link)
	}
	// The §1 rendering carries the link and reparses through the
	// subtask-rewrite path (taskFromFields).
	fields, err := task.Fields()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := taskFromFields("main", fields)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Link == nil || parsed.Link.String() != "github/acme/widgets#214" {
		t.Fatalf("taskFromFields lost link: %+v", parsed.Link)
	}
}

// buildV5Store writes a hand-built store at the exact v5 schema: the
// preceding released binary's catalog, no link columns, one task row.
func buildV5Store(t *testing.T) (dir string, path string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "tasks.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(schemaV5DDL); err != nil {
		t.Fatalf("v5 DDL: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version=5`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO tags(name, next_id, task_count, completed_count) VALUES ('main', 2, 1, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`INSERT INTO tasks(tag, id, title, description, details, status, dependencies, subtasks, revision, updated_at)
		VALUES ('main', '1', 'legacy', '', '', 'pending', '[]', '[]', 4, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestV5StoreUpgradesToV6PreservingTasks(t *testing.T) {
	dir, path := buildV5Store(t)
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open v5: %v", err)
	}
	defer db.Close()
	// Reads work before any mutation; the link columns read as NULL.
	task, err := db.GetTask("main", "1")
	if err != nil || task == nil || task.Title != "legacy" || task.Link != nil {
		t.Fatalf("v5 read = %+v, %v", task, err)
	}
	if v, err := db.UserVersion(); err != nil || v != 5 {
		t.Fatalf("user_version before mutation = %d, %v", v, err)
	}
	if err := db.SetOwnerBoundary(dir); err != nil {
		t.Fatal(err)
	}
	// First mutation upgrades transactionally to v6.
	if err := db.SetLink("main", "1", *linkOf("github", "acme/widgets", 1), 4, nil); err != nil {
		t.Fatalf("set-link upgrade mutation: %v", err)
	}
	if v, err := db.UserVersion(); err != nil || v != 6 {
		t.Fatalf("user_version after mutation = %d, %v", v, err)
	}
	task, err = db.GetTask("main", "1")
	if err != nil || task.Title != "legacy" || task.Revision != 5 || task.Link == nil {
		t.Fatalf("upgraded task = %+v, %v", task, err)
	}
}

func TestStoreNewerThanBinaryRefuses(t *testing.T) {
	_, path := buildV5Store(t)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(fmt.Sprintf("PRAGMA user_version=%d", SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := Open(path); !errors.As(err, new(*SchemaNewerError)) {
		t.Fatalf("newer store open = %v; want SchemaNewerError", err)
	}
}
