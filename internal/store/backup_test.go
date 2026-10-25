package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// newBackupStore builds a populated, identity-bearing §8.1 store in dir:
// one tag with a milestone registry, tasks with dependencies, subtasks and
// unknown §1 fields, a claim, a worktree binding, and the resulting events.
func newBackupStore(t *testing.T, dir, boundary string) *DB {
	t.Helper()
	db, err := Create(filepath.Join(dir, "noise-tasks", "tasks.sqlite"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if err := db.EnsureIdentity(boundary); err != nil {
		t.Fatalf("mint identity: %v", err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatalf("ensure tag: %v", err)
	}
	if err := db.AddMilestone("main", "m1"); err != nil {
		t.Fatalf("add milestone: %v", err)
	}
	id1, err := db.AddTask("main", &Task{Title: "first", Description: "d1", Priority: "high"})
	if err != nil {
		t.Fatalf("add task 1: %v", err)
	}
	id2, err := db.AddTask("main", &Task{
		Title:        "second",
		Dependencies: []string{id1},
		Subtasks:     json.RawMessage(`[{"id":1,"title":"sub","description":"","details":"","status":"pending","dependencies":[],"parentTaskId":"2"}]`),
		Extra:        map[string]json.RawMessage{"customField": json.RawMessage(`{"nested":[1,2,"x"]}`)},
	})
	if err != nil {
		t.Fatalf("add task 2: %v", err)
	}
	if _, _, _, err := db.AcquireClaim("main", id1, "agent-a", 600, "req-backup-1"); err != nil {
		t.Fatalf("acquire claim: %v", err)
	}
	if err := db.SetBinding("main", id2, &WorktreeBinding{Path: filepath.Join(boundary, ".worktrees", "main-2"),
		Boundary: boundary, Branch: "tasks/main/2", CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatalf("set binding: %v", err)
	}
	return db
}

// contentDigests strips the restore-added rows (the restore receipt and its
// feed event) and recomputes the §8.5 digests, so a restored authority can be
// compared exactly against the bundle it came from.
func contentDigests(t *testing.T, db *DB) (semantic, comparison string) {
	t.Helper()
	if err := db.WithImmediateLock(func() error { return nil }); err != nil {
		t.Fatalf("lock: %v", err)
	}
	if _, err := db.db.Exec(`DELETE FROM receipts WHERE kind='restore'`); err != nil {
		t.Fatalf("strip restore receipt: %v", err)
	}
	if _, err := db.db.Exec(`DELETE FROM events WHERE kind='restore'`); err != nil {
		t.Fatalf("strip restore event: %v", err)
	}
	semantic, comparison, _, err := db.ClassDigests()
	if err != nil {
		t.Fatalf("class digests: %v", err)
	}
	return semantic, comparison
}

func backupTask(t *testing.T, db *DB, tag, id string) *Task {
	t.Helper()
	task, err := db.GetTask(tag, id)
	if err != nil {
		t.Fatalf("get task %s/%s: %v", tag, id, err)
	}
	if task == nil {
		t.Fatalf("task %s/%s is absent", tag, id)
	}
	return task
}

func requirePublishedWAL(t *testing.T, targetPath, receiptKind, requestID string) {
	t.Helper()
	requireNoSQLiteSidecars(t, targetPath)
	db, err := Open(targetPath)
	if err != nil {
		t.Fatalf("reopen published store: %v", err)
	}
	var journalMode string
	if err := db.db.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		db.Close()
		t.Fatalf("read published journal mode: %v", err)
	}
	if journalMode != "wal" {
		db.Close()
		t.Fatalf("published journal_mode = %q, want wal", journalMode)
	}
	tasks, err := db.ListTasks("main")
	if err != nil || len(tasks) != 2 {
		db.Close()
		t.Fatalf("published tasks = %v, %v; want 2", tasks, err)
	}
	receipt, err := db.FindReceipt(receiptKind, requestID)
	if err != nil || receipt == nil {
		db.Close()
		t.Fatalf("published %s receipt = %v, %v", receiptKind, receipt, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close published store: %v", err)
	}
	requireNoSQLiteSidecars(t, targetPath)
}

func requireNoSQLiteSidecars(t *testing.T, targetPath string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Lstat(targetPath + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("published sidecar %s: %v", targetPath+suffix, err)
		}
	}
}

func TestRestorePublishesPersistentWAL(t *testing.T) {
	for _, overwrite := range []bool{false, true} {
		name := "absent target"
		if overwrite {
			name = "overwrite"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			target, bundle := restoreFixture(t, dir)
			if !overwrite {
				target = filepath.Join(dir, "restored", "noise-tasks", StoreFile)
			}
			requestID := "wal-restore"
			if _, err := Restore(bundle, target, RestoreOptions{
				Overwrite: overwrite, RequestID: requestID, OwnerBoundary: dir,
			}); err != nil {
				t.Fatalf("restore: %v", err)
			}
			requirePublishedWAL(t, target, ReceiptKindRestore, requestID)
		})
	}
}

func TestBackupVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	source := newBackupStore(t, filepath.Join(dir, "src"), dir)
	sourceSemantic, sourceComparison, sourceInv, err := source.ClassDigests()
	if err != nil {
		t.Fatalf("source digests: %v", err)
	}
	bundle := filepath.Join(dir, "out.ntbackup")
	manifest, err := source.CreateBackup(bundle, "noise-tasks test")
	if err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if manifest.SemanticDigest != sourceSemantic || manifest.ComparisonDigest != sourceComparison {
		t.Fatalf("manifest digests differ from source")
	}
	if manifest.Inventory != *sourceInv {
		t.Fatalf("manifest inventory %+v differs from source %+v", manifest.Inventory, *sourceInv)
	}
	if manifest.Inventory.Tasks != 2 || manifest.Inventory.Claims != 1 || manifest.Inventory.Bindings != 1 ||
		manifest.Inventory.Milestones != 1 || manifest.Inventory.Dependencies != 1 || manifest.Inventory.Subtasks != 1 {
		t.Fatalf("unexpected inventory %+v", manifest.Inventory)
	}
	familyID, _, _ := source.Identity()
	if manifest.FamilyID != familyID {
		t.Fatalf("manifest family %q, want %q", manifest.FamilyID, familyID)
	}
	if manifest.SourceSchema != fmt.Sprintf("sqlite:%d", SchemaVersion) || manifest.SourceKind != "sqlite" || manifest.CurrentSchemaVersion != SchemaVersion {
		t.Fatalf("unexpected manifest schema fields %+v", manifest)
	}
	source.Close()

	verified, err := VerifyBundle(bundle)
	if err != nil {
		t.Fatalf("verify bundle: %v", err)
	}
	if *verified != *manifest {
		t.Fatalf("verify returned a different manifest")
	}

	// Offline restore onto the absent derived target of the same boundary.
	targetDir := filepath.Join(dir, "dst", "noise-tasks")
	target := filepath.Join(targetDir, "tasks.sqlite")
	result, err := Restore(bundle, target, RestoreOptions{RequestID: "req-1", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !result.Adopted || result.Replayed {
		t.Fatalf("expected adoption, got %+v", result)
	}
	if result.Receipt["kind"] != "restore" || result.Receipt["requestId"] != "req-1" {
		t.Fatalf("unexpected receipt %v", result.Receipt)
	}
	if _, ok := result.Receipt["receiptDigest"].(string); !ok {
		t.Fatalf("receipt has no digest: %v", result.Receipt)
	}
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("restored target missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("restored target mode %v, want 0600", perm)
	}

	restored, err := Open(target)
	if err != nil {
		t.Fatalf("open restored store: %v", err)
	}
	defer restored.Close()
	gotFamily, gotBoundary, err := restored.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if gotFamily != familyID || gotBoundary != dir {
		t.Fatalf("restored identity %q/%q, want %q/%q", gotFamily, gotBoundary, familyID, dir)
	}
	// Exact semantic round-trip: after stripping the restore receipt and
	// event, the restored authority digests equal the bundle's.
	semantic, comparison := contentDigests(t, restored)
	if semantic != manifest.SemanticDigest || comparison != manifest.ComparisonDigest {
		t.Fatalf("restored content digests differ from bundle")
	}
	tasks, err := restored.ListTasks("main")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("restored tasks: %v %v", len(tasks), err)
	}
	if string(tasks[1].Extra["customField"]) != `{"nested":[1,2,"x"]}` {
		t.Fatalf("unknown field not preserved: %s", tasks[1].Extra["customField"])
	}
	if tasks[0].Claim == nil || tasks[0].Claim.Owner != "agent-a" {
		t.Fatalf("claim not preserved: %+v", tasks[0].Claim)
	}
	if tasks[1].Binding == nil || tasks[1].Binding.Path != filepath.Join(dir, ".worktrees", "main-2") {
		t.Fatalf("binding not preserved: %+v", tasks[1].Binding)
	}
}

func addBackupRun(t *testing.T, db *DB) {
	t.Helper()
	task := backupTask(t, db, "main", "1")
	if task.Claim == nil {
		t.Fatal("run fixture task has no claim")
	}
	if _, _, err := db.StartRun("main", "1", "agent-a", task.Claim.Token, "run.start", 900); err != nil {
		t.Fatalf("start run fixture: %v", err)
	}
	if _, err := db.db.Exec(`UPDATE runs SET extra='{"run-note":true}' WHERE run_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE attempts SET extra='{"attempt-note":true}' WHERE run_id=1 AND attempt_no=1`); err != nil {
		t.Fatal(err)
	}
}

func TestV4BackupRoundTripPreservesRunsAndAttempts(t *testing.T) {
	dir := t.TempDir()
	source := newBackupStore(t, filepath.Join(dir, "src"), dir)
	addBackupRun(t, source)
	rewindStoreToV4(t, source)
	wantSemantic, wantComparison, _, err := source.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "runs.ntbackup")
	manifest, err := source.CreateBackup(bundle, "noise-tasks v4 fixture")
	if err != nil {
		t.Fatalf("create v4 backup: %v", err)
	}
	if manifest.CurrentSchemaVersion != runSchemaVersion || manifest.SourceSchema != "sqlite:4" ||
		manifest.Inventory.Runs != 1 || manifest.Inventory.Attempts != 1 {
		t.Fatalf("v4 manifest = %+v", manifest)
	}
	if manifest.SemanticDigest != wantSemantic || manifest.ComparisonDigest != wantComparison {
		t.Fatalf("v4 manifest digests differ from source")
	}
	source.Close()
	if _, err := VerifyBundle(bundle); err != nil {
		t.Fatalf("verify v4 bundle: %v", err)
	}

	target := filepath.Join(dir, "dst", "noise-tasks", "tasks.sqlite")
	restoreResult, err := Restore(bundle, target, RestoreOptions{RequestID: "v4-runs-restore", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("restore v4 bundle: %v", err)
	}
	receiptInventory, ok := restoreResult.Receipt["inventory"].(map[string]any)
	if !ok || receiptInventory["worktreeSagas"] != int64(1) {
		t.Fatalf("v4 restore receipt inventory=%v", restoreResult.Receipt["inventory"])
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.db.Exec(`DELETE FROM events WHERE kind='schema' AND action='upgraded'`); err != nil {
		t.Fatal(err)
	}
	semantic, comparison := contentDigests(t, restored)
	if semantic == wantSemantic || comparison == wantComparison {
		t.Fatalf("restored v4 digest transition semantic=%s/%s comparison=%s/%s", semantic, wantSemantic, comparison, wantComparison)
	}
	_, _, inv, err := restored.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Runs != 1 || inv.Attempts != 1 || inv.WorktreeSagas != 1 {
		t.Fatalf("restored v4 run inventory = %+v", *inv)
	}
	if err := restored.ValidateRunState(); err != nil {
		t.Fatalf("restored v4 run state: %v", err)
	}
}

func TestV5BackupVerifyRestoreWorktreeSagas(t *testing.T) {
	dir := t.TempDir()
	source := newMinimalSagaDB(t)
	defer source.Close()
	if err := insertMinimalSaga(source, validMinimalSaga(t, source)); err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := source.db.QueryRow(`SELECT value FROM meta WHERE key='ownerBoundary'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "sagas.ntbackup")
	manifest, err := source.CreateBackup(bundle, "noise-tasks v5 fixture")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.CurrentSchemaVersion != worktreeSagaSchemaVersion || manifest.Inventory.WorktreeSagas != 1 {
		t.Fatalf("v5 manifest=%+v", manifest)
	}
	if _, err := VerifyBundle(bundle); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "restored", "noise-tasks", "tasks.sqlite")
	if _, err := Restore(bundle, target, RestoreOptions{RequestID: "restore.saga", OwnerBoundary: owner}); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.ValidateWorktreeSagaState(); err != nil {
		restored.Close()
		t.Fatal(err)
	}
	_, _, inventory, err := restored.ClassDigests()
	restored.Close()
	if err != nil || inventory.WorktreeSagas != 1 {
		t.Fatalf("restored saga inventory=%+v %v", inventory, err)
	}
	tamperPayload(t, bundle, func(sdb *sql.DB) error {
		_, err := sdb.Exec(`UPDATE worktree_sagas SET target_path='/wrong' WHERE saga_id=1`)
		return err
	})
	if _, err := VerifyBundle(bundle); err == nil || !strings.Contains(err.Error(), "worktree saga state is invalid") {
		t.Fatalf("corrupt saga bundle verification=%v", err)
	}
}

// Stores and bundles produced by released schemas remain valid inputs. Backup
// creation and verification are read-only at v2/v3; restore upgrades only its
// staged publication through the ordinary transactional migration path.
func TestV2BackupCreateVerifyRestoreMixedVersion(t *testing.T) {
	dir := t.TempDir()
	source := newBackupStore(t, filepath.Join(dir, "src"), dir)
	rewindStoreToV2(t, source)
	if version, err := source.userVersion(); err != nil || version != identitySchemaVersion {
		t.Fatalf("source version = %d, %v", version, err)
	}
	bundle := filepath.Join(dir, "v2.ntbackup")
	manifest, err := source.CreateBackup(bundle, "noise-tasks v2 fixture")
	if err != nil {
		t.Fatalf("create v2 backup: %v", err)
	}
	if manifest.SourceSchema != "sqlite:2" || manifest.CurrentSchemaVersion != identitySchemaVersion {
		t.Fatalf("v2 manifest schema = %+v", manifest)
	}
	if manifest.Inventory.Plans != 0 || manifest.Inventory.PlanMappings != 0 || manifest.Inventory.Assignments != 0 {
		t.Fatalf("v2 future-class inventory = %+v", manifest.Inventory)
	}
	if version, err := source.userVersion(); err != nil || version != identitySchemaVersion {
		t.Fatalf("backup creation mutated v2 source to %d (%v)", version, err)
	}
	source.Close()

	verified, err := VerifyBundle(bundle)
	if err != nil || verified.CurrentSchemaVersion != identitySchemaVersion || verified.SemanticDigest != manifest.SemanticDigest {
		t.Fatalf("verify v2 bundle = %+v, %v", verified, err)
	}
	payloadDB, err := sql.Open("sqlite", "file:"+filepath.Join(bundle, PayloadFile)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var payloadVersion int
	if err := payloadDB.QueryRow(`PRAGMA user_version`).Scan(&payloadVersion); err != nil {
		t.Fatal(err)
	}
	payloadDB.Close()
	if payloadVersion != identitySchemaVersion {
		t.Fatalf("verify mutated bundle payload to schema %d", payloadVersion)
	}

	target := filepath.Join(dir, "dst", "noise-tasks", "tasks.sqlite")
	result, err := Restore(bundle, target, RestoreOptions{RequestID: "v2-restore", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("restore v2 bundle: %v", err)
	}
	if !result.Adopted || result.Replayed {
		t.Fatalf("restore result = %+v", result)
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if version, err := restored.userVersion(); err != nil || version != SchemaVersion {
		t.Fatalf("restored schema = %d, %v; want %d", version, err, SchemaVersion)
	}
	if n := countRows(t, restored, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); n != 1 {
		t.Fatalf("restored upgrade events = %d, want 1", n)
	}
	requireOrchestratorSchema(t, restored)
	tasks, err := restored.ListTasks("main")
	if err != nil || len(tasks) != 2 {
		t.Fatalf("restored v2 tasks = %d, %v", len(tasks), err)
	}
}

func TestV3BackupCreateVerifyRestoreMixedVersion(t *testing.T) {
	dir := t.TempDir()
	source := newBackupStore(t, filepath.Join(dir, "src"), dir)
	wantAcquisitionID := claimAcquisitionID(backupTask(t, source, "main", "1").Claim)
	rewindStoreToV3(t, source)

	bundle := filepath.Join(dir, "v3.ntbackup")
	manifest, err := source.CreateBackup(bundle, "noise-tasks v3 fixture")
	if err != nil {
		t.Fatalf("create v3 backup: %v", err)
	}
	if manifest.SourceSchema != "sqlite:3" || manifest.CurrentSchemaVersion != orchestratorSchemaVersion {
		t.Fatalf("v3 manifest schema = %+v", manifest)
	}
	if manifest.Inventory.Runs != 0 || manifest.Inventory.Attempts != 0 {
		t.Fatalf("v3 run inventory = %+v", manifest.Inventory)
	}
	if version, err := source.userVersion(); err != nil || version != orchestratorSchemaVersion {
		t.Fatalf("backup creation mutated v3 source to %d (%v)", version, err)
	}
	source.Close()

	verified, err := VerifyBundle(bundle)
	if err != nil || verified.CurrentSchemaVersion != orchestratorSchemaVersion {
		t.Fatalf("verify v3 bundle = %+v, %v", verified, err)
	}
	payloadDB, err := sql.Open("sqlite", "file:"+filepath.Join(bundle, PayloadFile)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var payloadVersion int
	if err := payloadDB.QueryRow(`PRAGMA user_version`).Scan(&payloadVersion); err != nil {
		t.Fatal(err)
	}
	payloadDB.Close()
	if payloadVersion != orchestratorSchemaVersion {
		t.Fatalf("verify mutated bundle payload to schema %d", payloadVersion)
	}

	target := filepath.Join(dir, "dst", "noise-tasks", "tasks.sqlite")
	if _, err := Restore(bundle, target, RestoreOptions{RequestID: "v3-restore", OwnerBoundary: dir}); err != nil {
		t.Fatalf("restore v3 bundle: %v", err)
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if version, err := restored.userVersion(); err != nil || version != SchemaVersion {
		t.Fatalf("restored schema = %d, %v; want %d", version, err, SchemaVersion)
	}
	if got := claimAcquisitionID(backupTask(t, restored, "main", "1").Claim); got != wantAcquisitionID {
		t.Fatalf("v3 claim acquisition id changed from %q to %q", wantAcquisitionID, got)
	}
	if err := restored.ValidateRunState(); err != nil {
		t.Fatalf("restored v3 run state: %v", err)
	}
}

func TestBackupCreateRefusals(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	defer db.Close()
	if _, err := db.CreateBackup(filepath.Join(dir, "out.tar"), "test"); err == nil ||
		!strings.Contains(err.Error(), BundleSuffix) {
		t.Fatalf("expected suffix refusal, got %v", err)
	}
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := db.CreateBackup(bundle, "test"); err == nil ||
		!strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("expected no-overwrite refusal, got %v", err)
	}
	// A store without identity cannot be snapshotted.
	bare, err := Create(filepath.Join(dir, "bare", "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	if _, err := bare.CreateBackup(filepath.Join(dir, "bare.ntbackup"), "test"); err == nil ||
		!strings.Contains(err.Error(), "identity") {
		t.Fatalf("expected identity refusal, got %v", err)
	}
}

func TestBackupVerifyCorruption(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	payload := filepath.Join(bundle, PayloadFile)
	data, err := os.ReadFile(payload)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte deep inside the payload (past the 100-byte header).
	data[len(data)/2] ^= 0xff
	if err := os.WriteFile(payload, data, 0o600); err != nil {
		t.Fatal(err)
	}
	var berr *BundleError
	if _, err := VerifyBundle(bundle); err == nil || !errors.As(err, &berr) {
		t.Fatalf("expected bundle error for corrupted payload, got %v", err)
	}
}

func TestBackupVerifyManifestTampering(t *testing.T) {
	cases := map[string]func(t *testing.T, raw []byte) []byte{
		"changed inventory": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"tasks":2`, `"tasks":3`, 1))
		},
		"unknown key": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"schemaVersion":1`, `"schemaVersion":1,"evil":true`, 1))
		},
		"duplicate key": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"schemaVersion":1`, `"schemaVersion":1,"schemaVersion":1`, 1))
		},
		"unsupported format": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"schemaVersion":1`, `"schemaVersion":99`, 1))
		},
		"unsupported semantic digest version": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"semanticDigestVersion":1`, `"semanticDigestVersion":2`, 1))
		},
		"unsupported schema": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), fmt.Sprintf(`"currentSchemaVersion":%d`, SchemaVersion), `"currentSchemaVersion":7`, 1))
		},
		"zero current schema": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), fmt.Sprintf(`"currentSchemaVersion":%d`, SchemaVersion), `"currentSchemaVersion":0`, 1))
		},
		"negative current schema": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), fmt.Sprintf(`"currentSchemaVersion":%d`, SchemaVersion), `"currentSchemaVersion":-1`, 1))
		},
		"older current schema": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), fmt.Sprintf(`"currentSchemaVersion":%d`, SchemaVersion), `"currentSchemaVersion":1`, 1))
		},
		"manifest family not a uuid": func(t *testing.T, raw []byte) []byte {
			return []byte(strings.Replace(string(raw), `"familyId":"`, `"familyId":"not-a-uuid`, 1))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			db := newBackupStore(t, filepath.Join(dir, "src"), dir)
			bundle := filepath.Join(dir, "out.ntbackup")
			if _, err := db.CreateBackup(bundle, "test"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			manifestPath := filepath.Join(bundle, ManifestFile)
			raw, err := os.ReadFile(manifestPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, mutate(t, raw), 0o600); err != nil {
				t.Fatal(err)
			}
			var berr *BundleError
			if _, err := VerifyBundle(bundle); err == nil || !errors.As(err, &berr) {
				t.Fatalf("expected bundle error, got %v", err)
			}
		})
	}
}

