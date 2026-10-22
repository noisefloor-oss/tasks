package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func provisionFixture(t *testing.T, requestID string) (*DB, WorktreeProvisionOptions, *Claim, *RunStartResult) {
	t.Helper()
	db, _ := newDB(t)
	// Keep capabilities distinguishable from ordinary ids/counts when the
	// redaction assertions inspect durable JSON.
	if _, err := db.db.Exec(`UPDATE meta SET value='900000000000000000' WHERE key='fencing'`); err != nil {
		t.Fatal(err)
	}
	id := addTask(t, db, "main", "provision")
	claim, _, _, err := db.AcquireClaim("main", id, "agent-a", 3600, "claim."+requestID)
	if err != nil {
		t.Fatal(err)
	}
	run, replayed, err := db.StartRun("main", id, "agent-a", claim.Token, "run."+requestID, 3600)
	if err != nil || replayed {
		t.Fatalf("start run = %+v replayed=%v err=%v", run, replayed, err)
	}
	_, boundary, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	return db, WorktreeProvisionOptions{
		RunID: run.RunID, Owner: "agent-a", ClaimToken: claim.Token,
		Attempt: run.Attempt, AttemptToken: run.AttemptToken, RequestID: requestID,
		OwnerBoundary: boundary, RegistrationRoot: filepath.Join(boundary, ".git", "worktrees"),
		TargetPath: filepath.Join(boundary, ".worktrees", WorktreeName("main", id)),
		Branch:     WorktreeBranch("main", id), BaseCommit: strings.Repeat("a", 40),
	}, claim, run
}

func TestWorktreeProvisionDigestAndIDVectors(t *testing.T) {
	input, err := worktreeInputDigest("agent-a", "wt.provision", 1, false, "provision")
	if err != nil || input != "sha256:b53d9c2f78031fe22f0a068a5801ce58d605c1ab6d51b2db6719483e1dc08ab9" {
		t.Fatalf("input digest = %q, %v", input, err)
	}
	want := "worktree-" + "28838d4060bde225ace4279d0963e75edba4ee478dabe37270c9f5ea00db81c2"
	if got := worktreeReceiptID("wt.provision", input); got != want {
		t.Fatalf("receipt id = %q, want %q", got, want)
	}
	got, err := worktreeRecoveryRequestID("123e4567-e89b-42d3-a456-426614174000", 1, 1)
	if err != nil || got != "recover-b9b9942729a6879f2266171a38aee8bd1180e34262584040608bc5464eff150c" {
		t.Fatalf("recovery id = %q, %v", got, err)
	}
	runID, base := int64(7), strings.Repeat("a", 40)
	recoverID := "recover-" + strings.Repeat("b", 64)
	result := WorktreeMutationResult{SagaID: 3, Operation: "provision", Status: "needs-broker",
		RunID: &runID, OwnerBoundary: "/repo", Path: "/repo/.worktrees/main-9",
		Branch: "tasks/main/9", BaseCommit: &base, SagaToken: 44, RecoveryGeneration: 0,
		RecoverRequestID: &recoverID, BrokerRequest: &WorktreeBrokerRequest{FamilyID: "family",
			Verb: "create", RegistrationRoot: "/repo/.git/worktrees", OwnerBoundary: "/repo",
			TargetPath: "/repo/.worktrees/main-9", Branch: "tasks/main/9", BaseCommit: &base,
			Force: false, RecoverRequestID: recoverID}}
	encoded, err := json.Marshal(result)
	wantJSON := `{"sagaId":3,"operation":"provision","status":"needs-broker","runId":7,"ownerBoundary":"/repo","path":"/repo/.worktrees/main-9","branch":"tasks/main/9","baseCommit":"` + base + `","sagaToken":44,"recoveryGeneration":0,"recoverRequestId":"` + recoverID + `","brokerRequest":{"familyId":"family","verb":"create","registrationRoot":"/repo/.git/worktrees","ownerBoundary":"/repo","targetPath":"/repo/.worktrees/main-9","branch":"tasks/main/9","baseCommit":"` + base + `","force":false,"recoverRequestId":"` + recoverID + `"}}`
	if err != nil || string(encoded) != wantJSON {
		t.Fatalf("mutation JSON = %s, %v; want %s", encoded, err, wantJSON)
	}
}

