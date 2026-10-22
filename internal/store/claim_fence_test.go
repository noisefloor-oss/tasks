package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

func TestClaimFenceProtectsTaskMutationSurfaces(t *testing.T) {
	db, _ := newDB(t)
	target := addTask(t, db, "main", "target")
	claim, _, _, err := db.AcquireClaim("main", target, "agent-a", 300, "req-fence-target")
	if err != nil {
		t.Fatal(err)
	}
	gate := &ClaimGate{Owner: "agent-a", Token: claim.Token}
	if err := db.UpdateTask("main", target, 1, nil, func(*Task) error { return nil }); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("ungated update: %v, want ClaimConflict", err)
	}
	if _, err := db.AppendDetails("main", target, "note", 1, nil); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("ungated append: %v, want ClaimConflict", err)
	}
	if err := db.UpdateTask("main", target, 1, gate, func(task *Task) error { task.Title = "updated"; return nil }); err != nil {
		t.Fatalf("gated update: %v", err)
	}
	if _, err := db.RemoveTask("main", target, 2, false, nil); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("ungated remove: %v, want ClaimConflict", err)
	}

	parent := addTask(t, db, "main", "parent")
	subtasks := json.RawMessage(fmt.Sprintf(`[{"id":1,"title":"leaf","status":"pending","dependencies":[],"parentTaskId":%q}]`, parent))
	if err := db.UpdateTask("main", parent, 1, nil, func(task *Task) error { task.Subtasks = subtasks; return nil }); err != nil {
		t.Fatal(err)
	}
	parentClaim, _, _, err := db.AcquireClaim("main", parent, "agent-b", 300, "req-fence-parent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveTask("main", parent+".1", 2, false, nil); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("ungated subtask remove: %v, want ClaimConflict", err)
	}
	if _, err := db.RemoveTask("main", parent+".1", 2, false,
		&ClaimGate{Owner: "agent-b", Token: parentClaim.Token}); err != nil {
		t.Fatalf("gated subtask remove: %v", err)
	}
}

