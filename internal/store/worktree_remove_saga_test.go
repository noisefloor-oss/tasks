package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func removalFixture(t *testing.T, suffix string, force bool) (*DB, WorktreeRemoveOptions, *WorktreeProvisionPlan, *Claim, *RunStartResult) {
	t.Helper()
	db, provision, claim, run := provisionFixture(t, "wt.source."+suffix)
	source, err := db.PlanWorktreeProvision(provision)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeProvision(source, WorktreeProvisionBound); err != nil {
		t.Fatal(err)
	}
	return db, WorktreeRemoveOptions{RunID: run.RunID, Owner: provision.Owner,
		ClaimToken: claim.Token, Attempt: run.Attempt, AttemptToken: run.AttemptToken,
		RequestID: "wt.remove." + suffix, Force: force}, source, claim, run
}

func TestWorktreeRemoveDigestAndReceiptIDVector(t *testing.T) {
	input, err := worktreeInputDigest("agent-a", "wt.remove", 1, true, "remove")
	if err != nil || input != "sha256:d6b5817494d30d8012edba0990641b8dfc03396177beb0c155b6d396e9cdbc2f" {
		t.Fatalf("remove input digest = %q, %v", input, err)
	}
	want := "worktree-04ecc6d6a7743d7c743556130c0b32cbc1ccc30dd6013885f1c8ded95dc22c68"
	if got := worktreeReceiptID("wt.remove", input); got != want {
		t.Fatalf("remove receipt id = %q, want %q", got, want)
	}
}

func TestPlanWorktreeRemovalSnapshotsExactActiveSource(t *testing.T) {
	db, options, source, claim, run := removalFixture(t, "plan", true)
	beforeFence, _ := db.Fencing()
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.EffectRequired || plan.Result != nil || plan.SourceSagaID != source.SagaID ||
		plan.SagaID <= source.SagaID || plan.SagaToken <= beforeFence || plan.Force != options.Force ||
		plan.OwnerBoundary != source.OwnerBoundary || plan.RegistrationRoot != source.RegistrationRoot ||
		plan.TargetPath != source.TargetPath || plan.Branch != source.Branch || plan.BaseCommit != source.BaseCommit {
		t.Fatalf("remove plan = %+v; source=%+v", plan, source)
	}
	var runID, sourceID, fence, generation, force, active int64
	var operation, status, request, boundary, registration, target, branch, base string
	if err := db.db.QueryRow(`SELECT run_id,source_saga_id,operation,status,binding_active,saga_fence,
		request_id,recovery_generation,owner_boundary,registration_root,target_path,branch,base_commit,force
		FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&runID, &sourceID, &operation, &status,
		&active, &fence, &request, &generation, &boundary, &registration, &target, &branch, &base, &force); err != nil {
		t.Fatal(err)
	}
	if runID != options.RunID || sourceID != source.SagaID || operation != "remove" || status != "pending" ||
		active != 0 || fence != plan.SagaToken || request != options.RequestID || generation != 0 ||
		boundary != source.OwnerBoundary || registration != source.RegistrationRoot || target != source.TargetPath ||
		branch != source.Branch || base != source.BaseCommit || force != 1 {
		t.Fatalf("stored removal mismatch: run/source=%d/%d op/status=%s/%s active/fence=%d/%d request/gen=%s/%d identity=%q %q %q %q %q force=%d",
			runID, sourceID, operation, status, active, fence, request, generation,
			boundary, registration, target, branch, base, force)
	}
	var sourceActive int
	if err := db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&sourceActive); err != nil || sourceActive != 1 {
		t.Fatalf("source active after plan = %d, %v", sourceActive, err)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "worktree" || last.Action != "removing" || last.Owner != options.Owner || len(last.Extra) != 0 {
		t.Fatalf("removing event = %+v", last)
	}
	encoded, _ := json.Marshal(last)
	for _, forbidden := range []string{"claimToken", "attemptToken", "sagaToken", fmt.Sprint(claim.Token), fmt.Sprint(run.AttemptToken)} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("removing event leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestFinalizeWorktreeRemovalRemovedDeactivatesExactSourceAndReplays(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "removed", false)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveRemoved)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "removed" || result.Operation != "remove" || result.SagaToken != plan.SagaToken ||
		result.RecoverRequestID != nil || result.BrokerRequest != nil || result.RunID == nil || *result.RunID != options.RunID {
		t.Fatalf("removed result = %+v", result)
	}
	var childStatus string
	var childActive, sourceActive int
	if err := db.db.QueryRow(`SELECT status,binding_active FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).
		Scan(&childStatus, &childActive); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&sourceActive); err != nil {
		t.Fatal(err)
	}
	if childStatus != "removed" || childActive != 0 || sourceActive != 0 {
		t.Fatalf("removed child/source state = %s/%d source=%d", childStatus, childActive, sourceActive)
	}
	receipt, err := db.FindReceipt(ReceiptKindWorktree, options.RequestID)
	if err != nil || receipt == nil || receipt["operation"] != "remove" || receipt["status"] != "removed" ||
		receipt["active"] != false || receipt["sourceSagaId"] == nil || receipt["force"] != false {
		t.Fatalf("removed receipt = %v, %v", receipt, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate removed state = %v", err)
	}
	again, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveRemoved)
	if err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("finalize replay = %+v, %v; want %+v", again, err, result)
	}
	if _, err := db.PlanWorktreeRemoval(WorktreeRemoveOptions{RunID: options.RunID, Owner: options.Owner,
		ClaimToken: options.ClaimToken, Attempt: options.Attempt, AttemptToken: options.AttemptToken,
		RequestID: "wt.remove.after-removed"}); !errors.As(err, new(*WorktreeConflict)) {
		t.Fatalf("second removal after removed = %v", err)
	}
}

