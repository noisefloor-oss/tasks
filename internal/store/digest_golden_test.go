package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// §8.5 golden vectors: fixed, immutable digests computed independently of
// this implementation (a from-spec rendering of the framing and canonical
// JSON), so the tests pin the algorithm rather than comparing the
// implementation to itself. The fixture is fully deterministic — fixed
// identity, counters, timestamps — and exercises Unicode titles, owners,
// branches, and unknown-field keys whose raw UTF-8 byte order differs from
// both insertion order and code-point order, nested unknown values with
// unsorted keys, absent optional fields, and every populated record class.
//
// If a legitimate schema change alters a record shape, regenerate both sides
// together and record why in the change; never edit the constants to match
// a new implementation output without an independent recomputation.
const (
	goldenFamilyID    = "6f9619ff-8b86-d011-b42d-00cf4fc964ff"
	goldenBoundary    = "/gold/boundary"
	goldenSemantic    = "sha256:67a8062d459674fc05264cfe198f0727fb3ea310a8a98c3b11ac4e36d4dedac0"
	goldenComparison  = "sha256:52680770e25a633ca8248e3c8900aeee3de357e2309115c8cf068d2ae61331ac"
	goldenRunSemantic = "sha256:cbd63bd44a8de7638476294c54574b68c8c164b329369aaf4f45548f3048bc04"
	goldenRunCompare  = "sha256:d40075d9a13c3a1e83a39a05aff4b03c91c256e74bcce0f22478d530bd90061b"
	goldenTaskExtra   = `{"é-key":"vø","zzNested":{"b":[1,"a",true,null],"a":0}}`
	goldenSubtasks    = `{"parentTaskId":"2","id":1,"title":"süb","dependencies":[],"status":"pending","description":"","details":""}`
	goldenReceiptBody = `{"committedAt":"2026-01-03T00:00:00Z","schemaVersion":1,"kind":"restore","receiptId":"rec-1","zz":null,"aa":{"é":1}}`
)

// newGoldenStore builds the deterministic golden fixture by direct SQL so no
// clock, UUID mint, or counter allocation participates in the digest.
func newGoldenStore(t *testing.T) *DB {
	t.Helper()
	db, err := Create(filepath.Join(t.TempDir(), "noise-tasks", "tasks.sqlite"))
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.db.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`INSERT OR REPLACE INTO meta(key, value) VALUES ('familyId', ?), ('ownerBoundary', ?), ('fencing', '7')`,
		goldenFamilyID, goldenBoundary)
	exec(`INSERT INTO tags(name, next_id, task_count, completed_count, milestones, extra) VALUES
		('main', 3, 2, 1, '["m1","m2"]', '{"zzTag":1,"aTag":null}'),
		('ünï-tag', 1, 0, 0, NULL, NULL)`)
	exec(`INSERT INTO tasks(tag, id, title, description, details, status, priority, milestone,
		dependencies, subtasks, extra, revision, updated_at,
		claim_owner, claim_token, claim_ttl, claim_acquired_at, claim_expires_at,
		wt_path, wt_boundary, wt_branch, wt_created_at, wt_keep) VALUES
		('main', '1', 'título — naïve ✓', '', 'line'||char(10)||'break'||char(9)||'tab', 'done', NULL, 'm1',
		 '[]', '[]', ?, 2, '2026-01-02T03:04:05Z',
		 'ägents', 11, 600, '2026-01-02T03:00:00Z', '2026-01-02T03:10:00Z',
		 NULL, NULL, NULL, NULL, NULL),
		('main', '2', 'second', 'd', '', 'pending', 'high', NULL,
		 '["1"]', ?, NULL, 1, '2026-01-01T00:00:00Z',
		 NULL, NULL, NULL, NULL, NULL,
		 '/wt/päth', ?, 'br-ün', '2026-01-01T00:00:00Z', 1)`,
		goldenTaskExtra, "["+goldenSubtasks+"]", goldenBoundary)
	exec(`INSERT INTO events(seq, tag, task_id, kind, status, revision, action, extra, created_at) VALUES
		(1, 'main', '1', 'task', 'done', 2, 'updated', NULL, '2026-01-02T03:04:05Z'),
		(2, NULL, NULL, 'restore', NULL, NULL, 'restored', '{"receiptId":"r-1"}', '2026-01-03T00:00:00Z')`)
	exec(`INSERT INTO receipts(kind, receipt_id, request_id, body, digest, created_at) VALUES
		('restore', 'rec-1', 'req-1', ?, 'sha256:0000000000000000000000000000000000000000000000000000000000000000', '2026-01-03T00:00:00Z')`,
		goldenReceiptBody)
	return db
}