func TestNextFenceRefusesCorruptOrExhaustedCounter(t *testing.T) {
	for _, raw := range []string{"", "abc", "01", "-1", "9223372036854775807"} {
		t.Run(fmt.Sprintf("value_%q", raw), func(t *testing.T) {
			db, _ := newDB(t)
			id := addTask(t, db, "main", "target")
			if _, err := db.db.Exec(`UPDATE meta SET value=? WHERE key='fencing'`, raw); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-corrupt-fence"); err == nil || !strings.Contains(err.Error(), "fencing counter") {
				t.Fatalf("AcquireClaim with counter %q: %v, want fencing refusal", raw, err)
			}
			var got string
			if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='fencing'`).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got != raw {
				t.Fatalf("counter changed on refusal: got %q, want %q", got, raw)
			}
			task, err := db.GetTask("main", id)
			if err != nil {
				t.Fatal(err)
			}
			if task.Claim != nil {
				t.Fatalf("claim published on fencing refusal: %+v", task.Claim)
			}
		})
	}

	t.Run("missing", func(t *testing.T) {
		db, _ := newDB(t)
		id := addTask(t, db, "main", "target")
		if _, err := db.db.Exec(`DELETE FROM meta WHERE key='fencing'`); err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := db.AcquireClaim("main", id, "agent-a", 300, "req-missing-fence"); err == nil || !strings.Contains(err.Error(), "fencing counter") {
			t.Fatalf("AcquireClaim with missing counter: %v, want fencing refusal", err)
		}
	})
}

func TestClaimFenceProtectsCollateralRewrites(t *testing.T) {
	db, _ := newDB(t)
	source := addTask(t, db, "main", "source")
	dependent, err := db.AddTask("main", &Task{Title: "dependent", Status: "pending", Dependencies: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.AcquireClaim("main", dependent, "agent-a", 300, "req-fence-dependent"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveTask("main", source, 1, true, nil); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("force strip of claimed dependent: %v, want ClaimConflict", err)
	}

	if err := db.AddMilestone("main", "m"); err != nil {
		t.Fatal(err)
	}
	milestoned := addTask(t, db, "main", "milestoned")
	if _, err := db.SetTaskMilestone("main", milestoned, "m", 1, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.AcquireClaim("main", milestoned, "agent-b", 300, "req-fence-milestone"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveMilestone("main", "m", true); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("force milestone clear: %v, want ClaimConflict", err)
	}
	if _, err := db.SetTaskMilestone("main", milestoned, " padded ", 2, nil); !errors.As(err, new(*backlog.ErrMilestoneName)) {
		t.Fatalf("invalid milestone precedence: %v", err)
	}
	if _, err := db.SetTaskMilestone("main", milestoned, "unknown", 2, nil); !errors.As(err, new(*backlog.ErrMilestoneNotFound)) {
		t.Fatalf("unknown milestone precedence: %v", err)
	}
}

// TestForcedRemoveRefusesClaimedDependentEvenGated pins §8.3: the supplied
// gate can authorize only the command's named target. A forced removal that
// would strip a reference from a claimed dependent refuses the whole
// transaction even when that dependent's own owner and token are supplied.
func TestForcedRemoveRefusesClaimedDependentEvenGated(t *testing.T) {
	db, _ := newDB(t)
	source := addTask(t, db, "main", "source")
	dependent, err := db.AddTask("main", &Task{Title: "dependent", Status: "pending", Dependencies: []string{source}})
	if err != nil {
		t.Fatal(err)
	}
	claim, _, _, err := db.AcquireClaim("main", dependent, "agent-a", 300, "req-collateral-dependent")
	if err != nil {
		t.Fatal(err)
	}
	gate := &ClaimGate{Owner: "agent-a", Token: claim.Token}
	if _, err := db.RemoveTask("main", source, 1, true, gate); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("forced strip of claimed dependent with its own gate: %v, want ClaimConflict", err)
	}
	// The refusal covers the whole transaction: neither row changed.
	kept, err := db.GetTask("main", source)
	if err != nil || kept == nil {
		t.Fatalf("source removed by refused transaction: %v, %v", kept, err)
	}
	dep, err := db.GetTask("main", dependent)
	if err != nil {
		t.Fatal(err)
	}
	if len(dep.Dependencies) != 1 || dep.Revision != 1 || dep.Claim == nil {
		t.Errorf("dependent rewritten by refused transaction: %+v", dep)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range feed.Events {
		kinds = append(kinds, e.Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]string{"add", "add", "claim"}) {
		t.Errorf("feed kinds = %v, want [add add claim]", kinds)
	}
}

// TestRemoveTaskValidationPrecedesFencing pins the §8.3 precedence for the
// hierarchical surface: the stale-revision conflict comes first, then the
// independently invalid structural conditions (--force, missing subtask,
// dependent subtasks), then the claim fence — and validation is side-effect
// free.
func TestRemoveTaskValidationPrecedesFencing(t *testing.T) {
	db, _ := newDB(t)
	parent := addTask(t, db, "main", "parent")
	subs := json.RawMessage(fmt.Sprintf(`[{"id":1,"title":"leaf","status":"pending","dependencies":[],"parentTaskId":%q},{"id":2,"title":"watcher","status":"pending","dependencies":["1"],"parentTaskId":%q},{"id":3,"title":"free","status":"pending","dependencies":[],"parentTaskId":%q}]`,
		parent, parent, parent))
	if err := db.UpdateTask("main", parent, 1, nil, func(task *Task) error { task.Subtasks = subs; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.AcquireClaim("main", parent, "agent-a", 300, "req-remove-parent"); err != nil {
		t.Fatal(err)
	}

	// Hierarchical --force is independently invalid and outranks the fence.
	_, err := db.RemoveTask("main", parent+".1", 2, true, nil)
	if err == nil || errors.As(err, new(*ClaimConflict)) || !strings.Contains(err.Error(), "--force is not supported") {
		t.Fatalf("hierarchical force: %v, want structural refusal", err)
	}
	// A missing subtask outranks the fence.
	if _, err := db.RemoveTask("main", parent+".9", 2, false, nil); err == nil || errors.As(err, new(*ClaimConflict)) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing subtask: %v, want not-found refusal", err)
	}
	// A dependent subtask outranks the fence.
	if _, err := db.RemoveTask("main", parent+".1", 2, false, nil); err == nil || errors.As(err, new(*ClaimConflict)) || !strings.Contains(err.Error(), "possible dependency") {
		t.Fatalf("dependent subtask: %v, want dependency refusal", err)
	}
	// The stale revision outranks the structural conditions.
	if _, err := db.RemoveTask("main", parent+".1", 1, false, nil); !errors.As(err, new(*StaleWriteConflict)) {
		t.Fatalf("stale remove: %v, want StaleWriteConflict", err)
	}
	// A structurally valid removal still fences.
	if _, err := db.RemoveTask("main", parent+".3", 2, false, nil); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("valid ungated subtask remove: %v, want ClaimConflict", err)
	}

	// Validation was side-effect free: the row, its subtasks, the claim,
	// and the feed are untouched.
	task, err := db.GetTask("main", parent)
	if err != nil {
		t.Fatal(err)
	}
	if task.Revision != 2 || task.Claim == nil {
		t.Errorf("parent mutated by refused validations: %+v", task)
	}
	var remaining []struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(task.Subtasks, &remaining); err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 3 {
		t.Errorf("subtasks after refusals = %s", task.Subtasks)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range feed.Events {
		kinds = append(kinds, e.Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]string{"add", "update", "claim"}) {
		t.Errorf("feed kinds = %v, want [add update claim]", kinds)
	}
}

// TestRemoveTaskTopLevelDependencyRefusalPrecedesFencing pins the §8.3
// precedence for the top-level surface: the unforced dependency refusal is
// dependency validation and is reported before the target's claim fence.
func TestRemoveTaskTopLevelDependencyRefusalPrecedesFencing(t *testing.T) {
	db, _ := newDB(t)
	target := addTask(t, db, "main", "target")
	if _, err := db.AddTask("main", &Task{Title: "dependent", Dependencies: []string{target}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := db.AcquireClaim("main", target, "agent-a", 300, "req-remove-target"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RemoveTask("main", target, 1, false, nil); err == nil || errors.As(err, new(*ClaimConflict)) || !strings.Contains(err.Error(), "is a dependency of") {
		t.Fatalf("unforced dependent removal: %v, want dependency refusal", err)
	}
	// With --force the dependency condition is satisfied and the target's
	// fence applies.
	if _, err := db.RemoveTask("main", target, 1, true, nil); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("forced removal of claimed target: %v, want ClaimConflict", err)
	}
	if kept, err := db.GetTask("main", target); err != nil || kept == nil {
		t.Fatalf("target removed by refused transaction: %v, %v", kept, err)
	}
}

// TestUpdateTaskValidationPrecedesFencing pins §8.3: the proposed mutation is
// applied and validated in memory before the claim fence, nothing persists
// before the gate, and the stale revision outranks both.
func TestUpdateTaskValidationPrecedesFencing(t *testing.T) {
	db, _ := newDB(t)
	target := addTask(t, db, "main", "target")
	claim, _, _, err := db.AcquireClaim("main", target, "agent-a", 300, "req-update-target")
	if err != nil {
		t.Fatal(err)
	}
	// An independently invalid proposed value reports its validation error,
	// not the claim conflict.
	err = db.UpdateTask("main", target, 1, nil, func(task *Task) error { task.Status = "bogus"; return nil })
	if err == nil || errors.As(err, new(*ClaimConflict)) || !strings.Contains(err.Error(), "invalid status") {
		t.Fatalf("invalid proposal: %v, want validation error", err)
	}
	// A proposal fn refusal likewise precedes the fence.
	refused := errors.New("proposal refused")
	if err := db.UpdateTask("main", target, 1, nil, func(*Task) error { return refused }); !errors.Is(err, refused) {
		t.Fatalf("fn refusal: %v, want the proposal error", err)
	}
	// Nothing persisted before the gate.
	task, err := db.GetTask("main", target)
	if err != nil {
		t.Fatal(err)
	}
	if task.Revision != 1 || task.Status != "pending" || task.Title != "target" || task.Claim == nil {
		t.Errorf("task mutated by refused proposals: %+v", task)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range feed.Events {
		kinds = append(kinds, e.Kind)
	}
	if fmt.Sprint(kinds) != fmt.Sprint([]string{"add", "claim"}) {
		t.Errorf("feed kinds = %v, want [add claim]", kinds)
	}
	// A valid proposal still fences without the gate.
	if err := db.UpdateTask("main", target, 1, nil, func(task *Task) error { task.Title = "renamed"; return nil }); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("valid ungated update: %v, want ClaimConflict", err)
	}
	// The proposal callback cannot clear the stored claim to bypass the
	// fence. Claim and binding are coordination state, not mutable §1 fields.
	if err := db.UpdateTask("main", target, 1, nil, func(task *Task) error {
		task.Claim = nil
		task.Title = "bypass"
		return nil
	}); !errors.As(err, new(*ClaimConflict)) {
		t.Fatalf("claim-clearing proposal: %v, want ClaimConflict", err)
	}
	// The stale revision outranks validation.
	err = db.UpdateTask("main", target, 7, &ClaimGate{Owner: "agent-a", Token: claim.Token}, func(task *Task) error { task.Status = "bogus"; return nil })
	if !errors.As(err, new(*StaleWriteConflict)) {
		t.Fatalf("stale update: %v, want StaleWriteConflict", err)
	}
	if err := db.SetBinding("main", target, &WorktreeBinding{Path: "/worktree", Boundary: "/repo", Branch: "task-1", CreatedAt: "2026-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	// The gate still authorizes a valid §1 proposal, but even a callback that
	// mutates the pointed-to coordination values cannot persist those changes.
	if err := db.UpdateTask("main", target, 1, &ClaimGate{Owner: "agent-a", Token: claim.Token}, func(task *Task) error {
		task.Title = "renamed"
		task.Claim.Owner = "forged"
		task.Claim.Token = 999
		task.Binding.Path = "/forged"
		return nil
	}); err != nil {
		t.Fatalf("gated update: %v", err)
	}
	updated, err := db.GetTask("main", target)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "renamed" || updated.Claim == nil || updated.Claim.Owner != "agent-a" || updated.Claim.Token != claim.Token || updated.Binding == nil || updated.Binding.Path != "/worktree" {
		t.Fatalf("coordination state changed through proposal callback: %+v", updated)
	}
}