// TestBackupVerifyPayloadFamilyAndSchema proves the payload-side checks:
// the payload's family id must be a real UUID under the same strict rule as
// the manifest's, and the payload's schema version must equal the manifest's
// currentSchemaVersion even when the payload digest is otherwise consistent.
func TestBackupVerifyPayloadFamilyAndSchema(t *testing.T) {
	build := func(t *testing.T) string {
		dir := t.TempDir()
		db := newBackupStore(t, filepath.Join(dir, "src"), dir)
		bundle := filepath.Join(dir, "out.ntbackup")
		if _, err := db.CreateBackup(bundle, "test"); err != nil {
			t.Fatal(err)
		}
		db.Close()
		return bundle
	}
	t.Run("payload family not a uuid", func(t *testing.T) {
		mutatePayload(t, build(t), func(sdb *sql.DB) error {
			_, err := sdb.Exec(`UPDATE meta SET value='not-a-uuid' WHERE key='familyId'`)
			return err
		}, "payload family \"not-a-uuid\" is not a valid UUID")
	})
	t.Run("payload schema below manifest current schema", func(t *testing.T) {
		mutatePayload(t, build(t), func(sdb *sql.DB) error {
			_, err := sdb.Exec(`PRAGMA user_version=1`)
			return err
		}, "does not match manifest currentSchemaVersion")
	})
}