func TestPlanWorktreeProvisionPersistsIntentBeforeEffect(t *testing.T) {
	db, opts, claim, run := provisionFixture(t, "wt.plan")
	beforeFence, _ := db.Fencing()
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.EffectRequired || plan.Result != nil || plan.SagaID <= 0 || plan.SagaToken <= beforeFence ||
		plan.RunID != run.RunID || plan.Tag != "main" || plan.Task == "" || plan.TargetPath != opts.TargetPath {
		t.Fatalf("plan = %+v", plan)
	}
	var status, request, boundary, registration, target, branch, base string
	var runID, fence, generation, active int64
	if err := db.db.QueryRow(`SELECT run_id,status,binding_active,saga_fence,request_id,recovery_generation,
		owner_boundary,registration_root,target_path,branch,base_commit FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).
		Scan(&runID, &status, &active, &fence, &request, &generation, &boundary, &registration, &target, &branch, &base); err != nil {
		t.Fatal(err)
	}
	if runID != run.RunID || status != "pending" || active != 0 || fence != plan.SagaToken ||
		request != opts.RequestID || generation != 0 || boundary != opts.OwnerBoundary || registration != opts.RegistrationRoot ||
		target != opts.TargetPath || branch != opts.Branch || base != opts.BaseCommit {
		t.Fatalf("stored plan mismatch: run=%d status=%s active=%d fence=%d request=%s generation=%d identity=%q %q %q %q %q",
			runID, status, active, fence, request, generation, boundary, registration, target, branch, base)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, opts.RequestID); err != nil || receipt != nil {
		t.Fatalf("planning receipt = %v, %v", receipt, err)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != "worktree" || last.Action != "planned" || last.Owner != opts.Owner || len(last.Extra) != 0 {
		t.Fatalf("planned event = %+v", last)
	}
	encoded, _ := json.Marshal(last)
	for _, secret := range []string{"claimToken", "attemptToken", fmt.Sprint(claim.Token), fmt.Sprint(run.AttemptToken)} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("planned event leaked %q: %s", secret, encoded)
		}
	}
}

func TestFinalizeWorktreeProvisionBoundReceiptAndReplay(t *testing.T) {
	db, opts, _, _ := provisionFixture(t, "wt.bound")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionBound)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "bound" || result.SagaToken != plan.SagaToken || result.RecoverRequestID != nil ||
		result.BrokerRequest != nil || result.RunID == nil || *result.RunID != opts.RunID {
		t.Fatalf("bound result = %+v", result)
	}
	var status string
	var active int
	if err := db.db.QueryRow(`SELECT status,binding_active FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status, &active); err != nil || status != "bound" || active != 1 {
		t.Fatalf("bound row = %q/%d, %v", status, active, err)
	}
	receipt, err := db.FindReceipt(ReceiptKindWorktree, opts.RequestID)
	if err != nil || receipt == nil || receipt["status"] != "bound" || receipt["active"] != true ||
		receipt["receiptId"] != worktreeReceiptID(opts.RequestID, receipt["inputDigest"].(string)) {
		t.Fatalf("bound receipt = %v, %v", receipt, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate saga = %v", err)
	}
	fence, _ := db.Fencing()
	replay, err := db.PlanWorktreeProvision(opts)
	if err != nil || replay.EffectRequired || replay.Result == nil || !reflect.DeepEqual(replay.Result, result) {
		t.Fatalf("plan replay = %+v, %v; want %+v", replay, err, result)
	}
	afterFence, _ := db.Fencing()
	if afterFence != fence {
		t.Fatalf("receipt replay advanced fence %d -> %d", fence, afterFence)
	}
	again, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionBound)
	if err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("finalize replay = %+v, %v; want %+v", again, err, result)
	}
}

