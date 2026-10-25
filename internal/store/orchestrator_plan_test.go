package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const validOrchestratorPlan = `{
  "schemaVersion": 1,
  "planId": "release-25",
  "tag": "main",
  "milestones": [{"key":"ms","name":"v2.5","status":"open"}],
  "tasks": [
    {"key":"spec","title":"Settle protocol","description":"d","details":"x","priority":"high","milestone":{"key":"ms"},"dependencies":[{"id":"1"}]},
    {"key":"code","title":"Implement","description":"","details":"","priority":"medium","milestone":{"name":"existing"},"dependencies":[{"key":"spec"}]}
  ],
  "assignments": [{"key":"primary","task":{"key":"spec"},"owner":"planner-a"}]
}`

func orchestratorDB(t *testing.T) *DB {
	t.Helper()
	db, _ := newDB(t)
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddTask("main", &Task{Title: "existing"}); err != nil {
		t.Fatal(err)
	}
	if err := db.AddMilestone("main", "existing"); err != nil {
		t.Fatal(err)
	}
	return db
}

func mustOrchestratorPlan(t *testing.T, raw string) *OrchestratorPlan {
	t.Helper()
	p, err := ParseOrchestratorPlan([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOrchestratorPlanStrictParsingAndDigest(t *testing.T) {
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	if !digestForm(p.Digest) {
		t.Fatalf("digest = %q", p.Digest)
	}
	// Whitespace and object-member order are semantically irrelevant; array
	// order remains covered.
	reordered := `{"tasks":[{"title":"Settle protocol","key":"spec","dependencies":[{"id":"1"}],"details":"x","description":"d","priority":"high","milestone":{"key":"ms"}},{"priority":"medium","dependencies":[{"key":"spec"}],"key":"code","title":"Implement","description":"","details":"","milestone":{"name":"existing"}}],"tag":"main","schemaVersion":1,"assignments":[{"owner":"planner-a","task":{"key":"spec"},"key":"primary"}],"planId":"release-25","milestones":[{"status":"open","name":"v2.5","key":"ms"}]}`
	p2 := mustOrchestratorPlan(t, reordered)
	if p.Digest != p2.Digest || string(p.canonical) != string(p2.canonical) {
		t.Fatalf("canonical digest differs: %s / %s", p.Digest, p2.Digest)
	}

	cases := map[string]string{
		"duplicate key":        strings.Replace(validOrchestratorPlan, `"schemaVersion": 1,`, `"schemaVersion": 1,"schemaVersion":1,`, 1),
		"unknown key":          strings.Replace(validOrchestratorPlan, `"tag": "main",`, `"tag":"main","surprise":true,`, 1),
		"fraction":             strings.Replace(validOrchestratorPlan, `"schemaVersion": 1`, `"schemaVersion": 1.0`, 1),
		"unknown local dep":    strings.Replace(validOrchestratorPlan, `{"key":"spec"}]`, `{"key":"missing"}]`, 1),
		"duplicate task key":   strings.Replace(validOrchestratorPlan, `"key":"code"`, `"key":"spec"`, 1),
		"duplicate assignment": strings.Replace(validOrchestratorPlan, `]}`, `,{"key":"secondary","task":{"key":"spec"},"owner":"b"}]}`, 1),
		"bad owner":            strings.Replace(validOrchestratorPlan, `"planner-a"`, `" planner-a"`, 1),
		"empty title":          strings.Replace(validOrchestratorPlan, `"title":"Settle protocol"`, `"title":""`, 1),
		"bad reference shape":  strings.Replace(validOrchestratorPlan, `{"id":"1"}`, `{"id":"1","key":"spec"}`, 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseOrchestratorPlan([]byte(raw)); err == nil {
				t.Fatal("invalid plan accepted")
			}
		})
	}
	if _, err := ParseOrchestratorPlan([]byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("invalid UTF-8 = %v", err)
	}
	tooLarge := make([]byte, OrchestratorPlanMaxBytes+1)
	if _, err := ParseOrchestratorPlan(tooLarge); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize = %v", err)
	}
}

func TestOrchestratorPlanOfflineAndOnlinePreflight(t *testing.T) {
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	offline := ValidateOrchestratorPlanOffline(p)
	if !offline.Offline || offline.Inventory != (PlanInventory{Milestones: 1, Tasks: 2, Assignments: 1, Dependencies: 2}) {
		t.Fatalf("offline = %+v", offline)
	}
	db := orchestratorDB(t)
	online, err := db.ValidateOrchestratorPlan(p)
	if err != nil || online.Offline || online.PlanDigest != p.Digest {
		t.Fatalf("online = %+v, %v", online, err)
	}

	missing := mustOrchestratorPlan(t, strings.Replace(validOrchestratorPlan, `"id":"1"`, `"id":"999"`, 1))
	if _, err := db.ValidateOrchestratorPlan(missing); err == nil || !strings.Contains(err.Error(), "unknown existing task") {
		t.Fatalf("missing dependency = %v", err)
	}
	duplicateMS := mustOrchestratorPlan(t, strings.Replace(validOrchestratorPlan, `"name":"v2.5"`, `"name":"existing"`, 1))
	if _, err := db.ValidateOrchestratorPlan(duplicateMS); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate milestone = %v", err)
	}
	nameInsteadOfKey := mustOrchestratorPlan(t, strings.Replace(validOrchestratorPlan, `"milestone":{"key":"ms"}`, `"milestone":{"name":"v2.5"}`, 1))
	if _, err := db.ValidateOrchestratorPlan(nameInsteadOfKey); err == nil || !strings.Contains(err.Error(), "unknown milestone") {
		t.Fatalf("plan-created milestone addressed by name = %v", err)
	}
	wrongTag := mustOrchestratorPlan(t, strings.Replace(validOrchestratorPlan, `"tag": "main"`, `"tag":"other"`, 1))
	if _, err := db.ValidateOrchestratorPlan(wrongTag); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing tag = %v", err)
	}
}