// TestDigestGoldenVectors pins the §8.5 version-1 semantic and comparison
// digests of the deterministic fixture to independently computed constants.
func TestDigestGoldenVectors(t *testing.T) {
	db := newGoldenStore(t)
	defer db.Close()
	semantic, comparison, inv, err := db.ClassDigests()
	if err != nil {
		t.Fatalf("class digests: %v", err)
	}
	if semantic != goldenSemantic {
		t.Errorf("semantic digest drift:\n got %s\nwant %s", semantic, goldenSemantic)
	}
	if comparison != goldenComparison {
		t.Errorf("comparison digest drift:\n got %s\nwant %s", comparison, goldenComparison)
	}
	wantInv := ClassInventory{
		Tags: 2, Milestones: 2, Tasks: 2, Dependencies: 1, Subtasks: 1,
		Bindings: 1, Claims: 1, Events: 2, Receipts: 1,
	}
	if *inv != wantInv {
		t.Errorf("inventory drift: got %+v want %+v", *inv, wantInv)
	}
}

// TestDigestGoldenIndependentFraming recomputes both digests from the spec's
// framing rules over the same logical records — written out longhand here,
// not via the implementation's record builders — and cross-checks the golden
// constants. This fails if either the constants or the framing drift.
func TestDigestGoldenIndependentFraming(t *testing.T) {
	sem, cmp := independentGoldenDigests(goldenDigestRecords())
	if sem != goldenSemantic {
		t.Errorf("independent framing semantic:\n got %s\nwant %s", sem, goldenSemantic)
	}
	if cmp != goldenComparison {
		t.Errorf("independent framing comparison:\n got %s\nwant %s", cmp, goldenComparison)
	}
}

func goldenDigestRecords() map[string][]string {
	return map[string][]string{
		"store": {`{"familyId":"` + goldenFamilyID + `","ownerBoundary":"` + goldenBoundary + `"}`},
		"tags": {
			`{"aTag":null,"completedCount":1,"name":"main","nextId":3,"taskCount":2,"zzTag":1}`,
			`{"completedCount":0,"name":"ünï-tag","nextId":1,"taskCount":0}`,
		},
		"milestones": {
			`{"index":0,"tag":"main","value":"m1"}`,
			`{"index":1,"tag":"main","value":"m2"}`,
		},
		"tasks": {
			`{"revision":2,"tag":"main","task":{"description":"","details":"line\nbreak\ttab","id":"1","milestone":"m1","status":"done","title":"título — naïve ✓","updatedAt":"2026-01-02T03:04:05Z","zzNested":{"a":0,"b":[1,"a",true,null]},"é-key":"vø"}}`,
			`{"revision":1,"tag":"main","task":{"description":"d","details":"","id":"2","priority":"high","status":"pending","title":"second","updatedAt":"2026-01-01T00:00:00Z"}}`,
		},
		"dependencies": {`{"dependency":"1","index":0,"tag":"main","taskId":"2"}`},
		"subtasks":     {`{"index":0,"parentId":"2","subtask":{"dependencies":[],"description":"","details":"","id":1,"parentTaskId":"2","status":"pending","title":"süb"},"tag":"main"}`},
		"bindings":     {`{"boundary":"` + goldenBoundary + `","branch":"br-ün","createdAt":"2026-01-01T00:00:00Z","keep":true,"path":"/wt/päth","tag":"main","taskId":"2"}`},
		"claims":       {`{"acquiredAt":"2026-01-02T03:00:00Z","expiresAt":"2026-01-02T03:10:00Z","owner":"ägents","tag":"main","taskId":"1","token":11,"ttl":600}`},
		"fence":        {`{"counter":7}`},
		"events": {
			`{"action":"updated","createdAt":"2026-01-02T03:04:05Z","kind":"task","revision":2,"seq":1,"status":"done","tag":"main","task":"1"}`,
			`{"action":"restored","createdAt":"2026-01-03T00:00:00Z","kind":"restore","receiptId":"r-1","seq":2}`,
		},
		"receipts": {`{"kind":"restore","receipt":{"aa":{"é":1},"committedAt":"2026-01-03T00:00:00Z","kind":"restore","receiptId":"rec-1","schemaVersion":1,"zz":null},"receiptId":"rec-1"}`},
	}
}