func TestWorktreeProvisionNeedsBrokerExactHandoffAndReplay(t *testing.T) {
	db, opts, _, _ := provisionFixture(t, "wt.broker")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker)
	if err != nil {
		t.Fatal(err)
	}
	wantRecover, _ := worktreeRecoveryRequestID(result.BrokerRequest.FamilyID, plan.SagaID, 1)
	if result.Status != "needs-broker" || result.RecoverRequestID == nil || *result.RecoverRequestID != wantRecover ||
		result.BrokerRequest == nil || result.BrokerRequest.Verb != "create" ||
		result.BrokerRequest.RecoverRequestID != wantRecover || result.BrokerRequest.RegistrationRoot != opts.RegistrationRoot {
		t.Fatalf("needs-broker result = %+v", result)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, opts.RequestID); err != nil || receipt != nil {
		t.Fatalf("nonterminal receipt = %v, %v", receipt, err)
	}
	replay, err := db.PlanWorktreeProvision(opts)
	if err != nil || replay.EffectRequired || !reflect.DeepEqual(replay.Result, result) {
		t.Fatalf("handoff replay = %+v, %v; want %+v", replay, err, result)
	}
}

func TestWorktreeProvisionCleanupRequiredFinalizationIsEventOnly(t *testing.T) {
	db, opts, _, _ := provisionFixture(t, "wt.cleanup")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionCleanupRequired)
	if err != nil {
		t.Fatal(err)
	}
	familyID, _, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	wantRecover, err := worktreeRecoveryRequestID(familyID, plan.SagaID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "cleanup-required" || result.SagaToken != plan.SagaToken || result.RecoveryGeneration != 0 ||
		result.RecoverRequestID == nil || *result.RecoverRequestID != wantRecover || result.BrokerRequest != nil {
		t.Fatalf("cleanup-required result = %+v", result)
	}
	var status string
	var active int
	if err := db.db.QueryRow(`SELECT status,binding_active FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).
		Scan(&status, &active); err != nil || status != "cleanup-required" || active != 0 {
		t.Fatalf("cleanup-required row = %q/%d, %v", status, active, err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, opts.RequestID); err != nil || receipt != nil {
		t.Fatalf("cleanup-required receipt = %v, %v", receipt, err)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last := feed.Events[len(feed.Events)-1]
	if last.Kind != ReceiptKindWorktree || last.Action != "cleanup-required" || last.Owner != opts.Owner || len(last.Extra) != 0 {
		t.Fatalf("cleanup-required event = %+v", last)
	}
	events := len(feed.Events)
	fence, _ := db.Fencing()
	replay, err := db.PlanWorktreeProvision(opts)
	if err != nil || replay.EffectRequired || !reflect.DeepEqual(replay.Result, result) {
		t.Fatalf("handoff replay = %+v, %v; want %+v", replay, err, result)
	}
	after, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	afterFence, _ := db.Fencing()
	if len(after.Events) != events || afterFence != fence {
		t.Fatalf("handoff replay mutated audit state: events %d->%d fence %d->%d",
			events, len(after.Events), fence, afterFence)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, opts.RequestID); err != nil || receipt != nil {
		t.Fatalf("replay receipt = %v, %v", receipt, err)
	}
}

func TestWorktreeProvisionExhaustedGenerationAdvertisesNoRecovery(t *testing.T) {
	db, opts, _, _ := provisionFixture(t, "wt.exhausted")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	committed, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker)
	if err != nil || committed.RecoverRequestID == nil || committed.BrokerRequest == nil {
		t.Fatalf("pre-exhaustion handoff = %+v, %v", committed, err)
	}
	// A recovery from the preceding generation committed SQLite's maximum and
	// the saga stayed nonterminal: the stored generation now sits at the
	// boundary where no next id may be derived or advertised.
	if _, err := db.db.Exec(`UPDATE worktree_sagas SET recovery_generation=? WHERE saga_id=?`,
		int64(math.MaxInt64), plan.SagaID); err != nil {
		t.Fatal(err)
	}
	replay, err := db.PlanWorktreeProvision(opts)
	if err != nil || replay.EffectRequired || replay.Result == nil || replay.RecoveryGeneration != math.MaxInt64 {
		t.Fatalf("exhausted replay = %+v, %v", replay, err)
	}
	result := replay.Result
	if result.Status != "needs-broker" || result.RecoveryGeneration != math.MaxInt64 ||
		result.RecoverRequestID != nil || result.BrokerRequest != nil {
		t.Fatalf("exhausted result = %+v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["recoveryGeneration"]) != "9223372036854775807" ||
		string(fields["recoverRequestId"]) != "null" || string(fields["brokerRequest"]) != "null" {
		t.Fatalf("exhausted mutation JSON = %s", encoded)
	}
	again, err := db.FinalizeWorktreeProvision(replay, WorktreeProvisionNeedsBroker)
	if err != nil || !reflect.DeepEqual(again, result) {
		t.Fatalf("exhausted finalize handoff = %+v, %v; want %+v", again, err, result)
	}
}

func TestWorktreeProvisionFailedReceiptAndSecretRedaction(t *testing.T) {
	db, opts, claim, run := provisionFixture(t, "wt.failed")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionFailed)
	if err != nil || result.Status != "failed" {
		t.Fatalf("failed result = %+v, %v", result, err)
	}
	var body string
	if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindWorktree, opts.RequestID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	last, _ := json.Marshal(feed.Events[len(feed.Events)-1])
	for label, encoded := range map[string]string{"receipt": body, "event": string(last)} {
		for _, forbidden := range []string{"claimToken", "attemptToken", "sagaToken", "registrationRoot", fmt.Sprint(claim.Token), fmt.Sprint(run.AttemptToken)} {
			if strings.Contains(encoded, forbidden) {
				t.Fatalf("%s leaked %q: %s", label, forbidden, encoded)
			}
		}
	}
	bad := opts
	bad.RequestID = "wt.bad-token"
	bad.AttemptToken++
	if _, err := db.PlanWorktreeProvision(bad); err == nil || strings.Contains(err.Error(), fmt.Sprint(bad.AttemptToken)) {
		t.Fatalf("bad token diagnostic = %v", err)
	}
}

func TestWorktreeProvisionReplayCollisionAndOwnershipFence(t *testing.T) {
	db, opts, _, _ := provisionFixture(t, "wt.collision")
	first, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	collision := opts
	collision.BaseCommit = strings.Repeat("b", 40)
	if _, err := db.PlanWorktreeProvision(collision); !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("identity collision = %v", err)
	}
	wrong := opts
	wrong.AttemptToken++
	if _, err := db.PlanWorktreeProvision(wrong); !errors.As(err, new(*RunConflict)) {
		t.Fatalf("stale attempt fence = %v", err)
	}
	other := opts
	other.RequestID = "wt.other"
	if _, err := db.PlanWorktreeProvision(other); !errors.As(err, new(*WorktreeConflict)) {
		t.Fatalf("second live effect = %v", err)
	}
	var count int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("saga count = %d, %v", count, err)
	}
	if first.SagaID <= 0 {
		t.Fatalf("first plan = %+v", first)
	}
}

func TestWorktreeProvisionCrashBoundariesReplaySafely(t *testing.T) {
	t.Run("planning rollback", func(t *testing.T) {
		db, opts, _, _ := provisionFixture(t, "wt.crash-plan")
		old := worktreeProvisionFaultHook
		worktreeProvisionFaultHook = func(point string) error {
			if point == "planned-before-commit" {
				return errors.New("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { worktreeProvisionFaultHook = old })
		before, _ := db.Fencing()
		if _, err := db.PlanWorktreeProvision(opts); err == nil {
			t.Fatal("faulted plan committed")
		}
		var count int
		_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas`).Scan(&count)
		after, _ := db.Fencing()
		if count != 0 || after != before {
			t.Fatalf("planning rollback rows=%d fence=%d->%d", count, before, after)
		}
		worktreeProvisionFaultHook = old
		if plan, err := db.PlanWorktreeProvision(opts); err != nil || !plan.EffectRequired {
			t.Fatalf("retry plan = %+v, %v", plan, err)
		}
	})
	t.Run("finalization rollback", func(t *testing.T) {
		db, opts, _, _ := provisionFixture(t, "wt.crash-finalize")
		plan, err := db.PlanWorktreeProvision(opts)
		if err != nil {
			t.Fatal(err)
		}
		old := worktreeProvisionFaultHook
		worktreeProvisionFaultHook = func(point string) error {
			if point == "finalized-before-commit" {
				return errors.New("injected crash")
			}
			return nil
		}
		t.Cleanup(func() { worktreeProvisionFaultHook = old })
		if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionBound); err == nil {
			t.Fatal("faulted finalize committed")
		}
		var status string
		_ = db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status)
		if status != "pending" {
			t.Fatalf("status after rollback = %q", status)
		}
		worktreeProvisionFaultHook = old
		replay, err := db.PlanWorktreeProvision(opts)
		if err != nil || !replay.EffectRequired || replay.SagaID != plan.SagaID || replay.SagaToken != plan.SagaToken {
			t.Fatalf("crash replay = %+v, %v; plan=%+v", replay, err, plan)
		}
		if _, err := db.FinalizeWorktreeProvision(replay, WorktreeProvisionBound); err != nil {
			t.Fatal(err)
		}
	})
}