func TestApplyOrchestratorPlanUsesVersionedV2ToV3Upgrade(t *testing.T) {
	db := orchestratorDB(t)
	rewindStoreToV2(t, db)
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	if _, err := db.ValidateOrchestratorPlan(p); err != nil {
		t.Fatalf("read-only validation on early schema: %v", err)
	}
	if _, err := db.ApplyOrchestratorPlan(p, p.Digest, "early-v2"); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if version, err := db.userVersion(); err != nil || version != SchemaVersion {
		t.Fatalf("user_version = %d, %v; want %d", version, err, SchemaVersion)
	}
	var assignments int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM assignments`).Scan(&assignments); err != nil || assignments != 1 {
		t.Fatalf("assignments = %d, %v", assignments, err)
	}
}

func TestApplyOrchestratorPlanReplayRefusesCorruptCanonicalRow(t *testing.T) {
	db := orchestratorDB(t)
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	if _, err := db.ApplyOrchestratorPlan(p, p.Digest, "corrupt-replay"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE plans SET canonical_json='{}' WHERE plan_id=?`, p.PlanID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ApplyOrchestratorPlan(p, p.Digest, "corrupt-replay"); err == nil {
		t.Fatal("replay accepted a corrupt canonical plan row")
	} else {
		var conflict *ReplayConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("replay corruption = %T %v, want ReplayConflict", err, err)
		}
	}
}

