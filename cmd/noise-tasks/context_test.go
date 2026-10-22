package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// writeCutoverReceipt plants a historic §8.6 receipt in the store directory.
func writeCutoverReceipt(t *testing.T, storeDir, phase string) {
	t.Helper()
	receipt := `{"version":1,"binary":"test","schemaVersion":1,"phase":` + `"` + phase + `"` +
		`,"sourceDigest":"` + strings.Repeat("0", 64) +
		`","inventory":{"tags":0,"tasks":0,"milestones":0,"claims":0,"bindings":0,"events":0},` +
		`"startedAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "sqlite-migration.json"), []byte(receipt), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeRecoveryJournal plants a strict-valid §8.6 apply journal in the store
// directory. When body is empty a valid journal for the family is written.
func writeRecoveryJournal(t *testing.T, storeDir, body string) {
	writeRecoveryJournalTarget(t, storeDir, body, store.SchemaVersion)
}

func writeRecoveryJournalTarget(t *testing.T, storeDir, body string, targetSchema int) {
	t.Helper()
	if body == "" {
		var err error
		var jb []byte
		jb, err = store.JournalBytes(&store.MigrationJournal{
			SchemaVersion:          1,
			RequestID:              "recover1",
			FamilyID:               "01234567-89ab-cdef-0123-456789abcdef",
			PlanDigest:             "sha256:" + strings.Repeat("0", 64),
			SourceComparisonDigest: "sha256:" + strings.Repeat("1", 64),
			BackupPayloadDigest:    "sha256:" + strings.Repeat("2", 64),
			TargetSchemaVersion:    targetSchema,
		})
		if err != nil {
			t.Fatal(err)
		}
		body = string(jb)
	}
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "migration-apply.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runContext(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cmdContext(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func decodeContext(t *testing.T, stdout string) contextResult {
	t.Helper()
	var got contextResult
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("decode context JSON: %v\n%s", err, stdout)
	}
	return got
}

func TestContextCommandJSONAndUsage(t *testing.T) {
	project := t.TempDir()
	if err := os.Mkdir(filepath.Join(project, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOISE_PROJECT_DIR", project)

	code, stdout, stderr := runContext(t, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeContext(t, stdout)
	wantStore := projectStorePath(project)
	if got.SchemaVersion != 5 || got.Start != project || got.ProjectBoundary != project ||
		got.StorePath != wantStore || got.DocsPath != ".tasks/docs/PRD.md" ||
		got.PreferencesPath != ".tasks/preferences.json" || got.Initialized ||
		got.StoreOwnerBoundary != "" || got.StoreSchemaVersion != 0 ||
		got.EngineSchemaVersion != store.SchemaVersion ||
		!got.Writable || got.Blocked || got.BlockedReason != "" {
		t.Fatalf("context = %+v", got)
	}

	for _, args := range [][]string{nil, {"--file", "tasks.json", "--json"}, {"--json", "extra"}, {"--tag", "main", "--json"}} {
		code, stdout, stderr := runContext(t, args...)
		if code != 2 || stdout != "" || stderr == "" {
			t.Errorf("args %q = code %d stdout %q stderr %q", args, code, stdout, stderr)
		}
	}
}

func TestContextCommandInitializedStore(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, stdout, stderr := runContext(t, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeContext(t, stdout)
	if !got.Initialized || !got.Writable || got.Blocked {
		t.Fatalf("context = %+v", got)
	}
	if got.StoreOwnerBoundary != project ||
		got.StoreSchemaVersion != store.SchemaVersion ||
		got.EngineSchemaVersion != store.SchemaVersion {
		t.Fatalf("store facts = %+v", got)
	}
}

func TestContextCommandBlockedStates(t *testing.T) {
	// A strict-valid journal with no published database is exactly
	// migration-recovery-required; beside a published database publication
	// completed and the project is not blocked; a malformed journal is an
	// operational failure (exit 1, empty stdout).
	t.Run("valid journal without store", func(t *testing.T) {
		project := setupProject(t)
		writeRecoveryJournal(t, filepath.Join(project, ".git", "noise-tasks"), "")
		code, stdout, stderr := runContext(t, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		got := decodeContext(t, stdout)
		if !got.Blocked || got.BlockedReason != "migration-recovery-required" || got.Writable || got.SchemaVersion != 5 {
			t.Fatalf("context = %+v", got)
		}
	})
	t.Run("valid journal with store", func(t *testing.T) {
		project := setupProject(t)
		writeRecoveryJournal(t, filepath.Join(project, ".git", "noise-tasks"), "")
		db, err := store.CreateWithOwner(projectStorePath(project), project)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		code, stdout, stderr := runContext(t, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		got := decodeContext(t, stdout)
		if got.Blocked || got.BlockedReason != "" || !got.Writable {
			t.Fatalf("context = %+v", got)
		}
	})

	t.Run("older target journal with store", func(t *testing.T) {
		project := setupProject(t)
		db, err := store.CreateWithOwner(projectStorePath(project), project)
		if err != nil {
			t.Fatal(err)
		}
		db.Close()
		writeRecoveryJournalTarget(t, filepath.Join(project, ".git", "noise-tasks"), "", 3)
		code, stdout, stderr := runContext(t, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		got := decodeContext(t, stdout)
		if !got.Blocked || got.BlockedReason != "migration-recovery-required" || got.Writable {
			t.Fatalf("older target recovery context = %+v", got)
		}
		code, stdout, stderr = runCmd(cmdList)
		if code != 1 || stdout != "" || !strings.Contains(stderr, "migration recovery required") {
			t.Fatalf("older target normal command = code %d stdout %q stderr %q", code, stdout, stderr)
		}
	})
	t.Run("malformed journal", func(t *testing.T) {
		project := setupProject(t)
		writeRecoveryJournal(t, filepath.Join(project, ".git", "noise-tasks"), "not json")
		code, stdout, stderr := runContext(t, "--json")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid migration recovery journal") {
			t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
		}
	})
	t.Run("historic migration receipt inert", func(t *testing.T) {
		project := setupProject(t)
		writeCutoverReceipt(t, filepath.Join(project, ".git", "noise-tasks"), "migrating")
		code, stdout, stderr := runContext(t, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		got := decodeContext(t, stdout)
		if got.Blocked || got.BlockedReason != "" {
			t.Fatalf("context = %+v", got)
		}
	})
}

func TestContextCommandRetiredLayoutsAreInert(t *testing.T) {
	project := setupProject(t)
	for _, relative := range []string{".tasks/tasks/tasks.json", ".taskmaster/tasks/tasks.json"} {
		path := filepath.Join(project, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not parsed"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	code, stdout, stderr := runContext(t, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeContext(t, stdout)
	if got.Blocked || got.Initialized {
		t.Fatalf("context = %+v", got)
	}
}

func TestContextCommandDoesNotEchoInvalidEnvironmentValue(t *testing.T) {
	const privateValue = "relative-private-project-value"
	t.Setenv("NOISE_PROJECT_DIR", privateValue)
	code, stdout, stderr := runContext(t, "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "project context unavailable") || strings.Contains(stderr, privateValue) {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// setStoreUserVersion rewrites the store's PRAGMA user_version for the
// §2.2 schema-5 store-fact tests.
func setStoreUserVersion(t *testing.T, project string, version int) {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(project)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(fmt.Sprintf("PRAGMA user_version=%d", version)); err != nil {
		t.Fatal(err)
	}
}

func TestContextCommandUnreadableStoreFailsClosed(t *testing.T) {

	project := setupProject(t)
	if err := os.MkdirAll(filepath.Dir(projectStorePath(project)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectStorePath(project), []byte("not a database at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runContext(t, "--json")
	if code != 1 || stdout != "" || stderr == "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestContextCommandSchemaNewerStoreRefused(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	setStoreUserVersion(t, project, store.SchemaVersion+1)
	code, stdout, stderr := runContext(t, "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, store.SchemaNewerDiagnostic) {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestContextCommandOlderStoreReportsObservedVersion(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	setStoreUserVersion(t, project, store.SchemaVersion-1)
	code, stdout, stderr := runContext(t, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	got := decodeContext(t, stdout)
	if !got.Initialized || got.StoreOwnerBoundary != project ||
		got.StoreSchemaVersion != store.SchemaVersion-1 ||
		got.EngineSchemaVersion != store.SchemaVersion {
		t.Fatalf("store facts = %+v", got)
	}
}

func TestContextCommandJSONFieldOrderMatchesSpec(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	code, stdout, stderr := runContext(t, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	if _, err := dec.Token(); err != nil {
		t.Fatal(err)
	}
	var got []string
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, fmt.Sprint(key))
		if _, err := dec.Token(); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"schemaVersion", "start", "projectBoundary", "storePath",
		"storeOwnerBoundary", "storeSchemaVersion", "engineSchemaVersion",
		"docsPath", "preferencesPath", "initialized", "writable", "blocked", "blockedReason"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("field order = %q, want %q", got, want)
	}
}

func TestContextCommandUnreleasedSchemaStoreFailsClosed(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	setStoreUserVersion(t, project, 0)
	code, stdout, stderr := runContext(t, "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not initialized with a released schema") {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestContextCommandDoesNotUpgradeOlderStore(t *testing.T) {
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	setStoreUserVersion(t, project, store.SchemaVersion-1)
	code, stdout, stderr := runContext(t, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(project)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	var version int
	if err := sqlDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != store.SchemaVersion-1 {
		t.Fatalf("user_version = %d, want the unchanged %d", version, store.SchemaVersion-1)
	}
}

func TestContextCommandReadOnlyStoreDirectoryFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	storeDir := filepath.Dir(projectStorePath(project))
	if err := os.Chmod(storeDir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(storeDir, 0o755)
	code, stdout, stderr := runContext(t, "--json")
	if code != 1 || stdout != "" || stderr == "" {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}
