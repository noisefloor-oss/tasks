package store

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

// boundProvisionFixture plans and binds one worktree provision for a live
// run: the state every automatic-cleanup parent acts on.
func boundProvisionFixture(t *testing.T, suffix string) (*DB, WorktreeProvisionOptions, *WorktreeProvisionPlan, *Claim, *RunStartResult) {
	t.Helper()
	db, provision, claim, run := provisionFixture(t, "wt.auto."+suffix)
	source, err := db.PlanWorktreeProvision(provision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeProvision(source, WorktreeProvisionBound); err != nil {
		t.Fatal(err)
	}
	return db, provision, source, claim, run
}

// scheduledCleanupRow loads the one remove saga for a tag/task with its
// identity columns, or fails when none exists.
func scheduledCleanupRow(t *testing.T, db *DB, tag, task string) (request string, sagaID, runID, sourceID, fence, force int64, status string) {
	t.Helper()
	var run sql.NullInt64
	if err := db.db.QueryRow(`SELECT saga_id,run_id,source_saga_id,saga_fence,request_id,force,status
		FROM worktree_sagas WHERE tag=? AND task_id=? AND operation='remove' ORDER BY saga_id`,
		tag, task).Scan(&sagaID, &run, &sourceID, &fence, &request, &force, &status); err != nil {
		t.Fatalf("read scheduled cleanup for %s/%s: %v", tag, task, err)
	}
	if run.Valid {
		runID = run.Int64
	}
	return request, sagaID, runID, sourceID, fence, force, status
}

// TestAutomaticCleanupRequestIDVectors pins the exact §9.4 automatic request
// id derivations against golden digests over the spec's example family: the
// run-terminal cause, the literal task-done cause, and the legacy conversion
// domain already used by the v4→v5 upgrade.
func TestAutomaticCleanupRequestIDVectors(t *testing.T) {
	family := "123e4567-e89b-42d3-a456-426614174000"
	vectors := []struct {
		name string
		got  string
		want string
	}{
		{"run cause", automaticCleanupRequestID(family, 1, 7),
			"auto-0835a026370b884856a9857a074c058a4659bcfb76bbebffc870c3d814816b8f"},
		{"task-done cause", automaticCleanupRequestID(family, 1, 0),
			"auto-33de24e91ad8e29970b65442888393f3e25d1198726ca1fd8f5ce6055fa74229"},
		{"legacy conversion", legacyWorktreeSagaRequestID(family, "main", "42",
			"/repo/.worktrees/main-42", "tasks/main/42"),
			"auto-d047996c27e9c3f91b10f983c1942adbbce5b91582068eb94e93987e7eb5ef97"},
	}
	for _, v := range vectors {
		if v.got != v.want {
			t.Errorf("%s request id = %q, want %q", v.name, v.got, v.want)
		}
		if !strings.HasPrefix(v.got, "auto-") || !ValidLocalKey(v.got) {
			t.Errorf("%s request id %q is not a valid auto- local key", v.name, v.got)
		}
	}
	// Distinct causes and sources never collide.
	if automaticCleanupRequestID(family, 1, 7) == automaticCleanupRequestID(family, 2, 7) ||
		automaticCleanupRequestID(family, 1, 7) == automaticCleanupRequestID(family, 1, 8) {
		t.Fatal("automatic derivation is not injective over source and cause")
	}
}

// TestRunFinishSucceededSchedulesUnforcedCleanupRemoval covers the insert
// path: run finish recording succeeded inserts one unforced remove/pending
// saga against the active bound source, referencing the source run, with the
// derived automatic request id. The parent's own event is the only appended
// event, its receipt's range still covers exactly that one event, and no
// worktree receipt exists.
func TestRunFinishSucceededSchedulesUnforcedCleanupRemoval(t *testing.T) {
	db, provision, source, claim, run := boundProvisionFixture(t, "finish")
	familyID := testFamilyID(t, db)
	eventsBefore := len(feedOf(t, db))
	if _, _, err := db.FinishRun(run.RunID, provision.Owner, claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.auto", "", 0, ""); err != nil {
		t.Fatal(err)
	}
	want := automaticCleanupRequestID(familyID, source.SagaID, run.RunID)
	request, sagaID, rowRun, rowSource, fence, force, status := scheduledCleanupRow(t, db, "main", source.Task)
	if request != want || sagaID <= source.SagaID || rowRun != run.RunID || rowSource != source.SagaID ||
		status != "pending" || force != 0 || fence <= source.SagaToken {
		t.Fatalf("scheduled cleanup = request %q saga %d run %d source %d fence %d force %d status %s; want request %q saga>%d run %d source %d fence>%d force 0 pending",
			request, sagaID, rowRun, rowSource, fence, force, status, want, source.SagaID, run.RunID, source.SagaID, source.SagaToken)
	}
	var boundary, registration, target, branch, base string
	if err := db.db.QueryRow(`SELECT owner_boundary,registration_root,target_path,branch,base_commit
		FROM worktree_sagas WHERE saga_id=?`, sagaID).
		Scan(&boundary, &registration, &target, &branch, &base); err != nil {
		t.Fatal(err)
	}
	if boundary != source.OwnerBoundary || registration != source.RegistrationRoot ||
		target != source.TargetPath || branch != source.Branch || base != source.BaseCommit {
		t.Fatalf("scheduled cleanup identity = %q %q %q %q %q; want source %q %q %q %q %q",
			boundary, registration, target, branch, base,
			source.OwnerBoundary, source.RegistrationRoot, source.TargetPath, source.Branch, source.BaseCommit)
	}
	if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID); active != 1 {
		t.Fatalf("source binding_active = %d after scheduling", active)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, want); err != nil || receipt != nil {
		t.Fatalf("scheduled cleanup receipt = %v, %v; want none until an effect commits", receipt, err)
	}
	// The parent event audits the decision: exactly one new event, no
	// worktree event, and the finish receipt's range covers exactly it.
	events := feedOf(t, db)
	if len(events) != eventsBefore+1 {
		t.Fatalf("finish appended %d events, want exactly 1", len(events)-eventsBefore)
	}
	if last := events[len(events)-1]; last.Kind != ReceiptKindRun || last.Action != "finished" {
		t.Fatalf("finish event = %+v", last)
	}
	receipt, err := db.FindReceipt(ReceiptKindRun, "finish.auto")
	if err != nil || receipt == nil {
		t.Fatalf("finish receipt = %v, %v", receipt, err)
	}
	seq := receipt["eventSeq"].(map[string]any)
	if seq["first"] != seq["last"] || seq["first"] == nil {
		t.Fatalf("finish receipt eventSeq = %v, want exactly one event", seq)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate scheduled state = %v", err)
	}
	// A failed finish leaves the run retryable and schedules nothing.
	db2, provision2, _, claim2, run2 := boundProvisionFixture(t, "finish-failed")
	if _, _, err := db2.FinishRun(run2.RunID, provision2.Owner, claim2.Token, 1, run2.AttemptToken,
		"failed", "finish.nofx", "", 0, ""); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt64(t, db2, `SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`); n != 0 {
		t.Fatalf("failed finish scheduled %d removals", n)
	}
}