func TestWorktreeProvisionFinalizationRequiresCompensationAfterTerminalRun(t *testing.T) {
	db, opts, _, run := provisionFixture(t, "wt.compensate")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	// The run terminalized without §9.4's parent flip — an older binary's
	// store — so the saga is still pending and the observed late bind must
	// demand compensation. A parent-terminalized run flips the row first.
	if _, err := db.db.Exec(`UPDATE runs SET status='cancelled',terminal_at=updated_at WHERE run_id=?`, run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionBound); !errors.As(err, new(*WorktreeProvisionCompensationRequired)) {
		t.Fatalf("late bind = %v", err)
	}
	result, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionFailed)
	if err != nil || result.Status != "failed" {
		t.Fatalf("compensated finalize = %+v, %v", result, err)
	}
}

func TestWorktreeProvisionNeedsBrokerAfterTerminalRunRequiresCompensation(t *testing.T) {
	db, opts, _, run := provisionFixture(t, "wt.broker-compensate")
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	// Same pre-flip terminalization: the confined handoff observation still
	// demands compensation while the row is pending.
	if _, err := db.db.Exec(`UPDATE runs SET status='cancelled',terminal_at=updated_at WHERE run_id=?`, run.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeProvision(plan, WorktreeProvisionNeedsBroker); !errors.As(err, new(*WorktreeProvisionCompensationRequired)) {
		t.Fatalf("confined handoff after terminal run = %v", err)
	}
	var status string
	if err := db.db.QueryRow(`SELECT status FROM worktree_sagas WHERE saga_id=?`, plan.SagaID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("saga after refused confined observation = %q, %v", status, err)
	}
}

func TestWorktreeProvisionValidateAfterFailedAndNeedsBrokerFinalization(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome WorktreeProvisionOutcome
	}{
		{"failed", WorktreeProvisionFailed},
		{"needs-broker", WorktreeProvisionNeedsBroker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, opts, _, _ := provisionFixture(t, "wt.validate."+tc.name)
			plan, err := db.PlanWorktreeProvision(opts)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.FinalizeWorktreeProvision(plan, tc.outcome); err != nil {
				t.Fatal(err)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate saga after %s finalization = %v", tc.outcome, err)
			}
		})
	}
}