var goldenComparisonClasses = map[string]bool{
	"tags": true, "milestones": true, "tasks": true, "dependencies": true,
	"subtasks": true, "bindings": true, "claims": true, "fence": true, "events": true,
}

var goldenClassOrder = []string{"store", "tags", "milestones", "tasks", "dependencies", "subtasks",
	"bindings", "assignments", "claims", "fence", "events", "receipts",
	"plans", "planMappings", "runs", "attempts", "worktreeSagas"}

func independentGoldenDigests(records map[string][]string) (semantic, comparison string) {
	sem := sha256.New()
	sem.Write([]byte(semanticDigestDomain))
	cmp := sha256.New()
	cmp.Write([]byte(comparisonDigestDomain))
	for _, class := range goldenClassOrder {
		writeGoldenClass(sem, class, records[class])
		if goldenComparisonClasses[class] {
			writeGoldenClass(cmp, class, records[class])
		}
	}
	return "sha256:" + hex.EncodeToString(sem.Sum(nil)),
		"sha256:" + hex.EncodeToString(cmp.Sum(nil))
}

func writeGoldenClass(h hash.Hash, class string, records []string) {
	h.Write([]byte(class))
	h.Write([]byte{0})
	var frame [8]byte
	for _, record := range records {
		binary.BigEndian.PutUint64(frame[:], uint64(len([]byte(record))))
		h.Write(frame[:])
		h.Write([]byte(record))
	}
	h.Write(make([]byte, 8))
}

// populateGoldenRuns inserts in deliberately non-numeric order. The semantic
// vectors below require the digest to reorder runs by run_id and attempts by
// (run_id, attempt_no), independent of physical insertion order.
func populateGoldenRuns(t *testing.T, db *DB) {
	t.Helper()
	tx, err := db.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}
	exec(`UPDATE meta SET value='40' WHERE key='fencing'`)
	exec(`INSERT INTO runs(run_id, tag, task_id, owner, claim_acquisition_id,
		claim_token, status, current_attempt, start_request_id,
		terminal_result_digest, force_cancel_fence, started_at, updated_at,
		terminal_at, extra) VALUES
		(10, 'main', '1', 'agent-z', ?, 31, 'succeeded', 2, 'req.ten', ?, NULL,
		 '2026-02-10T00:00:00Z', '2026-02-10T00:20:00Z', '2026-02-10T00:20:00Z', ?),
		(2, 'main', '2', 'agent-b', ?, 21, 'cancelled', 1, 'req.two', NULL, 30,
		 '2026-02-01T00:00:00Z', '2026-02-01T00:10:00Z', '2026-02-01T00:10:00Z', NULL)`,
		strings.Repeat("10", 16), "sha256:"+strings.Repeat("a", 64),
		`{"zzRun":{"b":2,"a":1},"aRun":null}`, strings.Repeat("02", 16))
	exec(`INSERT INTO attempts(run_id, attempt_no, acquisition_id, attempt_token,
		status, ttl_seconds, lease_generation, created_request_id, started_at,
		expires_at, updated_at, terminal_at, terminal_result_digest, extra) VALUES
		(10, 2, ?, 33, 'succeeded', 300, 2, 'req.retry',
		 '2026-02-10T00:10:00Z', '2026-02-10T00:15:00Z', '2026-02-10T00:20:00Z',
		 '2026-02-10T00:20:00Z', ?, NULL),
		(2, 1, ?, 22, 'cancelled', 900, 1, 'req.two',
		 '2026-02-01T00:00:00Z', '2026-02-01T00:15:00Z', '2026-02-01T00:10:00Z',
		 '2026-02-01T00:10:00Z', NULL, NULL),
		(10, 1, ?, 32, 'failed', 600, 1, 'req.ten',
		 '2026-02-10T00:00:00Z', '2026-02-10T00:10:00Z', '2026-02-10T00:10:00Z',
		 '2026-02-10T00:10:00Z', ?, ?)`,
		strings.Repeat("12", 16), "sha256:"+strings.Repeat("a", 64),
		strings.Repeat("22", 16), strings.Repeat("11", 16),
		"sha256:"+strings.Repeat("b", 64), `{"zzAttempt":true,"aAttempt":{"é":1}}`)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit run fixture: %v", err)
	}
}

