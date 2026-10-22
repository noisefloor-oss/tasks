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
	"time"
)

func testFamilyID(t *testing.T, db *DB) string {
	t.Helper()
	var family string
	if err := db.db.QueryRow(`SELECT value FROM meta WHERE key='familyId'`).Scan(&family); err != nil {
		t.Fatal(err)
	}
	return family
}

func sagaFenceOf(t *testing.T, db *DB, sagaID int64) int64 {
	t.Helper()
	var fence int64
	if err := db.db.QueryRow(`SELECT saga_fence FROM worktree_sagas WHERE saga_id=?`, sagaID).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	return fence
}

func sagaRow(t *testing.T, db *DB, sagaID int64) (status string, generation int64, active int64) {
	t.Helper()
	if err := db.db.QueryRow(`SELECT status,recovery_generation,binding_active FROM worktree_sagas WHERE saga_id=?`, sagaID).
		Scan(&status, &generation, &active); err != nil {
		t.Fatal(err)
	}
	return status, generation, active
}

func recoveryOptions(t *testing.T, db *DB, sagaID, generation int64) WorktreeRecoveryOptions {
	t.Helper()
	request, err := worktreeRecoveryRequestID(testFamilyID(t, db), sagaID, generation)
	if err != nil {
		t.Fatal(err)
	}
	return WorktreeRecoveryOptions{SagaID: sagaID, SagaToken: sagaFenceOf(t, db, sagaID), RequestID: request}
}

func scalarInt64(t *testing.T, db *DB, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func storeInventory(t *testing.T, db *DB) *ClassInventory {
	t.Helper()
	var inv *ClassInventory
	if err := db.deferred(func(t *tx) error {
		var err error
		inv, err = countClassInventory(t)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return inv
}

func feedOf(t *testing.T, db *DB) []Event {
	t.Helper()
	feed, err := db.Feed(0)
	if err != nil {
		t.Fatal(err)
	}
	return feed.Events
}

func extraString(t *testing.T, e Event, key string) string {
	t.Helper()
	raw, ok := e.Extra[key]
	if !ok {
		t.Fatalf("event %+v has no extra %q", e, key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func extraInt64(t *testing.T, e Event, key string) int64 {
	t.Helper()
	raw, ok := e.Extra[key]
	if !ok {
		t.Fatalf("event %+v has no extra %q", e, key)
	}
	var value int64
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// recoverableProvision plans and finalizes one provision saga to a nonterminal
// outcome at generation zero.
func recoverableProvision(t *testing.T, suffix string, outcome WorktreeProvisionOutcome) (*DB, *WorktreeProvisionPlan, WorktreeProvisionOptions, *Claim, *RunStartResult) {
	t.Helper()
	db, opts, claim, run := provisionFixture(t, "wt.rec."+suffix)
	plan, err := db.PlanWorktreeProvision(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeProvision(plan, outcome); err != nil {
		t.Fatal(err)
	}
	return db, plan, opts, claim, run
}

// recoverableRemoval plans and finalizes one linked removal saga to a
// nonterminal outcome at generation zero against a bound source.
func recoverableRemoval(t *testing.T, suffix string, outcome WorktreeRemoveOutcome) (*DB, *WorktreeRemovePlan, WorktreeRemoveOptions, *WorktreeProvisionPlan, *Claim, *RunStartResult) {
	t.Helper()
	db, options, source, claim, run := removalFixture(t, "rec."+suffix, false)
	plan, err := db.PlanWorktreeRemoval(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeRemoval(plan, outcome); err != nil {
		t.Fatal(err)
	}
	return db, plan, options, source, claim, run
}

func TestWorktreeRecoveryInputDigestVector(t *testing.T) {
	input, err := canonicalDigest(map[string]any{
		"force": false, "operation": "recover", "recoveryGeneration": int64(1),
		"requestId": "req.recover", "runId": int64(0), "sagaId": int64(1),
	})
	if err != nil || input != "sha256:56a1cc42bcad2f72b2b4b74dd7de7260cbdfe242cd5cf5b29f60f0ca96ad5afb" {
		t.Fatalf("recovery input digest = %q, %v", input, err)
	}
}

// TestRecoverWorktreeSagaReceiptFirstReplayAcrossGenerations pins §9.4's
// receipt-first rule: an exact recovery receipt replays its historical public
// result with no effect, even after later generations committed and even
// after the saga terminalized.
func TestRecoverWorktreeSagaReceiptFirstReplayAcrossGenerations(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "replay", WorktreeProvisionNeedsBroker)
	firstOpts := recoveryOptions(t, db, plan.SagaID, 1)

	first, err := db.RecoverWorktreeSaga(firstOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !first.EffectRequired || first.Result != nil || first.SagaID != plan.SagaID ||
		first.SagaToken != plan.SagaToken || first.Status != "needs-broker" ||
		first.RecoveryGeneration != 0 || first.Operation != "provision" ||
		first.OriginalRequestID != plan.RequestID || first.RequestID != firstOpts.RequestID ||
		first.SourceSagaID != 0 || first.RunID != plan.RunID || first.Force ||
		first.RegistrationRoot != plan.RegistrationRoot || first.BaseCommit != plan.BaseCommit ||
		first.TargetPath != plan.TargetPath || first.Branch != plan.Branch ||
		first.OwnerBoundary != plan.OwnerBoundary || first.Tag != plan.Tag || first.Task != plan.Task {
		t.Fatalf("fresh attempt snapshot = %+v", first)
	}
	firstResult, err := db.CommitWorktreeRecovery(first, WorktreeRecoveryNeedsBroker)
	if err != nil {
		t.Fatal(err)
	}
	if firstResult.Status != "needs-broker" || firstResult.RecoveryGeneration != 1 ||
		firstResult.RecoverRequestID == nil || *firstResult.RecoverRequestID != recoveryOptions(t, db, plan.SagaID, 2).RequestID ||
		firstResult.BrokerRequest == nil || firstResult.BrokerRequest.Verb != "create" ||
		firstResult.SagaToken != 0 {
		t.Fatalf("generation-1 result = %+v", firstResult)
	}

	second, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 2))
	if err != nil {
		t.Fatal(err)
	}
	terminalResult, err := db.CommitWorktreeRecovery(second, WorktreeRecoveryBound)
	if err != nil {
		t.Fatal(err)
	}
	if terminalResult.Status != "bound" || terminalResult.RecoveryGeneration != 2 ||
		terminalResult.RecoverRequestID != nil || terminalResult.BrokerRequest != nil {
		t.Fatalf("terminalizing result = %+v", terminalResult)
	}

	// Later-generation row: the generation-1 receipt still replays its
	// historical projection — including the derived members it carried at
	// that generation: the then-advertised next request id and, for
	// needs-broker, the broker mint bindings over the row's immutable
	// identity (SPEC §9.4 replay contract).
	replayOne, err := db.RecoverWorktreeSaga(firstOpts)
	if err != nil {
		t.Fatal(err)
	}
	if replayOne.EffectRequired || replayOne.Result == nil || !reflect.DeepEqual(replayOne.Result, firstResult) {
		t.Fatalf("generation-1 replay = %+v; want historical result %+v", replayOne.Result, firstResult)
	}
	if replayOne.Result.RecoverRequestID == nil || firstResult.RecoverRequestID == nil ||
		*replayOne.Result.RecoverRequestID != *firstResult.RecoverRequestID ||
		replayOne.Result.BrokerRequest == nil || replayOne.Result.BrokerRequest.Verb != "create" ||
		replayOne.Result.BrokerRequest.RecoverRequestID != *replayOne.Result.RecoverRequestID {
		t.Fatalf("generation-1 replay derived members = %+v", replayOne.Result)
	}
	// Terminal row: the terminalizing receipt replays exactly.
	replayTwo, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 2))
	if err != nil {
		t.Fatal(err)
	}
	if replayTwo.EffectRequired || replayTwo.Result == nil || !reflect.DeepEqual(replayTwo.Result, terminalResult) {
		t.Fatalf("generation-2 replay = %+v result=%+v; want historical %+v", replayTwo, replayTwo.Result, terminalResult)
	}
	events, receipts := int64(len(feedOf(t, db))), scalarInt64(t, db, `SELECT COUNT(*) FROM receipts WHERE kind=?`, ReceiptKindWorktree)
	for range 3 {
		if _, err := db.RecoverWorktreeSaga(firstOpts); err != nil {
			t.Fatal(err)
		}
	}
	if got := int64(len(feedOf(t, db))); got != events {
		t.Fatalf("replay appended %d events", got-events)
	}
	if got := scalarInt64(t, db, `SELECT COUNT(*) FROM receipts WHERE kind=?`, ReceiptKindWorktree); got != receipts {
		t.Fatalf("replay appended %d receipts", got-receipts)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate replayed state = %v", err)
	}
}

func TestRecoverWorktreeSagaWrongTokenWrongIDAndTerminalRefusals(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "refuse", WorktreeProvisionNeedsBroker)
	opts := recoveryOptions(t, db, plan.SagaID, 1)

	wrongToken := opts
	wrongToken.SagaToken++
	if attempt, err := db.RecoverWorktreeSaga(wrongToken); attempt != nil ||
		!errors.As(err, new(*WorktreeRecoveryConflict)) || !strings.Contains(err.Error(), "saga fence") {
		t.Fatalf("wrong saga token = %+v, %v; want WorktreeRecoveryConflict", attempt, err)
	}

	wrongID := opts
	wrongID.RequestID = "recover-" + strings.Repeat("c", 64)
	if attempt, err := db.RecoverWorktreeSaga(wrongID); attempt != nil ||
		!errors.As(err, new(*WorktreeRecoveryConflict)) || !strings.Contains(err.Error(), "advertised") {
		t.Fatalf("wrong request id = %+v, %v; want WorktreeRecoveryConflict", attempt, err)
	}

	// A stale future id is equally refused while its generation is uncommitted.
	stale := recoveryOptions(t, db, plan.SagaID, 2)
	if _, err := db.RecoverWorktreeSaga(stale); !errors.As(err, new(*WorktreeRecoveryConflict)) {
		t.Fatalf("future generation id = %v", err)
	}

	if _, err := db.RecoverWorktreeSaga(WorktreeRecoveryOptions{SagaID: plan.SagaID + 99, SagaToken: 1, RequestID: opts.RequestID}); err == nil {
		t.Fatal("unknown saga id accepted")
	}

	// A terminal saga refuses every receipt-missing request id.
	bound, err := db.RecoverWorktreeSaga(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitWorktreeRecovery(bound, WorktreeRecoveryBound); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{recoveryOptions(t, db, plan.SagaID, 2).RequestID, "recover-" + strings.Repeat("d", 64)} {
		if attempt, err := db.RecoverWorktreeSaga(WorktreeRecoveryOptions{SagaID: plan.SagaID, SagaToken: opts.SagaToken, RequestID: id}); attempt != nil ||
			!errors.As(err, new(*WorktreeRecoveryConflict)) || !strings.Contains(err.Error(), "terminal") {
			t.Fatalf("terminal saga new id %q = %+v, %v; want terminal WorktreeRecoveryConflict", id, attempt, err)
		}
	}
	// The exact terminalizing receipt still replays on the terminal row.
	if attempt, err := db.RecoverWorktreeSaga(opts); err != nil || attempt == nil || attempt.EffectRequired || attempt.Result == nil || attempt.Result.Status != "bound" {
		t.Fatalf("terminalizing receipt replay = %+v, %v", attempt, err)
	}
}

// TestRecoverWorktreeSagaCollisionAndMalformedReceipt pins the two fail-closed
// receipt classes: another saga's committed request is the common typed §8.4
// collision, and an altered receipt is corruption.
func TestRecoverWorktreeSagaCollisionAndMalformedReceipt(t *testing.T) {
	db, planA, optsA, _, _ := recoverableProvision(t, "collide.a", WorktreeProvisionNeedsBroker)

	// A second task/run/saga in the same family and database.
	idB := addTask(t, db, "main", "collide-b")
	claimB, _, _, err := db.AcquireClaim("main", idB, "agent-a", 3600, "claim.wt.rec.collide.b")
	if err != nil {
		t.Fatal(err)
	}
	runB, replayed, err := db.StartRun("main", idB, "agent-a", claimB.Token, "run.wt.rec.collide.b", 3600)
	if err != nil || replayed {
		t.Fatalf("start run b = %+v %v %v", runB, replayed, err)
	}
	_, boundary, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	optsB := WorktreeProvisionOptions{RunID: runB.RunID, Owner: "agent-a", ClaimToken: claimB.Token,
		Attempt: runB.Attempt, AttemptToken: runB.AttemptToken, RequestID: "wt.rec.collide.b",
		OwnerBoundary: boundary, RegistrationRoot: optsA.RegistrationRoot,
		TargetPath: filepath.Join(boundary, ".worktrees", WorktreeName("main", idB)),
		Branch:     WorktreeBranch("main", idB), BaseCommit: strings.Repeat("a", 40)}
	planB, err := db.PlanWorktreeProvision(optsB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.FinalizeWorktreeProvision(planB, WorktreeProvisionNeedsBroker); err != nil {
		t.Fatal(err)
	}

	attemptA, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, planA.SagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitWorktreeRecovery(attemptA, WorktreeRecoveryNeedsBroker); err != nil {
		t.Fatal(err)
	}

	collision := recoveryOptions(t, db, planB.SagaID, 1)
	collision.RequestID = attemptA.RequestID
	if result, err := db.RecoverWorktreeSaga(collision); result != nil || !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("cross-saga receipt collision = %+v, %v; want ReplayConflict", result, err)
	}

	// Alter the committed receipt body: the stored digest no longer matches
	// the recomputed one, so replay fails closed as corruption.
	var body string
	if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`, ReceiptKindWorktree, attemptA.RequestID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	tampered := strings.Replace(body, `"status":"needs-broker"`, `"status":"cleanup-required"`, 1)
	if tampered == body {
		t.Fatal("receipt body had no status member to alter")
	}
	if _, err := db.db.Exec(`UPDATE receipts SET body=? WHERE kind=? AND request_id=?`, tampered, ReceiptKindWorktree, attemptA.RequestID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, planA.SagaID, 1)); !errors.As(err, new(*ReceiptIntegrityError)) {
		t.Fatalf("altered receipt replay = %v; want ReceiptIntegrityError", err)
	}
}

// TestRecoverWorktreeSagaImpossibleHistoryFailsClosed pins §9.4's
// state-machine-authorized historical differences on replay: a nonterminal
// receipt must match the row it left — exactly at the current generation, or
// through the sole authorized divergence where run/task terminalization
// flipped a provisioning needs-broker row to cleanup-required without
// advancing the generation, or behind later generations only through
// transitions the recovery table allows. Every impossible history fails
// closed as corruption.
func TestRecoverWorktreeSagaImpossibleHistoryFailsClosed(t *testing.T) {
	t.Run("authorized terminalization divergence replays", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "history.divergence", WorktreeProvisionNeedsBroker)
		opts := recoveryOptions(t, db, plan.SagaID, 1)
		attempt, err := db.RecoverWorktreeSaga(opts)
		if err != nil {
			t.Fatal(err)
		}
		fresh, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker)
		if err != nil {
			t.Fatal(err)
		}
		// Run/task terminalization flips the row without advancing its
		// generation; the generation-1 receipt may retain needs-broker.
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET status='cleanup-required' WHERE saga_id=?`, plan.SagaID); err != nil {
			t.Fatal(err)
		}
		replay, err := db.RecoverWorktreeSaga(opts)
		if err != nil || replay.EffectRequired || replay.Result == nil {
			t.Fatalf("divergent replay = %+v, %v", replay, err)
		}
		if !reflect.DeepEqual(replay.Result, fresh) {
			t.Fatalf("divergent replay result = %+v; want historical %+v", replay.Result, fresh)
		}
	})
	t.Run("cleanup-required receipt with bound row fails closed", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "history.bound", WorktreeProvisionNeedsBroker)
		opts := recoveryOptions(t, db, plan.SagaID, 1)
		attempt, err := db.RecoverWorktreeSaga(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryCleanupRequired); err != nil {
			t.Fatal(err)
		}
		// A cleanup-required generation can never precede a bound row:
		// cleanup-required is cleanup-only and never binds.
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET status='bound' WHERE saga_id=?`, plan.SagaID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RecoverWorktreeSaga(opts); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("cleanup-required receipt against bound row = %v; want ReceiptIntegrityError", err)
		}
	})
	t.Run("generation without its deterministic receipt fails closed", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "history.gap", WorktreeProvisionNeedsBroker)
		opts := recoveryOptions(t, db, plan.SagaID, 1)
		attempt, err := db.RecoverWorktreeSaga(opts)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker); err != nil {
			t.Fatal(err)
		}
		// The row claims generation 2 without the deterministic receipt that
		// must justify it; the generation-1 receipt sits behind a gap.
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET recovery_generation=2 WHERE saga_id=?`, plan.SagaID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RecoverWorktreeSaga(opts); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("receipt behind missing successor = %v; want ReceiptIntegrityError", err)
		}
	})
}