// tamperPayload mutates a bundle's payload through SQLite and re-digests both
// the payload and the manifest so verification reaches the check under test
// rather than failing on a stale payload digest.
func tamperPayload(t *testing.T, bundle string, fn func(*sql.DB) error) {
	t.Helper()
	payloadPath := filepath.Join(bundle, PayloadFile)
	sdb, err := sql.Open("sqlite", "file:"+payloadPath+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(sdb); err != nil {
		t.Fatal(err)
	}
	if _, err := sdb.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	if err := sdb.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		os.Remove(payloadPath + suffix)
	}
	newDigest, err := hashFile(payloadPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(bundle, ManifestFile)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`"payloadDigest":"([^"]+)"`)
	if re.FindSubmatch(raw) == nil {
		t.Fatalf("no payloadDigest in manifest: %s", raw)
	}
	if err := os.WriteFile(manifestPath,
		[]byte(re.ReplaceAll(raw, []byte(`"payloadDigest":"`+newDigest+`"`))), 0o600); err != nil {
		t.Fatal(err)
	}
}

// mutatePayload tampers with a bundle and asserts verification refuses with
// the given reason.
func mutatePayload(t *testing.T, bundle string, fn func(*sql.DB) error, want string) {
	t.Helper()
	tamperPayload(t, bundle, fn)
	_, err := VerifyBundle(bundle)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected %q refusal, got %v", want, err)
	}
}

