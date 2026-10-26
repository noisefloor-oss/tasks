package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func rewindStoreToV3(t *testing.T, db *DB) {
	t.Helper()
	rewindStoreToV4(t, db)
	// The v4 catalog deliberately has a cycle. Rewinding a freshly-created,
	// empty test store therefore disables FK enforcement on one pinned
	// connection while removing the complete additive v4 delta.
	conn, err := db.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP INDEX runs_status_owner`,
		`DROP INDEX runs_task_history`,
		`DROP INDEX runs_one_nonterminal_task`,
		`DROP TABLE attempts`,
		`DROP TABLE runs`,
		`DELETE FROM events WHERE kind='schema'`,
		`PRAGMA user_version=3`,
	} {
		if _, err := conn.ExecContext(context.Background(), q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
}

func rewindStoreToV4(t *testing.T, db *DB) {
	t.Helper()
	for _, q := range []string{
		`DROP INDEX tasks_external_link`,
		`ALTER TABLE tasks DROP COLUMN link_family`,
		`ALTER TABLE tasks DROP COLUMN link_repository`,
		`ALTER TABLE tasks DROP COLUMN link_issue`,
		`DROP INDEX worktree_sagas_status`,
		`DROP INDEX worktree_sagas_run_history`,
		`DROP INDEX worktree_sagas_one_removed_source`,
		`DROP INDEX worktree_sagas_one_live_effect`,
		`DROP INDEX worktree_sagas_one_active_binding`,
		`DROP TABLE worktree_sagas`,
		`DELETE FROM events WHERE kind='schema'`,
		`PRAGMA user_version=4`,
	} {
		if _, err := db.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

// rewindStoreToV5 removes the complete additive v6 external-link delta from
// a freshly created, empty test store, restoring the exact v5 catalog.
func rewindStoreToV5(t *testing.T, db *DB) {
	t.Helper()
	for _, q := range []string{
		`DROP INDEX tasks_external_link`,
		`ALTER TABLE tasks DROP COLUMN link_family`,
		`ALTER TABLE tasks DROP COLUMN link_repository`,
		`ALTER TABLE tasks DROP COLUMN link_issue`,
		`PRAGMA user_version=5`,
	} {
		if _, err := db.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func rewindStoreToV2(t *testing.T, db *DB) {
	t.Helper()
	rewindStoreToV3(t, db)
	for _, q := range []string{
		`DROP INDEX assignments_owner_task`,
		`DROP TABLE assignments`,
		`DROP TABLE plan_mappings`,
		`DROP TABLE plans`,
		`DELETE FROM events WHERE kind='schema'`,
		`PRAGMA user_version=2`,
	} {
		if _, err := db.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func TestUpgradeV4ConvertsLegacyWorktreeBindingsDeterministically(t *testing.T) {
	owner := "/repo"
	db, err := CreateWithOwner(filepath.Join(t.TempDir(), "tasks.sqlite"), owner)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tag := range []string{"z", "a"} {
		if err := db.EnsureTag(tag); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 10; i++ {
		if _, err := db.AddTask("z", &Task{Title: fmt.Sprintf("z task %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	if id, err := db.AddTask("a", &Task{Title: "a task"}); err != nil || id != "1" {
		t.Fatalf("a task=%q %v", id, err)
	}
	for i, key := range []struct{ tag, task string }{{"z", "10"}, {"a", "1"}, {"z", "2"}} {
		binding := &WorktreeBinding{Boundary: owner,
			Path:   filepath.Join(owner, ".worktrees", WorktreeName(key.tag, key.task)),
			Branch: WorktreeBranch(key.tag, key.task), CreatedAt: fmt.Sprintf("2026-08-29T00:00:0%dZ", i+1)}
		if err := db.SetBinding(key.tag, key.task, binding); err != nil {
			t.Fatal(err)
		}
	}
	const family = "00000000-0000-4000-8000-000000000001"
	if _, err := db.db.Exec(`UPDATE meta SET value=? WHERE key='familyId'`, family); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE meta SET value='40' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	rewindStoreToV4(t, db)
	if err := db.maybeUpgrade(owner); err != nil {
		t.Fatal(err)
	}
	rows, err := db.db.Query(`SELECT saga_id,tag,task_id,saga_fence,request_id,status,created_at,updated_at
		FROM worktree_sagas ORDER BY saga_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := []struct{ tag, task string }{{"a", "1"}, {"z", "2"}, {"z", "10"}}
	for i := 0; rows.Next(); i++ {
		var saga, fence int64
		var tag, task, request, status, created, updated string
		if err := rows.Scan(&saga, &tag, &task, &fence, &request, &status, &created, &updated); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(owner, ".worktrees", WorktreeName(want[i].tag, want[i].task))
		wantRequest := legacyWorktreeSagaRequestID(family, want[i].tag, want[i].task, path, WorktreeBranch(want[i].tag, want[i].task))
		if i == 0 && request != "auto-02b035ec626c00101190837c3692b869d5723129b8f08974a40a0ddc0796be6d" {
			t.Fatalf("literal legacy request=%q", request)
		}
		wantID := int64(i + 1)
		if saga != wantID || tag != want[i].tag || task != want[i].task || fence != 40+wantID ||
			request != wantRequest || status != "cleanup-required" || created != updated {
			t.Fatalf("converted row = saga=%d tag/task=%s/%s fence=%d request=%q status=%q times=%q/%q", saga, tag, task, fence, request, status, created, updated)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM worktree_sagas`); got != 3 {
		t.Fatalf("converted rows=%d", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); got != 1 {
		t.Fatalf("upgrade events=%d", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM receipts WHERE kind=?`, ReceiptKindWorktree); got != 0 {
		t.Fatalf("conversion receipts=%d", got)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeV4WithoutBindingsAllocatesNoFence(t *testing.T) {
	db, _ := newDB(t)
	owner := db.ownerBoundary
	if _, err := db.db.Exec(`UPDATE meta SET value='40' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	rewindStoreToV4(t, db)
	if err := db.maybeUpgrade(owner); err != nil {
		t.Fatal(err)
	}
	if got, err := db.Fencing(); err != nil || got != 40 {
		t.Fatalf("fencing=%d %v", got, err)
	}
}

func TestUpgradeV4RejectsInvalidLegacyBindingAtomically(t *testing.T) {
	db, _ := newDB(t)
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if id, err := db.AddTask("main", &Task{Title: "target"}); err != nil || id != "1" {
		t.Fatalf("task=%q %v", id, err)
	}
	if err := db.SetBinding("main", "1", &WorktreeBinding{Boundary: db.ownerBoundary, Path: "/wrong",
		Branch: "tasks/main/1", CreatedAt: "2026-08-29T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE meta SET value='40' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	rewindStoreToV4(t, db)
	if err := db.maybeUpgrade(db.ownerBoundary); err == nil || !strings.Contains(err.Error(), "noncanonical effect paths") {
		t.Fatalf("upgrade validation=%v", err)
	}
	if version, err := db.userVersion(); err != nil || version != runSchemaVersion {
		t.Fatalf("rolled-back version=%d %v", version, err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name='worktree_sagas'`); got != 0 {
		t.Fatalf("rolled-back saga catalog count=%d", got)
	}
	if fencing, err := db.Fencing(); err != nil || fencing != 40 {
		t.Fatalf("rolled-back fence=%d %v", fencing, err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); got != 0 {
		t.Fatalf("rolled-back upgrade events=%d", got)
	}
}

func TestUpgradeV4RejectsLegacyAutomaticRequestCollisionAtomically(t *testing.T) {
	db, _ := newDB(t)
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if id, err := db.AddTask("main", &Task{Title: "target"}); err != nil || id != "1" {
		t.Fatalf("task=%q %v", id, err)
	}
	created := "2026-08-29T00:00:00Z"
	path := filepath.Join(db.ownerBoundary, ".worktrees", "main-1")
	if err := db.SetBinding("main", "1", &WorktreeBinding{Boundary: db.ownerBoundary, Path: path,
		Branch: "tasks/main/1", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	var family string
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&family); err != nil {
		t.Fatal(err)
	}
	request := legacyWorktreeSagaRequestID(family, "main", "1", path, "tasks/main/1")
	if _, err := db.db.Exec(`INSERT INTO receipts(kind,receipt_id,request_id,body,digest,created_at)
		VALUES(?,?,?,?,?,?)`, ReceiptKindWorktree, "collision", request, `{}`, "sha256:collision", created); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE meta SET value='40' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	rewindStoreToV4(t, db)
	if err := db.maybeUpgrade(db.ownerBoundary); err == nil {
		t.Fatal("colliding legacy automatic request upgraded")
	}
	if version, err := db.userVersion(); err != nil || version != runSchemaVersion {
		t.Fatalf("rolled-back version=%d %v", version, err)
	}
	if fencing, err := db.Fencing(); err != nil || fencing != 40 {
		t.Fatalf("rolled-back fence=%d %v", fencing, err)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name='worktree_sagas'`); got != 0 {
		t.Fatalf("rolled-back saga catalog count=%d", got)
	}
	if got := countRows(t, db, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); got != 0 {
		t.Fatalf("rolled-back upgrade events=%d", got)
	}
}

func TestSchemaCatalogsV2ThroughV5WorktreeClass(t *testing.T) {
	catalogs, err := schemaObjectsByVersion()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{identitySchemaVersion, orchestratorSchemaVersion, runSchemaVersion, worktreeSagaSchemaVersion} {
		catalog, ok := catalogs[version]
		if !ok {
			t.Fatalf("missing schema-v%d catalog", version)
		}
		_, hasSagas := catalog["worktree_sagas"]
		if hasSagas != (version == worktreeSagaSchemaVersion) {
			t.Fatalf("schema-v%d worktree_sagas=%v", version, hasSagas)
		}
	}
	for _, name := range []string{"worktree_sagas_one_active_binding", "worktree_sagas_one_live_effect",
		"worktree_sagas_one_removed_source", "worktree_sagas_run_history", "worktree_sagas_status"} {
		if _, ok := catalogs[worktreeSagaSchemaVersion][name]; !ok {
			t.Fatalf("schema-v5 missing %s", name)
		}
	}
}

// makeV1Store creates a store and rewinds it to a released v1 shape:
// pre-receipts, pre-identity, user_version 1.
func makeV1Store(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "noise-tasks", "tasks.sqlite")
	db, err := Create(path)
	if err != nil {
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

func countRows(t *testing.T, db *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func requireOrchestratorSchema(t *testing.T, db *DB) {
	t.Helper()
	for _, name := range []string{"plans", "plan_mappings", "assignments", "assignments_owner_task"} {
		if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name); n != 1 {
			t.Fatalf("schema object %q count = %d, want 1", name, n)
		}
	}
}

func requireRunSchema(t *testing.T, db *DB) {
	t.Helper()
	for _, name := range []string{
		"runs", "attempts", "runs_one_nonterminal_task",
		"runs_task_history", "runs_status_owner", "attempts_running_expiry",
	} {
		if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name); n != 1 {
			t.Fatalf("schema object %q count = %d, want 1", name, n)
		}
	}
	var runsSQL string
	if err := db.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='runs'`).Scan(&runsSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runsSQL, "DEFERRABLE INITIALLY DEFERRED") {
		t.Fatalf("runs catalog lacks deferred current-attempt FK: %s", runsSQL)
	}
}

// Two handles racing the first mutation on a v1 store produce exactly one
// upgrade event and exactly one identity pair.
func TestConcurrentFirstMutationsSingleUpgradeAndIdentity(t *testing.T) {
	dir := t.TempDir()
	path := makeV1Store(t, dir)
	owner := filepath.Join(dir, "family")

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := Open(path)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			if err := db.SetOwnerBoundary(owner); err != nil {
				errs[i] = err
				return
			}
			errs[i] = db.EnsureTag("main")
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent mutation %d: %v", i, err)
		}
	}

	check, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if v, err := check.userVersion(); err != nil || v != SchemaVersion {
		t.Fatalf("user_version %d (%v), want %d", v, err, SchemaVersion)
	}
	if n := countRows(t, check, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); n != 1 {
		t.Fatalf("upgrade events = %d, want 1", n)
	}
	if n := countRows(t, check, `SELECT COUNT(*) FROM meta WHERE key IN ('familyId','ownerBoundary')`); n != 2 {
		t.Fatalf("identity rows = %d, want 2", n)
	}
	familyID, stored, err := check.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if _, perr := uuid.Parse(familyID); perr != nil {
		t.Fatalf("family id %q is not a valid UUID", familyID)
	}
	if stored != owner {
		t.Fatalf("owner boundary %q, want %q", stored, owner)
	}
	if n := countRows(t, check, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='receipts_request_id'`); n != 1 {
		t.Fatal("replay index missing after upgrade")
	}
	requireOrchestratorSchema(t, check)
	requireRunSchema(t, check)
}

func TestV2FirstMutationUpgradesThroughV4(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := makeV2Store(t, dir, owner)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if current, err := db.PrepareIdentity(owner); err != nil || current {
		t.Fatalf("PrepareIdentity(v2) = (%t, %v), want older valid schema", current, err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if version, err := db.userVersion(); err != nil || version != SchemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", version, err, SchemaVersion)
	}
	requireOrchestratorSchema(t, db)
	requireRunSchema(t, db)
	if n := countRows(t, db, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); n != 1 {
		t.Fatalf("upgrade events = %d, want 1", n)
	}
}

// A damaged v2 identity refuses the upgrade and rolls back the complete
// v3/v4
// DDL delta and version stamp. The migration never tries to heal identity.
func TestV2UpgradeIdentityFailureRollsBackDDL(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := makeV2Store(t, dir, owner)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetOwnerBoundary(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`DELETE FROM meta WHERE key='ownerBoundary'`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err == nil || !strings.Contains(err.Error(), "partial family identity") {
		t.Fatalf("damaged identity upgrade = %v", err)
	}
	if version, err := db.userVersion(); err != nil || version != identitySchemaVersion {
		t.Fatalf("refused upgrade schema = %d, %v; want %d", version, err, identitySchemaVersion)
	}
	for _, name := range []string{"plans", "plan_mappings", "assignments", "assignments_owner_task"} {
		if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name); n != 0 {
			t.Fatalf("refused upgrade published schema object %q", name)
		}
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); n != 0 {
		t.Fatalf("refused upgrade events = %d, want 0", n)
	}
}

func insertV3Claim(t *testing.T, db *DB, taskID string, token int64, acquiredAt, claimExtra string) {
	t.Helper()
	if _, err := db.db.Exec(`INSERT OR IGNORE INTO tags(name,next_id,task_count,completed_count,milestones,extra)
		VALUES ('main', 3, 0, 0, '[]', NULL)`); err != nil {
		t.Fatal(err)
	}
	var extra any
	if claimExtra != "" {
		extra = claimExtra
	}
	if _, err := db.db.Exec(`INSERT INTO tasks(
		tag,id,title,description,details,status,priority,milestone,dependencies,subtasks,
		extra,record_extra,revision,updated_at,claim_owner,claim_token,claim_ttl,
		claim_acquired_at,claim_expires_at,claim_extra)
		VALUES ('main', ?, 'title', '', '', 'pending', NULL, NULL, '[]', '[]',
		NULL, NULL, 1, '2026-08-28T12:00:00Z', 'agent-a', ?, 300,
		?, '2026-08-28T12:05:00Z', ?)`, taskID, token, acquiredAt, extra); err != nil {
		t.Fatal(err)
	}
}

func claimExtraForTask(t *testing.T, db *DB, taskID string) map[string]json.RawMessage {
	t.Helper()
	var raw sql.NullString
	if err := db.db.QueryRow(`SELECT claim_extra FROM tasks WHERE tag='main' AND id=?`, taskID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var extra map[string]json.RawMessage
	if !raw.Valid || json.Unmarshal([]byte(raw.String), &extra) != nil {
		t.Fatalf("claim_extra for %s is not an object: %q", taskID, raw.String)
	}
	return extra
}

func acquisitionIDFromExtra(t *testing.T, extra map[string]json.RawMessage) string {
	t.Helper()
	var id string
	if err := json.Unmarshal(extra["acquisitionId"], &id); err != nil {
		t.Fatalf("decode acquisitionId: %v", err)
	}
	return id
}

func TestLegacyClaimAcquisitionIDFixedVector(t *testing.T) {
	got := legacyClaimAcquisitionID(
		"123e4567-e89b-42d3-a456-426614174000", "main", "7", 41,
		"2026-08-28T12:00:00Z",
	)
	const want = "dff1700f7b0b2b205267212b5e456a71"
	if got != want {
		t.Fatalf("legacy acquisition id = %s, want %s", got, want)
	}
}

func TestV3UpgradeMintsRandomClaimAcquisitionID(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := filepath.Join(dir, "tasks.sqlite")
	db, err := CreateWithOwner(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	rewindStoreToV3(t, db)
	insertV3Claim(t, db, "7", 41, "2026-08-28T12:00:00Z", `{"x-note":true}`)

	if err := db.immediate(func(*tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	extra := claimExtraForTask(t, db, "7")
	id := acquisitionIDFromExtra(t, extra)
	if !validStoredAcquisitionID(id) {
		t.Fatalf("random acquisition id = %q", id)
	}
	if string(extra["x-note"]) != "true" {
		t.Fatalf("unknown claim field changed: %s", extra["x-note"])
	}
	if id == legacyClaimAcquisitionID("123e4567-e89b-42d3-a456-426614174000", "main", "7", 41, "2026-08-28T12:00:00Z") {
		t.Fatal("normal SQLite upgrade used the deterministic legacy derivation")
	}
	requireRunSchema(t, db)
}

func TestConcurrentV3ClaimUpgradeMintsOnceAndEmitsOneEvent(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := filepath.Join(dir, "tasks.sqlite")
	seed, err := CreateWithOwner(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	rewindStoreToV3(t, seed)
	insertV3Claim(t, seed, "7", 41, "2026-08-28T12:00:00Z", `{"x-note":true}`)
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db, err := Open(path)
			if err != nil {
				errs[i] = err
				return
			}
			defer db.Close()
			if err := db.SetOwnerBoundary(owner); err != nil {
				errs[i] = err
				return
			}
			errs[i] = db.immediate(func(*tx) error { return nil })
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent v3 upgrade %d: %v", i, err)
		}
	}

	check, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if version, err := check.userVersion(); err != nil || version != SchemaVersion {
		t.Fatalf("schema version = %d (%v), want %d", version, err, SchemaVersion)
	}
	if n := countRows(t, check, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); n != 1 {
		t.Fatalf("upgrade events = %d, want 1", n)
	}
	id := acquisitionIDFromExtra(t, claimExtraForTask(t, check, "7"))
	if !validStoredAcquisitionID(id) {
		t.Fatalf("concurrently minted acquisition id = %q", id)
	}
}

func TestV3LegacyStageMintsDeterministicClaimAcquisitionID(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := filepath.Join(dir, "tasks.sqlite")
	db, err := CreateWithOwner(path, owner)
	if err != nil {
		t.Fatal(err)
	}
	rewindStoreToV3(t, db)
	insertV3Claim(t, db, "7", 41, "2026-08-28T12:00:00Z", "")
	familyID, _, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.useLegacyClaimAcquisitionIDs(); err != nil {
		t.Fatal(err)
	}
	if err := db.immediate(func(*tx) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got := acquisitionIDFromExtra(t, claimExtraForTask(t, db, "7"))
	want := legacyClaimAcquisitionID(familyID, "main", "7", 41, "2026-08-28T12:00:00Z")
	if got != want {
		t.Fatalf("legacy-stage acquisition id = %s, want %s", got, want)
	}
}

func TestV3UpgradeRejectsMalformedOrCollidingClaimAcquisitionIDs(t *testing.T) {
	const id = "00112233445566778899aabbccddeeff"
	cases := []struct {
		name   string
		extras []string
		want   string
	}{
		{"malformed", []string{`{"acquisitionId":"00112233445566778899AABBCCDDEEFF"}`}, "malformed claim acquisition id"},
		{"collision", []string{`{"acquisitionId":"` + id + `"}`, `{"acquisitionId":"` + id + `"}`}, "collision"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			owner := filepath.Join(dir, "family")
			db, err := CreateWithOwner(filepath.Join(dir, "tasks.sqlite"), owner)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rewindStoreToV3(t, db)
			for i, extra := range tc.extras {
				insertV3Claim(t, db, string(rune('7'+i)), int64(41+i), "2026-08-28T12:00:00Z", extra)
			}
			err = db.immediate(func(*tx) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("upgrade error = %v, want %q", err, tc.want)
			}
			if version, err := db.userVersion(); err != nil || version != orchestratorSchemaVersion {
				t.Fatalf("refused upgrade version = %d (%v), want 3", version, err)
			}
			if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name='runs'`); n != 0 {
				t.Fatal("refused acquisition-id upgrade published v4 DDL")
			}
		})
	}
}