// TestRunTerminalParentsScheduleCleanup covers the remaining run-caused
// parents — unforced cancel, forced cancel, and force release that
// terminalizes the run — against the shared insert path.
func TestRunTerminalParentsScheduleCleanup(t *testing.T) {
	cases := []struct {
		name   string
		parent func(t *testing.T, db *DB, provision WorktreeProvisionOptions, source *WorktreeProvisionPlan, claim *Claim, run *RunStartResult)
	}{
		{"unforced cancel", func(t *testing.T, db *DB, p WorktreeProvisionOptions, s *WorktreeProvisionPlan, c *Claim, r *RunStartResult) {
			if _, _, err := db.CancelRun(r.RunID, p.Owner, c.Token, 1, r.AttemptToken, false, "run.cancel.auto"); err != nil {
				t.Fatal(err)
			}
		}},
		{"forced cancel", func(t *testing.T, db *DB, p WorktreeProvisionOptions, s *WorktreeProvisionPlan, c *Claim, r *RunStartResult) {
			if _, _, err := db.CancelRun(r.RunID, "", 0, 0, 0, true, "run.cancel.forced"); err != nil {
				t.Fatal(err)
			}
		}},
		{"force release", func(t *testing.T, db *DB, p WorktreeProvisionOptions, s *WorktreeProvisionPlan, c *Claim, r *RunStartResult) {
			if err := db.ReleaseClaim("main", s.Task, "operator", 0, true); err != nil {
				t.Fatal(err)
			}
		}},
		{"force release through the worktree surface", func(t *testing.T, db *DB, p WorktreeProvisionOptions, s *WorktreeProvisionPlan, c *Claim, r *RunStartResult) {
			if _, err := db.ReleaseClaimWorktree("", "main", s.Task, "operator", 0, true, false); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, provision, source, claim, run := boundProvisionFixture(t, strings.ReplaceAll(tc.name, " ", "-"))
			familyID := testFamilyID(t, db)
			tc.parent(t, db, provision, source, claim, run)
			request, _, rowRun, rowSource, _, force, status := scheduledCleanupRow(t, db, "main", source.Task)
			if request != automaticCleanupRequestID(familyID, source.SagaID, run.RunID) ||
				rowRun != run.RunID || rowSource != source.SagaID || status != "pending" || force != 0 {
				t.Fatalf("scheduled cleanup = %q run %d source %d force %d status %s", request, rowRun, rowSource, force, status)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate scheduled state = %v", err)
			}
		})
	}
}