func TestApplyOrchestratorPlanPublishesCompleteBatchAndReceipt(t *testing.T) {
	db := orchestratorDB(t)
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	semanticBefore, _, _, err := db.ClassDigests()
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed {
		t.Fatal("first apply reported replay")
	}
	rawMappings, ok := result.Receipt["taskMappings"].([]any)
	if !ok || len(rawMappings) != 2 || rawMappings[0].(map[string]any)["id"] != "2" || rawMappings[1].(map[string]any)["id"] != "3" {
		t.Fatalf("mappings = %#v", result.Receipt["taskMappings"])
	}
	if !digestForm(result.Receipt["resultDigest"].(string)) || !digestForm(result.Receipt["receiptDigest"].(string)) {
		t.Fatalf("receipt digests = %+v", result.Receipt)
	}
	spec, err := db.GetTask("main", "2")
	if err != nil || spec == nil || spec.Milestone != "v2.5" || len(spec.Dependencies) != 1 || spec.Dependencies[0] != "1" || spec.Revision != 1 {
		t.Fatalf("task 2 = %+v, %v", spec, err)
	}
	code, err := db.GetTask("main", "3")
	if err != nil || code == nil || code.Milestone != "existing" || len(code.Dependencies) != 1 || code.Dependencies[0] != "2" {
		t.Fatalf("task 3 = %+v, %v", code, err)
	}
	entries, err := db.MilestoneEntries("main")
	if err != nil || len(entries) != 2 || entries[1].Name != "v2.5" || entries[1].Status != "open" {
		t.Fatalf("milestones = %+v, %v", entries, err)
	}
	var assignmentKey, owner, planID string
	if err := db.db.QueryRow(`SELECT assignment_key, owner, plan_id FROM assignments WHERE tag='main' AND task_id='2'`).Scan(&assignmentKey, &owner, &planID); err != nil {
		t.Fatal(err)
	}
	if assignmentKey != "primary" || owner != "planner-a" || planID != p.PlanID {
		t.Fatalf("assignment = %s %s %s", assignmentKey, owner, planID)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "plan" || last.Action != "applied" || last.Count != 2 || last.Extra["receiptId"] == nil || last.Extra["receiptDigest"] == nil {
		t.Fatalf("plan event = %+v", last)
	}
	semanticAfter, _, inv, err := db.ClassDigests()
	if err != nil || semanticAfter == semanticBefore || !digestForm(semanticAfter) || inv.Plans != 1 || inv.PlanMappings != 2 || inv.Assignments != 1 || inv.Tasks != 3 {
		t.Fatalf("inventory = %+v, %v", inv, err)
	}
	bundle := filepath.Join(t.TempDir(), "planned.ntbackup")
	if _, err := db.CreateBackup(bundle, "noise-tasks test"); err != nil {
		t.Fatal(err)
	}
	manifest, err := VerifyBundle(bundle)
	if err != nil || manifest.SemanticDigest != semanticAfter || manifest.Inventory.Plans != 1 || manifest.Inventory.PlanMappings != 2 || manifest.Inventory.Assignments != 1 {
		t.Fatalf("verified plan backup = %+v, %v", manifest, err)
	}
}

func TestApplyOrchestratorPlanReplayAndDivergence(t *testing.T) {
	db := orchestratorDB(t)
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	first, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-1")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-1")
	if err != nil || !replay.Replayed || replay.Receipt["receiptDigest"] != first.Receipt["receiptDigest"] {
		t.Fatalf("replay = %+v, %v", replay, err)
	}
	var events, tasks int
	db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='plan'`).Scan(&events)
	db.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasks)
	if events != 1 || tasks != 3 {
		t.Fatalf("replay repeated effects: events=%d tasks=%d", events, tasks)
	}

	other := mustOrchestratorPlan(t, strings.Replace(validOrchestratorPlan, `"planId": "release-25"`, `"planId":"other"`, 1))
	if _, err := db.ApplyOrchestratorPlan(other, other.Digest, "req-1"); err == nil {
		t.Fatal("divergent request-id replay accepted")
	} else {
		var conflict *ReplayConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("divergent request error = %T %v", err, err)
		}
	}
	if _, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-2"); err == nil {
		t.Fatal("plan-id reuse accepted")
	} else {
		var conflict *PlanIDConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("plan id error = %T %v", err, err)
		}
	}
}

func TestApplyOrchestratorPlanAllOrNothingOnLateFailure(t *testing.T) {
	db := orchestratorDB(t)
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	oldHook := orchestratorPlanFaultHook
	orchestratorPlanFaultHook = func(point string) error {
		if point == "before-commit" {
			return errors.New("injected crash")
		}
		return nil
	}
	t.Cleanup(func() { orchestratorPlanFaultHook = oldHook })
	if _, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-crash"); err == nil || !strings.Contains(err.Error(), "injected crash") {
		t.Fatalf("apply = %v", err)
	}
	for table, want := range map[string]int{"tasks": 1, "plans": 0, "plan_mappings": 0, "assignments": 0, "receipts": 0} {
		var got int
		if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil || got != want {
			t.Fatalf("%s count = %d, %v; want %d", table, got, err, want)
		}
	}
	entries, err := db.MilestoneEntries("main")
	if err != nil || len(entries) != 1 || entries[0].Name != "existing" {
		t.Fatalf("milestones after rollback = %+v, %v", entries, err)
	}
}

func TestApplyOrchestratorPlanConcurrentIdenticalReplay(t *testing.T) {
	db := orchestratorDB(t)
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	const writers = 8
	var wg sync.WaitGroup
	results := make(chan *OrchestratorPlanApplyResult, writers)
	errs := make(chan error, writers)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-race")
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent apply: %v", err)
	}
	count, fresh := 0, 0
	var digest any
	for result := range results {
		count++
		if !result.Replayed {
			fresh++
		}
		if digest == nil {
			digest = result.Receipt["receiptDigest"]
		} else if digest != result.Receipt["receiptDigest"] {
			t.Errorf("receipt digest differs")
		}
	}
	if count != writers || fresh != 1 {
		t.Fatalf("results=%d fresh=%d", count, fresh)
	}
	var plans, tasks int
	db.db.QueryRow(`SELECT COUNT(*) FROM plans`).Scan(&plans)
	db.db.QueryRow(`SELECT COUNT(*) FROM tasks`).Scan(&tasks)
	if plans != 1 || tasks != 3 {
		t.Fatalf("plans=%d tasks=%d", plans, tasks)
	}
}

func TestApplyOrchestratorPlanConcurrentBatchesAllocateDisjointIDs(t *testing.T) {
	db := orchestratorDB(t)
	makePlan := func(id, key string) *OrchestratorPlan {
		return mustOrchestratorPlan(t, `{"schemaVersion":1,"planId":"`+id+`","tag":"main","milestones":[],"tasks":[{"key":"`+key+`","title":"`+key+`","description":"","details":"","priority":"medium","dependencies":[{"id":"1"}]}],"assignments":[]}`)
	}
	plans := []*OrchestratorPlan{makePlan("batch-a", "a"), makePlan("batch-b", "b")}
	results := make(chan *OrchestratorPlanApplyResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for i, plan := range plans {
		wg.Add(1)
		go func(i int, plan *OrchestratorPlan) {
			defer wg.Done()
			result, err := db.ApplyOrchestratorPlan(plan, plan.Digest, fmt.Sprintf("batch-req-%d", i))
			if err != nil {
				errs <- err
				return
			}
			results <- result
		}(i, plan)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("concurrent batch: %v", err)
	}
	ids := map[string]bool{}
	for result := range results {
		mappings, err := receiptMappings(result.Receipt)
		if err != nil || len(mappings) != 1 {
			t.Fatalf("mapping = %+v, %v", mappings, err)
		}
		ids[mappings[0].ID] = true
	}
	if !ids["2"] || !ids["3"] || len(ids) != 2 {
		t.Fatalf("allocated ids = %+v", ids)
	}
	for _, id := range []string{"2", "3"} {
		task, err := db.GetTask("main", id)
		if err != nil || task == nil || len(task.Dependencies) != 1 || task.Dependencies[0] != "1" {
			t.Fatalf("task %s = %+v, %v", id, task, err)
		}
	}
}

func TestRemoveAssignedTaskPreservesPlanRecords(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		id       string
		force    bool
		stripped string
	}{
		{name: "non-force", raw: strings.Replace(validOrchestratorPlan, `"task":{"key":"spec"}`, `"task":{"key":"code"}`, 1), id: "3", stripped: "[]"},
		{name: "force", raw: validOrchestratorPlan, id: "2", force: true, stripped: "[3]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := orchestratorDB(t)
			p := mustOrchestratorPlan(t, tt.raw)
			applied, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-remove-assigned")
			if err != nil {
				t.Fatal(err)
			}
			stripped, err := db.RemoveTask("main", tt.id, 1, tt.force, nil)
			if err != nil || fmt.Sprint(stripped) != tt.stripped {
				t.Fatalf("removal stripped=%v err=%v", stripped, err)
			}
			if task, err := db.GetTask("main", tt.id); err != nil || task != nil {
				t.Fatalf("removed task=%+v err=%v", task, err)
			}
			for table, want := range map[string]int{"assignments": 0, "plans": 1, "plan_mappings": 2, "receipts": 1} {
				var got int
				if err := db.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&got); err != nil || got != want {
					t.Fatalf("%s count=%d err=%v want=%d", table, got, err, want)
				}
			}
			replay, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-remove-assigned")
			if err != nil || !replay.Replayed || replay.Receipt["receiptDigest"] != applied.Receipt["receiptDigest"] ||
				len(replay.Receipt["taskMappings"].([]any)) != 2 {
				t.Fatalf("replay=%+v err=%v", replay, err)
			}
			if tt.force {
				dependent, err := db.GetTask("main", "3")
				if err != nil || dependent == nil || len(dependent.Dependencies) != 0 || dependent.Revision != 2 {
					t.Fatalf("dependent=%+v err=%v", dependent, err)
				}
			}
		})
	}
}

func TestRemoveAssignedTaskRollsBackAssignmentCleanup(t *testing.T) {
	db := orchestratorDB(t)
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	if _, err := db.ApplyOrchestratorPlan(p, p.Digest, "req-remove-rollback"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER reject_assigned_task_delete BEFORE DELETE ON tasks
		WHEN OLD.tag='main' AND OLD.id='2' BEGIN SELECT RAISE(ABORT, 'injected delete failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveTask("main", "2", 1, true, nil); err == nil || !strings.Contains(err.Error(), "injected delete failure") {
		t.Fatalf("removal=%v", err)
	}
	if task, err := db.GetTask("main", "2"); err != nil || task == nil {
		t.Fatalf("task after rollback=%+v err=%v", task, err)
	}
	dependent, err := db.GetTask("main", "3")
	if err != nil || dependent == nil || len(dependent.Dependencies) != 1 || dependent.Dependencies[0] != "2" || dependent.Revision != 1 {
		t.Fatalf("dependent after rollback=%+v err=%v", dependent, err)
	}
	var assignments int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM assignments WHERE tag='main' AND task_id='2'`).Scan(&assignments); err != nil || assignments != 1 {
		t.Fatalf("assignments after rollback=%d err=%v", assignments, err)
	}
}