func TestV3UpgradeRejectsMalformedFencingCounter(t *testing.T) {
	for _, value := range []string{"-1", "01", "9223372036854775807", "9223372036854775808", "not-an-integer"} {
		t.Run(value, func(t *testing.T) {
			dir := t.TempDir()
			db, err := CreateWithOwner(filepath.Join(dir, "tasks.sqlite"), filepath.Join(dir, "family"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			rewindStoreToV3(t, db)
			if _, err := db.db.Exec(`UPDATE meta SET value=? WHERE key='fencing'`, value); err != nil {
				t.Fatal(err)
			}
			err = db.immediate(func(*tx) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "fencing counter") {
				t.Fatalf("counter %q upgrade = %v", value, err)
			}
			if version, _ := db.userVersion(); version != orchestratorSchemaVersion {
				t.Fatalf("counter refusal version = %d, want 3", version)
			}
		})
	}
}

func TestV3UpgradeConflictingRunCatalogRollsBackClaimAugmentation(t *testing.T) {
	dir := t.TempDir()
	db, err := CreateWithOwner(filepath.Join(dir, "tasks.sqlite"), filepath.Join(dir, "family"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rewindStoreToV3(t, db)
	insertV3Claim(t, db, "7", 41, "2026-08-28T12:00:00Z", `{"x-note":true}`)
	if _, err := db.db.Exec(`CREATE TABLE runs (wrong TEXT)`); err != nil {
		t.Fatal(err)
	}

	err = db.immediate(func(*tx) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("conflicting v4 catalog upgrade = %v", err)
	}
	if version, err := db.userVersion(); err != nil || version != orchestratorSchemaVersion {
		t.Fatalf("refused catalog upgrade version = %d (%v), want 3", version, err)
	}
	extra := claimExtraForTask(t, db, "7")
	if _, augmented := extra["acquisitionId"]; augmented {
		t.Fatal("rolled-back catalog upgrade retained claim acquisition augmentation")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name='attempts'`); n != 0 {
		t.Fatal("rolled-back catalog upgrade retained attempts table")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM events WHERE kind='schema' AND action='upgraded'`); n != 0 {
		t.Fatalf("rolled-back catalog upgrade events = %d, want 0", n)
	}
}

func TestRunSchemaDeferredCyclePartialUniquenessAndRestrictions(t *testing.T) {
	dir := t.TempDir()
	db, err := CreateWithOwner(filepath.Join(dir, "tasks.sqlite"), filepath.Join(dir, "family"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	insertV3Claim(t, db, "7", 41, "2026-08-28T12:00:00Z", `{"acquisitionId":"00112233445566778899aabbccddeeff"}`)

	insertRun := func(runID int64, status, terminalAt, attemptID string) error {
		return db.transact("BEGIN IMMEDIATE", func(tx *tx) error {
			if _, err := tx.Exec(`INSERT INTO runs(
				run_id,tag,task_id,owner,claim_acquisition_id,claim_token,status,current_attempt,
				start_request_id,terminal_result_digest,force_cancel_fence,started_at,updated_at,terminal_at,extra)
				VALUES (?, 'main', '7', 'agent-a', '00112233445566778899aabbccddeeff', 41, ?, 1,
				?, NULL, NULL, '2026-08-28T12:00:00Z', '2026-08-28T12:00:00Z', ?, NULL)`,
				runID, status, "req."+attemptID, nullString(terminalAt)); err != nil {
				return err
			}
			_, err := tx.Exec(`INSERT INTO attempts(
				run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
				created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
				VALUES (?, 1, ?, 42, ?, 900, 1, ?, '2026-08-28T12:00:00Z',
				'2026-08-28T12:15:00Z', '2026-08-28T12:00:00Z', ?, NULL, NULL)`,
				runID, attemptID, map[string]string{"running": "running", "succeeded": "succeeded"}[status],
				"req."+attemptID, nullString(terminalAt))
			return err
		})
	}
	if err := insertRun(1, "running", "", "11223344556677889900aabbccddeeff"); err != nil {
		t.Fatalf("deferred run/attempt insert: %v", err)
	}
	if err := insertRun(2, "running", "", "21223344556677889900aabbccddeeff"); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("second nonterminal run = %v, want partial-unique refusal", err)
	}
	if _, err := db.db.Exec(`INSERT INTO attempts(
		run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
		created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
		VALUES (1, 2, '31223344556677889900aabbccddeeff', 42, 'failed', 900, 1,
		'req.unique-token', '2026-08-28T12:00:00Z', '2026-08-28T12:15:00Z',
		'2026-08-28T12:00:00Z', '2026-08-28T12:00:00Z', NULL, NULL)`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate attempt token = %v, want uniqueness refusal", err)
	}
	if _, err := db.db.Exec(`INSERT INTO attempts(
		run_id,attempt_no,acquisition_id,attempt_token,status,ttl_seconds,lease_generation,
		created_request_id,started_at,expires_at,updated_at,terminal_at,terminal_result_digest,extra)
		VALUES (1, 2, '41223344556677889900aabbccddeeff', 43, 'failed', 900, 1,
		'req.11223344556677889900aabbccddeeff', '2026-08-28T12:00:00Z', '2026-08-28T12:15:00Z',
		'2026-08-28T12:00:00Z', '2026-08-28T12:00:00Z', NULL, NULL)`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate attempt request id = %v, want uniqueness refusal", err)
	}

	insertV3Claim(t, db, "8", 44, "2026-08-28T12:00:00Z", `{"acquisitionId":"50112233445566778899aabbccddeeff"}`)
	if _, err := db.db.Exec(`INSERT INTO runs(
		run_id,tag,task_id,owner,claim_acquisition_id,claim_token,status,current_attempt,
		start_request_id,terminal_result_digest,force_cancel_fence,started_at,updated_at,terminal_at,extra)
		VALUES (2, 'main', '8', 'agent-a', '50112233445566778899aabbccddeeff', 44,
		'running', 1, 'req.11223344556677889900aabbccddeeff', NULL, NULL,
		'2026-08-28T12:00:00Z', '2026-08-28T12:00:00Z', NULL, NULL)`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate start request id = %v, want uniqueness refusal", err)
	}
	if _, err := db.db.Exec(`UPDATE attempts SET status='cancelled', terminal_at='2026-08-28T12:01:00Z' WHERE run_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE runs SET status='cancelled', force_cancel_fence=50, terminal_at='2026-08-28T12:01:00Z' WHERE run_id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`INSERT INTO runs(
		run_id,tag,task_id,owner,claim_acquisition_id,claim_token,status,current_attempt,
		start_request_id,terminal_result_digest,force_cancel_fence,started_at,updated_at,terminal_at,extra)
		VALUES (2, 'main', '8', 'agent-a', '50112233445566778899aabbccddeeff', 44,
		'cancelled', 1, 'req.distinct', NULL, 50,
		'2026-08-28T12:00:00Z', '2026-08-28T12:01:00Z', '2026-08-28T12:01:00Z', NULL)`); err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate force-cancel fence = %v, want uniqueness refusal", err)
	}
	if _, err := db.db.Exec(`DELETE FROM tasks WHERE tag='main' AND id='7'`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("delete task with run = %v, want restrictive-FK refusal", err)
	}
}

// A conflicting prerelease object is corruption, not a reason to stamp the
// store current. Plain versioned DDL fails and rolls back objects created
// earlier in the same migration transaction.
func TestV2UpgradeConflictingSchemaRollsBack(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	path := makeV2Store(t, dir, owner)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SetOwnerBoundary(owner); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TABLE plan_mappings (wrong TEXT)`); err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("conflicting schema upgrade = %v", err)
	}
	if version, err := db.userVersion(); err != nil || version != identitySchemaVersion {
		t.Fatalf("refused upgrade schema = %d, %v; want %d", version, err, identitySchemaVersion)
	}
	for _, name := range []string{"plans", "assignments", "assignments_owner_task"} {
		if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name); n != 0 {
			t.Fatalf("refused upgrade published schema object %q", name)
		}
	}
}

// A v1 store with no configured owner boundary fails closed on mutation:
// identity is never minted from a guessed boundary.
func TestUpgradeWithoutOwnerBoundaryFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := makeV1Store(t, dir)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.EnsureTag("main")
	if err == nil || !strings.Contains(err.Error(), "owner boundary") {
		t.Fatalf("expected missing-owner refusal, got %v", err)
	}
	if v, verr := db.userVersion(); verr != nil || v != 1 {
		t.Fatalf("user_version %d (%v), want unchanged 1", v, verr)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM sqlite_master WHERE name='receipts'`); n != 0 {
		t.Fatal("refused mutation still applied DDL")
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM meta WHERE key IN ('familyId','ownerBoundary')`); n != 0 {
		t.Fatal("refused mutation still minted identity")
	}
}

// Read-only opens never upgrade or mint.
func TestReadOnlyOpenNeverUpgrades(t *testing.T) {
	dir := t.TempDir()
	path := makeV1Store(t, dir)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, _, err := db.Identity(); err != nil {
		t.Fatal(err)
	}
	if err := db.CheckIdentity(filepath.Join(dir, "f")); err == nil ||
		!strings.Contains(err.Error(), "no family identity") {
		t.Fatalf("CheckIdentity on v1 = %v, want absent-identity refusal", err)
	}
	if v, verr := db.userVersion(); verr != nil || v != 1 {
		t.Fatalf("user_version %d (%v), want 1", v, verr)
	}
}

// A partial identity is never repaired; an invalid familyId UUID or a
// supplied canonical-owner mismatch is never accepted.
func TestEnsureIdentityPairValidation(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	other := filepath.Join(dir, "other")
	valid := uuid.NewString()

	cases := []struct {
		name     string
		rows     [][2]string
		supply   string
		wantErr  string
		checkErr string // expected from CheckIdentity; empty skips the check
	}{
		{"partial family id", [][2]string{{"familyId", valid}}, owner, "partial family identity", ""},
		{"partial owner boundary", [][2]string{{"ownerBoundary", owner}}, owner, "partial family identity", ""},
		{"invalid uuid", [][2]string{{"familyId", "not-a-uuid"}, {"ownerBoundary", owner}}, owner, "not a valid UUID", "not a valid UUID"},
		{"empty family id", [][2]string{{"familyId", ""}, {"ownerBoundary", owner}}, owner, "not a valid UUID", ""},
		{"owner mismatch", [][2]string{{"familyId", valid}, {"ownerBoundary", owner}}, other, "does not match", "does not match"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "-")+".sqlite")
			db, err := Create(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, kv := range tc.rows {
				if _, err := db.db.Exec(`INSERT INTO meta(key, value) VALUES (?, ?)`, kv[0], kv[1]); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.EnsureIdentity(tc.supply); err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("EnsureIdentity = %v, want %q", err, tc.wantErr)
			}
			// Nothing was repaired or rewritten.
			for _, kv := range tc.rows {
				var got string
				if err := db.db.QueryRow(`SELECT value FROM meta WHERE key=?`, kv[0]).Scan(&got); err != nil || got != kv[1] {
					t.Fatalf("meta[%s] = %q (%v), want unchanged %q", kv[0], got, err, kv[1])
				}
			}
			if tc.checkErr != "" {
				if err := db.CheckIdentity(tc.supply); err == nil || !strings.Contains(err.Error(), tc.checkErr) {
					t.Fatalf("CheckIdentity = %v, want %q", err, tc.checkErr)
				}
			}
		})
	}
}