// TestTerminalParentAdoptsLiveRemovalWithManualForceBit pins §9.4 adoption:
// when the source already has a live linked removal, the parent adopts that
// exact saga — no second row, and no change to its identity, request id,
// run/source edge, fence, or manual force bit.
func TestTerminalParentAdoptsLiveRemovalWithManualForceBit(t *testing.T) {
	db, provision, source, claim, run := boundProvisionFixture(t, "adopt")
	options := WorktreeRemoveOptions{RunID: run.RunID, Owner: provision.Owner,
		ClaimToken: claim.Token, Attempt: run.Attempt, AttemptToken: run.AttemptToken,
		RequestID: "wt.remove.manual-adopt", Force: true}
	manual, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CancelRun(run.RunID, provision.Owner, claim.Token, 1, run.AttemptToken, false, "run.cancel.adopt"); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas`); n != 2 {
		t.Fatalf("adopt inserted a second saga; total rows = %d", n)
	}
	request, sagaID, rowRun, rowSource, fence, force, status := scheduledCleanupRow(t, db, "main", source.Task)
	if request != options.RequestID || sagaID != manual.SagaID || rowRun != run.RunID ||
		rowSource != source.SagaID || fence != manual.SagaToken || force != 1 || status != "pending" {
		t.Fatalf("adopted manual removal changed: request %q saga %d run %d source %d fence %d force %d status %s",
			request, sagaID, rowRun, rowSource, fence, force, status)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate adopted state = %v", err)
	}
}

// TestTaskDoneSchedulesRunlessCleanupAndRepeatCoalesces covers the direct
// top-level set-status done path: it schedules one runless unforced removal
// with the task-done request id, and repeating the done transition after
// reopening coalesces onto the same saga.
func TestTaskDoneSchedulesRunlessCleanupAndRepeatCoalesces(t *testing.T) {
	for _, surface := range []struct {
		name string
		done func(t *testing.T, db *DB, tag, task string, gate *ClaimGate, revision int)
	}{
		{"SetStatus", func(t *testing.T, db *DB, tag, task string, gate *ClaimGate, revision int) {
			if _, err := db.SetStatus(tag, task, "done", revision, gate); err != nil {
				t.Fatal(err)
			}
		}},
		{"SetStatusWorktree", func(t *testing.T, db *DB, tag, task string, gate *ClaimGate, revision int) {
			if _, _, err := db.SetStatusWorktree("", tag, task, "done", revision, gate, false); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(surface.name, func(t *testing.T) {
			db, provision, source, claim, run := boundProvisionFixture(t, "done-"+surface.name)
			familyID := testFamilyID(t, db)
			task, err := db.GetTask("main", source.Task)
			if err != nil {
				t.Fatal(err)
			}
			// The task's run is still live, so the direct done transition is
			// gated by it; the scheduled cleanup row is runless regardless.
			gate := &ClaimGate{Owner: provision.Owner, Token: claim.Token, RunID: run.RunID,
				Attempt: run.Attempt, AttemptToken: run.AttemptToken}
			surface.done(t, db, "main", task.ID, gate, task.Revision)
			want := automaticCleanupRequestID(familyID, source.SagaID, 0)
			request, sagaID, rowRun, rowSource, fence, force, status := scheduledCleanupRow(t, db, "main", task.ID)
			if request != want || rowRun != 0 || rowSource != source.SagaID || status != "pending" || force != 0 {
				t.Fatalf("runless cleanup = %q saga %d run %d source %d force %d status %s", request, sagaID, rowRun, rowSource, force, status)
			}
			if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`); n != 1 {
				t.Fatalf("task-done scheduled %d removals", n)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate runless state = %v", err)
			}
			// Reopen and re-done: done cleared the claim, so both further
			// transitions pass ungated; repeat scheduling adopts the same
			// saga instead of inserting a second row.
			reopened, err := db.GetTask("main", task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SetStatus("main", task.ID, "review", reopened.Revision, nil); err != nil {
				t.Fatal(err)
			}
			again, err := db.GetTask("main", task.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SetStatus("main", task.ID, "done", again.Revision, nil); err != nil {
				t.Fatal(err)
			}
			if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`); n != 1 {
				t.Fatalf("repeat scheduling inserted a second removal; rows = %d", n)
			}
			request2, sagaID2, rowRun2, rowSource2, fence2, force2, status2 := scheduledCleanupRow(t, db, "main", task.ID)
			if request2 != want || sagaID2 != sagaID || rowRun2 != 0 || rowSource2 != source.SagaID ||
				fence2 != fence || force2 != 0 || status2 != "pending" {
				t.Fatalf("repeat scheduling changed the saga: request %q saga %d run %d source %d fence %d force %d status %s",
					request2, sagaID2, rowRun2, rowSource2, fence2, force2, status2)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate coalesced state = %v", err)
			}
		})
	}
}

// TestTaskDoneAfterFailedCleanupRefusesCoalescing pins the terminal half of
// the generated-id collision rule. The task-done id is deterministic over the
// source saga, so done → reopen → failed cleanup → done again re-derives the
// same id over the terminal saga and its original receipt. §9.4 adopts only a
// live linked removal and mandates a fresh insert otherwise, but never a
// second row over a taken generated id: the collision is with a nonidentical
// (terminal) saga, so the second done fails closed with the task left
// reopened and the failed cleanup untouched, never done with an active bound
// source and no live removal.
func TestTaskDoneAfterFailedCleanupRefusesCoalescing(t *testing.T) {
	db, provision, source, claim, run := boundProvisionFixture(t, "done-failed")
	familyID := testFamilyID(t, db)
	task, err := db.GetTask("main", source.Task)
	if err != nil {
		t.Fatal(err)
	}
	gate := &ClaimGate{Owner: provision.Owner, Token: claim.Token, RunID: run.RunID,
		Attempt: run.Attempt, AttemptToken: run.AttemptToken}
	if _, err := db.SetStatus("main", task.ID, "done", task.Revision, gate); err != nil {
		t.Fatal(err)
	}
	want := automaticCleanupRequestID(familyID, source.SagaID, 0)
	request, sagaID, rowRun, rowSource, _, force, status := scheduledCleanupRow(t, db, "main", task.ID)
	if request != want || rowRun != 0 || rowSource != source.SagaID || force != 0 || status != "pending" {
		t.Fatalf("runless cleanup = %q saga %d run %d source %d force %d status %s",
			request, sagaID, rowRun, rowSource, force, status)
	}
	// Reopen — done cleared the claim, so the transition passes ungated.
	reopened, err := db.GetTask("main", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetStatus("main", task.ID, "review", reopened.Revision, nil); err != nil {
		t.Fatal(err)
	}
	// With the task reopened and the source run still nonterminal, ordinary
	// removal semantics apply: recovery may terminalize the cleanup failed.
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryFailed); err != nil {
		t.Fatal(err)
	}
	if failed, generation, _ := sagaRow(t, db, sagaID); failed != "failed" || generation != 1 {
		t.Fatalf("terminal cleanup = %s/%d; want failed/1", failed, generation)
	}
	// Terminalizing also wrote the original receipt under the derived id,
	// whose namespace the terminal saga now permanently holds.
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, want); err != nil || receipt == nil {
		t.Fatalf("failed cleanup original receipt = %v, %v; want one under %q", receipt, err, want)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate failed cleanup = %v", err)
	}
	// Done again re-derives the same id over the terminal saga: no silent
	// coalesce with the dead removal and no second row — the parent fails.
	again, err := db.GetTask("main", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetStatus("main", task.ID, "done", again.Revision, nil); !errors.As(err, new(*ReceiptIntegrityError)) {
		t.Fatalf("done over a terminal cleanup = %v; want ReceiptIntegrityError", err)
	}
	if after, err := db.GetTask("main", task.ID); err != nil || after.Status != "review" {
		t.Fatalf("task after refused done = %s, %v; want still review", after.Status, err)
	}
	if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`); n != 1 {
		t.Fatalf("refused done left %d removals; want the one terminal saga", n)
	}
	if failed, generation, _ := sagaRow(t, db, sagaID); failed != "failed" || generation != 1 {
		t.Fatalf("terminal cleanup after refusal = %s/%d; want failed/1", failed, generation)
	}
	if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID); active != 1 {
		t.Fatalf("source binding_active = %d; want the failed cleanup to leave it bound", active)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate refused state = %v", err)
	}
}

