package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// adoptionDoc is one §1 retired-layout document exercising the §8.6A
// fidelity contract: two tags (a sole legacy "master" kept verbatim plus
// "room-mvp"), non-dense ids 19..38, all seven statuses, top-level and
// subtask dependencies, byte-faithful subtasks, unknown fields on tasks and
// the tag block, verbatim updatedAt values, and a milestones registry with
// an unknown entry field.
const adoptionDoc = `{
  "master": {
    "milestones": [
      {"name": "fleet hardening", "status": "open"},
      {"name": "v1", "status": "closed", "notes": "kept verbatim"}
    ],
    "tasks": [
      {"id": "19", "title": "Relay cutover", "description": "d19", "details": "line one\nline two", "status": "pending", "dependencies": ["38"], "subtasks": [
        {"id": 1, "title": "sub one", "description": "", "details": "", "status": "pending", "dependencies": [], "parentTaskId": "19"},
        {"id": 2, "title": "sub two", "description": "", "details": "", "status": "done", "dependencies": ["1"], "parentTaskId": "19"}
      ], "updatedAt": "2026-10-01T10:00:00Z", "triage": {"score": 7, "note": "odd field"}},
      {"id": "38", "title": "Research agent", "description": "d38", "details": "", "status": "in-progress", "priority": "low", "dependencies": [], "subtasks": [], "updatedAt": "2026-10-02T11:00:00Z", "extraFlag": true},
      {"id": "20", "title": "review one", "description": "", "details": "", "status": "review", "priority": "high", "dependencies": ["19"], "milestone": "fleet hardening", "subtasks": [], "updatedAt": "2026-10-03T12:00:00Z"},
      {"id": "21", "title": "done one", "description": "", "details": "", "status": "done", "dependencies": [], "subtasks": [], "updatedAt": "2026-10-04T13:00:00Z"},
      {"id": "22", "title": "deferred one", "description": "", "details": "", "status": "deferred", "dependencies": [], "subtasks": [], "updatedAt": "2026-10-05T14:00:00Z"},
      {"id": "23", "title": "cancelled one", "description": "", "details": "", "status": "cancelled", "dependencies": [], "subtasks": [], "updatedAt": "2026-10-06T15:00:00Z"},
      {"id": "24", "title": "blocked one", "description": "", "details": "", "status": "blocked", "dependencies": [], "subtasks": [], "updatedAt": "2026-10-07T16:00:00Z"},
      {"id": "25", "title": "qualified dep", "description": "", "details": "", "status": "pending", "dependencies": ["19.2"], "subtasks": [], "updatedAt": "2026-10-08T17:00:00Z"}
    ],
    "metadata": {"version": "1", "lastModified": "2026-10-08T17:00:00Z", "taskCount": 8, "completedCount": 1, "tags": ["master"]},
    "unknownTagKey": {"kept": true}
  },
  "room-mvp": {
    "tasks": [
      {"id": 7, "title": "int id", "description": "", "details": "", "status": "pending", "dependencies": [], "subtasks": [], "updatedAt": "2026-10-09T18:00:00Z"}
    ],
    "metadata": {"version": "1", "lastModified": "2026-10-09T18:00:00Z", "taskCount": 1, "completedCount": 0, "tags": ["room-mvp"]}
  }
}`

const adoptionSubtasks19 = `[{"id":1,"title":"sub one","description":"","details":"","status":"pending","dependencies":[],"parentTaskId":"19"},{"id":2,"title":"sub two","description":"","details":"","status":"done","dependencies":["1"],"parentTaskId":"19"}]`