// rewriteWorktreeReceipt loads one stored worktree receipt's canonical body,
// applies mutate, and rewrites the row with a freshly computed envelope
// digest: the tampered receipt stays structurally perfect, so only the
// semantic receipt checks can catch the change.
func rewriteWorktreeReceipt(t *testing.T, db *DB, requestID string, mutate func(map[string]any)) {
	t.Helper()
	var body string
	if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`,
		ReceiptKindWorktree, requestID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeJSONObject([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	mutate(decoded)
	delete(decoded, "receiptDigest")
	digest, err := receiptDigest(decoded)
	if err != nil {
		t.Fatal(err)
	}
	decoded["receiptDigest"] = digest
	canonical, err := canonicalJSON(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE receipts SET body=?, digest=? WHERE kind=? AND request_id=?`,
		string(canonical), digest, ReceiptKindWorktree, requestID); err != nil {
		t.Fatal(err)
	}
}

// recoverySagaStateOf loads one saga row through the store's own reader.
func recoverySagaStateOf(t *testing.T, db *DB, sagaID int64) *worktreeSagaState {
	t.Helper()
	var state *worktreeSagaState
	if err := db.deferred(func(t *tx) error {
		loaded, err := loadWorktreeSaga(t, sagaID)
		state = loaded
		return err
	}); err != nil || state == nil {
		t.Fatalf("load saga %d = %v, %v", sagaID, state, err)
	}
	return state
}