// A wholly absent identity mints exactly once and validates on re-run.
func TestEnsureIdentityMintIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	owner := filepath.Join(dir, "family")
	db, err := Create(filepath.Join(dir, "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.EnsureIdentity(owner); err != nil {
		t.Fatal(err)
	}
	first, stored, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureIdentity(owner); err != nil {
		t.Fatal(err)
	}
	again, stored2, _ := db.Identity()
	if first != again || stored != stored2 || stored != owner {
		t.Fatalf("identity changed across EnsureIdentity runs")
	}
}

// Receipt replay: the same (kind, request_id) can only ever name one receipt,
// so replay lookup is deterministic and a divergent reuse is refused.
func TestReceiptRequestIDUniqueness(t *testing.T) {
	dir := t.TempDir()
	db, err := Create(filepath.Join(dir, "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	err = db.immediate(func(t *tx) error {
		_, err := t.insertReceipt(ReceiptKindRestore, "r-a", "req-1",
			map[string]any{"receiptId": "r-a"}, "2026-01-01T00:00:00Z")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	// Replaying the same request with the same input names the same receipt
	// (same deterministic id) and is accepted.
	err = db.immediate(func(t *tx) error {
		_, err := t.insertReceipt(ReceiptKindRestore, "r-a", "req-1",
			map[string]any{"receiptId": "r-a"}, "2026-01-01T00:00:00Z")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("same-id re-insert = %v, want primary-key refusal", err)
	}

	// The same request id with a different input derives a different receipt
	// id: the partial uniqueness index must refuse it rather than fork
	// replay state.
	err = db.immediate(func(t *tx) error {
		_, err := t.insertReceipt(ReceiptKindRestore, "r-b", "req-1",
			map[string]any{"receiptId": "r-b"}, "2026-01-02T00:00:00Z")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "UNIQUE") {
		t.Fatalf("duplicate (kind, request_id) insert = %v, want uniqueness refusal", err)
	}

	// Deterministic replay lookup: the survivor is exactly the first record.
	got, err := db.FindReceipt(ReceiptKindRestore, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got["receiptId"] != "r-a" {
		t.Fatalf("replay lookup returned %+v, want r-a", got)
	}
}
