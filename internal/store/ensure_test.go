package store

import (
	"errors"
	"sync"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// ensureLink is the triple an importer and an agent both race to track.
func ensureLink() backlog.ExternalLink {
	return backlog.ExternalLink{Family: "github", Repository: "acme/widgets", Issue: 214}
}

func feedLen(t *testing.T, db *DB) int {
	t.Helper()
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	return len(feed.Events)
}

func TestEnsureTaskCreateThenReplay(t *testing.T) {
	db, _ := newDB(t)
	link := ensureLink()

	id, created, err := db.EnsureTask("main", &Task{Title: "Fix crash on import", Priority: "medium", Link: &link})
	if err != nil || !created {
		t.Fatalf("first ensure = (%q, %v, %v); want created", id, created, err)
	}
	events := feedLen(t, db)

	// Replay with different caller-supplied text: same id, no write, no
	// counter bump, no event, and the stored task is untouched.
	replayID, created, err := db.EnsureTask("main", &Task{Title: "importer replay title", Link: &link})
	if err != nil || created || replayID != id {
		t.Fatalf("replay ensure = (%q, %v, %v); want (%q, false, nil)", replayID, created, err, id)
	}
	if feedLen(t, db) != events {
		t.Fatalf("replay appended events: %d -> %d", events, feedLen(t, db))
	}
	// The replay must not consume an id: the next plain add draws the
	// counter value immediately after the created task's id.
	next := addTask(t, db, "main", "unrelated")
	if next != "2" {
		t.Fatalf("post-replay add id = %q; want 2 (no counter bump)", next)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "Fix crash on import" || task.Link == nil || task.Link.String() != "github/acme/widgets#214" {
		t.Fatalf("replayed task = %+v", task)
	}
	if task.Status != "pending" || task.Priority != "medium" {
		t.Fatalf("created task status/priority = %q/%q", task.Status, task.Priority)
	}
}

func TestEnsureTaskCrossTagHolderIsTypedConflict(t *testing.T) {
	db, _ := newDB(t)
	if err := db.EnsureTag("import"); err != nil {
		t.Fatal(err)
	}
	link := ensureLink()
	if _, _, err := db.EnsureTask("import", &Task{Title: "importer task", Link: &link}); err != nil {
		t.Fatal(err)
	}
	_, _, err := db.EnsureTask("main", &Task{Title: "agent task", Link: &link})
	var exists *LinkExistsConflict
	if !errors.As(err, &exists) {
		t.Fatalf("cross-tag ensure = %v; want LinkExistsConflict", err)
	}
	if exists.ExistingTag != "import" {
		t.Fatalf("holder tag = %q", exists.ExistingTag)
	}
	if tasks, err := db.ListTasks("main"); err != nil {
		t.Fatal(err)
	} else if len(tasks) != 0 {
		t.Fatalf("cross-tag refusal wrote %d tasks to main", len(tasks))
	}
}

// TestEnsureTaskConcurrentCallersSerializeOnOneTask proves the §3
// serialization: an importer and an agent racing on separate database
// connections produce exactly one task, and every caller observes the
// same id.
func TestEnsureTaskConcurrentCallersSerializeOnOneTask(t *testing.T) {
	db, path := newDB(t)
	peer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peer.Close() })
	link := ensureLink()

	const callers = 8
	ids := make([]string, callers)
	createdCount := 0
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			caller := db
			if i%2 != 0 {
				caller = peer
			}
			id, created, err := caller.EnsureTask("main", &Task{Title: "race", Link: &link})
			if err != nil {
				t.Errorf("concurrent ensure %d: %v", i, err)
				return
			}
			mu.Lock()
			ids[i] = id
			if created {
				createdCount++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for i, id := range ids {
		if id != ids[0] {
			t.Fatalf("caller %d observed id %q; want %q", i, id, ids[0])
		}
	}
	if createdCount != 1 {
		t.Fatalf("created count = %d; want 1", createdCount)
	}
	if tasks, err := db.ListTasks("main"); err != nil {
		t.Fatal(err)
	} else if len(tasks) != 1 {
		t.Fatalf("race produced %d tasks; want 1", len(tasks))
	}
}