// TestPlanWorktreeRemovalDistinctRequestConflictsWithLiveEffect pins the
// one-live-effect rule across the exact nonterminal status set backed by
// worktree_sagas_one_live_effect: while any saga for the task is pending,
// needs-broker, or cleanup-required, a fresh distinct request id must get the
// typed WorktreeConflict, never a second saga row or a raw index error.
func TestPlanWorktreeRemovalDistinctRequestConflictsWithLiveEffect(t *testing.T) {
	for _, live := range []WorktreeRemoveOutcome{"pending", WorktreeRemoveNeedsBroker, WorktreeRemoveCleanupRequired} {
		t.Run(string(live), func(t *testing.T) {
			db, options, source, _, _ := removalFixture(t, "live-"+string(live), false)
			plan, err := db.PlanWorktreeRemoval(options)
			if err != nil {
				t.Fatal(err)
			}
			if live != "pending" {
				if _, err := db.FinalizeWorktreeRemoval(plan, live); err != nil {
					t.Fatal(err)
				}
			}
			distinct := options
			distinct.RequestID = options.RequestID + ".distinct"
			_, err = db.PlanWorktreeRemoval(distinct)
			var conflict *WorktreeConflict
			if !errors.As(err, &conflict) || conflict.Kind != "exists" {
				t.Fatalf("distinct plan against %s sibling = %v (%T), want WorktreeConflict exists", live, err, err)
			}
			if strings.Contains(err.Error(), "constraint") {
				t.Fatalf("typed conflict leaked a raw index error: %v", err)
			}
			var removals, liveEffects, active int
			_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`).Scan(&removals)
			_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas
				WHERE status IN ('pending','needs-broker','cleanup-required')`).Scan(&liveEffects)
			_ = db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active)
			if removals != 1 || liveEffects != 1 || active != 1 {
				t.Fatalf("rejected distinct plan rows = %d removals, %d live effects, source active=%d", removals, liveEffects, active)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate state after rejected distinct plan = %v", err)
			}
		})
	}
}