// TestTaskDoneAdoptsRunCausedCleanup pins the cross-cause coalescing: after
// the run terminalized and scheduled its run-caused removal, the direct
// task-done parent adopts that exact live saga instead of inserting a
// runless sibling.
func TestTaskDoneAdoptsRunCausedCleanup(t *testing.T) {
	db, provision, source, claim, run := boundProvisionFixture(t, "done-adopts")
	familyID := testFamilyID(t, db)
	if _, _, err := db.FinishRun(run.RunID, provision.Owner, claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.before-done", "", 0, ""); err != nil {
		t.Fatal(err)
	}
	task, err := db.GetTask("main", source.Task)
	if err != nil {
		t.Fatal(err)
	}
	// The run is terminal, so only the claim gate applies.
	if _, err := db.SetStatus("main", task.ID, "done", task.Revision,
		&ClaimGate{Owner: provision.Owner, Token: claim.Token}); err != nil {
		t.Fatal(err)
	}
	want := automaticCleanupRequestID(familyID, source.SagaID, run.RunID)
	if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`); n != 1 {
		t.Fatalf("task-done after finish inserted a second removal; rows = %d", n)
	}
	request, _, rowRun, rowSource, _, force, status := scheduledCleanupRow(t, db, "main", task.ID)
	if request != want || rowRun != run.RunID || rowSource != source.SagaID || force != 0 || status != "pending" {
		t.Fatalf("adopted run-caused cleanup = %q run %d source %d force %d status %s", request, rowRun, rowSource, force, status)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate adopted state = %v", err)
	}
}

// TestTerminalParentFlipsNonterminalProvisionToCleanupRequired pins §9.4's
// provision flip: when the only live effect is the run's nonterminal
// provision, the parent changes it to cleanup-required instead of inventing a
// removal source, without advancing its recovery generation.
func TestTerminalParentFlipsNonterminalProvisionToCleanupRequired(t *testing.T) {
	cases := []struct {
		name       string
		status     string
		generation int64
		recover    func(t *testing.T, db *DB, plan *WorktreeProvisionPlan)
	}{
		{name: "pending", status: "pending"},
		{name: "needs-broker", status: "needs-broker"},
		{name: "needs-broker at generation 1", status: "needs-broker", generation: 1,
			recover: func(t *testing.T, db *DB, plan *WorktreeProvisionPlan) {
				attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker); err != nil {
					t.Fatal(err)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name+" via run cancel", func(t *testing.T) {
			db, opts, claim, run := provisionFixture(t, "wt.auto.flip."+strings.ReplaceAll(tc.name, " ", "-"))
			plan, err := db.PlanWorktreeProvision(opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status != "pending" {
				if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker); err != nil {
					t.Fatal(err)
				}
			}
			if tc.recover != nil {
				tc.recover(t, db, plan)
			}
			if _, _, err := db.CancelRun(run.RunID, opts.Owner, claim.Token, 1, run.AttemptToken, false, "run.cancel.flip"); err != nil {
				t.Fatal(err)
			}
			gotStatus, gotGeneration, _ := sagaRow(t, db, plan.SagaID)
			if gotStatus != "cleanup-required" || gotGeneration != tc.generation {
				t.Fatalf("flipped provision = %s generation %d; want cleanup-required generation %d",
					gotStatus, gotGeneration, tc.generation)
			}
			if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`); n != 0 {
				t.Fatalf("flip invented %d removal sources", n)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate flipped state = %v", err)
			}
		})
	}
	// The direct task-done cause flips the task's own nonterminal
	// provision, which a done task can never bind.
	t.Run("pending via task done", func(t *testing.T) {
		db, opts, claim, run := provisionFixture(t, "wt.auto.flip.done")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		task, err := db.GetTask("main", plan.Task)
		if err != nil {
			t.Fatal(err)
		}
		gate := &ClaimGate{Owner: opts.Owner, Token: claim.Token, RunID: run.RunID,
			Attempt: run.Attempt, AttemptToken: run.AttemptToken}
		if _, err := db.SetStatus("main", task.ID, "done", task.Revision, gate); err != nil {
			t.Fatal(err)
		}
		gotStatus, gotGeneration, _ := sagaRow(t, db, plan.SagaID)
		if gotStatus != "cleanup-required" || gotGeneration != 0 {
			t.Fatalf("done-flipped provision = %s generation %d; want cleanup-required generation 0", gotStatus, gotGeneration)
		}
		if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`); n != 0 {
			t.Fatalf("flip invented %d removal sources", n)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate done-flipped state = %v", err)
		}
	})
}

// TestAutoCleanupRequestCollisionIsCorruption proves a derived request id
// colliding with a nonidentical saga or a receipt fails the parent
// transaction closed: the run is left nonterminal, nothing half-scheduled.
func TestAutoCleanupRequestCollisionIsCorruption(t *testing.T) {
	finish := func(t *testing.T, db *DB, provision WorktreeProvisionOptions, claim *Claim, run *RunStartResult, request string) error {
		_, _, err := db.FinishRun(run.RunID, provision.Owner, claim.Token, 1, run.AttemptToken,
			"succeeded", request, "", 0, "")
		return err
	}
	t.Run("nonidentical saga", func(t *testing.T) {
		db, provision, source, claim, run := boundProvisionFixture(t, "collide-saga")
		familyID := testFamilyID(t, db)
		request := automaticCleanupRequestID(familyID, source.SagaID, run.RunID)
		// Forge a terminal forced removal under the derived id: the same
		// identity fields, but a force bit no automatic cleanup may set.
		if _, err := db.db.Exec(`INSERT INTO worktree_sagas(
			run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
			request_id,recovery_generation,owner_boundary,registration_root,target_path,
			branch,base_commit,force,created_at,updated_at,extra)
			SELECT run_id,saga_id,tag,task_id,'remove','failed',0,?,
				?,0,owner_boundary,registration_root,target_path,branch,base_commit,1,updated_at,updated_at,NULL
			FROM worktree_sagas WHERE saga_id=?`, source.SagaID+1000, request, source.SagaID); err != nil {
			t.Fatal(err)
		}
		if err := finish(t, db, provision, claim, run, "finish.collide"); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("colliding finish = %v; want ReceiptIntegrityError", err)
		}
		if n := scalarInt64(t, db, `SELECT COUNT(*) FROM runs WHERE run_id=? AND status='running'`, run.RunID); n != 1 {
			t.Fatal("corrupt scheduling rolled the parent mutation in")
		}
	})
	t.Run("reserved receipt", func(t *testing.T) {
		db, provision, source, claim, run := boundProvisionFixture(t, "collide-receipt")
		familyID := testFamilyID(t, db)
		request := automaticCleanupRequestID(familyID, source.SagaID, run.RunID)
		if _, err := db.db.Exec(`INSERT INTO receipts(kind,receipt_id,request_id,body,digest,created_at)
			VALUES (?,?,?,?,?,?)`, ReceiptKindWorktree, "worktree-forged", request, "{}",
			"sha256:"+strings.Repeat("0", 64), "2026-08-29T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
		if err := finish(t, db, provision, claim, run, "finish.collide-receipt"); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("receipt-colliding finish = %v; want ReceiptIntegrityError", err)
		}
		if n := scalarInt64(t, db, `SELECT COUNT(*) FROM runs WHERE run_id=? AND status='running'`, run.RunID); n != 1 {
			t.Fatal("corrupt scheduling rolled the parent mutation in")
		}
	})
}

// TestAutoCleanupSkipsWorktreelessTasks pins the no-op: terminal parents on
// a task with no worktree saga schedule nothing and still succeed.
func TestAutoCleanupSkipsWorktreelessTasks(t *testing.T) {
	db, _, claim, run := startRunFixture(t, 300)
	if _, _, err := db.FinishRun(run.RunID, "agent-a", claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.bare", "done", 1, testResultDigest); err != nil {
		t.Fatal(err)
	}
	if n := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas`); n != 0 {
		t.Fatalf("worktreeless finish scheduled %d sagas", n)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatal(err)
	}
}