func populatedRunDigestRecords() map[string][]string {
	records := goldenDigestRecords()
	records["fence"] = []string{`{"counter":40}`}
	records["runs"] = []string{
		`{"claimAcquisitionId":"02020202020202020202020202020202","claimToken":21,"currentAttempt":1,"forceCancelFence":30,"owner":"agent-b","runId":2,"startRequestId":"req.two","startedAt":"2026-02-01T00:00:00Z","status":"cancelled","tag":"main","task":"2","terminalAt":"2026-02-01T00:10:00Z","terminalResultDigest":null,"updatedAt":"2026-02-01T00:10:00Z"}`,
		`{"aRun":null,"claimAcquisitionId":"10101010101010101010101010101010","claimToken":31,"currentAttempt":2,"forceCancelFence":null,"owner":"agent-z","runId":10,"startRequestId":"req.ten","startedAt":"2026-02-10T00:00:00Z","status":"succeeded","tag":"main","task":"1","terminalAt":"2026-02-10T00:20:00Z","terminalResultDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","updatedAt":"2026-02-10T00:20:00Z","zzRun":{"a":1,"b":2}}`,
	}
	records["attempts"] = []string{
		`{"acquisitionId":"22222222222222222222222222222222","attempt":1,"attemptToken":22,"createdRequestId":"req.two","expiresAt":"2026-02-01T00:15:00Z","leaseGeneration":1,"runId":2,"startedAt":"2026-02-01T00:00:00Z","status":"cancelled","terminalAt":"2026-02-01T00:10:00Z","terminalResultDigest":null,"ttlSeconds":900,"updatedAt":"2026-02-01T00:10:00Z"}`,
		`{"aAttempt":{"é":1},"acquisitionId":"11111111111111111111111111111111","attempt":1,"attemptToken":32,"createdRequestId":"req.ten","expiresAt":"2026-02-10T00:10:00Z","leaseGeneration":1,"runId":10,"startedAt":"2026-02-10T00:00:00Z","status":"failed","terminalAt":"2026-02-10T00:10:00Z","terminalResultDigest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","ttlSeconds":600,"updatedAt":"2026-02-10T00:10:00Z","zzAttempt":true}`,
		`{"acquisitionId":"12121212121212121212121212121212","attempt":2,"attemptToken":33,"createdRequestId":"req.retry","expiresAt":"2026-02-10T00:15:00Z","leaseGeneration":2,"runId":10,"startedAt":"2026-02-10T00:10:00Z","status":"succeeded","terminalAt":"2026-02-10T00:20:00Z","terminalResultDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ttlSeconds":300,"updatedAt":"2026-02-10T00:20:00Z"}`,
	}
	return records
}