func TestWorktreeRemovalNeedsBrokerPreservesForceAndSource(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "broker", true)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveNeedsBroker)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "needs-broker" || result.RecoverRequestID == nil || result.BrokerRequest == nil ||
		result.BrokerRequest.Verb != "remove" || !result.BrokerRequest.Force ||
		result.BrokerRequest.RegistrationRoot != source.RegistrationRoot ||
		result.BrokerRequest.RecoverRequestID != *result.RecoverRequestID {
		t.Fatalf("remove broker handoff = %+v", result)
	}
	var active int
	if err := db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("source active after broker handoff = %d, %v", active, err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, options.RequestID); err != nil || receipt != nil {
		t.Fatalf("needs-broker original receipt = %v, %v", receipt, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate needs-broker state = %v", err)
	}
	replay, err := db.PlanWorktreeRemoval(options)
	if err != nil || replay.EffectRequired || !reflect.DeepEqual(replay.Result, result) {
		t.Fatalf("broker handoff replay = %+v, %v; want %+v", replay, err, result)
	}
}

func TestWorktreeRemovalCleanupRequiredRemainsRecoverable(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "cleanup", false)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveCleanupRequired)
	if err != nil || result.Status != "cleanup-required" || result.RecoverRequestID == nil || result.BrokerRequest != nil {
		t.Fatalf("cleanup-required result = %+v, %v", result, err)
	}
	var active int
	if err := db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("source active after cleanup-required = %d, %v", active, err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, options.RequestID); err != nil || receipt != nil {
		t.Fatalf("cleanup-required original receipt = %v, %v", receipt, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate cleanup-required state = %v", err)
	}
	replay, err := db.PlanWorktreeRemoval(options)
	if err != nil || replay.EffectRequired || !reflect.DeepEqual(replay.Result, result) {
		t.Fatalf("cleanup-required replay = %+v, %v; want %+v", replay, err, result)
	}
}

func TestFinalizeWorktreeRemovalReplaysCommittedNonTerminalOutcome(t *testing.T) {
	for _, tt := range []struct {
		outcome WorktreeRemoveOutcome
		force   bool
	}{
		{outcome: WorktreeRemoveNeedsBroker, force: true},
		{outcome: WorktreeRemoveCleanupRequired, force: false},
	} {
		t.Run(string(tt.outcome), func(t *testing.T) {
			db, options, _, _, _ := removalFixture(t, "replay-"+string(tt.outcome), tt.force)
			plan, err := db.PlanWorktreeRemoval(options)
			if err != nil {
				t.Fatal(err)
			}
			result, err := db.FinalizeWorktreeRemoval(plan, tt.outcome)
			if err != nil {
				t.Fatal(err)
			}
			again, err := db.FinalizeWorktreeRemoval(plan, tt.outcome)
			var conflict *WorktreeSagaConflict
			if errors.As(err, &conflict) {
				t.Fatalf("finalize replay of committed %s saga = %v; want previously committed result", tt.outcome, err)
			}
			if err != nil || !reflect.DeepEqual(again, result) {
				t.Fatalf("finalize replay = %+v, %v; want %+v", again, err, result)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate %s replay state = %v", tt.outcome, err)
			}
		})
	}
}

func TestWorktreeRemovalFailedWhileOrdinaryKeepsSourceActive(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "failed", true)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveFailed)
	if err != nil || result.Status != "failed" {
		t.Fatalf("failed removal = %+v, %v", result, err)
	}
	var active int
	if err := db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("source active after failed removal = %d, %v", active, err)
	}
	receipt, err := db.FindReceipt(ReceiptKindWorktree, options.RequestID)
	if err != nil || receipt == nil || receipt["status"] != "failed" || receipt["active"] != true || receipt["force"] != true {
		t.Fatalf("failed removal receipt = %v, %v", receipt, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatal(err)
	}
	next := options
	next.RequestID = "wt.remove.failed.retry"
	retry, err := db.PlanWorktreeRemoval(next)
	if err != nil || !retry.EffectRequired || retry.SourceSagaID != source.SagaID {
		t.Fatalf("new removal after failed = %+v, %v", retry, err)
	}
	if _, err := db.FinalizeWorktreeRemoval(retry, WorktreeRemoveRemoved); err != nil {
		t.Fatal(err)
	}
	replay, err := db.PlanWorktreeRemoval(options)
	if err != nil || replay.EffectRequired || replay.Result == nil || replay.Result.Status != "failed" {
		t.Fatalf("historical failed replay after removed sibling = %+v, %v", replay, err)
	}
}