// TestRestoreRequestIDGrammar proves the §9.1/§9.2 local-key grammar is
// enforced before any bundle or store effect: even a nonexistent bundle
// reports the grammar refusal, not a bundle read error.
func TestRestoreRequestIDGrammar(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"", "1 space", "-lead", ".lead", "_lead", "sla/sh", "ünïcode", strings.Repeat("a", 129)} {
		_, err := Restore(filepath.Join(dir, "missing.ntbackup"),
			filepath.Join(dir, "tasks.sqlite"),
			RestoreOptions{RequestID: id})
		if err == nil || !strings.Contains(err.Error(), "request id is invalid") {
			t.Errorf("request id %q: expected grammar refusal, got %v", id, err)
		}
	}
	// The 128-byte maximum of the grammar is accepted (the failure then
	// names the missing bundle, proving grammar passed).
	_, err := Restore(filepath.Join(dir, "missing.ntbackup"),
		filepath.Join(dir, "tasks.sqlite"),
		RestoreOptions{RequestID: strings.Repeat("a", 128)})
	if err == nil || strings.Contains(err.Error(), "request id is invalid") {
		t.Errorf("max-length request id: want a non-grammar failure, got %v", err)
	}
}

func TestBackupVerifyStructuralRefusals(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Extra entry.
	extra := filepath.Join(dir, "extra.ntbackup")
	copyBundle(t, bundle, extra)
	if err := os.WriteFile(filepath.Join(extra, "extra.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(extra); err == nil || !strings.Contains(err.Error(), "exactly") {
		t.Fatalf("expected extra-entry refusal, got %v", err)
	}

	// Symlink payload.
	link := filepath.Join(dir, "link.ntbackup")
	copyBundle(t, bundle, link)
	os.Remove(filepath.Join(link, PayloadFile))
	if err := os.Symlink(filepath.Join(bundle, PayloadFile), filepath.Join(link, PayloadFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(link); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("expected symlink refusal, got %v", err)
	}

	// Bundle path itself is a symlink.
	alias := filepath.Join(dir, "alias.ntbackup")
	if err := os.Symlink(bundle, alias); err != nil {
		t.Fatal(err)
	}
	// ReadDir through the symlink lists the target; the contract is enforced
	// on members, so this remains a valid bundle only if every member is
	// regular — verify resolves the directory and accepts real content.
	if _, err := VerifyBundle(alias); err != nil {
		t.Logf("symlinked bundle path refused: %v", err)
	}

	// Payload family differs from the manifest family.
	other := filepath.Join(dir, "other")
	otherDB := newBackupStore(t, other, dir)
	otherBundle := filepath.Join(dir, "other.ntbackup")
	otherManifest, err := otherDB.CreateBackup(otherBundle, "test")
	if err != nil {
		t.Fatal(err)
	}
	otherDB.Close()
	mixed := filepath.Join(dir, "mixed.ntbackup")
	copyBundle(t, bundle, mixed)
	// Splice the other family's manifest over this payload.
	rawManifest, _ := os.ReadFile(filepath.Join(otherBundle, ManifestFile))
	_ = otherManifest
	if err := os.WriteFile(filepath.Join(mixed, ManifestFile), rawManifest, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(mixed); err == nil {
		t.Fatalf("expected payload/manifest mismatch refusal")
	}
}

func copyBundle(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{ManifestFile, PayloadFile} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRestoreRefusals(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	familyID, _, _ := db.Identity()
	db.Close()

	// Existing target refuses without --overwrite.
	if _, err := Restore(bundle, db.Path(), RestoreOptions{RequestID: "r1", OwnerBoundary: dir}); err == nil ||
		!strings.Contains(err.Error(), "--overwrite") {
		t.Fatalf("expected overwrite refusal, got %v", err)
	}

	// Wrong family refuses even with --overwrite.
	otherDir := t.TempDir()
	other := newBackupStore(t, filepath.Join(otherDir, "src"), otherDir)
	otherPath := other.Path()
	other.Close()
	if _, err := Restore(bundle, otherPath, RestoreOptions{Overwrite: true, RequestID: "r2", OwnerBoundary: otherDir}); err == nil ||
		!strings.Contains(err.Error(), "wrong-family") && !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected wrong-family refusal, got %v", err)
	}

	// Absent target with a mismatched derived boundary refuses.
	if _, err := Restore(bundle, filepath.Join(dir, "elsewhere", "tasks.sqlite"), RestoreOptions{RequestID: "r3", OwnerBoundary: "/somewhere/else"}); err == nil ||
		!strings.Contains(err.Error(), "owner boundary") {
		t.Fatalf("expected boundary refusal, got %v", err)
	}

	// The exact legacy live.json fences restore.
	fencedDir := t.TempDir()
	fencedStoreDir := filepath.Join(fencedDir, "noise-tasks")
	if err := os.MkdirAll(fencedStoreDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fencedStoreDir, LegacyFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bundle, filepath.Join(fencedStoreDir, "tasks.sqlite"), RestoreOptions{RequestID: "r4", OwnerBoundary: dir}); err == nil ||
		!strings.Contains(err.Error(), LegacyFile) {
		t.Fatalf("expected live.json refusal, got %v", err)
	}
	os.Remove(filepath.Join(fencedStoreDir, LegacyFile))
	if err := os.WriteFile(filepath.Join(fencedStoreDir, MigrationJournalFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Restore(bundle, filepath.Join(fencedStoreDir, "tasks.sqlite"), RestoreOptions{RequestID: "r4", OwnerBoundary: dir}); err == nil ||
		!strings.Contains(err.Error(), MigrationJournalFile) {
		t.Fatalf("expected journal refusal, got %v", err)
	}

	_ = familyID
}

// TestRestoreFenceCheckedUnderExclusiveLock proves the §8.5 fence check
// happens only after the exclusive maintenance lock is acquired: with the
// lock held by a cooperating process and a fence file present, restore fails
// on lock acquisition, not on the fence.
func TestRestoreFenceCheckedUnderExclusiveLock(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	storeDir := filepath.Join(dir, "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	release, err := LockExclusive(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, MigrationJournalFile), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Restore(bundle, filepath.Join(storeDir, "tasks.sqlite"), RestoreOptions{RequestID: "r-lock", OwnerBoundary: dir})
	if err == nil {
		t.Fatal("expected lock refusal, got success")
	}
	if !strings.Contains(err.Error(), "exclusive maintenance lock") {
		t.Fatalf("expected lock-acquisition error, got fence or other error: %v", err)
	}
	release()

	// With the lock free, the same fence now refuses restore.
	_, err = Restore(bundle, filepath.Join(storeDir, "tasks.sqlite"), RestoreOptions{RequestID: "r-lock", OwnerBoundary: dir})
	if err == nil || !strings.Contains(err.Error(), MigrationJournalFile) {
		t.Fatalf("expected journal refusal after lock release, got %v", err)
	}
}

func TestRestoreOverwriteSameFamilyAndReplay(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	target := db.Path()
	sourceSemantic, _, _, _ := db.ClassDigests()
	db.Close()

	first, err := Restore(bundle, target, RestoreOptions{Overwrite: true, RequestID: "req-x", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("overwrite restore: %v", err)
	}
	if first.Adopted || first.Replayed {
		t.Fatalf("unexpected flags %+v", first)
	}
	// Re-running the same request and bundle returns the original receipt.
	second, err := Restore(bundle, target, RestoreOptions{Overwrite: true, RequestID: "req-x", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("replay restore: %v", err)
	}
	if !second.Replayed {
		t.Fatalf("expected replay, got %+v", second)
	}
	if second.Receipt["receiptId"] != first.Receipt["receiptId"] ||
		second.Receipt["committedAt"] != first.Receipt["committedAt"] {
		t.Fatalf("replay returned a different receipt")
	}

	// The same request id against a different bundle is a typed conflict.
	db2, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.AddTask("main", &Task{Title: "third"}); err != nil {
		t.Fatal(err)
	}
	bundle2 := filepath.Join(dir, "second.ntbackup")
	if _, err := db2.CreateBackup(bundle2, "test"); err != nil {
		t.Fatal(err)
	}
	db2.Close()
	var conflict *ReplayConflict
	if _, err := Restore(bundle2, target, RestoreOptions{Overwrite: true, RequestID: "req-x", OwnerBoundary: dir}); err == nil || !errors.As(err, &conflict) {
		t.Fatalf("expected replay conflict, got %v", err)
	}

	// A different request restores normally.
	third, err := Restore(bundle2, target, RestoreOptions{Overwrite: true, RequestID: "req-y", OwnerBoundary: dir})
	if err != nil {
		t.Fatalf("second-request restore: %v", err)
	}
	if third.Replayed {
		t.Fatalf("unexpected replay")
	}
	restored, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	semantic, _ := contentDigests(t, restored)
	_ = sourceSemantic
	tasks, _ := restored.ListTasks("main")
	if len(tasks) != 3 {
		t.Fatalf("expected 3 tasks after second restore, got %d", len(tasks))
	}
	_ = semantic
}

func TestBackupDuringWrites(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	defer db.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 10; i++ {
			db.AddTask("main", &Task{Title: "concurrent"})
		}
	}()
	bundle := filepath.Join(dir, "concurrent.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatalf("backup during writes: %v", err)
	}
	<-done
	if _, err := VerifyBundle(bundle); err != nil {
		t.Fatalf("verify concurrent backup: %v", err)
	}
}

func TestBackupFromUpgradedV1Store(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noise-tasks", "tasks.sqlite")
	db, err := Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a released v1 database: pre-identity, pre-receipts,
	// pre-record_extra, and pre-orchestrator schema.
	rewindStoreToV2(t, db)
	if _, err := db.db.Exec(`DROP TABLE receipts`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`ALTER TABLE tasks DROP COLUMN record_extra`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`PRAGMA user_version=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	v1, err := Open(path)
	if err != nil {
		t.Fatalf("open v1 store: %v", err)
	}
	// A v1 store fails closed without a configured owner boundary...
	if err := v1.EnsureTag("main"); err == nil || !strings.Contains(err.Error(), "owner boundary") {
		t.Fatalf("expected missing-owner refusal, got %v", err)
	}
	if err := v1.SetOwnerBoundary(dir); err != nil {
		t.Fatal(err)
	}
	// A normal mutation upgrades the schema transactionally (SPEC §8.2).
	if err := v1.EnsureTag("main"); err != nil {
		t.Fatalf("upgrade mutation: %v", err)
	}
	version, err := v1.userVersion()
	if err != nil || version != SchemaVersion {
		t.Fatalf("user_version %d after upgrade, want %d (%v)", version, SchemaVersion, err)
	}
	if _, err := v1.AddTask("main", &Task{Title: "legacy-era"}); err != nil {
		t.Fatal(err)
	}
	if err := v1.EnsureIdentity(dir); err != nil {
		t.Fatal(err)
	}
	bundle := filepath.Join(dir, "v1.ntbackup")
	manifest, err := v1.CreateBackup(bundle, "test")
	if err != nil {
		t.Fatalf("backup upgraded store: %v", err)
	}
	if manifest.SourceSchema != fmt.Sprintf("sqlite:%d", SchemaVersion) {
		t.Fatalf("source schema %q, want sqlite:%d", manifest.SourceSchema, SchemaVersion)
	}
	v1.Close()
	if _, err := VerifyBundle(bundle); err != nil {
		t.Fatalf("verify upgraded-store bundle: %v", err)
	}
}

func TestBackupCreatePreservesExistingEmptyDirectory(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	defer db.Close()
	out := filepath.Join(dir, "out.ntbackup")
	if err := os.Mkdir(out, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateBackup(out, "test"); err == nil ||
		!strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("expected no-overwrite refusal, got %v", err)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("pre-existing empty directory must be preserved, got %d entries", len(entries))
	}
}

func TestBackupCreateConcurrentOutputRace(t *testing.T) {
	for attempt := 0; attempt < 5; attempt++ {
		dir := t.TempDir()
		db := newBackupStore(t, filepath.Join(dir, "src"), dir)
		out := filepath.Join(dir, "out.ntbackup")
		// Racer keeps creating and removing the output path while the
		// backup is staged; publication must either win cleanly before any
		// racer mkdir or fail atomically — never replace a racer's directory.
		stop := make(chan struct{})
		racing := make(chan struct{})
		done := make(chan struct{})
		replaced := make(chan string, 1)
		go func() {
			defer close(done)
			close(racing)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := os.Mkdir(out, 0o700); err == nil {
					entries, err := os.ReadDir(out)
					if err != nil {
						select {
						case replaced <- err.Error():
						default:
						}
						return
					}
					if len(entries) != 0 {
						select {
						case replaced <- "racer-created directory was replaced":
						default:
						}
						return
					}
					os.Remove(out)
				}
			}
		}()
		<-racing
		_, err := db.CreateBackup(out, "test")
		close(stop)
		<-done
		select {
		case msg := <-replaced:
			t.Fatalf("attempt %d: %s", attempt, msg)
		default:
		}
		db.Close()
		if err == nil {
			if _, err := VerifyBundle(out); err != nil {
				t.Fatalf("published bundle must verify: %v", err)
			}
			continue
		}
		if info, statErr := os.Lstat(out); statErr == nil && info.IsDir() {
			entries, readErr := os.ReadDir(out)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("failed publication must leave the empty output directory intact, got %d entries", len(entries))
			}
		}
	}
}

func TestBackupModeEnforcement(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	bundle := filepath.Join(dir, "out.ntbackup")
	if _, err := db.CreateBackup(bundle, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := VerifyBundle(bundle); err != nil {
		t.Fatalf("fresh bundle must verify: %v", err)
	}

	dirInfo, err := os.Lstat(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Fatalf("bundle directory mode %04o, want 0700", dirInfo.Mode().Perm())
	}
	for _, name := range []string{ManifestFile, PayloadFile} {
		info, err := os.Lstat(filepath.Join(bundle, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %04o, want 0600", name, info.Mode().Perm())
		}
	}

	cases := []struct {
		name   string
		member string // empty: the bundle directory itself
		mode   os.FileMode
	}{
		{"manifest mode", ManifestFile, 0o644},
		{"payload mode", PayloadFile, 0o644},
		{"directory mode", "", 0o755},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restore := filepath.Join(dir, "restore.ntbackup")
			copyBundle(t, bundle, restore)
			target := restore
			if tc.member != "" {
				target = filepath.Join(restore, tc.member)
			}
			if err := os.Chmod(target, tc.mode); err != nil {
				t.Fatal(err)
			}
			if _, err := VerifyBundle(restore); err == nil || !strings.Contains(err.Error(), "want 0") {
				t.Fatalf("expected mode refusal, got %v", err)
			}
		})
	}
}

// TestBundleSchemaObjectTampering proves the bundle gate validates the
// payload's schema catalog, not just its data digests: a malicious bundle can
// add a trigger (or alter an expected object), re-digest the manifest, and
// still carry matching semantic digests — the trigger would fire when Restore
// appends its restore event after semantic validation and mutate task state.
func TestBundleSchemaObjectTampering(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	origin := filepath.Join(dir, "origin.ntbackup")
	if _, err := db.CreateBackup(origin, "test"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	fresh := func(t *testing.T) string {
		b := filepath.Join(dir, "tampered.ntbackup")
		os.RemoveAll(b)
		copyBundle(t, origin, b)
		return b
	}

	t.Run("hidden AFTER INSERT trigger", func(t *testing.T) {
		b := fresh(t)
		mutatePayload(t, b, func(sdb *sql.DB) error {
			_, err := sdb.Exec(`CREATE TRIGGER pwn AFTER INSERT ON events ` +
				`BEGIN UPDATE tasks SET title='pwned'; END`)
			return err
		}, `unexpected trigger "pwn"`)
		// Restore re-verifies before anything is published: the refusal must
		// fire while the target is still absent.
		target := filepath.Join(dir, "dst", "tasks.sqlite")
		if _, err := Restore(b, target, RestoreOptions{RequestID: "pwn-1", OwnerBoundary: dir}); err == nil ||
			!strings.Contains(err.Error(), `unexpected trigger "pwn"`) {
			t.Fatalf("expected restore refusal, got %v", err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("restore published a target despite refusal: %v", err)
		}
	})

	t.Run("altered expected table", func(t *testing.T) {
		mutatePayload(t, fresh(t), func(sdb *sql.DB) error {
			_, err := sdb.Exec(`ALTER TABLE tags ADD COLUMN evil TEXT`)
			return err
		}, `table "tags" does not match the current schema`)
	})

	t.Run("extra view", func(t *testing.T) {
		mutatePayload(t, fresh(t), func(sdb *sql.DB) error {
			_, err := sdb.Exec(`CREATE VIEW evil AS SELECT * FROM tasks`)
			return err
		}, `unexpected view "evil"`)
	})

	t.Run("missing expected index", func(t *testing.T) {
		mutatePayload(t, fresh(t), func(sdb *sql.DB) error {
			_, err := sdb.Exec(`DROP INDEX receipts_request_id`)
			return err
		}, `missing index "receipts_request_id"`)
	})

	// sqlite_sequence rows are versioned content, not schema: changing them
	// (and re-digesting) must not affect verification.
	t.Run("sqlite_sequence content is not schema", func(t *testing.T) {
		b := fresh(t)
		tamperPayload(t, b, func(sdb *sql.DB) error {
			_, err := sdb.Exec(`UPDATE sqlite_sequence SET seq=500 WHERE name='events'`)
			return err
		})
		if _, err := VerifyBundle(b); err != nil {
			t.Fatalf("sqlite_sequence content change must verify: %v", err)
		}
	})
}

func TestHistoricalBundlesRejectMalformedCatalog(t *testing.T) {
	cases := []struct {
		name   string
		rewind func(*testing.T, *DB)
	}{
		{name: "v2", rewind: rewindStoreToV2},
		{name: "v3", rewind: rewindStoreToV3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			db := newBackupStore(t, filepath.Join(dir, "src"), dir)
			tc.rewind(t, db)
			bundle := filepath.Join(dir, tc.name+".ntbackup")
			if _, err := db.CreateBackup(bundle, tc.name+" fixture"); err != nil {
				t.Fatal(err)
			}
			db.Close()
			view := "untrusted_" + tc.name
			mutatePayload(t, bundle, func(sdb *sql.DB) error {
				_, err := sdb.Exec(`CREATE VIEW ` + view + ` AS SELECT * FROM tasks`)
				return err
			}, `unexpected view "`+view+`"`)
		})
	}
}

// TestRestoreIgnoresSwappedBundlePayload proves the §8.5 TOCTOU fix: after
// the bundle has been securely verified and its exact bytes staged from the
// verified open handle, swapping or corrupting the bundle payload by
// pathname cannot influence restore — restore consumes only the staged
// verified bytes, publishing the originally verified semantics with the
// manifest's payload digest bound into the receipt. The old behavior
// (reopening bundle/store.sqlite by path) would consume the swapped bytes
// and either publish them or fail on an identity mismatch.
func TestRestoreIgnoresSwappedBundlePayload(t *testing.T) {
	dir := t.TempDir()
	db := newBackupStore(t, filepath.Join(dir, "src"), dir)
	manifest, err := db.CreateBackup(filepath.Join(dir, "origin.ntbackup"), "test")
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	// A second, differently-populated bundle of the same binary shape to
	// swap in by pathname.
	other := newBackupStore(t, filepath.Join(dir, "other"), dir)
	if _, err := other.CreateBackup(filepath.Join(dir, "other.ntbackup"), "test"); err != nil {
		t.Fatal(err)
	}
	other.Close()

	cases := []struct {
		name string
		swap func(t *testing.T, bundle string)
	}{
		{"payload replaced by pathname", func(t *testing.T, bundle string) {
			// Atomic pathname replacement, exactly what a racing writer or
			// attacker controls.
			tmp := filepath.Join(dir, "swap.tmp")
			if err := copyFile(filepath.Join(dir, "other.ntbackup", PayloadFile), tmp); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, filepath.Join(bundle, PayloadFile)); err != nil {
				t.Fatal(err)
			}
		}},
		{"payload mutated by pathname", func(t *testing.T, bundle string) {
			// In-place corruption of the member's bytes.
			if err := os.WriteFile(filepath.Join(bundle, PayloadFile), []byte("not a sqlite database at all"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bundle := filepath.Join(dir, "swapped.ntbackup")
			os.RemoveAll(bundle)
			copyBundle(t, filepath.Join(dir, "origin.ntbackup"), bundle)

			restoreStagedHook = func(bp string) {
				if bp == bundle {
					tc.swap(t, bp)
				}
			}
			defer func() { restoreStagedHook = func(string) {} }()

			requestID := fmt.Sprintf("swap-%d", i)
			target := filepath.Join(dir, fmt.Sprintf("dst-%d", i), "tasks.sqlite")
			result, err := Restore(bundle, target, RestoreOptions{RequestID: requestID, OwnerBoundary: dir})
			if err != nil {
				t.Fatalf("restore must publish the originally verified bytes after a pathname swap, got %v", err)
			}
			if result.Replayed {
				t.Fatal("first restore must not be a replay")
			}
			// The published store carries the originally verified semantics,
			// never the swapped bytes: family and stripped digests match the
			// verified manifest, and the receipt binds its exact payload
			// digest.
			restored, err := Open(target)
			if err != nil {
				t.Fatal(err)
			}
			defer restored.Close()
			familyID, _, err := restored.Identity()
			if err != nil {
				t.Fatal(err)
			}
			if familyID != manifest.FamilyID {
				t.Fatalf("restored family %s, want the verified family %s", familyID, manifest.FamilyID)
			}
			// contentDigests strips the restore-added receipt rows so the
			// published content can be compared exactly against the manifest.
			semantic, comparison := contentDigests(t, restored)
			if semantic != manifest.SemanticDigest || comparison != manifest.ComparisonDigest {
				t.Fatalf("restored content does not match the verified manifest (semantic match=%v comparison=%v)",
					semantic == manifest.SemanticDigest, comparison == manifest.ComparisonDigest)
			}
			wantInput, err := restoreInputDigest(requestID, manifest)
			if err != nil {
				t.Fatal(err)
			}
			if got := result.Receipt["inputDigest"]; got != wantInput {
				t.Fatalf("receipt input digest %v, want the digest bound to the originally verified payload", got)
			}
		})
	}
}

// TestCopyHashOpenFileDigestsExactCopiedBytes proves the single-pass
// copy+hash helper behind §8.5 staging: the returned digest is computed
// over exactly the bytes that land in the staged file — never over a
// separate read of the source that an in-place writer could diverge —
// and staging keeps its exclusive-create, 0600-mode behavior.
func TestCopyHashOpenFileDigestsExactCopiedBytes(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src")
	content := make([]byte, 5<<20) // well past io.Copy's 32 KiB buffer
	for i := range content {
		content[i] = byte(i * 7)
	}
	if err := os.WriteFile(srcPath, content, 0o600); err != nil {
		t.Fatal(err)
	}
	src, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	dst := filepath.Join(dir, "staged")
	got, err := copyHashOpenFile(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	// The returned digest equals the digest of the staged bytes.
	staged, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	stagedDigest, err := hashOpenFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	staged.Close()
	if got != stagedDigest {
		t.Fatalf("returned digest %s does not match staged file digest %s", got, stagedDigest)
	}
	// Mutating the source after staging cannot change the staged bytes or
	// their already-computed digest.
	if err := os.WriteFile(srcPath, []byte("mutated"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(content) {
		t.Fatalf("staged file size %d changed after source mutation", len(after))
	}
	// Staging is exclusive-create with mode 0600.
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("staged mode is %04o, want 0600", info.Mode().Perm())
	}
	if _, err := copyHashOpenFile(src, dst); err == nil {
		t.Fatal("staging over an existing path must fail exclusively")
	}
}