func TestRunAttemptDigestGoldenVectors(t *testing.T) {
	db := newGoldenStore(t)
	defer db.Close()
	populateGoldenRuns(t, db)

	semantic, comparison, inv, err := db.ClassDigests()
	if err != nil {
		t.Fatalf("class digests: %v", err)
	}
	wantSemantic, wantComparison := independentGoldenDigests(populatedRunDigestRecords())
	if wantSemantic != goldenRunSemantic || semantic != goldenRunSemantic {
		t.Errorf("populated run semantic:\n implementation %s\nindependent %s\nwant %s",
			semantic, wantSemantic, goldenRunSemantic)
	}
	if wantComparison != goldenRunCompare || comparison != goldenRunCompare {
		t.Errorf("populated run comparison:\n implementation %s\nindependent %s\nwant %s",
			comparison, wantComparison, goldenRunCompare)
	}
	if semantic == goldenSemantic {
		t.Error("populated runs and attempts did not change semantic digest")
	}
	wantInv := ClassInventory{
		Tags: 2, Milestones: 2, Tasks: 2, Dependencies: 1, Subtasks: 1,
		Bindings: 1, Claims: 1, Events: 2, Receipts: 1, Runs: 2, Attempts: 3,
	}
	if *inv != wantInv {
		t.Errorf("inventory drift: got %+v want %+v", *inv, wantInv)
	}
	var counted *ClassInventory
	if err := db.deferred(func(tx *tx) error {
		var err error
		counted, err = countClassInventory(tx)
		return err
	}); err != nil {
		t.Fatalf("count class inventory: %v", err)
	}
	if *counted != wantInv {
		t.Errorf("physical inventory drift: got %+v want %+v", *counted, wantInv)
	}
}

func TestRunAttemptDigestComparisonStability(t *testing.T) {
	db := newGoldenStore(t)
	defer db.Close()
	if _, err := db.db.Exec(`UPDATE meta SET value='40' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	_, before, _, err := db.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	populateGoldenRuns(t, db)
	_, after, _, err := db.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	if after != before || after != goldenRunCompare {
		t.Errorf("run/attempt comparison drift: before %s after %s want %s", before, after, goldenRunCompare)
	}
}

func TestRunAttemptDigestClassesEmptyBeforeV4(t *testing.T) {
	for _, version := range []int{2, 3} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			db := newGoldenStore(t)
			defer db.Close()
			db.db.SetMaxOpenConns(1)
			if _, err := db.db.Exec(`PRAGMA foreign_keys=OFF;
				DROP TABLE attempts;
				DROP TABLE runs;`); err != nil {
				t.Fatalf("remove v4 catalog: %v", err)
			}
			if version == 2 {
				if _, err := db.db.Exec(`DROP INDEX assignments_owner_task;
					DROP TABLE assignments;
					DROP TABLE plan_mappings;
					DROP TABLE plans;`); err != nil {
					t.Fatalf("remove v3 catalog: %v", err)
				}
			}
			if _, err := db.db.Exec(`PRAGMA user_version=` + strconv.Itoa(version) + `;
				PRAGMA foreign_keys=ON`); err != nil {
				t.Fatalf("rewind to v%d: %v", version, err)
			}
			semantic, comparison, inv, err := db.ClassDigests()
			if err != nil {
				t.Fatalf("v%d class digests: %v", version, err)
			}
			if semantic != goldenSemantic || comparison != goldenComparison {
				t.Errorf("v%d digest drift: semantic %s comparison %s", version, semantic, comparison)
			}
			if inv.Runs != 0 || inv.Attempts != 0 {
				t.Errorf("v%d run inventory = (%d,%d), want zero", version, inv.Runs, inv.Attempts)
			}
		})
	}
}

func TestRunAttemptDigestExtraCollision(t *testing.T) {
	tests := []struct {
		name, update, want string
	}{
		{"run", `UPDATE runs SET extra='{"runId":99}' WHERE run_id=2`, `run 2: unknown field "runId" collides with a logical field`},
		{"attempt", `UPDATE attempts SET extra='{"attemptToken":99}' WHERE run_id=2 AND attempt_no=1`, `run 2 attempt 1: unknown field "attemptToken" collides with a logical field`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newGoldenStore(t)
			defer db.Close()
			populateGoldenRuns(t, db)
			if _, err := db.db.Exec(tt.update); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := db.ClassDigests(); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("collision error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