func TestWorktreeRemovalFailedForbiddenForTerminalRunOrDoneTask(t *testing.T) {
	t.Run("terminal run", func(t *testing.T) {
		db, options, source, _, run := removalFixture(t, "terminal", false)
		plan, err := db.PlanWorktreeRemoval(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := db.CancelRun(run.RunID, options.Owner, options.ClaimToken,
			options.Attempt, options.AttemptToken, false, "run.cancel.remove"); err != nil {
			t.Fatal(err)
		}
		if _, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveFailed); !errors.As(err, new(*WorktreeRemovalFailedForbidden)) {
			t.Fatalf("terminal-run failed removal = %v", err)
		}
		result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveRemoved)
		if err != nil || result.Status != "removed" {
			t.Fatalf("terminal-run removed = %+v, %v", result, err)
		}
		var active int
		_ = db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active)
		if active != 0 {
			t.Fatalf("terminal cleanup retained source active=%d", active)
		}
	})
	t.Run("done task", func(t *testing.T) {
		db, options, _, _, _ := removalFixture(t, "done", false)
		plan, err := db.PlanWorktreeRemoval(options)
		if err != nil {
			t.Fatal(err)
		}
		task, err := db.GetTask(plan.Tag, plan.Task)
		if err != nil {
			t.Fatal(err)
		}
		gate := &ClaimGate{Owner: options.Owner, Token: options.ClaimToken, RunID: options.RunID,
			Attempt: options.Attempt, AttemptToken: options.AttemptToken}
		if _, err := db.SetStatus(plan.Tag, plan.Task, "done", task.Revision, gate); err != nil {
			t.Fatal(err)
		}
		if _, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveFailed); !errors.As(err, new(*WorktreeRemovalFailedForbidden)) {
			t.Fatalf("done-task failed removal = %v", err)
		}
		if result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveRemoved); err != nil || result.Status != "removed" {
			t.Fatalf("done-task removed = %+v, %v", result, err)
		}
	})
}

func TestWorktreeRemovalReplayCollisionFencingAndRedaction(t *testing.T) {
	db, options, _, claim, run := removalFixture(t, "collision", false)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	collision := options
	collision.Force = true
	if _, err := db.PlanWorktreeRemoval(collision); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("force collision = %v", err)
	}
	wrong := options
	wrong.AttemptToken++
	if _, err := db.PlanWorktreeRemoval(wrong); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("attempt fence = %v", err)
	}
	result, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveFailed)
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindWorktree, options.RequestID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(result)
	for label, value := range map[string]string{"receipt": body, "result": string(encoded)} {
		for _, forbidden := range []string{"claimToken", "attemptToken", fmt.Sprint(claim.Token), fmt.Sprint(run.AttemptToken)} {
			if strings.Contains(value, forbidden) {
				t.Fatalf("%s leaked %q: %s", label, forbidden, value)
			}
		}
	}
	if strings.Contains(body, "sagaToken") || strings.Contains(body, "registrationRoot") {
		t.Fatalf("receipt leaked private/broker identity: %s", body)
	}
}

func TestFinalizeWorktreeRemovalCASRejectsChangedIdentity(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "cas", false)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	tampered := *plan
	tampered.Branch += "-changed"
	if _, err := db.FinalizeWorktreeRemoval(&tampered, WorktreeRemoveRemoved); !errors.As(err, new(*WorktreeSagaConflict)) {
		t.Fatalf("tampered plan finalize = %v", err)
	}
	var status string
	var active int
	_ = db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status)
	_ = db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active)
	if status != "pending" || active != 1 {
		t.Fatalf("tampered CAS changed child/source = %s/%d", status, active)
	}
}