// TestRecoverWorktreeSagaFullReceiptChainValidation pins that a
// behind-generation replay validates the entire receipt chain from the
// replayed generation to the row's current generation — not merely the
// immediate successor step — so a corrupt middle link cannot launder a
// history whose later transitions are impossible.
func TestRecoverWorktreeSagaFullReceiptChainValidation(t *testing.T) {
	// chainGeneration commits one more nonterminal needs-broker generation
	// through the public API and returns the historical result it published.
	chainGeneration := func(t *testing.T, db *DB, sagaID, generation int64) *WorktreeMutationResult {
		t.Helper()
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, generation))
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	t.Run("fully valid multi-generation chain replays", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "chain.valid", WorktreeProvisionNeedsBroker)
		firstOpts := recoveryOptions(t, db, plan.SagaID, 1)
		first := chainGeneration(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 2)
		chainGeneration(t, db, plan.SagaID, 3)
		chainGeneration(t, db, plan.SagaID, 4)
		if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "needs-broker" || generation != 4 {
			t.Fatalf("row after chain = %q gen %d; want needs-broker gen 4", status, generation)
		}
		events, receipts := int64(len(feedOf(t, db))), scalarInt64(t, db,
			`SELECT COUNT(*) FROM receipts WHERE kind=?`, ReceiptKindWorktree)
		// The generation-1 receipt sits three authorized transitions behind
		// the row and still replays its exact historical projection.
		replay, err := db.RecoverWorktreeSaga(firstOpts)
		if err != nil {
			t.Fatal(err)
		}
		if replay.EffectRequired || replay.Result == nil || !reflect.DeepEqual(replay.Result, first) {
			t.Fatalf("generation-1 replay = %+v; want historical result %+v", replay.Result, first)
		}
		if replay.Result.RecoverRequestID == nil ||
			*replay.Result.RecoverRequestID != recoveryOptions(t, db, plan.SagaID, 2).RequestID {
			t.Fatalf("generation-1 replay advertised id = %+v", replay.Result.RecoverRequestID)
		}
		for range 2 {
			if _, err := db.RecoverWorktreeSaga(firstOpts); err != nil {
				t.Fatal(err)
			}
		}
		if got := int64(len(feedOf(t, db))); got != events {
			t.Fatalf("replay appended %d events", got-events)
		}
		if got := scalarInt64(t, db, `SELECT COUNT(*) FROM receipts WHERE kind=?`, ReceiptKindWorktree); got != receipts {
			t.Fatalf("replay appended %d receipts", got-receipts)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate chained state = %v", err)
		}
	})
	t.Run("missing middle receipt fails closed", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "chain.gap", WorktreeProvisionNeedsBroker)
		firstOpts := recoveryOptions(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 2)
		chainGeneration(t, db, plan.SagaID, 3)
		chainGeneration(t, db, plan.SagaID, 4)
		// The generation-3 link vanishes while its successors and the row's
		// generation remain: the immediate successor of the replayed
		// generation-1 receipt is intact, so only the full walk catches the
		// gap two steps further on.
		missing := recoveryOptions(t, db, plan.SagaID, 3).RequestID
		if _, err := db.db.Exec(`DELETE FROM receipts WHERE kind=? AND request_id=?`,
			ReceiptKindWorktree, missing); err != nil {
			t.Fatal(err)
		}
		if _, err := db.RecoverWorktreeSaga(firstOpts); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("replay over missing middle receipt = %v; want ReceiptIntegrityError", err)
		}
	})
	t.Run("illegal middle transition fails closed", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "chain.launder", WorktreeProvisionNeedsBroker)
		firstOpts := recoveryOptions(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 1)
		// needs-broker→cleanup-required is authorized, but a
		// cleanup-required provision commits failed only: rewriting the row
		// back to needs-broker lets the next commit record a
		// cleanup-required→needs-broker receipt no honest writer could leave.
		if _, err := db.CommitWorktreeRecovery(mustAttempt(t, db, plan.SagaID, 2),
			WorktreeRecoveryCleanupRequired); err != nil {
			t.Fatal(err)
		}
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET status='needs-broker' WHERE saga_id=?`, plan.SagaID); err != nil {
			t.Fatal(err)
		}
		chainGeneration(t, db, plan.SagaID, 3)
		// The generation-1 receipt's immediate successor transition is
		// authorized; the 2→3 step is impossible, so the replay must fail.
		if _, err := db.RecoverWorktreeSaga(firstOpts); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("laundered history replay = %v; want ReceiptIntegrityError", err)
		}
	})
	t.Run("corrupted middle receipt digest fails closed", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "chain.digest", WorktreeProvisionNeedsBroker)
		firstOpts := recoveryOptions(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 2)
		chainGeneration(t, db, plan.SagaID, 3)
		chainGeneration(t, db, plan.SagaID, 4)
		// Corrupt only the generation-3 link's result digest: the envelope,
		// field set, saga identity, and recorded generation all stay exact,
		// so the middle receipt's own digest verification is the sole
		// witness of the corruption.
		rewriteWorktreeReceipt(t, db, recoveryOptions(t, db, plan.SagaID, 3).RequestID,
			func(r map[string]any) {
				r["resultDigest"] = "sha256:" + strings.Repeat("00", 32)
			})
		if _, err := db.RecoverWorktreeSaga(firstOpts); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("replay over corrupted middle digest = %v; want ReceiptIntegrityError", err)
		}
	})
	t.Run("middle receipt recording wrong generation fails closed", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "chain.wronggen", WorktreeProvisionNeedsBroker)
		firstOpts := recoveryOptions(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 2)
		chainGeneration(t, db, plan.SagaID, 3)
		chainGeneration(t, db, plan.SagaID, 4)
		// Re-point the generation-3 receipt at another generation. The
		// envelope, input and result digests, and receipt id all remain
		// exactly what generation 3 committed — the verifiers recompute the
		// digests from the walked step's request id, not from the field — so
		// only the recorded-generation equality against the step catches the
		// lie.
		rewriteWorktreeReceipt(t, db, recoveryOptions(t, db, plan.SagaID, 3).RequestID,
			func(r map[string]any) {
				r["recoveryGeneration"] = json.Number("2")
			})
		if _, err := db.RecoverWorktreeSaga(firstOpts); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("replay over mis-generationed middle receipt = %v; want ReceiptIntegrityError", err)
		}
	})
	t.Run("terminal middle receipt before later generation fails closed", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "chain.terminal", WorktreeProvisionNeedsBroker)
		firstOpts := recoveryOptions(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 2)
		chainGeneration(t, db, plan.SagaID, 3)
		chainGeneration(t, db, plan.SagaID, 4)
		// Rewrite the generation-3 link into an honestly-shaped terminal
		// failure: envelope, input digest, and receipt id stay untouched and
		// the result digest is recomputed exactly as the writer would, so
		// every per-receipt check passes and only the walk's transition
		// table can refuse the history — a failed provision is final, yet a
		// generation-4 receipt exists beyond it.
		state := recoverySagaStateOf(t, db, plan.SagaID)
		rewriteWorktreeReceipt(t, db, recoveryOptions(t, db, plan.SagaID, 3).RequestID,
			func(r map[string]any) {
				committed, _ := r["committedAt"].(string)
				digest, err := worktreeResultDigest(state, "failed", false, committed, 3)
				if err != nil {
					t.Fatal(err)
				}
				r["status"] = "failed"
				r["active"] = false
				r["resultDigest"] = digest
			})
		if _, err := db.RecoverWorktreeSaga(firstOpts); !errors.As(err, new(*ReceiptIntegrityError)) {
			t.Fatalf("replay over terminal middle receipt = %v; want ReceiptIntegrityError", err)
		}
	})
	t.Run("terminalization divergence behind later generations still replays", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "chain.divergence", WorktreeProvisionNeedsBroker)
		firstOpts := recoveryOptions(t, db, plan.SagaID, 1)
		first := chainGeneration(t, db, plan.SagaID, 1)
		chainGeneration(t, db, plan.SagaID, 2)
		third := chainGeneration(t, db, plan.SagaID, 3)
		// Run/task terminalization flips the needs-broker row to
		// cleanup-required without advancing its generation; every walk that
		// crosses or lands on generation 3 must absorb the flip.
		if _, err := db.db.Exec(`UPDATE worktree_sagas SET status='cleanup-required' WHERE saga_id=?`, plan.SagaID); err != nil {
			t.Fatal(err)
		}
		replayOne, err := db.RecoverWorktreeSaga(firstOpts)
		if err != nil || replayOne.EffectRequired || replayOne.Result == nil {
			t.Fatalf("generation-1 replay across divergence = %+v, %v", replayOne, err)
		}
		if !reflect.DeepEqual(replayOne.Result, first) {
			t.Fatalf("generation-1 replay result = %+v; want historical %+v", replayOne.Result, first)
		}
		replayThree, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 3))
		if err != nil || replayThree.EffectRequired || replayThree.Result == nil {
			t.Fatalf("generation-3 replay under divergence = %+v, %v", replayThree, err)
		}
		if !reflect.DeepEqual(replayThree.Result, third) {
			t.Fatalf("generation-3 replay result = %+v; want historical %+v", replayThree.Result, third)
		}
	})
}

// mustAttempt reads one fresh recovery attempt for a generation, failing the
// test if the read half refuses.
func mustAttempt(t *testing.T, db *DB, sagaID, generation int64) *WorktreeRecoveryAttempt {
	t.Helper()
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, generation))
	if err != nil {
		t.Fatal(err)
	}
	return attempt
}

// TestRecoverWorktreeSagaOriginalRequestIDCollides pins that the saga's own
// original request id used through recover resolves to the original
// operation's input vector under the same (kind, requestId): a different
// normalized input, so the common typed §8.4 collision — both for an
// original receipt at the current generation and one at generation zero.
func TestRecoverWorktreeSagaOriginalRequestIDCollides(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "origcollide", WorktreeProvisionNeedsBroker)
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryBound); err != nil {
		t.Fatal(err)
	}
	viaRecovery := recoveryOptions(t, db, plan.SagaID, 1)
	viaRecovery.RequestID = plan.RequestID
	if result, err := db.RecoverWorktreeSaga(viaRecovery); result != nil || !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("original id (current generation) under recover = %+v, %v; want ReplayConflict", result, err)
	}

	db2, opts2, _, _ := provisionFixture(t, "wt.rec.origcollide.zero")
	terminal, err := db2.PlanWorktreeProvision(opts2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.FinalizeWorktreeProvision(terminal, WorktreeProvisionBound); err != nil {
		t.Fatal(err)
	}
	original := recoveryOptions(t, db2, terminal.SagaID, 1)
	original.RequestID = terminal.RequestID
	if result, err := db2.RecoverWorktreeSaga(original); result != nil || !errors.As(err, new(*ReplayConflict)) {
		t.Fatalf("original id (generation zero) under recover = %+v, %v; want ReplayConflict", result, err)
	}
}

// TestRecoverWorktreeSagaExhaustionRefusalBeforeDerivation pins the pinned
// §9.4 ordering: at SQLite's maximum signed generation the refusal precedes
// any id derivation or comparison, so even a garbage request id sees the
// exhaustion diagnostic, and no effect can run.
func TestRecoverWorktreeSagaExhaustionRefusalBeforeDerivation(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "exhausted", WorktreeProvisionCleanupRequired)
	if _, err := db.db.Exec(`UPDATE worktree_sagas SET recovery_generation=? WHERE saga_id=?`,
		int64(math.MaxInt64), plan.SagaID); err != nil {
		t.Fatal(err)
	}
	fence := sagaFenceOf(t, db, plan.SagaID)
	for _, id := range []string{recoveryOptions(t, db, plan.SagaID, 1).RequestID, "garbage", ""} {
		attempt, err := db.RecoverWorktreeSaga(WorktreeRecoveryOptions{SagaID: plan.SagaID, SagaToken: fence, RequestID: id})
		if attempt != nil || !errors.Is(err, ErrWorktreeRecoveryExhausted) {
			t.Fatalf("exhausted recover with id %q = %+v, %v", id, attempt, err)
		}
		if err.Error() != "worktree recovery generation exhausted" {
			t.Fatalf("exhaustion diagnostic = %q", err.Error())
		}
	}
}

// TestCommitWorktreeRecoveryAtMaximumGeneration pins the generation boundary:
// the preceding generation may commit the maximum, a still-nonterminal
// outcome there has null recoverRequestId and null brokerRequest, receipt
// replay of the committed maximum stays valid, and every new raw recovery
// refuses exhausted.
func TestCommitWorktreeRecoveryAtMaximumGeneration(t *testing.T) {
	db, plan, _, _, _, _ := recoverableRemoval(t, "max", WorktreeRemoveCleanupRequired)
	if _, err := db.db.Exec(`UPDATE worktree_sagas SET recovery_generation=? WHERE saga_id=?`,
		int64(math.MaxInt64-1), plan.SagaID); err != nil {
		t.Fatal(err)
	}
	opts := recoveryOptions(t, db, plan.SagaID, math.MaxInt64)
	attempt, err := db.RecoverWorktreeSaga(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.EffectRequired || attempt.RecoveryGeneration != math.MaxInt64-1 {
		t.Fatalf("max-boundary attempt = %+v", attempt)
	}
	result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "needs-broker" || result.RecoveryGeneration != math.MaxInt64 ||
		result.RecoverRequestID != nil || result.BrokerRequest != nil {
		t.Fatalf("max-generation nonterminal result = %+v", result)
	}
	if status, _, _ := sagaRow(t, db, plan.SagaID); status != "needs-broker" {
		t.Fatalf("max-generation status = %q", status)
	}
	if replay, err := db.RecoverWorktreeSaga(opts); err != nil ||
		replay.EffectRequired || replay.Result == nil || replay.Result.RecoveryGeneration != math.MaxInt64 {
		t.Fatalf("max-generation receipt replay = %+v, %v", replay, err)
	}
	fresh := WorktreeRecoveryOptions{SagaID: plan.SagaID, SagaToken: opts.SagaToken,
		RequestID: "recover-" + strings.Repeat("e", 64)}
	if _, err := db.RecoverWorktreeSaga(fresh); !errors.Is(err, ErrWorktreeRecoveryExhausted) {
		t.Fatalf("new raw recovery after maximum = %v", err)
	}
}

// TestCommitWorktreeRemovalRecoveryTerminalOrdering pins the exact §9.4
// terminal-recovery transaction: recovering event and recovery receipt first,
// then the original terminal event and original receipt, each receipt covering
// exactly its own one event, and both §8.5 inventories computed from the
// final transaction state.
func TestCommitWorktreeRemovalRecoveryTerminalOrdering(t *testing.T) {
	db, plan, options, source, _, _ := recoverableRemoval(t, "terminal", WorktreeRemoveNeedsBroker)
	recoveryOpts := recoveryOptions(t, db, plan.SagaID, 1)
	attempt, err := db.RecoverWorktreeSaga(recoveryOpts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryRemoved)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "removed" || result.RecoveryGeneration != 1 || result.SagaToken != 0 ||
		result.RecoverRequestID != nil || result.BrokerRequest != nil {
		t.Fatalf("removed recovery result = %+v", result)
	}
	if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "removed" || generation != 1 {
		t.Fatalf("removed row = %s/%d", status, generation)
	}
	if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID); active != 0 {
		t.Fatalf("source active after removed recovery = %d", active)
	}

	events := feedOf(t, db)
	recovering, terminal := events[len(events)-2], events[len(events)-1]
	if recovering.Kind != "worktree" || recovering.Action != "recovering" || recovering.Tag != plan.Tag ||
		recovering.Task != plan.Task || len(recovering.Extra) != 3 || extraInt64(t, recovering, "sagaId") != plan.SagaID {
		t.Fatalf("recovering event = %+v", recovering)
	}
	if terminal.Kind != "worktree" || terminal.Action != "removed" || len(terminal.Extra) != 3 ||
		extraInt64(t, terminal, "sagaId") != plan.SagaID {
		t.Fatalf("terminal event = %+v", terminal)
	}
	if recovering.Seq >= terminal.Seq {
		t.Fatalf("event order = recovering %d, terminal %d", recovering.Seq, terminal.Seq)
	}

	recoveryReceipt, err := db.FindReceipt(ReceiptKindWorktree, recoveryOpts.RequestID)
	if err != nil || recoveryReceipt == nil {
		t.Fatalf("recovery receipt = %v, %v", recoveryReceipt, err)
	}
	originalReceipt, err := db.FindReceipt(ReceiptKindWorktree, options.RequestID)
	if err != nil || originalReceipt == nil {
		t.Fatalf("original receipt = %v, %v", originalReceipt, err)
	}
	if extraString(t, recovering, "receiptId") != recoveryReceipt["receiptId"] {
		t.Fatalf("recovering event references %s, want its own recovery receipt %v",
			extraString(t, recovering, "receiptId"), recoveryReceipt["receiptId"])
	}
	if extraString(t, terminal, "receiptId") != originalReceipt["receiptId"] {
		t.Fatalf("terminal event references %s, want the original receipt %v",
			extraString(t, terminal, "receiptId"), originalReceipt["receiptId"])
	}
	for _, receipt := range []map[string]any{recoveryReceipt, originalReceipt} {
		seq := receipt["eventSeq"].(map[string]any)
		if seq["first"] != seq["last"] {
			t.Fatalf("receipt event range is not one event: %v", seq)
		}
	}
	if recoveryReceipt["eventSeq"].(map[string]any)["first"] != json.Number(fmt.Sprint(recovering.Seq)) {
		t.Fatalf("recovery receipt range = %v, want recovering seq %d",
			recoveryReceipt["eventSeq"], recovering.Seq)
	}
	if originalReceipt["eventSeq"].(map[string]any)["first"] != json.Number(fmt.Sprint(terminal.Seq)) {
		t.Fatalf("original receipt range = %v, want terminal seq %d",
			originalReceipt["eventSeq"], terminal.Seq)
	}
	if recoveryReceipt["status"] != "removed" || recoveryReceipt["recoveryGeneration"] != json.Number("1") ||
		recoveryReceipt["active"] != false || recoveryReceipt["operation"] != "remove" {
		t.Fatalf("recovery receipt members = %v", recoveryReceipt)
	}
	// The original receipt uses the current generation when recovery made the
	// saga terminal (SPEC §9.4 receipt generation rule).
	if originalReceipt["status"] != "removed" || originalReceipt["recoveryGeneration"] != json.Number("1") {
		t.Fatalf("original receipt members = %v", originalReceipt)
	}

	// Both inventories observe the final transaction state: neither receipt
	// counts fewer events or receipts than the committed store holds.
	inv := storeInventory(t, db)
	for label, receipt := range map[string]map[string]any{"recovery": recoveryReceipt, "original": originalReceipt} {
		ri := receipt["inventory"].(map[string]any)
		if got, ok := receiptInt64(ri["events"]); !ok || got != int64(inv.Events) {
			t.Fatalf("%s receipt inventory events = %v, want final %d", label, ri["events"], inv.Events)
		}
		if got, ok := receiptInt64(ri["receipts"]); !ok || got != int64(inv.Receipts) {
			t.Fatalf("%s receipt inventory receipts = %v, want final %d", label, ri["receipts"], inv.Receipts)
		}
		if got, ok := receiptInt64(ri["worktreeSagas"]); !ok || got != int64(inv.WorktreeSagas) || got < 1 {
			t.Fatalf("%s receipt inventory worktreeSagas = %v, want final %d", label, ri["worktreeSagas"], inv.WorktreeSagas)
		}
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate terminal recovery = %v", err)
	}

	// Events and receipts stay content-free; the recover result omits the
	// saga token and every capability.
	for _, receipt := range []map[string]any{recoveryReceipt, originalReceipt} {
		encoded, _ := json.Marshal(receipt)
		for _, forbidden := range []string{"sagaToken", "sagaFence", "claimToken", "attemptToken", "registrationRoot"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Fatalf("recovery receipt leaked %q: %s", forbidden, encoded)
			}
		}
	}
	encodedEvent, _ := json.Marshal(recovering)
	for _, forbidden := range []string{"sagaToken", "claimToken", "attemptToken", "registrationRoot", plan.TargetPath} {
		if strings.Contains(string(encodedEvent), forbidden) {
			t.Fatalf("recovering event leaked %q: %s", forbidden, encodedEvent)
		}
	}
}

// TestCommitWorktreeRecoveryNonterminalCommitsOneGeneration pins the plain
// committed nonterminal attempt for both operations: generation g+1, exactly
// one recovery receipt and one recovering event, no original receipt, and the
// next advertised id in the result.
func TestCommitWorktreeRecoveryNonterminalCommitsOneGeneration(t *testing.T) {
	t.Run("provision needs-broker", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "nonterminal", WorktreeProvisionNeedsBroker)
		before := storeInventory(t, db)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "needs-broker" || result.RecoveryGeneration != 1 ||
			result.RecoverRequestID == nil || *result.RecoverRequestID != recoveryOptions(t, db, plan.SagaID, 2).RequestID ||
			result.BrokerRequest == nil || result.BrokerRequest.Verb != "create" ||
			result.BrokerRequest.RecoverRequestID != *result.RecoverRequestID {
			t.Fatalf("needs-broker recovery result = %+v", result)
		}
		if status, generation, active := sagaRow(t, db, plan.SagaID); status != "needs-broker" || generation != 1 || active != 0 {
			t.Fatalf("row after nonterminal recovery = %s/%d/%d", status, generation, active)
		}
		after := storeInventory(t, db)
		if after.Events != before.Events+1 || after.Receipts != before.Receipts+1 {
			t.Fatalf("nonterminal recovery counts = events %d->%d, receipts %d->%d",
				before.Events, after.Events, before.Receipts, after.Receipts)
		}
		if receipt, err := db.FindReceipt(ReceiptKindWorktree, plan.RequestID); err != nil || receipt != nil {
			t.Fatalf("nonterminal recovery wrote an original receipt: %v, %v", receipt, err)
		}
		events := feedOf(t, db)
		if last := events[len(events)-1]; last.Action != "recovering" {
			t.Fatalf("last event = %+v", last)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate nonterminal recovery = %v", err)
		}
	})
	t.Run("removal needs-broker", func(t *testing.T) {
		db, plan, _, source, _, _ := recoverableRemoval(t, "nonterminal", WorktreeRemoveNeedsBroker)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "needs-broker" || result.RecoveryGeneration != 1 ||
			result.BrokerRequest == nil || result.BrokerRequest.Verb != "remove" ||
			result.BrokerRequest.RegistrationRoot != source.RegistrationRoot ||
			result.BrokerRequest.Force {
			t.Fatalf("removal needs-broker recovery result = %+v", result)
		}
		if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID); active != 1 {
			t.Fatalf("source active after needs-broker recovery = %d", active)
		}
		if receipt, err := db.FindReceipt(ReceiptKindWorktree, attempt.RequestID); err != nil || receipt == nil ||
			receipt["status"] != "needs-broker" || receipt["active"] != true {
			t.Fatalf("needs-broker recovery receipt = %v, %v", receipt, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate removal needs-broker recovery = %v", err)
		}
	})
}

// TestCommitWorktreeRecoveryProvisionNoBurnException pins §9.4's sole
// commit-nothing case: an unproved cleanup-required provision attempt writes
// no transition, receipt, event, or generation, returns the unchanged
// handoff, and leaves the generation unburned for a later terminal attempt.
func TestCommitWorktreeRecoveryProvisionNoBurnException(t *testing.T) {
	for _, tt := range []struct {
		name     string
		outcome  WorktreeRecoveryOutcome
		confined bool
	}{{"unproved", WorktreeRecoveryCleanupRequired, false}, {"confined", WorktreeRecoveryCleanupConfined, true}} {
		t.Run(tt.name, func(t *testing.T) {
			db, plan, _, _, _ := recoverableProvision(t, "noburn."+tt.name, WorktreeProvisionCleanupRequired)
			before := storeInventory(t, db)
			opts := recoveryOptions(t, db, plan.SagaID, 1)
			attempt, err := db.RecoverWorktreeSaga(opts)
			if err != nil {
				t.Fatal(err)
			}
			if !attempt.EffectRequired || attempt.Status != "cleanup-required" || attempt.RecoveryGeneration != 0 {
				t.Fatalf("cleanup attempt = %+v", attempt)
			}
			result, err := db.CommitWorktreeRecovery(attempt, tt.outcome)
			if err != nil {
				t.Fatal(err)
			}
			// Same status, same generation, same advertised id: the identical
			// request may be retried.
			if result.Status != "cleanup-required" || result.RecoveryGeneration != 0 ||
				result.RecoverRequestID == nil || *result.RecoverRequestID != opts.RequestID {
				t.Fatalf("no-burn handoff = %+v", result)
			}
			if tt.confined != (result.BrokerRequest != nil) {
				t.Fatalf("no-burn broker request = %+v", result.BrokerRequest)
			}
			if tt.confined && (result.BrokerRequest.Verb != "remove" || result.BrokerRequest.Force ||
				result.BrokerRequest.RecoverRequestID != opts.RequestID ||
				result.BrokerRequest.TargetPath != plan.TargetPath) {
				t.Fatalf("confined compensation mint bindings = %+v", result.BrokerRequest)
			}
			after := storeInventory(t, db)
			if after.Events != before.Events || after.Receipts != before.Receipts {
				t.Fatalf("no-burn committed events %d->%d receipts %d->%d",
					before.Events, after.Events, before.Receipts, after.Receipts)
			}
			if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "cleanup-required" || generation != 0 {
				t.Fatalf("no-burn row = %s/%d", status, generation)
			}
			// The identical request retries unchanged.
			again, err := db.RecoverWorktreeSaga(opts)
			if err != nil || !again.EffectRequired || again.RecoveryGeneration != 0 {
				t.Fatalf("identical retry after no-burn = %+v, %v", again, err)
			}
			// The generation was not burned: a proved-absence observation
			// still terminalizes under the same request id.
			terminal, err := db.CommitWorktreeRecovery(again, WorktreeRecoveryFailed)
			if err != nil {
				t.Fatal(err)
			}
			if terminal.Status != "failed" || terminal.RecoveryGeneration != 1 {
				t.Fatalf("terminal after no-burn = %+v", terminal)
			}
			if err := db.ValidateWorktreeSagaState(); err != nil {
				t.Fatalf("validate no-burn state = %v", err)
			}
		})
	}
}

// TestCommitWorktreeRemovalRecoveryCleanupRequiredCommits pins the removal
// counterpart of the no-burn rule: a removal attempt that remains
// cleanup-required still commits g+1 with its recovery receipt.
func TestCommitWorktreeRemovalRecoveryCleanupRequiredCommits(t *testing.T) {
	db, plan, _, source, _, _ := recoverableRemoval(t, "cleanup", WorktreeRemoveCleanupRequired)
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryCleanupRequired)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "cleanup-required" || result.RecoveryGeneration != 1 ||
		result.RecoverRequestID == nil || *result.RecoverRequestID != recoveryOptions(t, db, plan.SagaID, 2).RequestID ||
		result.BrokerRequest != nil {
		t.Fatalf("cleanup-required removal recovery = %+v", result)
	}
	if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "cleanup-required" || generation != 1 {
		t.Fatalf("cleanup-required row = %s/%d", status, generation)
	}
	receipt, err := db.FindReceipt(ReceiptKindWorktree, attempt.RequestID)
	if err != nil || receipt == nil || receipt["status"] != "cleanup-required" ||
		receipt["recoveryGeneration"] != json.Number("1") || receipt["active"] != true {
		t.Fatalf("cleanup-required recovery receipt = %v, %v", receipt, err)
	}
	// The source binding stays active: cleanup-required proved nothing.
	if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID); active != 1 {
		t.Fatalf("source active after cleanup-required recovery = %d", active)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate cleanup-required recovery = %v", err)
	}
}

// TestCommitWorktreeRecoveryProvisionEligibilityAndCompensation pins the
// provisioning recovery recheck. §9.4's terminalizing parents flip the run's
// nonterminal provision to cleanup-required in their own transaction, so
// after a cancel the cleanup-only rule refuses binding outright; a store
// whose run was terminalized before that scheduling existed (or without a
// parent transaction) still holds a bindable-looking needs-broker provision,
// and there bound and needs-broker observations cannot commit and must be
// compensated. Failed commits in both shapes, and a cleanup-required
// provision can never bind or return to needs-broker.
func TestCommitWorktreeRecoveryProvisionEligibilityAndCompensation(t *testing.T) {
	t.Run("terminal run", func(t *testing.T) {
		db, plan, _, claim, run := recoverableProvision(t, "ineligible", WorktreeProvisionNeedsBroker)
		if _, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, run.Attempt,
			run.AttemptToken, false, "run.cancel.recovery"); err != nil {
			t.Fatal(err)
		}
		if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "cleanup-required" || generation != 0 {
			t.Fatalf("cancelled provision = %s/%d; want cleanup-required/0", status, generation)
		}
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		before := storeInventory(t, db)
		for _, outcome := range []WorktreeRecoveryOutcome{WorktreeRecoveryBound, WorktreeRecoveryNeedsBroker} {
			if _, err := db.CommitWorktreeRecovery(attempt, outcome); !errors.As(err, new(*WorktreeRecoveryStatusError)) {
				t.Fatalf("flipped %s recovery = %v; want cleanup-only state error", outcome, err)
			}
		}
		if after := storeInventory(t, db); after.Events != before.Events || after.Receipts != before.Receipts {
			t.Fatalf("refused flipped recovery committed events %d->%d receipts %d->%d",
				before.Events, after.Events, before.Receipts, after.Receipts)
		}
		result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryFailed)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "failed" || result.RecoveryGeneration != 1 || result.SagaToken != 0 {
			t.Fatalf("compensated failed recovery = %+v", result)
		}
		receipt, err := db.FindReceipt(ReceiptKindWorktree, attempt.RequestID)
		if err != nil || receipt == nil || receipt["status"] != "failed" {
			t.Fatalf("failed recovery receipt = %v, %v", receipt, err)
		}
		original, err := db.FindReceipt(ReceiptKindWorktree, plan.RequestID)
		if err != nil || original == nil || original["status"] != "failed" ||
			original["recoveryGeneration"] != json.Number("1") {
			t.Fatalf("failed original receipt = %v, %v", original, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate compensated recovery = %v", err)
		}
	})
	t.Run("terminal run without the parent flip", func(t *testing.T) {
		// A run terminalized outside §9.4's scheduling parents — an older
		// binary's store — leaves the needs-broker provision bindable; the
		// commit-time recheck must demand compensation, never bind.
		db, plan, _, _, run := recoverableProvision(t, "ineligible.preflip", WorktreeProvisionNeedsBroker)
		if _, err := db.db.Exec(`UPDATE runs SET status='cancelled',terminal_at=updated_at WHERE run_id=?`, run.RunID); err != nil {
			t.Fatal(err)
		}
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		for _, outcome := range []WorktreeRecoveryOutcome{WorktreeRecoveryBound, WorktreeRecoveryNeedsBroker} {
			if _, err := db.CommitWorktreeRecovery(attempt, outcome); !errors.As(err, new(*WorktreeProvisionCompensationRequired)) {
				t.Fatalf("pre-flip %s recovery = %v; want compensation required", outcome, err)
			}
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryFailed); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate compensated pre-flip recovery = %v", err)
		}
	})
	t.Run("cleanup-only provision", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "cleanuponly", WorktreeProvisionCleanupRequired)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		for _, outcome := range []WorktreeRecoveryOutcome{WorktreeRecoveryBound, WorktreeRecoveryNeedsBroker, WorktreeRecoveryRemoved} {
			if _, err := db.CommitWorktreeRecovery(attempt, outcome); !errors.As(err, new(*WorktreeRecoveryStatusError)) {
				t.Fatalf("cleanup-only provision %s = %v; want WorktreeRecoveryStatusError", outcome, err)
			}
		}
		if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "cleanup-required" || generation != 0 {
			t.Fatalf("cleanup-only row after refusals = %s/%d", status, generation)
		}
	})
}

// TestCommitWorktreeRemovalRecoveryFailedForbidden pins §9.4's cleanup
// authority during recovery: while the source run is terminal or the task is
// done, a removal recovery may terminalize only as removed.
func TestCommitWorktreeRemovalRecoveryFailedForbidden(t *testing.T) {
	t.Run("terminal run", func(t *testing.T) {
		db, plan, _, _, claim, run := recoverableRemoval(t, "forbidden", WorktreeRemoveNeedsBroker)
		if _, _, err := db.CancelRun(run.RunID, "agent-a", claim.Token, run.Attempt,
			run.AttemptToken, false, "run.cancel.recovery.forbidden"); err != nil {
			t.Fatal(err)
		}
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		before := storeInventory(t, db)
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryFailed); !errors.As(err, new(*WorktreeRemovalFailedForbidden)) {
			t.Fatalf("terminal-run failed recovery = %v; want WorktreeRemovalFailedForbidden", err)
		}
		if after := storeInventory(t, db); after.Events != before.Events || after.Receipts != before.Receipts {
			t.Fatalf("forbidden failed recovery committed events %d->%d receipts %d->%d",
				before.Events, after.Events, before.Receipts, after.Receipts)
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryCleanupRequired); err != nil {
			t.Fatalf("terminal-run cleanup-required recovery = %v", err)
		}
		next, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 2))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CommitWorktreeRecovery(next, WorktreeRecoveryRemoved); err != nil {
			t.Fatal(err)
		}
		if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "removed" || generation != 2 {
			t.Fatalf("terminal-run removed row = %s/%d", status, generation)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate terminal-run removal recovery = %v", err)
		}
	})
	t.Run("done task", func(t *testing.T) {
		db, plan, options, _, claim, run := recoverableRemoval(t, "forbidden.done", WorktreeRemoveNeedsBroker)
		task, err := db.GetTask(plan.Tag, plan.Task)
		if err != nil {
			t.Fatal(err)
		}
		gate := &ClaimGate{Owner: options.Owner, Token: claim.Token, RunID: run.RunID,
			Attempt: run.Attempt, AttemptToken: run.AttemptToken}
		if _, err := db.SetStatus(plan.Tag, plan.Task, "done", task.Revision, gate); err != nil {
			t.Fatal(err)
		}
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryFailed); !errors.As(err, new(*WorktreeRemovalFailedForbidden)) {
			t.Fatalf("done-task failed recovery = %v", err)
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryRemoved); err != nil {
			t.Fatal(err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate done-task removal recovery = %v", err)
		}
	})
}

// seedLegacyCleanupRow inserts one v4→v5 converted legacy row plus its
// retained task binding, exactly as the upgrade would have.
func seedLegacyCleanupRow(t *testing.T, db *DB, taskID, created string) int64 {
	t.Helper()
	_, boundary, err := db.Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(boundary, ".worktrees", WorktreeName("main", taskID))
	branch := WorktreeBranch("main", taskID)
	if _, err := db.db.Exec(`UPDATE tasks SET wt_boundary=?,wt_path=?,wt_branch=?,wt_created_at=?,wt_keep=0
		WHERE tag='main' AND id=?`, boundary, path, branch, created, taskID); err != nil {
		t.Fatal(err)
	}
	request := legacyWorktreeSagaRequestID(testFamilyID(t, db), "main", taskID, path, branch)
	insert, err := db.db.Exec(`INSERT INTO worktree_sagas(
		run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
		request_id,recovery_generation,owner_boundary,registration_root,target_path,
		branch,base_commit,force,created_at,updated_at,extra)
		VALUES (NULL,NULL,'main',?,'remove','cleanup-required',0,12345,?,0,?,NULL,?,?,NULL,0,?,?,NULL)`,
		taskID, request, boundary, path, branch, created, created)
	if err != nil {
		t.Fatal(err)
	}
	sagaID, err := insert.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return sagaID
}

// TestCommitWorktreeRecoveryLegacyCleanup pins the runless sourceless legacy
// rows: recovery is fence-authorized with a null run/source/base snapshot, a
// removed outcome clears the retained legacy binding in the same transaction,
// a failed outcome retains it, and receipts render the null members.
func TestCommitWorktreeRecoveryLegacyCleanup(t *testing.T) {
	t.Run("removed clears legacy binding", func(t *testing.T) {
		db, _, _, _, _ := recoverableProvision(t, "legacy", WorktreeProvisionBound)
		created := time.Now().UTC().Format(time.RFC3339)
		legacyTask := addTask(t, db, "main", "legacy binding")
		sagaID := seedLegacyCleanupRow(t, db, legacyTask, created)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		if !attempt.EffectRequired || attempt.RunID != 0 || attempt.SourceSagaID != 0 ||
			attempt.RegistrationRoot != "" || attempt.BaseCommit != "" || attempt.Operation != "remove" {
			t.Fatalf("legacy attempt snapshot = %+v", attempt)
		}
		result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryRemoved)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "removed" || result.RecoveryGeneration != 1 || result.SagaToken != 0 ||
			result.RunID != nil || result.BaseCommit != nil {
			t.Fatalf("legacy removed result = %+v (runId %v baseCommit %v)", result, result.RunID, result.BaseCommit)
		}
		var boundary, path, branch, at any
		if err := db.db.QueryRow(`SELECT wt_boundary,wt_path,wt_branch,wt_created_at FROM tasks WHERE tag='main' AND id=?`, legacyTask).
			Scan(&boundary, &path, &branch, &at); err != nil {
			t.Fatal(err)
		}
		if boundary != nil || path != nil || branch != nil || at != nil {
			t.Fatalf("legacy binding retained after removed: %v %v %v %v", boundary, path, branch, at)
		}
		receipt, err := db.FindReceipt(ReceiptKindWorktree, attempt.RequestID)
		if err != nil || receipt == nil || receipt["runId"] != nil || receipt["baseCommit"] != nil ||
			receipt["owner"] != "" || receipt["sourceSagaId"] != nil {
			t.Fatalf("legacy recovery receipt = %v, %v", receipt, err)
		}
		original, err := db.FindReceipt(ReceiptKindWorktree, attempt.OriginalRequestID)
		if err != nil || original == nil || original["runId"] != nil || original["baseCommit"] != nil {
			t.Fatalf("legacy original receipt = %v, %v", original, err)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate legacy removed = %v", err)
		}
		// Historical replay renders the null members as nulls.
		replay, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 1))
		if err != nil || replay.Result == nil || replay.Result.RunID != nil || replay.Result.BaseCommit != nil {
			t.Fatalf("legacy replay = %+v, %v", replay.Result, err)
		}
	})
	t.Run("failed retains legacy binding", func(t *testing.T) {
		db, _, _, _, _ := recoverableProvision(t, "legacy.failed", WorktreeProvisionBound)
		created := time.Now().UTC().Format(time.RFC3339)
		legacyTask := addTask(t, db, "main", "legacy binding")
		sagaID := seedLegacyCleanupRow(t, db, legacyTask, created)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryFailed); err != nil {
			t.Fatal(err)
		}
		var path any
		if err := db.db.QueryRow(`SELECT wt_path FROM tasks WHERE tag='main' AND id=?`, legacyTask).Scan(&path); err != nil {
			t.Fatal(err)
		}
		if path == nil {
			t.Fatal("legacy binding cleared by failed outcome")
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate legacy failed = %v", err)
		}
	})
	t.Run("needs-broker prunes the registration", func(t *testing.T) {
		db, _, _, _, _ := recoverableProvision(t, "legacy.prune", WorktreeProvisionBound)
		created := time.Now().UTC().Format(time.RFC3339)
		legacyTask := addTask(t, db, "main", "legacy binding")
		sagaID := seedLegacyCleanupRow(t, db, legacyTask, created)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		if attempt.ObservedRegistrationRoot != "" {
			t.Fatalf("fresh legacy attempt carries an observed root = %+v", attempt)
		}
		// Without an observation the store cannot derive the root: the
		// handoff still names the §9.4 prune verb over the null stored root.
		bare, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker)
		if err != nil {
			t.Fatal(err)
		}
		if bare.Status != "needs-broker" || bare.RecoveryGeneration != 1 ||
			bare.RecoverRequestID == nil || *bare.RecoverRequestID != recoveryOptions(t, db, sagaID, 2).RequestID ||
			bare.BrokerRequest == nil || bare.BrokerRequest.Verb != "prune-registration" ||
			bare.BrokerRequest.RegistrationRoot != "" || bare.BrokerRequest.Force {
			t.Fatalf("legacy prune handoff = %+v", bare)
		}
		// The trusted host threads its effect-time-derived root through the
		// attempt, so the mint binding carries the exact derived root.
		second, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 2))
		if err != nil {
			t.Fatal(err)
		}
		_, familyBoundary, err := db.Identity()
		if err != nil {
			t.Fatal(err)
		}
		second.ObservedRegistrationRoot = filepath.Join(familyBoundary, "common", "worktrees-root")
		if second.ObservedRegistrationRoot != filepath.Clean(second.ObservedRegistrationRoot) {
			t.Fatal("test root is not canonical")
		}
		result, err := db.CommitWorktreeRecovery(second, WorktreeRecoveryNeedsBroker)
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "needs-broker" || result.RecoveryGeneration != 2 ||
			result.BrokerRequest == nil || result.BrokerRequest.Verb != "prune-registration" ||
			result.BrokerRequest.RegistrationRoot != second.ObservedRegistrationRoot ||
			result.BrokerRequest.RecoverRequestID != recoveryOptions(t, db, sagaID, 3).RequestID {
			t.Fatalf("observed-root prune handoff = %+v", result)
		}
		if status, generation, _ := sagaRow(t, db, sagaID); status != "needs-broker" || generation != 2 {
			t.Fatalf("prune row = %s/%d", status, generation)
		}
		if root := scalarInt64(t, db, `SELECT COUNT(*) FROM worktree_sagas WHERE saga_id=? AND registration_root IS NOT NULL`, sagaID); root != 0 {
			t.Fatalf("observed root persisted into the saga row: %d", root)
		}
		if err := db.ValidateWorktreeSagaState(); err != nil {
			t.Fatalf("validate legacy prune = %v", err)
		}
	})
}

// TestRecoverWorktreeSagaAutomaticRunlessRow pins that a runless automatic
// cleanup row — §9.4's direct task-done shape with no run — recovers through
// its saga fence alone and terminalizes as removed.
func TestRecoverWorktreeSagaAutomaticRunlessRow(t *testing.T) {
	db, source, _, _, _ := recoverableProvision(t, "auto", WorktreeProvisionBound)
	counter, err := db.Fencing()
	if err != nil {
		t.Fatal(err)
	}
	fence := counter + 1
	if _, err := db.db.Exec(`UPDATE meta SET value=? WHERE key='fencing'`, fence); err != nil {
		t.Fatal(err)
	}
	request := automaticCleanupRequestID(testFamilyID(t, db), source.SagaID, 0)
	created := nowUTC()
	insert, err := db.db.Exec(`INSERT INTO worktree_sagas(
		run_id,source_saga_id,tag,task_id,operation,status,binding_active,saga_fence,
		request_id,recovery_generation,owner_boundary,registration_root,target_path,
		branch,base_commit,force,created_at,updated_at,extra)
		VALUES (NULL,?,?,?,'remove','pending',0,?,?,0,?,?,?,?,?,0,?,?,NULL)`,
		source.SagaID, source.Tag, source.Task, fence, request, source.OwnerBoundary,
		source.RegistrationRoot, source.TargetPath, source.Branch, source.BaseCommit, created, created)
	if err != nil {
		t.Fatal(err)
	}
	sagaID, err := insert.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.EffectRequired || attempt.RunID != 0 || attempt.SourceSagaID != source.SagaID {
		t.Fatalf("runless automatic attempt = %+v", attempt)
	}
	result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryRemoved)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "removed" || result.RecoveryGeneration != 1 || result.RunID != nil {
		t.Fatalf("runless automatic removed = %+v", result)
	}
	if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID); active != 0 {
		t.Fatalf("source active after runless automatic removal = %d", active)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate runless automatic removal = %v", err)
	}
}

// TestRecoverWorktreeSagaAutomaticRunCausedReceiptDigest pins §9.4's
// automatic-owner rule on the original receipt's input digest through the
// real scheduling path: run finish succeeded inserts the run-caused cleanup
// (an auto- request id with the recorded run id), recovery drives it to
// removed, and the stored original receipt's inputDigest is the empty-owner
// digest — the run owner never enters an automatic digest — while the
// receipt's owner member still records the run's owner.
func TestRecoverWorktreeSagaAutomaticRunCausedReceiptDigest(t *testing.T) {
	db, provision, source, claim, run := boundProvisionFixture(t, "rundigest")
	if _, _, err := db.FinishRun(run.RunID, provision.Owner, claim.Token, 1, run.AttemptToken,
		"succeeded", "finish.auto.rundigest", "", 0, ""); err != nil {
		t.Fatal(err)
	}
	familyID := testFamilyID(t, db)
	request, sagaID, rowRun, rowSource, _, force, status := scheduledCleanupRow(t, db, source.Tag, source.Task)
	if request != automaticCleanupRequestID(familyID, source.SagaID, run.RunID) ||
		rowRun != run.RunID || rowSource != source.SagaID || force != 0 || status != "pending" {
		t.Fatalf("scheduled cleanup = %q run %d source %d force %d status %s",
			request, rowRun, rowSource, force, status)
	}
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, sagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if !attempt.EffectRequired || attempt.RunID != run.RunID {
		t.Fatalf("run-caused automatic attempt = %+v", attempt)
	}
	result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryRemoved)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "removed" || result.RecoveryGeneration != 1 || result.RunID == nil || *result.RunID != run.RunID {
		t.Fatalf("run-caused automatic removed = %+v", result)
	}
	if active := scalarInt64(t, db, `SELECT binding_active FROM worktree_sagas WHERE saga_id=?`, source.SagaID); active != 0 {
		t.Fatalf("source active after run-caused automatic removal = %d", active)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate run-caused automatic removal = %v", err)
	}
	var body []byte
	if err := db.db.QueryRow(`SELECT body FROM receipts WHERE kind=? AND request_id=?`,
		ReceiptKindWorktree, request).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var receipt map[string]any
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	want, err := worktreeInputDigest(provision.Owner, request, run.RunID, false, "remove")
	if err != nil {
		t.Fatal(err)
	}
	literal, err := canonicalDigest(map[string]any{"force": false, "operation": "remove",
		"owner": "", "requestId": request, "runId": run.RunID, "sagaId": int64(0)})
	if err != nil {
		t.Fatal(err)
	}
	if want != literal {
		t.Fatalf("builder digest %q is not the empty-owner digest %q", want, literal)
	}
	if receipt["inputDigest"] != want {
		t.Fatalf("original receipt inputDigest = %v, want empty-owner digest %q",
			receipt["inputDigest"], want)
	}
	if receipt["owner"] != provision.Owner {
		t.Fatalf("original receipt owner = %v, want run owner %q", receipt["owner"], provision.Owner)
	}
}

// TestConcurrentIdenticalWorktreeRecovery pins §9.4's serialization rule:
// two identical concurrent attempts commit exactly one generation; the loser
// gets a typed exit-3 CAS conflict, and both readers afterwards get the same
// receipt replay without any event-history counting.
func TestConcurrentIdenticalWorktreeRecovery(t *testing.T) {
	db, plan, _, _, _, _ := recoverableRemoval(t, "concurrent", WorktreeRemoveNeedsBroker)
	opts := recoveryOptions(t, db, plan.SagaID, 1)
	type committed struct {
		result *WorktreeMutationResult
		err    error
	}
	start := make(chan struct{})
	out := make(chan committed, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			attempt, err := db.RecoverWorktreeSaga(opts)
			if err != nil {
				out <- committed{nil, err}
				return
			}
			result, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryRemoved)
			out <- committed{result, err}
		}()
	}
	close(start)
	wg.Wait()
	close(out)
	winners, conflicts := 0, 0
	for got := range out {
		switch {
		case got.err == nil:
			winners++
			if got.result.Status != "removed" || got.result.RecoveryGeneration != 1 {
				t.Fatalf("concurrent winner result = %+v", got.result)
			}
		case errors.As(got.err, new(*WorktreeSagaConflict)):
			conflicts++
		default:
			t.Fatalf("concurrent loser error = %T %v", got.err, got.err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("concurrent recovery winners=%d conflicts=%d", winners, conflicts)
	}
	if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "removed" || generation != 1 {
		t.Fatalf("concurrent row = %s/%d", status, generation)
	}
	if recovering := scalarInt64(t, db, `SELECT COUNT(*) FROM events WHERE kind='worktree' AND action='recovering'`); recovering != 1 {
		t.Fatalf("concurrent recovering events = %d", recovering)
	}
	if receipts := scalarInt64(t, db, `SELECT COUNT(*) FROM receipts WHERE kind=?`, ReceiptKindWorktree); receipts != 3 {
		t.Fatalf("concurrent worktree receipts = %d, want source original + recovery + removal original", receipts)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate concurrent recovery = %v", err)
	}
	var replay *WorktreeMutationResult
	for range 2 {
		attempt, err := db.RecoverWorktreeSaga(opts)
		if err != nil {
			t.Fatal(err)
		}
		if attempt.EffectRequired || attempt.Result == nil || attempt.Result.Status != "removed" {
			t.Fatalf("post-race replay = %+v", attempt)
		}
		if replay != nil && !reflect.DeepEqual(replay, attempt.Result) {
			t.Fatalf("post-race replays differ: %+v vs %+v", replay, attempt.Result)
		}
		replay = attempt.Result
	}
}

// TestWorktreeRecoveryCrashBoundaries pins the single commit boundary: a crash
// before the finalization commit leaves the generation and advertised id
// unchanged so the same id safely repeats, and after the commit the same id is
// exact receipt replay.
func TestWorktreeRecoveryCrashBoundaries(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "crash", WorktreeProvisionNeedsBroker)
	opts := recoveryOptions(t, db, plan.SagaID, 1)
	old := worktreeRecoveryFaultHook
	worktreeRecoveryFaultHook = func(point string) error {
		if point == "recovered-before-commit" {
			return errors.New("injected crash")
		}
		return nil
	}
	t.Cleanup(func() { worktreeRecoveryFaultHook = old })

	attempt, err := db.RecoverWorktreeSaga(opts)
	if err != nil {
		t.Fatal(err)
	}
	before := storeInventory(t, db)
	if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryBound); err == nil {
		t.Fatal("faulted recovery commit succeeded")
	}
	if after := storeInventory(t, db); after.Events != before.Events || after.Receipts != before.Receipts {
		t.Fatalf("crashed commit persisted events %d->%d receipts %d->%d",
			before.Events, after.Events, before.Receipts, after.Receipts)
	}
	if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "needs-broker" || generation != 0 {
		t.Fatalf("crashed row = %s/%d", status, generation)
	}

	// The same id retries: the crash consumed neither generation nor effect.
	worktreeRecoveryFaultHook = old
	retry, err := db.RecoverWorktreeSaga(opts)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.EffectRequired || retry.RecoveryGeneration != 0 {
		t.Fatalf("identical retry after crash = %+v", retry)
	}
	result, err := db.CommitWorktreeRecovery(retry, WorktreeRecoveryBound)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "bound" || result.RecoveryGeneration != 1 {
		t.Fatalf("post-crash commit = %+v", result)
	}
	// Crash after commit, before output: the same id replays the receipt.
	postCrash, err := db.RecoverWorktreeSaga(opts)
	if err != nil || postCrash.EffectRequired || !reflect.DeepEqual(postCrash.Result, result) {
		t.Fatalf("post-commit replay = %+v result=%+v, %v; want %+v", postCrash, postCrash.Result, err, result)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate crash boundaries = %v", err)
	}
}

// TestCommitWorktreeRecoveryCASRejectsChangedAndStaleAttempts pins the
// finalization CAS surface: a tampered identity snapshot, a stale generation,
// and a replay-only attempt all refuse without writing.
func TestCommitWorktreeRecoveryCASRejectsChangedAndStaleAttempts(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "cas", WorktreeProvisionNeedsBroker)
	attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	tampered := *attempt
	tampered.Branch += "-changed"
	before := storeInventory(t, db)
	if _, err := db.CommitWorktreeRecovery(&tampered, WorktreeRecoveryNeedsBroker); !errors.As(err, new(*WorktreeSagaConflict)) {
		t.Fatalf("tampered attempt = %v", err)
	}
	if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryBound); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryBound); !errors.As(err, new(*WorktreeSagaConflict)) {
		t.Fatalf("stale repeat commit = %v; want WorktreeSagaConflict", err)
	}
	replay, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CommitWorktreeRecovery(replay, WorktreeRecoveryFailed); err == nil ||
		!strings.Contains(err.Error(), "requires an effect") {
		t.Fatalf("commit on replay attempt = %v; want effect-required refusal", err)
	}
	if after := storeInventory(t, db); after.Events != before.Events+2 || after.Receipts != before.Receipts+2 {
		t.Fatalf("CAS refusals moved events %d->%d receipts %d->%d",
			before.Events, after.Events, before.Receipts, after.Receipts)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatal(err)
	}
}

// TestCommitWorktreeRecoveryRequestIDReDerivation pins deterministic
// generation identity inside the commit path: the finalization re-derives
// the request id advertised for the generation being committed and refuses a
// mutated one without writing, so no arbitrary receipt identity can consume
// the saga's next generation (SPEC §9.4).
func TestCommitWorktreeRecoveryRequestIDReDerivation(t *testing.T) {
	db, plan, _, _, _ := recoverableProvision(t, "reqid", WorktreeProvisionNeedsBroker)
	opts := recoveryOptions(t, db, plan.SagaID, 1)
	attempt, err := db.RecoverWorktreeSaga(opts)
	if err != nil {
		t.Fatal(err)
	}
	mutated := *attempt
	mutated.RequestID = "recover-" + strings.Repeat("f", 64)
	before := storeInventory(t, db)
	if _, err := db.CommitWorktreeRecovery(&mutated, WorktreeRecoveryNeedsBroker); !errors.As(err, new(*WorktreeRecoveryConflict)) ||
		!strings.Contains(err.Error(), "advertised") {
		t.Fatalf("mutated request id commit = %v; want advertised-id WorktreeRecoveryConflict", err)
	}
	if after := storeInventory(t, db); after.Events != before.Events || after.Receipts != before.Receipts {
		t.Fatalf("mutated-id refusal moved events %d->%d receipts %d->%d",
			before.Events, after.Events, before.Receipts, after.Receipts)
	}
	if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "needs-broker" || generation != 0 {
		t.Fatalf("row after mutated-id refusal = %s/%d", status, generation)
	}
	// The advertised id still commits, and only under its own identity.
	if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker); err != nil {
		t.Fatal(err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, opts.RequestID); err != nil || receipt == nil ||
		receipt["status"] != "needs-broker" {
		t.Fatalf("advertised-id receipt = %v, %v", receipt, err)
	}
	if receipt, err := db.FindReceipt(ReceiptKindWorktree, mutated.RequestID); err != nil || receipt != nil {
		t.Fatalf("mutated-id receipt = %v, %v; want none", receipt, err)
	}
	if err := db.ValidateWorktreeSagaState(); err != nil {
		t.Fatalf("validate after advertised-id commit = %v", err)
	}
}

// TestCommitWorktreeRecoveryInvalidOutcomeRejected pins the observation enum
// and operation/state-machine refusals: nothing is written for an invalid
// outcome, and the saga stays recoverable afterwards.
func TestCommitWorktreeRecoveryInvalidOutcomeRejected(t *testing.T) {
	t.Run("provision", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "invalid", WorktreeProvisionNeedsBroker)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		for _, outcome := range []WorktreeRecoveryOutcome{"", "pending", WorktreeRecoveryRemoved,
			WorktreeRecoveryCleanupConfined} {
			result, err := db.CommitWorktreeRecovery(attempt, outcome)
			var statusErr *WorktreeRecoveryStatusError
			if result != nil || !errors.As(err, &statusErr) || statusErr.Status != outcome {
				t.Fatalf("invalid provision outcome %q = (%+v, %T %v), want WorktreeRecoveryStatusError",
					outcome, result, err, err)
			}
		}
		if status, generation, _ := sagaRow(t, db, plan.SagaID); status != "needs-broker" || generation != 0 {
			t.Fatalf("row after invalid outcomes = %s/%d", status, generation)
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryBound); err != nil {
			t.Fatalf("valid outcome after refusals = %v", err)
		}
	})
	t.Run("removal", func(t *testing.T) {
		db, plan, _, _, _, _ := recoverableRemoval(t, "invalid", WorktreeRemoveNeedsBroker)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		for _, outcome := range []WorktreeRecoveryOutcome{WorktreeRecoveryBound, WorktreeRecoveryCleanupConfined} {
			if _, err := db.CommitWorktreeRecovery(attempt, outcome); !errors.As(err, new(*WorktreeRecoveryStatusError)) {
				t.Fatalf("invalid removal outcome %q = %v", outcome, err)
			}
		}
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryRemoved); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("terminal snapshot", func(t *testing.T) {
		db, plan, _, _, _ := recoverableProvision(t, "invalid.terminal", WorktreeProvisionNeedsBroker)
		attempt, err := db.RecoverWorktreeSaga(recoveryOptions(t, db, plan.SagaID, 1))
		if err != nil {
			t.Fatal(err)
		}
		before := storeInventory(t, db)
		// A mutated attempt claiming a terminal snapshot can never commit:
		// the §9.4 transition table admits no outcome from a terminal
		// status, so terminal-to-nonterminal and reterminalizing commits
		// alike are refused before any write.
		for _, status := range []string{"bound", "failed", "removed"} {
			for _, outcome := range []WorktreeRecoveryOutcome{WorktreeRecoveryBound, WorktreeRecoveryRemoved,
				WorktreeRecoveryNeedsBroker, WorktreeRecoveryFailed, WorktreeRecoveryCleanupRequired} {
				mutated := *attempt
				mutated.Status = status
				if _, err := db.CommitWorktreeRecovery(&mutated, outcome); !errors.As(err, new(*WorktreeRecoveryStatusError)) {
					t.Fatalf("terminal snapshot %s → %s = %v; want WorktreeRecoveryStatusError", status, outcome, err)
				}
			}
		}
		if after := storeInventory(t, db); after.Events != before.Events || after.Receipts != before.Receipts {
			t.Fatalf("terminal-snapshot refusals moved events %d->%d receipts %d->%d",
				before.Events, after.Events, before.Receipts, after.Receipts)
		}
		// The nonterminal snapshot still commits through the same attempt.
		if _, err := db.CommitWorktreeRecovery(attempt, WorktreeRecoveryNeedsBroker); err != nil {
			t.Fatalf("valid outcome after terminal-snapshot refusals = %v", err)
		}
	})
}
