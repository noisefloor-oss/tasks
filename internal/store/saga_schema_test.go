package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

type minimalSagaRow struct {
	run, source                                                                any
	tag, task, operation, status                                               any
	active, fence, request, generation                                         any
	boundary, registration, path, branch, base, force, created, updated, extra any
}

func newMinimalSagaDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := CreateWithOwner(filepath.Join(dir, "tasks.sqlite"), filepath.Join(dir, "family"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if id, err := db.AddTask("main", &Task{Title: "target"}); err != nil || id != "1" {
		t.Fatalf("task=%q %v", id, err)
	}
	if _, _, _, err := db.AcquireClaim("main", "1", "agent-a", 600, "claim.saga"); err != nil {
		t.Fatal(err)
	}
	addBackupRun(t, db)
	if _, err := db.db.Exec(`UPDATE meta SET value='100' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	return db
}

func validMinimalSaga(t *testing.T, db *DB) minimalSagaRow {
	t.Helper()
	var boundary, started string
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='ownerBoundary'`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT started_at FROM runs WHERE run_id=1`).Scan(&started); err != nil {
		t.Fatal(err)
	}
	return minimalSagaRow{run: int64(1), source: nil, tag: "main", task: "1", operation: "provision", status: "pending",
		active: int64(0), fence: int64(100), request: "wt.provision", generation: int64(0), boundary: boundary,
		registration: "/git-admin/worktrees", path: filepath.Join(boundary, ".worktrees", "main-1"), branch: "tasks/main/1",
		base: strings.Repeat("a", 40), force: int64(0), created: started, updated: started, extra: nil}
}

func insertMinimalSaga(db *DB, r minimalSagaRow) error {
	_, err := db.db.Exec(`INSERT INTO worktree_sagas(run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.run, r.source, r.tag, r.task, r.operation, r.status, r.active, r.fence, r.request, r.generation,
		r.boundary, r.registration, r.path, r.branch, r.base, r.force, r.created, r.updated, r.extra)
	return err
}

func loadTestSaga(t *testing.T, db *DB, id int64) *worktreeSagaState {
	t.Helper()
	s := &worktreeSagaState{}
	var active, force int64
	if err := db.db.QueryRow(`SELECT saga_id,run_id,source_saga_id,tag,task_id,operation,status,
		binding_active,saga_fence,request_id,recovery_generation,owner_boundary,registration_root,
		target_path,branch,base_commit,force,created_at,updated_at,extra FROM worktree_sagas WHERE saga_id=?`, id).
		Scan(&s.id, &s.run, &s.source, &s.tag, &s.task, &s.operation, &s.status, &active,
			&s.fence, &s.request, &s.generation, &s.boundary, &s.registration, &s.path,
			&s.branch, &s.base, &force, &s.created, &s.updated, &s.extra); err != nil {
		t.Fatal(err)
	}
	s.active, s.force = active == 1, force == 1
	return s
}

func insertTestWorktreeReceipt(t *testing.T, db *DB, sagaID, generation int64, status string, original bool) string {
	t.Helper()
	s := loadTestSaga(t, db, sagaID)
	var family, owner string
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&family); err != nil {
		t.Fatal(err)
	}
	if s.run.Valid {
		if err := db.db.QueryRow(`SELECT owner FROM runs WHERE run_id=?`, s.run.Int64).Scan(&owner); err != nil {
			t.Fatal(err)
		}
	}
	request := s.request
	if !original {
		var err error
		request, err = worktreeRecoveryRequestID(family, s.id, generation)
		if err != nil {
			t.Fatal(err)
		}
	}
	inputRun, inputSaga, inputOperation, inputOwner := int64(0), int64(0), s.operation, owner
	if s.run.Valid {
		inputRun = s.run.Int64
	}
	input := map[string]any{"force": s.force, "operation": inputOperation, "owner": inputOwner,
		"requestId": request, "runId": inputRun, "sagaId": inputSaga}
	if strings.HasPrefix(s.request, "auto-") {
		input["owner"] = ""
	}
	if !original {
		input = map[string]any{"force": s.force, "operation": "recover", "recoveryGeneration": generation,
			"requestId": request, "runId": int64(0), "sagaId": s.id}
	}
	inputDigest, err := canonicalDigest(input)
	if err != nil {
		t.Fatal(err)
	}
	active := s.operation == "provision" && status == "bound" || s.operation == "remove" && s.source.Valid && status != "removed"
	resultDigest, err := canonicalDigest(map[string]any{
		"active": active, "baseCommit": nullableString(s.base), "branch": s.branch, "committedAt": s.updated,
		"force": s.force, "operation": s.operation, "ownerBoundary": s.boundary, "path": s.path,
		"recoveryGeneration": generation, "runId": nullableInt64(s.run), "sagaId": s.id,
		"status": status, "tag": s.tag, "task": s.task,
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("noise-tasks-worktree-receipt-v1\x00" + request + "\x00" + inputDigest))
	receiptID := "worktree-" + hex.EncodeToString(sum[:])
	inventory := map[string]any{"tags": 1, "milestones": 0, "tasks": 1, "dependencies": 0,
		"subtasks": 0, "bindings": 0, "assignments": 0, "claims": 0, "events": 0,
		"receipts": 1, "plans": 0, "planMappings": 0, "runs": 0, "attempts": 0, "worktreeSagas": 1}
	receipt := map[string]any{
		"schemaVersion": ReceiptSchemaVersion, "kind": ReceiptKindWorktree, "receiptId": receiptID,
		"familyId": family, "requestId": request, "inputDigest": inputDigest, "resultDigest": resultDigest,
		"operation": s.operation, "sagaId": s.id, "sourceSagaId": nullableInt64(s.source), "tag": s.tag,
		"task": s.task, "runId": nullableInt64(s.run), "owner": owner, "status": status, "active": active,
		"ownerBoundary": s.boundary, "path": s.path, "branch": s.branch, "baseCommit": nullableString(s.base),
		"force": s.force, "recoveryGeneration": generation, "inventory": inventory,
		"eventSeq": map[string]any{"first": int64(1), "last": int64(1)}, "committedAt": s.updated,
	}
	if err := db.transact("BEGIN IMMEDIATE", func(tx *tx) error {
		_, err := tx.insertReceipt(ReceiptKindWorktree, receiptID, request, receipt, s.updated)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return request
}

func TestWorktreeSagaV5ExactCatalog(t *testing.T) {
	db := newMinimalSagaDB(t)
	defer db.Close()
	rows, err := db.db.Query(`PRAGMA table_info(worktree_sagas)`)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	rows.Close()
	want := []string{"saga_id", "run_id", "source_saga_id", "tag", "task_id", "operation", "status", "binding_active", "saga_fence", "request_id", "recovery_generation", "owner_boundary", "registration_root", "target_path", "branch", "base_commit", "force", "created_at", "updated_at", "extra"}
	if strings.Join(columns, ",") != strings.Join(want, ",") {
		t.Fatalf("columns=%v", columns)
	}
	var indexes int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name GLOB 'worktree_sagas_*' AND sql IS NOT NULL`).Scan(&indexes); err != nil || indexes != 5 {
		t.Fatalf("indexes=%d %v", indexes, err)
	}
}

func TestWorktreeSagaExactSQLiteMasterSQL(t *testing.T) {
	db := newMinimalSagaDB(t)
	defer db.Close()
	rows, err := db.db.Query(`SELECT name,sql FROM sqlite_master
		WHERE name='worktree_sagas' OR name GLOB 'worktree_sagas_*' AND sql IS NOT NULL ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := map[string]string{
		"worktree_sagas":                    "5035ac2253f80f556a6e1e97c1ccea6c0e4ef8365810545ae7caba3fa39faef4",
		"worktree_sagas_one_active_binding": "c876089dcb70b01ad6a431ab5114440ad1e26ab9a45840f55eb53679512d588b",
		"worktree_sagas_one_live_effect":    "b54e0e99784a01b9d709b79bff6530f8488bc48fe3253fdd256f118e68b5b1fd",
		"worktree_sagas_one_removed_source": "cd39f11cd4217ffb3d365f02ab45cd64cc14bdac14c2021bd3decd6a9b2dae16",
		"worktree_sagas_run_history":        "bb726bf790634a984eb0ec776f1d0b3e91c2ac7ecfbdf98cc1f891100d984a9a",
		"worktree_sagas_status":             "6735134768d8d2222a3b251927f8975c4a71d9e62a6ae23bb81eb7d5d564cb42",
	}
	seen := 0
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte(raw))
		if got := hex.EncodeToString(sum[:]); got != want[name] {
			t.Fatalf("sqlite_master SQL %s digest=%s", name, got)
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if seen != len(want) {
		t.Fatalf("catalog objects=%d want=%d", seen, len(want))
	}
}

func TestWorktreeSagaCatalogUniqueness(t *testing.T) {
	clone := func(r minimalSagaRow, fence int64, request string) minimalSagaRow {
		r.fence, r.request = fence, request
		return r
	}
	t.Run("fence and request", func(t *testing.T) {
		db := newMinimalSagaDB(t)
		defer db.Close()
		r := validMinimalSaga(t, db)
		if err := insertMinimalSaga(db, r); err != nil {
			t.Fatal(err)
		}
		if err := insertMinimalSaga(db, clone(r, 100, "wt.other")); err == nil {
			t.Fatal("duplicate saga fence accepted")
		}
		if err := insertMinimalSaga(db, clone(r, 101, "wt.provision")); err == nil {
			t.Fatal("duplicate request accepted")
		}
	})
	t.Run("one live effect", func(t *testing.T) {
		db := newMinimalSagaDB(t)
		defer db.Close()
		r := validMinimalSaga(t, db)
		if err := insertMinimalSaga(db, r); err != nil {
			t.Fatal(err)
		}
		if err := insertMinimalSaga(db, clone(r, 101, "wt.second")); err == nil {
			t.Fatal("second live effect accepted")
		}
	})
	t.Run("one active binding", func(t *testing.T) {
		db := newMinimalSagaDB(t)
		defer db.Close()
		r := validMinimalSaga(t, db)
		r.status, r.active = "bound", int64(1)
		if err := insertMinimalSaga(db, r); err != nil {
			t.Fatal(err)
		}
		if err := insertMinimalSaga(db, clone(r, 101, "wt.second")); err == nil {
			t.Fatal("second active binding accepted")
		}
	})
	t.Run("one removed child", func(t *testing.T) {
		db := newMinimalSagaDB(t)
		defer db.Close()
		source := validMinimalSaga(t, db)
		source.status, source.active = "bound", int64(0)
		if err := insertMinimalSaga(db, source); err != nil {
			t.Fatal(err)
		}
		child := clone(source, 101, "wt.remove.one")
		child.source, child.operation, child.status, child.active = int64(1), "remove", "removed", int64(0)
		if err := insertMinimalSaga(db, child); err != nil {
			t.Fatal(err)
		}
		if err := insertMinimalSaga(db, clone(child, 102, "wt.remove.two")); err == nil {
			t.Fatal("second removed child accepted")
		}
	})
}

func TestWorktreeSagaMinimalValidation(t *testing.T) {
	db := newMinimalSagaDB(t)
	defer db.Close()
	if err := insertMinimalSaga(db, validMinimalSaga(t, db)); err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, sql, want string }{
		{"request", `UPDATE worktree_sagas SET request_id='recover-bad'`, "recovery-reserved"},
		{"path", `UPDATE worktree_sagas SET target_path='/wrong'`, "effect paths"},
		{"registration", `UPDATE worktree_sagas SET registration_root='/git-admin/not-worktrees'`, "registration root"},
		{"extra collision", `UPDATE worktree_sagas SET extra='{"path":"shadow"}'`, "collides"},
		{"fence", `UPDATE meta SET value='99' WHERE key='fencing'`, "below saga fence"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			copy := newMinimalSagaDB(t)
			defer copy.Close()
			if err := insertMinimalSaga(copy, validMinimalSaga(t, copy)); err != nil {
				t.Fatal(err)
			}
			if _, err := copy.db.Exec(tc.sql); err != nil {
				t.Fatal(err)
			}
			if err := copy.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation=%v", err)
			}
		})
	}
}

func TestWorktreeSagaLegacyReachableStateOnly(t *testing.T) {
	for _, tc := range []struct {
		name, status, recoveryStatus string
		generation                   int64
		terminal, valid              bool
	}{
		{"generation-zero cleanup", "cleanup-required", "", 0, false, true},
		{"generation-zero needs broker", "needs-broker", "", 0, false, false},
		{"generation-zero removed", "removed", "", 0, true, false},
		{"generation-zero failed", "failed", "", 0, true, false},
		{"generation-one cleanup", "cleanup-required", "cleanup-required", 1, false, true},
		{"generation-one needs broker", "needs-broker", "needs-broker", 1, false, true},
		{"generation-one removed", "removed", "removed", 1, true, true},
		{"generation-one failed", "failed", "failed", 1, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newMinimalSagaDB(t)
			defer db.Close()
			row := validMinimalSaga(t, db)
			binding := &WorktreeBinding{Boundary: row.boundary.(string), Path: row.path.(string), Branch: row.branch.(string), CreatedAt: row.created.(string)}
			if err := db.SetBinding("main", "1", binding); err != nil {
				t.Fatal(err)
			}
			var family string
			if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&family); err != nil {
				t.Fatal(err)
			}
			row.run, row.source, row.registration, row.base = nil, nil, nil, nil
			row.operation, row.status, row.generation = "remove", tc.status, tc.generation
			row.request = legacyWorktreeSagaRequestID(family, "main", "1", binding.Path, binding.Branch)
			if err := insertMinimalSaga(db, row); err != nil {
				t.Fatal(err)
			}
			if tc.generation > 0 {
				insertTestWorktreeReceipt(t, db, 1, 1, tc.recoveryStatus, false)
			}
			if tc.terminal && tc.generation > 0 {
				insertTestWorktreeReceipt(t, db, 1, tc.generation, tc.status, true)
			}
			if tc.status == "removed" {
				if _, err := db.db.Exec(`UPDATE tasks SET wt_boundary=NULL,wt_path=NULL,wt_branch=NULL,wt_created_at=NULL,wt_keep=NULL,wt_extra=NULL WHERE tag='main' AND id='1'`); err != nil {
					t.Fatal(err)
				}
			}
			err := db.ValidateWorktreeSagaState()
			if tc.valid && err != nil || !tc.valid && err == nil {
				t.Fatalf("validation=%v valid=%v", err, tc.valid)
			}
			if tc.name == "generation-zero cleanup" {
				if _, err := db.db.Exec(`UPDATE worktree_sagas SET updated_at=strftime('%Y-%m-%dT%H:%M:%SZ',created_at,'+1 second') WHERE saga_id=1`); err != nil {
					t.Fatal(err)
				}
				if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "legacy identity") {
					t.Fatalf("mutated generation-zero validation=%v", err)
				}
			}
		})
	}
}

