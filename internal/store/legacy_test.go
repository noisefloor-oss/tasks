package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// legacyFixture is one fully-populated valid legacy live store document:
// two tags' worth of state compressed into one, with unknown fields at every
// level (task, claim, worktree, tag state, event), a milestone registry, a
// claim, a binding, dependencies, subtasks, and a retained event.
const legacyFixture = `{
  "schemaVersion": 1,
  "fencing": 7,
  "nextSeq": 4,
  "base": {"hash": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "tags": {}},
  "tags": {
    "main": {
      "nextId": 3,
      "taskCount": 2,
      "completedCount": 1,
      "milestones": [{"name": "m1", "status": "open", "order": 1}],
      "customTagField": {"x": 1},
      "tasks": [
        {
          "task": {"id": 1, "title": "One", "description": "first", "details": "keep", "status": "done", "dependencies": [], "updatedAt": "2026-01-02T03:04:05Z", "_custom": {"k": true}},
          "revision": 2,
          "claim": {"owner": "agent", "token": 7, "ttl": 300, "acquiredAt": "2026-01-02T03:04:05Z", "expiresAt": "2026-01-02T03:09:05Z", "_claimExtra": 1},
          "_outerRec": {"seen": true}
        },
        {
          "task": {"id": 2, "title": "Two", "description": "second", "details": "", "status": "pending", "priority": "high", "milestone": "m1", "dependencies": ["1"], "subtasks": [{"id": 1, "title": "sub", "status": "pending"}], "updatedAt": "2026-01-02T03:04:06Z"},
          "revision": 1,
          "origin": "migrate",
          "worktree": {"path": "/repo/.worktrees/main-2", "boundary": "/repo", "branch": "tasks/main/2", "createdAt": "2026-01-02T03:04:07Z", "keep": false, "_wtExtra": "z"}
        }
      ]
    },
    "aux": {
      "nextId": 2,
      "taskCount": 1,
      "completedCount": 0,
      "tasks": [
        {"task": {"id": 1, "title": "Aux", "description": "aux", "details": "", "status": "in-progress", "dependencies": [], "updatedAt": "2026-01-02T03:04:08Z"}, "revision": 3}
      ]
    }
  },
  "events": [
    {"seq": 1, "tag": "main", "kind": "add", "revision": 1, "at": "2026-01-02T03:04:05Z", "_eventExtra": "e"}
  ]
}`

// legacyStoreDir creates a store directory holding the fixture live.json and
// returns (storeDir, legacyPath).
func legacyStoreDir(t *testing.T, document string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, LegacyFile)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func TestLegacyFamilyUUIDStableAcrossContentChanges(t *testing.T) {
	a := LegacyFamilyUUID("/repo/.git", "/repo")
	if a == LegacyFamilyUUID("/other/.git", "/repo") {
		t.Fatal("family uuid must depend on the common directory")
	}
	if a == LegacyFamilyUUID("/repo/.git", "/other") {
		t.Fatal("family uuid must depend on the owner boundary")
	}
	// Independent recomputation of the specified derivation, with the RFC
	// 4122 version-5 and variant bits applied after the raw digest bytes.
	h := sha256.New()
	h.Write([]byte("noise-tasks-legacy-family-v1"))
	h.Write([]byte{0})
	h.Write([]byte("/repo/.git"))
	h.Write([]byte{0})
	h.Write([]byte("/repo"))
	sum := h.Sum(nil)
	sum[6] = (sum[6] & 0x0f) | 0x50
	sum[8] = (sum[8] & 0x3f) | 0x80
	want := hex.EncodeToString(sum[0:4]) + "-" + hex.EncodeToString(sum[4:6]) + "-" +
		hex.EncodeToString(sum[6:8]) + "-" + hex.EncodeToString(sum[8:10]) + "-" +
		hex.EncodeToString(sum[10:16])
	if a != want {
		t.Fatalf("family uuid %q, want %q", a, want)
	}

	// Two backups of one family with different content share the family id.
	_, legacy := legacyStoreDir(t, legacyFixture)
	out1 := filepath.Join(t.TempDir(), "b1.ntbackup")
	out2 := filepath.Join(t.TempDir(), "b2.ntbackup")
	m1, err := CreateLegacyBackup(out1, legacy, "/repo/.git", "/repo", "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	if m1.FamilyID != a || a == "" {
		t.Fatalf("family id %q, want derived %q", m1.FamilyID, a)
	}
	changed := strings.ReplaceAll(legacyFixture, `"title": "Two"`, `"title": "Changed"`)
	_, legacy2 := legacyStoreDir(t, changed)
	m2, err := CreateLegacyBackup(out2, legacy2, "/repo/.git", "/repo", "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	if m2.FamilyID != m1.FamilyID {
		t.Fatalf("family id changed with content: %s vs %s", m2.FamilyID, m1.FamilyID)
	}
	if m2.ComparisonDigest == m1.ComparisonDigest {
		t.Fatal("comparison digest must track content changes")
	}
}