func TestFinalizeWorktreeRemovalInvalidOutcomeRejected(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "invalid", false)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []WorktreeRemoveOutcome{"", "pending", "bound"} {
		result, err := db.FinalizeWorktreeRemoval(plan, outcome)
		var statusErr *WorktreeRemoveStatusError
		if result != nil || !errors.As(err, &statusErr) || statusErr.Status != outcome {
			t.Fatalf("invalid outcome %q = (%+v, %T %v), want WorktreeRemoveStatusError", outcome, result, err, err)
		}
		var status string
		var active int
		if err := db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status); err != nil || status != "pending" {
			t.Fatalf("invalid outcome %q mutated saga status = %q, %v", outcome, status, err)
		}
		if err := db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active); err != nil || active != 1 {
			t.Fatalf("invalid outcome %q changed source binding = %d, %v", outcome, active, err)
		}
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, options.RequestID); err != nil || receipt != nil {
		t.Fatalf("invalid outcome receipt = %v, %v", receipt, err)
	}
	if _, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveRemoved); err != nil {
		t.Fatalf("finalize after invalid outcomes = %v", err)
	}
}

func TestWorktreeRemovalCrashBoundaries(t *testing.T) {
	t.Run("planning rollback", func(t *testing.T) {
		db, options, _, _, _ := removalFixture(t, "crash-plan", false)
		old := worktreeRemoveFaultHook
		worktreeRemoveFaultHook = func(point string) error {
			if point == "planned-before-commit" {
				return errors.New("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { worktreeRemoveFaultHook = old })
		before, _ := db.Fencing()
		if _, err := db.PlanWorktreeRemoval(options); err == nil {
			t.Fatal("faulted removal plan committed")
		}
		var count int
		_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas`).Scan(&count)
		after, _ := db.Fencing()
		if count != 1 || after != before {
			t.Fatalf("planning rollback sagas=%d fence=%d->%d", count, before, after)
		}
		worktreeRemoveFaultHook = old
		if plan, err := db.PlanWorktreeRemoval(options); err != nil || !plan.EffectRequired {
			t.Fatalf("removal retry after crash = %+v, %v", plan, err)
		}
	})
	t.Run("finalization rollback", func(t *testing.T) {
		db, options, source, _, _ := removalFixture(t, "crash-finalize", false)
		plan, err := db.PlanWorktreeRemoval(options)
		if err != nil {
			t.Fatal(err)
		}
		old := worktreeRemoveFaultHook
		worktreeRemoveFaultHook = func(point string) error {
			if point == "finalized-before-commit" {
				return errors.New("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { worktreeRemoveFaultHook = old })
		if _, err := db.FinalizeWorktreeRemoval(plan, WorktreeRemoveRemoved); err == nil {
			t.Fatal("faulted removal finalize committed")
		}
		var status string
		var active int
		_ = db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status)
		_ = db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active)
		if status != "pending" || active != 1 {
			t.Fatalf("finalize rollback child/source=%s/%d", status, active)
		}
		worktreeRemoveFaultHook = old
		replay, err := db.PlanWorktreeRemoval(options)
		if err != nil || !replay.EffectRequired || replay.SagaID != plan.SagaID || replay.SagaToken != plan.SagaToken {
			t.Fatalf("removal crash replay = %+v, %v", replay, err)
		}
		if _, err := db.FinalizeWorktreeRemoval(replay, WorktreeRemoveRemoved); err != nil {
			t.Fatal(err)
		}
	})
}

func TestConcurrentWorktreeRemovalPlanAndFinalize(t *testing.T) {
	db, options, _, _, _ := removalFixture(t, "concurrent", false)
	type planned struct {
		plan *WorktreeRemovePlan
		err  error
	}
	start := make(chan struct{})
	plansOut := make(chan planned, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			plan, err := db.PlanWorktreeRemoval(options)
			plansOut <- planned{plan, err}
		}()
	}
	close(start)
	wg.Wait()
	close(plansOut)
	var plans []*WorktreeRemovePlan
	for got := range plansOut {
		if got.err != nil {
			t.Fatal(got.err)
		}
		plans = append(plans, got.plan)
	}
	if len(plans) != 2 || plans[0].SagaID != plans[1].SagaID || plans[0].SagaToken != plans[1].SagaToken ||
		!plans[0].EffectRequired || !plans[1].EffectRequired {
		t.Fatalf("concurrent removal plans = %+v", plans)
	}
	results, errs := make(chan *WorktreeMutationResult, 2), make(chan error, 2)
	for _, plan := range plans {
		wg.Add(1)
		go func(p *WorktreeRemovePlan) {
			defer wg.Done()
			result, err := db.FinalizeWorktreeRemoval(p, WorktreeRemoveRemoved)
			results <- result
			errs <- err
		}(plan)
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first *WorktreeMutationResult
	for result := range results {
		if first == nil {
			first = result
		} else if !reflect.DeepEqual(first, result) {
			t.Fatalf("concurrent removal results = %+v / %+v", first, result)
		}
	}
	var removed, receipts int
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove' AND status='removed'`).Scan(&removed)
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindWorktree, options.RequestID).Scan(&receipts)
	if removed != 1 || receipts != 1 {
		t.Fatalf("concurrent removal durable rows = %d removed, %d receipts", removed, receipts)
	}
}

// TestConcurrentWorktreeRemovalDistinctRequestPlanningRace pins the loser of
// two concurrent distinct-request plans against one task: BEGIN IMMEDIATE
// serializes the writers, so the loser re-runs the live-effect precheck and
// gets the deterministic typed WorktreeConflict instead of leaking the raw
// worktree_sagas_one_live_effect unique-index error from its INSERT.
func TestConcurrentWorktreeRemovalDistinctRequestPlanningRace(t *testing.T) {
	db, options, source, _, _ := removalFixture(t, "race", false)
	first, second := options, options
	first.RequestID = "wt.remove.race.first"
	second.RequestID = "wt.remove.race.second"
	type planned struct {
		plan *WorktreeRemovePlan
		err  error
	}
	start := make(chan struct{})
	out := make(chan planned, 2)
	var wg sync.WaitGroup
	for _, o := range []WorktreeRemoveOptions{first, second} {
		wg.Add(1)
		go func(o WorktreeRemoveOptions) {
			defer wg.Done()
			<-start
			plan, err := db.PlanWorktreeRemoval(o)
			out <- planned{plan, err}
		}(o)
	}
	close(start)
	wg.Wait()
	close(out)
	var winner *WorktreeRemovePlan
	losers := 0
	for got := range out {
		if got.err == nil {
			if winner != nil {
				t.Fatalf("two planning winners: %+v and %+v", winner, got.plan)
			}
			winner = got.plan
			continue
		}
		losers++
		var conflict *WorktreeConflict
		if !errors.As(got.err, &conflict) || conflict.Kind != "exists" {
			t.Fatalf("planning race loser = %v (%T), want typed WorktreeConflict exists", got.err, got.err)
		}
		if strings.Contains(got.err.Error(), "constraint") {
			t.Fatalf("planning race loser leaked a raw unique-index error: %v", got.err)
		}
	}
	if winner == nil || losers != 1 || !winner.EffectRequired || winner.SagaID <= 0 {
		t.Fatalf("planning race outcome winner=%+v losers=%d", winner, losers)
	}
	var winnerRequest string
	var removals, liveEffects, active int
	if err := db.db.QueryRow(`SELECT request_id FROM worktree_sagas WHERE saga_id=?`, winner.SagaID).
		Scan(&winnerRequest); err != nil || (winnerRequest != first.RequestID && winnerRequest != second.RequestID) {
		t.Fatalf("winner row = %q, %v", winnerRequest, err)
	}
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas WHERE operation='remove'`).Scan(&removals)
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas
		WHERE status IN ('pending','needs-broker','cleanup-required')`).Scan(&liveEffects)
	_ = db.db.QueryRow(`SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID).Scan(&active)
	if removals != 1 || liveEffects != 1 || active != 1 {
		t.Fatalf("planning race durable rows = %d removals, %d live effects, source active=%d", removals, liveEffects, active)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate state after planning race = %v", err)
	}
	if _, err := db.FinalizeWorktreeRemoval(winner, WorktreeRemoveRemoved); err != nil {
		t.Fatalf("winner finalize after planning race = %v", err)
	}
}