func TestConcurrentWorktreeProvisionPlanAndFinalize(t *testing.T) {
	db, opts, _, _ := provisionFixture(t, "wt.concurrent")
	type planned struct {
		plan *WorktreeProvisionPlan
		err  error
	}
	start := make(chan struct{})
	out := make(chan planned, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p, err := db.PlanWorktreeProvision(opts)
			out <- planned{p, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	var plans []*WorktreeProvisionPlan
	for got := range out {
		if got.err != nil {
			t.Fatal(got.err)
		}
		plans = append(plans, got.plan)
	}
	if len(plans) != 2 || plans[0].SagaID != plans[1].SagaID || plans[0].SagaToken != plans[1].SagaToken ||
		!plans[0].EffectRequired || !plans[1].EffectRequired {
		t.Fatalf("concurrent plans = %+v", plans)
	}
	results := make(chan *WorktreeMutationResult, 2)
	errs := make(chan error, 2)
	for _, plan := range plans {
		wg.Add(1)
		go func(p *WorktreeProvisionPlan) {
			defer wg.Done()
			result, err := db.FinalizeWorktreeProvision(p, WorktreeProvisionBound)
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
			t.Fatalf("concurrent finalize results = %+v and %+v", first, result)
		}
	}
	var sagas, receipts int
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM worktree_sagas`).Scan(&sagas)
	_ = db.db.QueryRow(`SELECT COUNT(*) FROM receipts WHERE kind=?`, ReceiptKindWorktree).Scan(&receipts)
	if sagas != 1 || receipts != 1 {
		t.Fatalf("concurrent durable rows = %d sagas, %d receipts", sagas, receipts)
	}
}