func TestWorktreeSagaRecoveryGenerationFailsClosedWithoutReceipts(t *testing.T) {
	db := newMinimalSagaDB(t)
	defer db.Close()
	r := validMinimalSaga(t, db)
	r.status = "cleanup-required"
	r.generation = int64(1)
	if err := insertMinimalSaga(db, r); err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "receipt count") {
		t.Fatalf("validation=%v", err)
	}
	if _, err := db.db.Exec(`UPDATE worktree_sagas SET recovery_generation=9223372036854775807`); err != nil {
		t.Fatal(err)
	}
	if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "receipt count") {
		t.Fatalf("max generation validation=%v", err)
	}
}

func TestWorktreeSagaLinkedActivityAndAutomaticCause(t *testing.T) {
	newBoundSource := func(t *testing.T, active int64) (*DB, minimalSagaRow) {
		db := newMinimalSagaDB(t)
		if _, err := db.db.Exec(`UPDATE meta SET value='200' WHERE key='fencing'`); err != nil {
			t.Fatal(err)
		}
		source := validMinimalSaga(t, db)
		source.status, source.active = "bound", active
		if err := insertMinimalSaga(db, source); err != nil {
			t.Fatal(err)
		}
		insertTestWorktreeReceipt(t, db, 1, 0, "bound", true)
		return db, source
	}
	t.Run("linked live removal requires active source", func(t *testing.T) {
		db, source := newBoundSource(t, 1)
		defer db.Close()
		child := source
		child.source, child.operation, child.status, child.active = int64(1), "remove", "pending", int64(0)
		child.fence, child.request = int64(101), "wt.remove"
		if err := insertMinimalSaga(db, child); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET base_commit=? WHERE saga_id=2`, strings.Repeat("b", 40)); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "does not match bound provision") {
			t.Fatalf("source mismatch validation=%v", err)
		}
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET base_commit=? WHERE saga_id=2`, strings.Repeat("a", 40)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET binding_active=0 WHERE saga_id=1`); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "inactive source") {
			t.Fatalf("validation=%v", err)
		}
	})
	t.Run("automatic run cause must be terminal", func(t *testing.T) {
		db, source := newBoundSource(t, 1)
		defer db.Close()
		var family string
		if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&family); err != nil {
			t.Fatal(err)
		}
		child := source
		child.source, child.operation, child.status, child.active = int64(1), "remove", "pending", int64(0)
		child.fence, child.request = int64(101), automaticCleanupRequestID(family, 1, 1)
		if err := insertMinimalSaga(db, child); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET request_id=? WHERE saga_id=2`, "auto-"+strings.Repeat("0", 64)); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "automatic request") {
			t.Fatalf("wrong automatic id validation=%v", err)
		}
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET request_id=? WHERE saga_id=2`, child.request); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "nonterminal automatic run cause") {
			t.Fatalf("running cause validation=%v", err)
		}
		if _, err := db.db.Exec(`UPDATE runs SET status='cancelled',terminal_at=updated_at WHERE run_id=1`); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("terminal cause validation=%v", err)
		}
	})
}

func TestWorktreeSagaInactiveSourceReceiptAndChildChronology(t *testing.T) {
	build := func(t *testing.T, failedFirst bool) *DB {
		db := newMinimalSagaDB(t)
		if _, err := db.db.Exec(`UPDATE meta SET value='200' WHERE key='fencing'`); err != nil {
			t.Fatal(err)
		}
		source := validMinimalSaga(t, db)
		source.status, source.active = "bound", int64(0)
		if err := insertMinimalSaga(db, source); err != nil {
			t.Fatal(err)
		}
		insertTestWorktreeReceipt(t, db, 1, 0, "bound", true)
		nextID := int64(2)
		if failedFirst {
			failed := source
			failed.source, failed.operation, failed.status, failed.active = int64(1), "remove", "failed", int64(0)
			failed.fence, failed.request = int64(101), "wt.remove.failed"
			if err := insertMinimalSaga(db, failed); err != nil {
				t.Fatal(err)
			}
			insertTestWorktreeReceipt(t, db, nextID, 0, "failed", true)
			nextID++
		}
		removed := source
		removed.source, removed.operation, removed.status, removed.active = int64(1), "remove", "removed", int64(0)
		removed.fence, removed.request = int64(100+nextID-1), "wt.remove.removed"
		if err := insertMinimalSaga(db, removed); err != nil {
			t.Fatal(err)
		}
		insertTestWorktreeReceipt(t, db, nextID, 0, "removed", true)
		return db
	}
	t.Run("bound receipt remains active after source becomes inactive", func(t *testing.T) {
		db := build(t, true)
		defer db.Close()
		if _, err := db.db.Exec(`DELETE FROM events`); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("failed child cannot postdate removed child", func(t *testing.T) {
		db := build(t, false)
		defer db.Close()
		source := validMinimalSaga(t, db)
		failed := source
		failed.source, failed.operation, failed.status, failed.active = int64(1), "remove", "failed", int64(0)
		failed.fence, failed.request = int64(102), "wt.remove.late-failed"
		if err := insertMinimalSaga(db, failed); err != nil {
			t.Fatal(err)
		}
		insertTestWorktreeReceipt(t, db, 3, 0, "failed", true)
		if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "planned after") {
			t.Fatalf("validation=%v", err)
		}
	})
}

func TestWorktreeSagaRecoveryReceiptSequence(t *testing.T) {
	legacy := func(t *testing.T, generation int64, status string) *DB {
		db := newMinimalSagaDB(t)
		row := validMinimalSaga(t, db)
		binding := &WorktreeBinding{Boundary: row.boundary.(string), Path: row.path.(string), Branch: row.branch.(string), CreatedAt: row.created.(string)}
		if err := db.SetBinding("main", "1", binding); err != nil {
			t.Fatal(err)
		}
		var family string
		if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&family); err != nil {
			t.Fatal(err)
		}
		row.run, row.source, row.registration, row.base = nil, nil, nil, nil
		row.operation, row.status, row.generation = "remove", status, generation
		row.request = legacyWorktreeSagaRequestID(family, "main", "1", binding.Path, binding.Branch)
		if err := insertMinimalSaga(db, row); err != nil {
			t.Fatal(err)
		}
		return db
	}
	t.Run("complete sequence ignores missing feed", func(t *testing.T) {
		db := legacy(t, 2, "cleanup-required")
		defer db.Close()
		first := insertTestWorktreeReceipt(t, db, 1, 1, "needs-broker", false)
		insertTestWorktreeReceipt(t, db, 1, 2, "cleanup-required", false)
		if _, err := db.db.Exec(`DELETE FROM events`); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`DELETE FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindWorktree, first); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "receipt count") {
			t.Fatalf("gap validation=%v", err)
		}
	})
	t.Run("generation zero reserves no receipt", func(t *testing.T) {
		db := legacy(t, 0, "cleanup-required")
		defer db.Close()
		insertTestWorktreeReceipt(t, db, 1, 0, "cleanup-required", true)
		if err := db.ValidateWorktreeSagaState(); err == nil {
			t.Fatal("generation-zero original receipt accepted")
		}
	})
	t.Run("terminal outcome cannot precede final generation", func(t *testing.T) {
		db := legacy(t, 2, "cleanup-required")
		defer db.Close()
		insertTestWorktreeReceipt(t, db, 1, 1, "failed", false)
		insertTestWorktreeReceipt(t, db, 1, 2, "cleanup-required", false)
		if err := db.ValidateWorktreeSagaState(); err == nil || !strings.Contains(err.Error(), "impossible") {
			t.Fatalf("validation=%v", err)
		}
	})
	t.Run("provision needs-broker cleanup drift", func(t *testing.T) {
		db := newMinimalSagaDB(t)
		defer db.Close()
		r := validMinimalSaga(t, db)
		r.status, r.generation = "cleanup-required", int64(1)
		if err := insertMinimalSaga(db, r); err != nil {
			t.Fatal(err)
		}
		insertTestWorktreeReceipt(t, db, 1, 1, "needs-broker", false)
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorktreeSagaSemanticDigestAndInventory(t *testing.T) {
	db := newMinimalSagaDB(t)
	defer db.Close()
	semanticBefore, comparisonBefore, inventoryBefore, err := db.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	if inventoryBefore.WorktreeSagas != 0 {
		t.Fatalf("initial saga inventory=%d", inventoryBefore.WorktreeSagas)
	}
	if err := insertMinimalSaga(db, validMinimalSaga(t, db)); err != nil {
		t.Fatal(err)
	}
	semanticAfter, comparisonAfter, inventoryAfter, err := db.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	if semanticAfter == semanticBefore || comparisonAfter != comparisonBefore || inventoryAfter.WorktreeSagas != 1 {
		t.Fatalf("digests/inventory before=(%s,%s,%d) after=(%s,%s,%d)", semanticBefore, comparisonBefore,
			inventoryBefore.WorktreeSagas, semanticAfter, comparisonAfter, inventoryAfter.WorktreeSagas)
	}
}

func TestRemoveTaskRefusesRetainedWorktreeSagaEvenForce(t *testing.T) {
	db, _ := newDB(t)
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if id, err := db.AddTask("main", &Task{Title: "target"}); err != nil || id != "1" {
		t.Fatalf("task=%q %v", id, err)
	}
	var family, boundary string
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&family); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='ownerBoundary'`).Scan(&boundary); err != nil {
		t.Fatal(err)
	}
	created := "2026-08-29T00:00:00Z"
	binding := &WorktreeBinding{Boundary: boundary, Path: filepath.Join(boundary, ".worktrees", "main-1"), Branch: "tasks/main/1", CreatedAt: created}
	if err := db.SetBinding("main", "1", binding); err != nil {
		t.Fatal(err)
	}
	r := minimalSagaRow{run: nil, source: nil, tag: "main", task: "1", operation: "remove", status: "cleanup-required",
		active: int64(0), fence: int64(1), request: legacyWorktreeSagaRequestID(family, "main", "1", binding.Path, binding.Branch), generation: int64(0),
		boundary: boundary, registration: nil, path: binding.Path, branch: binding.Branch, base: nil, force: int64(0), created: created, updated: created, extra: nil}
	if _, err := db.db.Exec(`UPDATE meta SET value='1' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	if err := insertMinimalSaga(db, r); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		_, err := db.RemoveTask("main", "1", 1, force, nil)
		var conflict *WorktreeConflict
		if !errors.As(err, &conflict) || conflict.Kind != "history" {
			t.Fatalf("force=%v removal=%v", force, err)
		}
	}
	if task, err := db.GetTask("main", "1"); err != nil || task == nil {
		t.Fatalf("retained task=%+v %v", task, err)
	}
}

func TestRemoveTaskPrefersWorktreeHistoryWhenRunAlsoRetained(t *testing.T) {
	db := newMinimalSagaDB(t)
	defer db.Close()
	if err := insertMinimalSaga(db, validMinimalSaga(t, db)); err != nil {
		t.Fatal(err)
	}
	_, err := db.RemoveTask("main", "1", 1, true, nil)
	var conflict *WorktreeConflict
	if !errors.As(err, &conflict) || conflict.Kind != "history" {
		t.Fatalf("both-history removal=%v", err)
	}
}
