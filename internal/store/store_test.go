package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "noise-tasks", "tasks.sqlite")
	// §8.2 identity is minted at creation exactly as production stores do,
	// so receipt-writing mutations (§8.4) work on every fixture.
	db, err := CreateWithOwner(path, dir)
	if err != nil {
		t.Fatalf("CreateWithOwner: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func addTask(t *testing.T, db *DB, tag, title string) string {
	t.Helper()
	id, err := db.AddTask(tag, &Task{Title: title})
	if err != nil {
		t.Fatalf("AddTask(%q): %v", title, err)
	}
	return id
}

func TestCreateModeAndSchemaVersion(t *testing.T) {
	db, path := newDB(t)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %04o, want 0600", info.Mode().Perm())
	}
	if fencing, err := db.Fencing(); err != nil || fencing != 0 {
		t.Errorf("fencing = %d, %v; want 0", fencing, err)
	}
	// user_version is schema version 1.
	var version int
	if err := db.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
	// WAL mode is active.
	var journal string
	if err := db.db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		t.Fatal(err)
	}
	if journal != "wal" {
		t.Errorf("journal_mode = %q, want wal", journal)
	}
}

func TestCreateRefusesExisting(t *testing.T) {
	_, path := newDB(t)
	if _, err := Create(path); err == nil {
		t.Fatal("Create over an existing database succeeded")
	}
}

func TestOpenMissingIsNotExist(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "noise-tasks", "tasks.sqlite"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open missing: %v, want os.ErrNotExist", err)
	}
}

func TestOpenRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	real, err := Create(filepath.Join(dir, "real.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	real.Close()
	link := filepath.Join(dir, "link.sqlite")
	if err := os.Symlink(real.Path(), link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link); err == nil {
		t.Fatal("Open on a symlink succeeded")
	}
}

func TestOpenRefusesDirectory(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("Open on a directory succeeded")
	}
}

func TestOpenCorruptFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	if err := os.WriteFile(path, []byte("this is not a sqlite database at all, definitely not"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(path)
	var notDB *NotSQLiteError
	if !errors.As(err, &notDB) {
		t.Fatalf("Open corrupt: %v, want NotSQLiteError", err)
	}
	want := "noise-tasks: task store is not a valid SQLite database: " + path
	if err.Error() != want {
		t.Errorf("diagnostic = %q, want %q", err.Error(), want)
	}
}

func TestOpenTruncatedFailsClosed(t *testing.T) {
	db, path := newDB(t)
	addTask(t, db, "main", "seed")
	db.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A truncated file that still carries the SQLite header reads as corrupt.
	if err := os.WriteFile(path, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Open(path)
	var notDB *NotSQLiteError
	if !errors.As(err, &notDB) {
		t.Fatalf("Open truncated: %v, want NotSQLiteError", err)
	}
}

func TestOpenFutureSchemaFailsClosed(t *testing.T) {
	db, path := newDB(t)
	if _, err := db.db.Exec(`PRAGMA user_version=99`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_, err := Open(path)
	var newer *SchemaNewerError
	if !errors.As(err, &newer) {
		t.Fatalf("Open future schema: %v, want SchemaNewerError", err)
	}
	if err.Error() != SchemaNewerDiagnostic {
		t.Errorf("diagnostic = %q, want %q", err.Error(), SchemaNewerDiagnostic)
	}
}

func TestOpenPermissionFailureFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	db, path := newDB(t)
	db.Close()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(path, 0o600) })
	if _, err := Open(path); err == nil {
		t.Fatal("Open on a mode-0000 database succeeded")
	}
}

func TestPathDerivation(t *testing.T) {
	// Outside Git: <boundary>/.tasks/tasks.sqlite.
	outside := t.TempDir()
	path, inGit := Path(outside)
	if inGit {
		t.Error("outside-Git boundary reported inGit")
	}
	if want := filepath.Join(outside, ".tasks", "tasks.sqlite"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	// Inside Git: <common>/noise-tasks/tasks.sqlite.
	inside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(inside, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	path, inGit = Path(inside)
	if !inGit {
		t.Error("git boundary reported outside Git")
	}
	if want := filepath.Join(inside, ".git", "noise-tasks", "tasks.sqlite"); path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
}

func TestTaskFieldsRoundTripUnknownFields(t *testing.T) {
	db, _ := newDB(t)
	id, err := db.AddTask("main", &Task{
		Title:    "with extras",
		Priority: "high",
		Extra: map[string]json.RawMessage{
			"zeta":  json.RawMessage(`{"nested":[1,2]}`),
			"alpha": json.RawMessage(`"kept"`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", id)
	if err != nil {
		t.Fatal(err)
	}
	fields, err := task.Fields()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(fields, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["zeta"]) != `{"nested":[1,2]}` || string(decoded["alpha"]) != `"kept"` {
		t.Errorf("unknown fields not preserved: %s", fields)
	}
	// An absent priority stays absent (SPEC §1).
	id2 := addTask(t, db, "main", "no priority")
	task2, err := db.GetTask("main", id2)
	if err != nil {
		t.Fatal(err)
	}
	fields2, err := task2.Fields()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(fields2), `"priority"`) {
		t.Errorf("absent priority was materialized: %s", fields2)
	}
}