func TestLegacyBackupRoundtripAndManifest(t *testing.T) {
	_, legacy := legacyStoreDir(t, legacyFixture)
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "legacy.ntbackup")
	manifest, err := CreateLegacyBackup(out, legacy, "/common", "/owner", "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("legacy live.json must not be mutated by backup create")
	}
	if manifest.SourceKind != SourceKindLegacyJSON || manifest.SourceSchema != LegacySourceSchema {
		t.Fatalf("manifest source %s/%s", manifest.SourceKind, manifest.SourceSchema)
	}
	if manifest.CurrentSchemaVersion != orchestratorSchemaVersion {
		t.Fatalf("current schema %d, want %d", manifest.CurrentSchemaVersion, orchestratorSchemaVersion)
	}
	wantInv := ClassInventory{
		Tags: 2, Milestones: 1, Tasks: 3, Dependencies: 1, Subtasks: 1,
		Bindings: 1, Claims: 1, Events: 1,
	}
	if manifest.Inventory != wantInv {
		t.Fatalf("inventory %+v, want %+v", manifest.Inventory, wantInv)
	}
	// No authority was published next to the legacy source.
	if _, err := os.Lstat(filepath.Join(filepath.Dir(legacy), "tasks.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("tasks.sqlite must not be published: %v", err)
	}

	// The created payload carries exactly the legacy retained event log:
	// no synthetic schema, import, or upgrade events were appended.
	scratch := t.TempDir()
	payloadCopy := filepath.Join(scratch, PayloadFile)
	if err := copyFile(filepath.Join(out, PayloadFile), payloadCopy); err != nil {
		t.Fatal(err)
	}
	payloadDB, err := openUnlocked(payloadCopy, false, "")
	if err != nil {
		t.Fatal(err)
	}
	if version, err := payloadDB.userVersion(); err != nil || version != orchestratorSchemaVersion {
		payloadDB.Close()
		t.Fatalf("legacy payload schema = %d, %v; want %d", version, err, orchestratorSchemaVersion)
	}
	payloadSnap, err := payloadDB.Snapshot()
	payloadDB.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(payloadSnap.Events) != 1 || payloadSnap.Events[0].Seq != 1 ||
		payloadSnap.Events[0].Kind != "add" || payloadSnap.Events[0].Extra["_eventExtra"] == nil {
		t.Fatalf("payload feed is not the exact legacy event log: %+v", payloadSnap.Events)
	}
	var payloadClaim *Claim
	for _, task := range payloadSnap.Tags["main"].Tasks {
		if task.ID == "1" {
			payloadClaim = task.Claim
		}
	}
	if got := claimAcquisitionID(payloadClaim); got != "" {
		t.Fatalf("legacy compatibility proof payload was augmented with acquisition id %q", got)
	}

	// The bundle verifies context-free.
	verified, err := VerifyBundle(out)
	if err != nil {
		t.Fatalf("verify legacy bundle: %v", err)
	}
	if *verified != *manifest {
		t.Fatalf("verified manifest %+v differs from created %+v", *verified, *manifest)
	}

	// The payload restores to SQLite (adoption onto a fresh boundary).
	target := filepath.Join(t.TempDir(), "tasks.sqlite")
	result, err := Restore(out, target, RestoreOptions{RequestID: "legacy-1", OwnerBoundary: "/owner"})
	if err != nil {
		t.Fatalf("restore legacy bundle: %v", err)
	}
	if !result.Adopted {
		t.Fatalf("expected adoption, got %+v", result)
	}
	db, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	family, owner, err := db.Identity()
	if err != nil || family != manifest.FamilyID || owner != "/owner" {
		t.Fatalf("restored identity %q/%q (%v), want %q//owner", family, owner, err, manifest.FamilyID)
	}
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snap.Fencing != 8 {
		t.Fatalf("fencing %d, want 8 after legacy saga allocation", snap.Fencing)
	}
	main := snap.Tags["main"]
	if main == nil || main.Info.NextID != 3 || main.Info.TaskCount != 2 || main.Info.CompletedCount != 1 {
		t.Fatalf("main tag info %+v", main.Info)
	}
	if len(main.Info.Extra) != 1 || main.Info.Extra["customTagField"] == nil {
		t.Fatalf("tag unknown fields lost: %+v", main.Info.Extra)
	}
	if len(main.Info.Milestones) == 0 {
		t.Fatal("milestone registry lost")
	}
	byID := map[string]*Task{}
	for _, task := range main.Tasks {
		byID[task.ID] = task
	}
	one := byID["1"]
	if one == nil || one.Revision != 2 || one.Details != "keep" || one.Extra["_custom"] == nil {
		t.Fatalf("task one not preserved: %+v", one)
	}
	if len(one.OuterExtra) != 1 || one.OuterExtra["_outerRec"] == nil {
		t.Fatalf("task one outer-record unknown fields lost: %+v", one.OuterExtra)
	}
	if one.Claim == nil || one.Claim.Owner != "agent" || one.Claim.Token != 7 || one.Claim.Extra["_claimExtra"] == nil {
		t.Fatalf("claim not preserved: %+v", one.Claim)
	}
	wantClaimID := legacyClaimAcquisitionID(manifest.FamilyID, "main", "1", 7, "2026-01-02T03:04:05Z")
	if got := claimAcquisitionID(one.Claim); got != wantClaimID || !validAcquisitionID(got) {
		t.Fatalf("restored legacy claim acquisition id %q, want deterministic %q", got, wantClaimID)
	}
	two := byID["2"]
	if two == nil || two.Priority != "high" || two.Milestone != "m1" || len(two.Dependencies) != 1 || two.Dependencies[0] != "1" {
		t.Fatalf("task two not preserved: %+v", two)
	}
	if len(two.OuterExtra) != 1 || two.OuterExtra["origin"] == nil {
		t.Fatalf("task two outer-record unknown fields lost: %+v", two.OuterExtra)
	}
	if two.Binding == nil || two.Binding.Branch != "tasks/main/2" || two.Binding.Extra["_wtExtra"] == nil {
		t.Fatalf("binding not preserved: %+v", two.Binding)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("restored worktree saga state: %v", err)
	}
	var sagas int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas WHERE tag='main' AND task_id='2'
		AND operation='remove' AND status='cleanup-required' AND recovery_generation=0`).Scan(&sagas); err != nil || sagas != 1 {
		t.Fatalf("restored legacy saga count=%d %v", sagas, err)
	}
	// The private v3→v5 upgrade and restore append their schema and receipt
	// events after the legacy log. The first is exactly legacy nextSeq (4),
	// not the AUTOINCREMENT default (2) that the truncated retained log
	// (max seq 1) would otherwise hand out.
	if len(snap.Events) != 3 || snap.Events[0].Seq != 1 || snap.Events[0].Kind != "add" ||
		snap.Events[1].Seq != 4 || snap.Events[1].Kind != "schema" ||
		snap.Events[2].Seq != 5 || snap.Events[2].Kind != "restore" {
		t.Fatalf("restored feed: %+v", snap.Events)
	}
}

func TestLegacyBackupRefusals(t *testing.T) {
	// Wrong source path: the exact derived live.json must exist.
	if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), filepath.Join(t.TempDir(), "missing.json"), "/c", "/o", "t"); err == nil {
		t.Fatal("missing legacy source must refuse")
	}
	// Dependency on a nonexistent task.
	bad := strings.ReplaceAll(legacyFixture, `"dependencies": ["1"]`, `"dependencies": ["9"]`)
	_, legacy := legacyStoreDir(t, bad)
	if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), legacy, "/c", "/o", "t"); err == nil {
		t.Fatal("missing dependency must refuse")
	}
	// Dependency cycle.
	cycled := strings.ReplaceAll(legacyFixture,
		`"task": {"id": 1, "title": "One", "description": "first", "details": "keep", "status": "done", "dependencies": [],`,
		`"task": {"id": 1, "title": "One", "description": "first", "details": "keep", "status": "done", "dependencies": ["2"],`)
	_, legacy = legacyStoreDir(t, cycled)
	if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), legacy, "/c", "/o", "t"); err == nil {
		t.Fatal("dependency cycle must refuse")
	}
	// Unsupported document version.
	wrongVersion := strings.ReplaceAll(legacyFixture, `"schemaVersion": 1`, `"schemaVersion": 2`)
	_, legacy = legacyStoreDir(t, wrongVersion)
	if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), legacy, "/c", "/o", "t"); err == nil {
		t.Fatal("unsupported legacy schema version must refuse")
	}
	// Tampered task status fails §1 validation.
	badStatus := strings.ReplaceAll(legacyFixture, `"status": "done"`, `"status": "bogus"`)
	_, legacy = legacyStoreDir(t, badStatus)
	if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), legacy, "/c", "/o", "t"); err == nil {
		t.Fatal("invalid task status must refuse")
	}
	// Every invalid milestone-registry class refuses before publication.
	ms := strings.ReplaceAll(legacyFixture,
		`"milestones": [{"name": "m1", "status": "open", "order": 1}]`, `%s`)
	for name, reg := range map[string]string{
		"entry not an object":    `"milestones": [5]`,
		"missing name":           `"milestones": [{"status": "open", "order": 1}]`,
		"missing status":         `"milestones": [{"name": "m1", "order": 1}]`,
		"status outside grammar": `"milestones": [{"name": "m1", "status": "proposed", "order": 1}]`,
		"padded name":            `"milestones": [{"name": " m1", "status": "open", "order": 1}]`,
		"empty name":             `"milestones": [{"name": "", "status": "open", "order": 1}]`,
		"leading hyphen name":    `"milestones": [{"name": "-m1", "status": "open", "order": 1}]`,
		"oversized name":         `"milestones": [{"name": "` + strings.Repeat("a", 257) + `", "status": "open"}]`,
		"control character name": `"milestones": [{"name": "m\u00011", "status": "open"}]`,
		"duplicate exact names":  `"milestones": [{"name": "m1", "status": "open"}, {"name": "m1", "status": "closed"}]`,
	} {
		t.Run("milestone/"+name, func(t *testing.T) {
			_, legacy := legacyStoreDir(t, strings.ReplaceAll(ms, `%s`, reg))
			if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), legacy, "/c", "/o", "t"); err == nil {
				t.Fatalf("invalid milestone registry (%s) must refuse", name)
			}
		})
	}
	// nextSeq must be positive and strictly above every retained event seq.
	for name, seq := range map[string]string{
		"zero":                               `"nextSeq": 0`,
		"negative":                           `"nextSeq": -3`,
		"not above max retained seq (equal)": `"nextSeq": 1`,
		"not above max retained seq (below)": `"nextSeq": -0`,
	} {
		t.Run("nextSeq/"+name, func(t *testing.T) {
			doc := strings.ReplaceAll(legacyFixture, `"nextSeq": 4`, seq)
			_, legacy := legacyStoreDir(t, doc)
			if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), legacy, "/c", "/o", "t"); err == nil {
				t.Fatalf("invalid nextSeq (%s) must refuse", name)
			}
		})
	}
	// (The former "inner unknown field named outerExtra must refuse" case is
	// gone: the v1 task record nests the task object, so an inner outerExtra
	// and an outer-record outerExtra coexist — see
	// TestLegacyInnerAndOuterOuterExtraCoexist.)
}

// The nested task record makes inner and outer unknown-field namespaces
// disjoint: a task-object unknown field named "outerExtra" and an
// outer-record unknown field of the same name coexist, both feed the
// digests, and both survive verification and restore.
func TestLegacyInnerAndOuterOuterExtraCoexist(t *testing.T) {
	both := legacyFixture
	both = strings.ReplaceAll(both,
		`"_custom": {"k": true}`,
		`"_custom": {"k": true}, "outerExtra": {"inner": 1}`)
	both = strings.ReplaceAll(both,
		`"_outerRec": {"seen": true}`,
		`"_outerRec": {"seen": true}, "outerExtra": {"outer": 2}`)
	_, legacy := legacyStoreDir(t, both)
	bundle := filepath.Join(t.TempDir(), "both.ntbackup")
	manifest, err := CreateLegacyBackup(bundle, legacy, "/c", "/o", "t")
	if err != nil {
		t.Fatalf("coexisting inner/outer outerExtra fields must back up: %v", err)
	}
	// Both sides affect the digests versus the plain fixture.
	base := filepath.Join(t.TempDir(), "base.ntbackup")
	_, lbase := legacyStoreDir(t, legacyFixture)
	mbase, err := CreateLegacyBackup(base, lbase, "/c", "/o", "t")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ComparisonDigest == mbase.ComparisonDigest {
		t.Fatal("coexisting outerExtra fields must change the comparison digest")
	}
	if manifest.SemanticDigest == mbase.SemanticDigest {
		t.Fatal("coexisting outerExtra fields must change the semantic digest")
	}
	// The bundle verifies (Restore re-verifies integrity and digests) and
	// restores both unknown fields byte-faithfully.
	target := filepath.Join(t.TempDir(), "tasks.sqlite")
	if _, err := Restore(bundle, target, RestoreOptions{RequestID: "oe-2", OwnerBoundary: "/o"}); err != nil {
		t.Fatalf("restore of coexisting extras must verify: %v", err)
	}
	db, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range snap.Tags["main"].Tasks {
		if task.ID != "1" {
			continue
		}
		if inner := string(task.Extra["outerExtra"]); !strings.Contains(inner, `"inner":1`) && !strings.Contains(inner, `"inner": 1`) {
			t.Fatalf("inner outerExtra lost: %q", inner)
		}
		if outer := string(task.OuterExtra["outerExtra"]); !strings.Contains(outer, `"outer":2`) && !strings.Contains(outer, `"outer": 2`) {
			t.Fatalf("outer outerExtra lost: %q", outer)
		}
		return
	}
	t.Fatal("task 1 missing after restore")
}

// A changed outer-record unknown field must change the comparison digest:
// the outer record is compared content, not transport debris.
func TestLegacyOuterExtraChangesComparisonDigest(t *testing.T) {
	base := legacyFixture
	changed := strings.ReplaceAll(base, `"_outerRec": {"seen": true}`, `"_outerRec": {"seen": false}`)
	out1 := filepath.Join(t.TempDir(), "b1.ntbackup")
	out2 := filepath.Join(t.TempDir(), "b2.ntbackup")
	_, l1 := legacyStoreDir(t, base)
	_, l2 := legacyStoreDir(t, changed)
	m1, err := CreateLegacyBackup(out1, l1, "/c", "/o", "t")
	if err != nil {
		t.Fatal(err)
	}
	m2, err := CreateLegacyBackup(out2, l2, "/c", "/o", "t")
	if err != nil {
		t.Fatal(err)
	}
	if m1.ComparisonDigest == m2.ComparisonDigest {
		t.Fatal("comparison digest must track outer-record unknown fields")
	}
	if m1.SemanticDigest == m2.SemanticDigest {
		t.Fatal("semantic digest must track outer-record unknown fields")
	}
	// Both bundles still verify and restore; the changed extra survives.
	target := filepath.Join(t.TempDir(), "tasks.sqlite")
	if _, err := Restore(out2, target, RestoreOptions{RequestID: "oe-1", OwnerBoundary: "/o"}); err != nil {
		t.Fatal(err)
	}
	db, err := Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snap, err := db.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range snap.Tags["main"].Tasks {
		if task.ID == "1" {
			raw := string(task.OuterExtra["_outerRec"])
			if !strings.Contains(raw, `"seen":false`) && !strings.Contains(raw, `"seen": false`) {
				t.Fatalf("changed outer extra not restored byte-faithfully: %s", raw)
			}
			return
		}
	}
	t.Fatal("task 1 missing after restore")
}

func TestLegacyDuplicateKeysRejected(t *testing.T) {
	docs := map[string]string{
		"top level": strings.Replace(legacyFixture, `"fencing": 7`, `"fencing": 7, "fencing": 7`, 1),
		"nested task object": strings.Replace(
			legacyFixture,
			`"title": "One", "description": "first"`,
			`"title": "One", "title": "One", "description": "first"`, 1),
		"nested unknown field": strings.Replace(
			legacyFixture,
			`"customTagField": {"x": 1}`,
			`"customTagField": {"x": 1, "x": 2}`, 1),
		"event object": strings.Replace(
			legacyFixture,
			`"kind": "add", "revision": 1`,
			`"kind": "add", "kind": "add", "revision": 1`, 1),
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			_, legacy := legacyStoreDir(t, doc)
			out := filepath.Join(t.TempDir(), "x.ntbackup")
			_, err := CreateLegacyBackup(out, legacy, "/c", "/o", "t")
			if err == nil || !strings.Contains(err.Error(), "duplicate object key") {
				t.Fatalf("expected duplicate-key refusal, got %v", err)
			}
			if _, statErr := os.Lstat(out); statErr == nil {
				t.Fatal("no bundle may be published from a duplicate-key document")
			}
		})
	}
}

func TestLegacyDocumentSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real.json")
	if err := os.WriteFile(real, []byte(legacyFixture), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, LegacyFile)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateLegacyBackup(filepath.Join(t.TempDir(), "x.ntbackup"), link, "/c", "/o", "t"); err == nil ||
		!strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("expected symlink refusal, got %v", err)
	}
}