func TestPlanResultDigestStable(t *testing.T) {
	p := mustOrchestratorPlan(t, validOrchestratorPlan)
	mappings := []PlanTaskMapping{{Key: "spec", ID: "2"}, {Key: "code", ID: "3"}}
	a, err := planResultDigest(p, mappings)
	if err != nil {
		t.Fatal(err)
	}
	b, err := planResultDigest(p, mappings)
	if err != nil || a != b {
		t.Fatalf("result digest = %s / %s (%v)", a, b, err)
	}
	changed, _ := planResultDigest(p, []PlanTaskMapping{{Key: "spec", ID: "20"}, {Key: "code", ID: "21"}})
	if a == changed {
		t.Fatal("mapping did not affect result digest")
	}
	if _, err := json.Marshal(ValidateOrchestratorPlanOffline(p)); err != nil {
		t.Fatal(err)
	}
}

func TestApplyEmptyOrchestratorPlanStillPublishesReceipt(t *testing.T) {
	db, _ := newDB(t)
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	p := mustOrchestratorPlan(t, `{"schemaVersion":1,"planId":"empty","tag":"main","milestones":[],"tasks":[],"assignments":[]}`)
	result, err := db.ApplyOrchestratorPlan(p, p.Digest, "empty-req")
	if err != nil {
		t.Fatal(err)
	}
	if result.Replayed || len(result.Receipt["taskMappings"].([]any)) != 0 {
		t.Fatalf("empty apply = %+v", result)
	}
	var plans, receipts, events int
	db.db.QueryRow(`SELECT COUNT(*) FROM plans`).Scan(&plans)
	db.db.QueryRow(`SELECT COUNT(*) FROM receipts WHERE kind='plan'`).Scan(&receipts)
	db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE kind='plan'`).Scan(&events)
	if plans != 1 || receipts != 1 || events != 1 {
		t.Fatalf("plans=%d receipts=%d events=%d", plans, receipts, events)
	}
}