// adoptionFamily prepares one repository family shape on disk: a Git common
// directory, its owner boundary, a store directory, and a retired layout at
// .tasks/tasks/tasks.json inside the boundary.
func adoptionFamily(t *testing.T) (commonDir, owner, storeDir, layoutPath string) {
	t.Helper()
	root := t.TempDir()
	commonDir = filepath.Join(root, ".git")
	owner = root
	storeDir = filepath.Join(commonDir, "noise-tasks")
	layoutPath = filepath.Join(owner, ".tasks", "tasks", "tasks.json")
	if err := os.MkdirAll(filepath.Dir(layoutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layoutPath, []byte(adoptionDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	return commonDir, owner, storeDir, layoutPath
}

// adoptionBundleOf creates and verifies one adoption bundle of the given
// family's layout in a fresh durable destination.
func adoptionBundleOf(t *testing.T, layoutPath, commonDir, owner, name string) string {
	t.Helper()
	bundlePath := filepath.Join(t.TempDir(), name+".ntbackup")
	manifest, err := CreateAdoptionBackup(bundlePath, layoutPath, commonDir, owner, "noise-tasks test")
	if err != nil {
		t.Fatalf("create adoption backup: %v", err)
	}
	if manifest.SourceKind != SourceKindAdoptedJSON || manifest.SourceSchema != AdoptionSourceSchema {
		t.Fatalf("bundle provenance %s/%s", manifest.SourceKind, manifest.SourceSchema)
	}
	if manifest.CurrentSchemaVersion != orchestratorSchemaVersion {
		t.Fatalf("bundle current schema %d", manifest.CurrentSchemaVersion)
	}
	if _, err := VerifyBundle(bundlePath); err != nil {
		t.Fatalf("verify adoption bundle: %v", err)
	}
	return bundlePath
}

// adoptionBundle prepares a family and its verified bundle.
func adoptionBundle(t *testing.T, name string) (bundlePath, commonDir, owner, storeDir, layoutPath string) {
	t.Helper()
	commonDir, owner, storeDir, layoutPath = adoptionFamily(t)
	return adoptionBundleOf(t, layoutPath, commonDir, owner, name), commonDir, owner, storeDir, layoutPath
}

// runAdoptionPlan plans against the family's default layout.
func runAdoptionPlan(t *testing.T, bundlePath, commonDir, owner, storeDir string) *MigrationPlan {
	t.Helper()
	layoutPath := filepath.Join(owner, ".tasks", "tasks", "tasks.json")
	plan, err := CreateAdoptionPlan(bundlePath, layoutPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatalf("create adoption plan: %v", err)
	}
	return plan
}

func TestAdoptionDocumentValidation(t *testing.T) {
	valid := func(doc string) func(*testing.T) {
		return func(t *testing.T) {
			if _, err := parseAdoptionDocument([]byte(doc)); err != nil {
				t.Fatalf("valid document refused: %v", err)
			}
		}
	}
	refuse := func(doc, want string) func(*testing.T) {
		return func(t *testing.T) {
			_, err := parseAdoptionDocument([]byte(doc))
			if err == nil {
				t.Fatal("invalid document accepted")
			}
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("refusal %q does not contain %q", err.Error(), want)
			}
		}
	}
	cases := map[string]func(*testing.T){
		"valid":            valid(adoptionDoc),
		"empty document":   refuse(`{}`, "no tag block"),
		"top-level array":  refuse(`[]`, "not an object"),
		"duplicate keys":   refuse(`{"main":{"tasks":[]},"main":{"tasks":[]}}`, "not strict JSON"),
		"empty tag name":   refuse(`{"":{"tasks":[]}}`, "tag name is empty"),
		"block not object": refuse(`{"main":7}`, "not an object"),
	}
	for name, fn := range cases {
		t.Run(name, fn)
	}
}

// TestAdoptionGraphRefusals covers the typed findings that refuse a whole
// adoption: missing dependency targets, self-dependencies, cycles, duplicate
// ids, unknown milestones, and malformed task shapes.
func TestAdoptionGraphRefusals(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"missing dep target", `{"main":{"tasks":[{"id":"1","title":"t","status":"pending","dependencies":["9"],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "does not exist"},
		{"missing subtask target", `{"main":{"tasks":[{"id":"1","title":"t","status":"pending","dependencies":["1.3"],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "does not exist"},
		{"cycle", `{"main":{"tasks":[{"id":"1","title":"a","status":"pending","dependencies":["2"],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"},{"id":"2","title":"b","status":"pending","dependencies":["1"],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "cycle"},
		{"duplicate task id", `{"main":{"tasks":[{"id":"1","title":"a","status":"pending","dependencies":[],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"},{"id":"1","title":"b","status":"pending","dependencies":[],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "duplicate task id"},
		{"duplicate subtask id", `{"main":{"tasks":[{"id":"1","title":"a","status":"pending","dependencies":[],"subtasks":[{"id":1,"title":"x","status":"pending","dependencies":[],"parentTaskId":"1"},{"id":1,"title":"y","status":"pending","dependencies":[],"parentTaskId":"1"}],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "duplicate subtask id"},
		{"subtask self dependency", `{"main":{"tasks":[{"id":"1","title":"a","status":"pending","dependencies":[],"subtasks":[{"id":1,"title":"x","status":"pending","dependencies":["1"],"parentTaskId":"1"}],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "depends on itself"},
		{"unknown milestone", `{"main":{"tasks":[{"id":"1","title":"a","status":"pending","milestone":"nope","dependencies":[],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "unknown milestone"},
		{"invalid status", `{"main":{"tasks":[{"id":"1","title":"a","status":"wat","dependencies":[],"subtasks":[],"updatedAt":"2026-01-01T00:00:00Z"}]}}`, "invalid status"},
		{"missing tasks key", `{"main":{"metadata":{}}}`, "missing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, err := parseAdoptionDocument([]byte(tc.doc))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if _, err := buildAdoptionSnapshot(doc); err == nil {
				t.Fatal("invalid graph accepted")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestAdoptionFullFidelity proves the adopted store carries every §1 fact:
// ids including the non-dense range, all seven statuses, dependencies
// (top-level and qualified), byte-faithful subtasks, unknown fields,
// verbatim updatedAt values, the milestones registry, the unknown tag-block
// key, the master tag name, per-tag nextId seeded to max(id)+1, revision 1
// on every task, and an empty pre-publication feed.
func TestAdoptionFullFidelity(t *testing.T) {
	bundlePath, commonDir, owner, storeDir, _ := adoptionBundle(t, "fidelity")
	plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
	res, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-fidelity-1", PlanDigest: plan.PlanDigest})
	if err != nil {
		t.Fatalf("apply adoption: %v", err)
	}
	if res.Replayed || res.Resumed {
		t.Fatalf("fresh apply reported replayed=%v resumed=%v", res.Replayed, res.Resumed)
	}
	if res.Receipt["kind"] != ReceiptKindAdoption {
		t.Fatalf("receipt kind %v", res.Receipt["kind"])
	}
	db, err := Open(filepath.Join(storeDir, StoreFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	tags, err := db.ListTags()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(tags, ",") != "master,room-mvp" {
		t.Fatalf("tags %v; the master tag must be preserved verbatim", tags)
	}

	type want struct {
		id         string
		status     string
		priority   string // "" = absent
		deps       string
		updatedAt  string
		subtasks   string
		extraKey   string
		extraValue string
		milestone  string
	}
	wants := []want{
		{"19", "pending", "", "38", "2026-10-01T10:00:00Z", adoptionSubtasks19, "triage", `{"score":7,"note":"odd field"}`, ""},
		{"38", "in-progress", "low", "", "2026-10-02T11:00:00Z", `[]`, "extraFlag", `true`, ""},
		{"20", "review", "high", "19", "2026-10-03T12:00:00Z", `[]`, "", "", "fleet hardening"},
		{"21", "done", "", "", "2026-10-04T13:00:00Z", `[]`, "", "", ""},
		{"22", "deferred", "", "", "2026-10-05T14:00:00Z", `[]`, "", "", ""},
		{"23", "cancelled", "", "", "2026-10-06T15:00:00Z", `[]`, "", "", ""},
		{"24", "blocked", "", "", "2026-10-07T16:00:00Z", `[]`, "", "", ""},
		{"25", "pending", "", "19.2", "2026-10-08T17:00:00Z", `[]`, "", "", ""},
	}
	for _, w := range wants {
		task, err := db.GetTask("master", w.id)
		if err != nil {
			t.Fatalf("get master/%s: %v", w.id, err)
		}
		if task.Revision != 1 {
			t.Fatalf("master/%s revision %d, want the seeded 1", w.id, task.Revision)
		}
		if task.Status != w.status || task.Priority != w.priority || task.UpdatedAt != w.updatedAt || task.Milestone != w.milestone {
			t.Fatalf("master/%s = %+v, want %+v", w.id, task, w)
		}
		if got := strings.Join(task.Dependencies, ","); got != w.deps {
			t.Fatalf("master/%s dependencies %q, want %q", w.id, got, w.deps)
		}
		if string(task.Subtasks) != w.subtasks {
			t.Fatalf("master/%s subtasks %s, want %s", w.id, task.Subtasks, w.subtasks)
		}
		if w.extraKey != "" {
			raw, ok := task.Extra[w.extraKey]
			if !ok || strings.TrimSpace(string(raw)) != w.extraValue {
				t.Fatalf("master/%s unknown field %s = %s", w.id, w.extraKey, raw)
			}
		}
	}
	// room-mvp keeps its integer-encoded id as the same logical id 7.
	if task, err := db.GetTask("room-mvp", "7"); err != nil || task.Title != "int id" {
		t.Fatalf("room-mvp/7 = %v, %v", task, err)
	}
	// nextId seeding per §1: max(master)=38 → 39; max(room-mvp)=7 → 8. The
	// next allocation must draw exactly that id and never collide.
	for tag, wantID := range map[string]int{"master": 39, "room-mvp": 8} {
		var next int
		if err := db.db.QueryRow(`SELECT next_id FROM tags WHERE name=?`, tag).Scan(&next); err != nil {
			t.Fatal(err)
		}
		if next != wantID {
			t.Fatalf("tag %s nextId %d, want %d", tag, next, wantID)
		}
	}
	id, err := db.AddTask("master", &Task{Title: "next allocated", Status: "pending"})
	if err != nil {
		t.Fatal(err)
	}
	if id != "39" {
		t.Fatalf("next allocated id %s, want 39", id)
	}
	// The unknown tag-block key and the milestones registry round-trip.
	var extra, milestones string
	err = db.db.QueryRow(`SELECT COALESCE(extra,''), COALESCE(milestones,'') FROM tags WHERE name='master'`).Scan(&extra, &milestones)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(extra, `"unknownTagKey"`) || !strings.Contains(extra, `"kept"`) {
		t.Fatalf("master tag unknown fields lost: %q", extra)
	}
	if !strings.Contains(milestones, `"fleet hardening"`) || !strings.Contains(milestones, `"notes"`) {
		t.Fatalf("master milestones registry not byte-faithful: %q", milestones)
	}
	// Counters are recounted: 1 done task in master at adoption time; the
	// allocation above leaves completedCount untouched.
	var completed int
	if err := db.db.QueryRow(`SELECT completed_count FROM tags WHERE name='master'`).Scan(&completed); err != nil {
		t.Fatal(err)
	}
	if completed != 1 {
		t.Fatalf("master completedCount %d, want 1", completed)
	}
	// The feed started empty; publication appended the cutover events.
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range feed.Events {
		if e.Seq <= 0 {
			t.Fatalf("unexpected adopted feed event %+v", e)
		}
		// The only tagged event may be the allocation probe above; the
		// publication events (schema upgrade, adoption) carry no task.
		if e.Kind == "add" && e.Task != "39" {
			t.Fatalf("unexpected adopted feed event %+v", e)
		}
		if e.Kind != "add" && e.Tag != "" {
			t.Fatalf("unexpected adopted feed event %+v", e)
		}
	}
}

// TestAdoptionRequestReplay proves the (adoption, requestId) replay rule:
// the same request returns the same receipt with no new effects, and a
// different request id fails closed against the published target.
func TestAdoptionRequestReplay(t *testing.T) {
	bundlePath, commonDir, owner, storeDir, _ := adoptionBundle(t, "replay")
	plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
	opts := ApplyOptions{RequestID: "adopt-replay-1", PlanDigest: plan.PlanDigest}
	first, err := ApplyAdoption(bundlePath, commonDir, owner, opts)
	if err != nil {
		t.Fatal(err)
	}
	again, err := ApplyAdoption(bundlePath, commonDir, owner, opts)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !again.Replayed {
		t.Fatal("replay not reported")
	}
	if again.Receipt["receiptId"] != first.Receipt["receiptId"] {
		t.Fatalf("replay receipt %v != %v", again.Receipt["receiptId"], first.Receipt["receiptId"])
	}
	// The operator may remove the layout after publication (§8.6A); replay
	// of the completed apply must depend on the published receipt alone.
	if err := os.Remove(filepath.Join(owner, ".tasks", "tasks", "tasks.json")); err != nil {
		t.Fatal(err)
	}
	final, err := ApplyAdoption(bundlePath, commonDir, owner, opts)
	if err != nil {
		t.Fatalf("replay with source removed: %v", err)
	}
	if !final.Replayed || final.Receipt["receiptId"] != first.Receipt["receiptId"] {
		t.Fatalf("source-removed replay %+v", final)
	}
	if other, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-replay-2", PlanDigest: plan.PlanDigest}); err == nil {
		t.Fatalf("different request id accepted: %+v", other)
	}
	if _, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-replay-3", PlanDigest: "sha256:" + strings.Repeat("a", 64)}); err == nil {
		t.Fatal("wrong digest accepted")
	}
}

// TestAdoptionRefusals covers the fail-closed presences: an existing store,
// the derived live.json, a §8.6 recovery journal, an adoption recovery
// journal, source location rules, a wrong-family bundle, cross-kind bundles,
// a changed or missing source, and an existing unrelated target.
func TestAdoptionRefusals(t *testing.T) {
	t.Run("existing store refuses adoption", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "refuse-store")
		if err := os.MkdirAll(storeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storeDir, StoreFile), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := AdoptionFreshGate(storeDir); err == nil {
			t.Fatal("gate accepted an existing store")
		}
		if _, err := CreateAdoptionPlan(bundlePath, layoutPath, commonDir, owner, storeDir, "noise-tasks test"); err == nil {
			t.Fatal("plan accepted an existing store")
		}
	})
	t.Run("live.json refuses adoption", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		if err := os.MkdirAll(storeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storeDir, LegacyFile), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := AdoptionFreshGate(storeDir); err == nil {
			t.Fatal("gate accepted the derived live.json")
		}
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "refuse-live")
		if _, err := CreateAdoptionPlan(bundlePath, layoutPath, commonDir, owner, storeDir, "noise-tasks test"); err == nil {
			t.Fatal("plan accepted the derived live.json")
		}
	})
	t.Run("migration journal refuses adoption", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		if err := os.MkdirAll(storeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		familyID := LegacyFamilyUUID(commonDir, owner)
		j := &MigrationJournal{
			SchemaVersion:          JournalSchemaVersion,
			RequestID:              "stuck-migration",
			FamilyID:               familyID,
			PlanDigest:             "sha256:" + strings.Repeat("b", 64),
			SourceComparisonDigest: "sha256:" + strings.Repeat("c", 64),
			BackupPayloadDigest:    "sha256:" + strings.Repeat("d", 64),
			TargetSchemaVersion:    SchemaVersion,
		}
		if err := writeMigrationJournal(storeDir, j); err != nil {
			t.Fatal(err)
		}
		if err := AdoptionFreshGate(storeDir); err == nil {
			t.Fatal("gate accepted a §8.6 recovery journal")
		}
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "refuse-migration")
		if _, err := CreateAdoptionPlan(bundlePath, layoutPath, commonDir, owner, storeDir, "noise-tasks test"); err == nil {
			t.Fatal("plan accepted a §8.6 recovery journal")
		}
	})
	t.Run("adoption journal fences plan and setup paths", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "fence-plan")
		plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
		familyID := LegacyFamilyUUID(commonDir, owner)
		j := &MigrationJournal{
			SchemaVersion:          JournalSchemaVersion,
			RequestID:              "adopt-fence-1",
			FamilyID:               familyID,
			PlanDigest:             plan.PlanDigest,
			SourceComparisonDigest: plan.Source.ComparisonDigest,
			BackupPayloadDigest:    plan.Backup.PayloadDigest,
			TargetSchemaVersion:    SchemaVersion,
		}
		if err := writeJournalTo(storeDir, AdoptionJournalFile, ".adoption-apply-", j); err != nil {
			t.Fatal(err)
		}
		recovery, err := ReadAdoptionRecovery(storeDir)
		if err != nil || recovery != RecoveryRequired {
			t.Fatalf("adoption recovery %v, %v; want RecoveryRequired", recovery, err)
		}
		// Only the matching apply may advance the state; a fresh plan and a
		// different request both fail closed.
		if _, err := CreateAdoptionPlan(bundlePath, layoutPath, commonDir, owner, storeDir, "noise-tasks test"); err == nil {
			t.Fatal("plan replaced a recovery-bound plan")
		}
		if _, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-fence-2", PlanDigest: plan.PlanDigest}); err == nil {
			t.Fatal("mismatched request advanced the journal")
		}
	})
	t.Run("source location rules", func(t *testing.T) {
		_, owner, storeDir, _ := adoptionFamily(t)
		outside := filepath.Join(t.TempDir(), "tasks.json")
		if err := os.WriteFile(outside, []byte(adoptionDoc), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveAdoptionSource(outside, owner, storeDir); err == nil {
			t.Fatal("source outside the owner boundary accepted")
		}
		inside := filepath.Join(storeDir, "stray.json")
		if err := os.MkdirAll(storeDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(inside, []byte(adoptionDoc), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveAdoptionSource(inside, owner, storeDir); err == nil {
			t.Fatal("source inside the store directory accepted")
		}
	})
	t.Run("wrong family bundle refuses plan", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		otherRoot := t.TempDir()
		otherCommon := filepath.Join(otherRoot, ".git")
		bundlePath := adoptionBundleOf(t, layoutPath, otherCommon, otherRoot, "wrong-family")
		if _, err := CreateAdoptionPlan(bundlePath, layoutPath, commonDir, owner, storeDir, "noise-tasks test"); err == nil {
			t.Fatal("wrong-family bundle accepted")
		}
	})
	t.Run("legacy and adoption surfaces refuse each other's bundles", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "cross-kind")
		// Rewrite the manifest to claim legacy provenance; the content
		// digests still match the payload, so only the kind check can refuse.
		legacyBundle := filepath.Join(t.TempDir(), "cross-legacy.ntbackup")
		if err := os.MkdirAll(legacyBundle, 0o700); err != nil {
			t.Fatal(err)
		}
		manifestRaw, err := os.ReadFile(filepath.Join(bundlePath, ManifestFile))
		if err != nil {
			t.Fatal(err)
		}
		manifestRaw = []byte(strings.Replace(strings.Replace(string(manifestRaw), `"adopted-json"`, `"legacy-json"`, 1), `"adopted-json:1"`, `"legacy-json:1"`, 1))
		if err := os.WriteFile(filepath.Join(legacyBundle, ManifestFile), manifestRaw, 0o600); err != nil {
			t.Fatal(err)
		}
		payload, err := os.ReadFile(filepath.Join(bundlePath, PayloadFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyBundle, PayloadFile), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := CreateAdoptionPlan(legacyBundle, layoutPath, commonDir, owner, storeDir, "noise-tasks test"); err == nil {
			t.Fatal("adoption plan accepted a legacy bundle")
		}
		if _, err := CreateAdoptionPlan(bundlePath, layoutPath, commonDir, owner, storeDir, "noise-tasks test"); err != nil {
			t.Fatalf("adoption bundle refused by adoption plan: %v", err)
		}
	})
	t.Run("changed and missing source refuse apply", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "changed-source")
		plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
		if err := os.WriteFile(layoutPath, []byte(strings.Replace(adoptionDoc, "Relay cutover", "Changed", 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-changed-1", PlanDigest: plan.PlanDigest}); err == nil {
			t.Fatal("changed source accepted")
		} else if !strings.Contains(err.Error(), "changed since planning") {
			t.Fatalf("changed-source refusal %q", err.Error())
		}
		if err := os.Remove(layoutPath); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-changed-1", PlanDigest: plan.PlanDigest}); err == nil {
			t.Fatal("missing source accepted")
		}
	})
	t.Run("forged plan with out-of-boundary source refused", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "forged-plan")
		plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
		// Hand-edit the source to a path outside the owner boundary and
		// recompute the (unauthenticated) plan digest: the strict reader
		// must still refuse on containment.
		plan.Source.Path = filepath.Join(t.TempDir(), "elsewhere", "tasks.json")
		body, err := plan.renderPlanWith(adoptionPlanCodec)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storeDir, AdoptionPlanFile), body, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPlanFile(filepath.Join(storeDir, AdoptionPlanFile), adoptionPlanCodec); err == nil {
			t.Fatal("forged out-of-boundary plan accepted")
		} else if !strings.Contains(err.Error(), "not inside the owner boundary") {
			t.Fatalf("forged-plan refusal %q", err.Error())
		}
	})
	t.Run("existing unrelated store refuses apply", func(t *testing.T) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		bundlePath := adoptionBundleOf(t, layoutPath, commonDir, owner, "unrelated")
		plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
		if err := os.WriteFile(filepath.Join(storeDir, StoreFile), []byte("not sqlite"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-unrelated-1", PlanDigest: plan.PlanDigest}); err == nil {
			t.Fatal("unrelated target accepted")
		}
	})
}

// TestAdoptionMidflightFences proves §8.6 artifacts appearing between plan
// and apply fail closed: the derived live.json or a §8.6 recovery journal
// never advances alongside adoption (§8.6A).
func TestAdoptionMidflightFences(t *testing.T) {
	newBundle := func(t *testing.T) (bundlePath, commonDir, owner, storeDir string, plan *MigrationPlan) {
		commonDir, owner, storeDir, layoutPath := adoptionFamily(t)
		bundlePath = adoptionBundleOf(t, layoutPath, commonDir, owner, "midflight")
		return bundlePath, commonDir, owner, storeDir, runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
	}
	t.Run("live.json appears between plan and apply", func(t *testing.T) {
		bundlePath, commonDir, owner, storeDir, plan := newBundle(t)
		if err := os.WriteFile(filepath.Join(storeDir, LegacyFile), []byte(legacyFixture), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-mid-1", PlanDigest: plan.PlanDigest})
		if err == nil || !strings.Contains(err.Error(), "belongs to migrate plan and migrate apply") {
			t.Fatalf("apply under mid-flight live.json: %v", err)
		}
	})
	t.Run("migration journal appears between plan and apply", func(t *testing.T) {
		bundlePath, commonDir, owner, storeDir, plan := newBundle(t)
		j := &MigrationJournal{
			SchemaVersion:          JournalSchemaVersion,
			RequestID:              "stuck-migration-2",
			FamilyID:               LegacyFamilyUUID(commonDir, owner),
			PlanDigest:             "sha256:" + strings.Repeat("2", 64),
			SourceComparisonDigest: "sha256:" + strings.Repeat("3", 64),
			BackupPayloadDigest:    "sha256:" + strings.Repeat("4", 64),
			TargetSchemaVersion:    SchemaVersion,
		}
		if err := writeMigrationJournal(storeDir, j); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-mid-2", PlanDigest: plan.PlanDigest})
		if err == nil || !strings.Contains(err.Error(), "§8.6 migration recovery journal exists") {
			t.Fatalf("apply under mid-flight migration journal: %v", err)
		}
	})
}

// TestAdoptionCrashStates walks the named §8.6A recovery states with the
// deterministic fault hook: a durable journal with no target resumes, a
// published target beside its journal replays and cleans up, and the layout
// document survives every step.
func TestAdoptionCrashStates(t *testing.T) {
	origHook := migrateFaultHook
	defer func() { migrateFaultHook = origHook }()

	t.Run("resume after journal durable", func(t *testing.T) {
		bundlePath, commonDir, owner, storeDir, layoutPath := adoptionBundle(t, "crash-journal")
		plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
		migrateFaultHook = func(op string) error {
			if op == "journal-durable" {
				return errors.New("injected crash")
			}
			return nil
		}
		opts := ApplyOptions{RequestID: "adopt-crash-1", PlanDigest: plan.PlanDigest}
		if _, err := ApplyAdoption(bundlePath, commonDir, owner, opts); err == nil {
			t.Fatal("injected crash not observed")
		}
		if _, err := os.Lstat(filepath.Join(storeDir, AdoptionJournalFile)); err != nil {
			t.Fatalf("journal not durable: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(storeDir, StoreFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("target unexpectedly present: %v", err)
		}
		migrateFaultHook = origHook
		res, err := ApplyAdoption(bundlePath, commonDir, owner, opts)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if !res.Resumed || res.Replayed {
			t.Fatalf("resume reported replayed=%v resumed=%v", res.Replayed, res.Resumed)
		}
		if _, err := os.Lstat(filepath.Join(storeDir, AdoptionJournalFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("journal not cleaned up after resume")
		}
		if _, err := os.Lstat(layoutPath); err != nil {
			t.Fatalf("layout document was removed: %v", err)
		}
	})

	t.Run("cleanup after target published", func(t *testing.T) {
		bundlePath, commonDir, owner, storeDir, layoutPath := adoptionBundle(t, "crash-published")
		plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
		migrateFaultHook = func(op string) error {
			if op == "target-published" {
				return errors.New("injected crash")
			}
			return nil
		}
		opts := ApplyOptions{RequestID: "adopt-crash-2", PlanDigest: plan.PlanDigest}
		if _, err := ApplyAdoption(bundlePath, commonDir, owner, opts); err == nil {
			t.Fatal("injected crash not observed")
		}
		if _, err := os.Lstat(filepath.Join(storeDir, StoreFile)); err != nil {
			t.Fatalf("target not published: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(storeDir, AdoptionJournalFile)); err != nil {
			t.Fatalf("journal not present for cleanup state: %v", err)
		}
		// The cleanup replay must not need the source (state 3).
		if err := os.Remove(layoutPath); err != nil {
			t.Fatal(err)
		}
		// A published target beside a valid journal is exactly RecoveryPublished.
		if recovery, err := ReadAdoptionRecovery(storeDir); err != nil || recovery != RecoveryPublished {
			t.Fatalf("recovery classification %v, %v", recovery, err)
		}
		migrateFaultHook = origHook
		res, err := ApplyAdoption(bundlePath, commonDir, owner, opts)
		if err != nil {
			t.Fatalf("cleanup replay: %v", err)
		}
		if !res.Replayed {
			t.Fatal("cleanup replay not reported")
		}
		if _, err := os.Lstat(filepath.Join(storeDir, AdoptionJournalFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("journal not removed by cleanup replay")
		}
		// (The layout was deleted before this replay; state 3/4 need it not.)
		// Final state is complete: replay once more without the journal.
		res, err = ApplyAdoption(bundlePath, commonDir, owner, opts)
		if err != nil || !res.Replayed {
			t.Fatalf("completed replay: %+v, %v", res, err)
		}
	})

	t.Run("restore refuses during adoption recovery", func(t *testing.T) {
		bundlePath, commonDir, owner, storeDir, _ := adoptionBundle(t, "crash-restore")
		plan := runAdoptionPlan(t, bundlePath, commonDir, owner, storeDir)
		migrateFaultHook = func(op string) error {
			if op == "journal-durable" {
				return errors.New("injected crash")
			}
			return nil
		}
		if _, err := ApplyAdoption(bundlePath, commonDir, owner, ApplyOptions{RequestID: "adopt-crash-3", PlanDigest: plan.PlanDigest}); err == nil {
			t.Fatal("injected crash not observed")
		}
		migrateFaultHook = origHook
		if _, err := Restore(bundlePath, filepath.Join(storeDir, StoreFile), RestoreOptions{RequestID: "adopt-restore-1"}); err == nil {
			t.Fatal("restore bypassed the adoption recovery journal")
		}
	})
}

// TestAdoptionJournalFencesMigration proves the §8.7 mutual fence at store
// level: both §8.6 verbs refuse while a strict adoption recovery journal
// exists, even with a valid legacy source, bundle, and plan in place.
func TestAdoptionJournalFencesMigration(t *testing.T) {
	storeDir, legacyPath, commonDir, owner := planFamily(t)
	bundle := planBundle(t, storeDir, legacyPath, commonDir, owner)
	plan, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test")
	if err != nil {
		t.Fatal(err)
	}
	j := &MigrationJournal{
		SchemaVersion:          JournalSchemaVersion,
		RequestID:              "adopt-fence-mig",
		FamilyID:               LegacyFamilyUUID(commonDir, owner),
		PlanDigest:             "sha256:" + strings.Repeat("e", 64),
		SourceComparisonDigest: "sha256:" + strings.Repeat("f", 64),
		BackupPayloadDigest:    "sha256:" + strings.Repeat("1", 64),
		TargetSchemaVersion:    SchemaVersion,
	}
	if err := writeJournalTo(storeDir, AdoptionJournalFile, ".adoption-apply-", j); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateMigrationPlan(bundle, legacyPath, commonDir, owner, storeDir, "noise-tasks test"); err == nil ||
		!strings.Contains(err.Error(), "adoption recovery journal") {
		t.Fatalf("migrate plan under adoption journal: %v", err)
	}
	if _, err := ApplyMigration(bundle, commonDir, owner, ApplyOptions{RequestID: "mig-under-adopt", PlanDigest: plan.PlanDigest}); err == nil ||
		!strings.Contains(err.Error(), "adoption recovery journal") {
		t.Fatalf("migrate apply under adoption journal: %v", err)
	}
}
