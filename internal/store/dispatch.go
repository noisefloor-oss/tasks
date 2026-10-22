package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// SPEC §9.3B replayable dispatch preparation and reconciliation: the
// receipt-first resume read, the authoritative live-controller read, and the
// one-lineage proof the dispatch surface composes from the underlying claim
// receipt, run receipt, worktree saga, and current controller state. Dispatch
// adds no table, receipt, cache, or shadow state; every helper here is a read
// over committed records, and every mutation still belongs to the underlying
// claim-next, run start, worktree provision, and worktree recover primitives.

// DispatchConflict is the typed §9.3B exit-3 refusal: the committed records
// do not prove that the derived claim, start, and provision request ids name
// the supplied currently live controller for the named provisioning saga's
// run. The diagnostic names only non-secret identifiers.
type DispatchConflict struct {
	Reason string
}

func (e *DispatchConflict) Error() string {
	return "dispatch lineage conflict: " + e.Reason
}

func dispatchConflictf(format string, args ...any) error {
	return &DispatchConflict{Reason: fmt.Sprintf(format, args...)}
}

// DispatchController is the authoritative current controller for one run: the
// run's owner and tag/task identity, the fencing token of the claim currently
// stored on that task, and the current attempt's number and token. It is a
// raw read of committed state — expiry, running/running, and claim-linkage
// judgments belong to the fences and projections that consume it — so a
// dispatch retry reads the current live capabilities instead of reusing the
// historical tokens a committed receipt returned (SPEC §9.3B).
type DispatchController struct {
	Owner        string
	Tag          string
	Task         string
	ClaimToken   int64
	Attempt      int64
	AttemptToken int64
}

// DispatchLiveController reads the current live claim and attempt
// capabilities for one run. A missing run, task, or current attempt is an
// operational failure; lineage and liveness conflicts are decided by the
// fences and projections that consume these values.
func (d *DB) DispatchLiveController(runID int64) (*DispatchController, error) {
	var controller DispatchController
	err := d.deferred(func(t *tx) error {
		return t.QueryRow(`SELECT r.tag,r.task_id,r.owner,r.current_attempt,
			tk.claim_token,a.attempt_token
			FROM runs r
			JOIN tasks tk ON tk.tag=r.tag AND tk.id=r.task_id
			JOIN attempts a ON a.run_id=r.run_id AND a.attempt_no=r.current_attempt
			WHERE r.run_id=?`, runID).
			Scan(&controller.Tag, &controller.Task, &controller.Owner, &controller.Attempt,
				&controller.ClaimToken, &controller.AttemptToken)
	})
	if err != nil {
		return nil, fmt.Errorf("read live controller for run %d: %w", runID, err)
	}
	return &controller, nil
}

// DispatchStartReceipt names the run one committed §9.3 start receipt proves
// for a derived dispatch start step.
type DispatchStartReceipt struct {
	RunID int64
}

// FindDispatchStartReceipt resolves the derived start request id
// receipt-first (SPEC §9.3B step 3 resume): no committed receipt returns nil
// so prepare performs run start against the live claim; a stored receipt is
// verified against the complete §9.3 start-receipt contract — the exact
// normalized request StartRun replays, then the same semantic validation
// StartRun replay enforces over the result digest, timestamps, acquisition
// ids, inventory, and event lineage — so a different task, owner, tag, or
// ttl is the common typed §8.4 collision, a re-signed altered result fails
// closed as corruption, and an identical one returns the run it proves,
// whose current live controller prepare then reads instead of the receipt's
// historical tokens. The ttl is normalized exactly as StartRun normalizes it.
func (d *DB) FindDispatchStartReceipt(requestID, tag, task, owner string, ttl int) (*DispatchStartReceipt, error) {
	if ttl <= 0 {
		ttl = DefaultClaimTTL
	}
	r := runRequest{operation: "start", requestID: requestID, tag: tag, task: task, owner: owner, ttl: ttl}
	inputDigest, err := runInputDigest(r)
	if err != nil {
		return nil, err
	}
	var found *DispatchStartReceipt
	err = d.deferred(func(t *tx) error {
		receipt, err := t.findVerifiedReceipt(ReceiptKindRun, requestID)
		if err != nil || receipt == nil {
			return err
		}
		if err := validateStartRunReceipt(t, receipt, r, inputDigest); err != nil {
			return err
		}
		runID, ok := receiptInt64(receipt["runId"])
		if !ok || runID <= 0 {
			return receiptIntegrityError(ReceiptKindRun)
		}
		found = &DispatchStartReceipt{RunID: runID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// DispatchSaga is the durable dispatch fact set of one worktree saga row.
type DispatchSaga struct {
	SagaID             int64
	RunID              int64
	Tag                string
	Task               string
	Operation          string
	Status             string
	RequestID          string
	RecoveryGeneration int64
	BindingActive      bool
}

// ProveDispatchLineage proves the §9.3B reconcile contract in one snapshot:
// the derived claim, start, and provision request ids name exactly the
// supplied currently live controller for the named provisioning saga's run.
// The verified claim receipt must be the lineage's one claim-next
// acquisition, and that acquisition must be both the run's captured
// acquisition and the live claim stored on the task; the run receipt under
// the derived start id must pass the complete §9.3 start-receipt contract —
// the same semantic validation StartRun replay enforces — and name the same
// run, tag, task, and owner; the saga row must reserve the derived provision
// request id and name that run; and the supplied claim token, attempt
// number, and attempt token must equal the authoritative live values — the
// current attempt may be a retry of the attempt that planned the saga. A
// terminal saga additionally requires its original provision receipt
// verified against the saga row by the §9.4 original-receipt contract, the
// §9.3B terminal-replay precondition. Any unproven link is the typed
// DispatchConflict; a malformed or inconsistent stored record fails closed
// as corruption. The read commits no effect: recovery authorization stays
// with the saga fence.
func (d *DB) ProveDispatchLineage(claimID, startID, provisionID string, runID, sagaID int64,
	owner string, claimToken, attempt, attemptToken int64) (*DispatchSaga, error) {
	var saga *DispatchSaga
	err := d.deferred(func(t *tx) error {
		claim, err := t.findVerifiedReceipt(ReceiptKindClaim, claimID)
		if err != nil {
			return err
		}
		if claim == nil {
			return dispatchConflictf("claim request %s has no committed receipt", claimID)
		}
		request, _, err := decodeClaimAcquisitionReceipt(claim)
		if err != nil {
			return err
		}
		// §9.3B composes exactly one claim-next lineage: the derived claim
		// id is the claim-next step's request id, so a plain claim
		// acquisition committed under it cannot prove the lineage.
		if request.operation != "claim-next" {
			return dispatchConflictf("claim request %s is not a claim-next acquisition", claimID)
		}
		acquisition, _ := claim["acquisitionId"].(string)

		var runTag, runTask, runOwner, runClaimID string
		var runCurrentAttempt int64
		err = t.QueryRow(`SELECT tag,task_id,owner,claim_acquisition_id,current_attempt
			FROM runs WHERE run_id=?`, runID).
			Scan(&runTag, &runTask, &runOwner, &runClaimID, &runCurrentAttempt)
		if errors.Is(err, sql.ErrNoRows) {
			return dispatchConflictf("run %d does not exist", runID)
		}
		if err != nil {
			return fmt.Errorf("read run %d for dispatch lineage: %w", runID, err)
		}
		if runOwner != owner || request.owner != owner || runTag != request.tag ||
			runTask != request.task || runClaimID != acquisition {
			return dispatchConflictf("claim request %s does not name run %d's controller acquisition", claimID, runID)
		}

		start, err := t.findVerifiedReceipt(ReceiptKindRun, startID)
		if err != nil {
			return err
		}
		if start == nil {
			return dispatchConflictf("start request %s has no committed receipt", startID)
		}
		startRun, startRunOK := receiptInt64(start["runId"])
		startTag, _ := start["tag"].(string)
		startTask, _ := start["task"].(string)
		startOwner, _ := start["owner"].(string)
		if start["operation"] != "start" || !startRunOK || startRun != runID ||
			startTag != runTag || startTask != runTask || startOwner != owner {
			return dispatchConflictf("start request %s does not name run %d's start", startID, runID)
		}
		// The complete §9.3 start-receipt contract — the same semantic
		// validation StartRun replay enforces — binds the deterministic
		// receipt id to the exact input vector and proves the result
		// digest, timestamps, acquisition ids, inventory, and event
		// lineage, so a re-signed altered start result cannot stand in as
		// proof of this lineage (SPEC §9.3B).
		startTTL, ttlOK := receiptInt64(start["ttlSeconds"])
		if !ttlOK {
			return receiptIntegrityError(ReceiptKindRun)
		}
		startRequest := runRequest{operation: "start", requestID: startID, tag: runTag,
			task: runTask, owner: runOwner, ttl: int(startTTL)}
		startInput, err := runInputDigest(startRequest)
		if err != nil {
			return err
		}
		if start["inputDigest"] != startInput {
			return receiptIntegrityError(ReceiptKindRun)
		}
		if err := validateStartRunReceipt(t, start, startRequest, startInput); err != nil {
			return err
		}

		task, err := t.getTask(runTag, runTask)
		if err != nil {
			return err
		}
		if task == nil {
			return fmt.Errorf("dispatch lineage task %s/%s is missing", runTag, runTask)
		}
		if task.Claim == nil || task.Claim.Owner != owner || task.Claim.Token != claimToken ||
			claimAcquisitionID(task.Claim) != acquisition {
			return dispatchConflictf("the supplied claim token is not the live claim of run %d", runID)
		}
		if runCurrentAttempt != attempt {
			return dispatchConflictf("run %d's current attempt is %d, not %d", runID, runCurrentAttempt, attempt)
		}
		var storedToken int64
		err = t.QueryRow(`SELECT attempt_token FROM attempts WHERE run_id=? AND attempt_no=?`,
			runID, attempt).Scan(&storedToken)
		if errors.Is(err, sql.ErrNoRows) {
			return dispatchConflictf("run %d has no attempt %d", runID, attempt)
		}
		if err != nil {
			return fmt.Errorf("read attempt %d of run %d for dispatch lineage: %w", attempt, runID, err)
		}
		if storedToken != attemptToken {
			return dispatchConflictf("the supplied attempt token is not the live token of run %d attempt %d", runID, attempt)
		}

		s, err := loadWorktreeSaga(t, sagaID)
		if err != nil {
			return err
		}
		if s == nil {
			return dispatchConflictf("worktree saga %d does not exist", sagaID)
		}
		if s.operation != "provision" || !s.run.Valid || s.run.Int64 != runID ||
			s.request != provisionID || s.tag != runTag || s.task != runTask {
			return dispatchConflictf("worktree saga %d does not reserve provision request %s for run %d",
				sagaID, provisionID, runID)
		}
		if isTerminalWorktreeStatus(s.status) {
			// §9.3B: a terminal replay requires the saga's original
			// provision receipt before dispatch returns a leaf brief. The
			// receipt is proven against the saga row by the §9.4
			// original-receipt contract — the same validation the
			// provision and recover primitives enforce — so an altered or
			// foreign receipt is corruption, as is a terminal row without
			// one (SPEC §9.4 verify).
			original, err := t.findVerifiedReceipt(ReceiptKindWorktree, s.request)
			if err != nil {
				return err
			}
			if original == nil {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			inputDigest, err := worktreeInputDigest(runOwner, s.request, s.run.Int64, s.force, s.operation)
			if err != nil || validateOriginalWorktreeReceipt(t, original, s, inputDigest) != nil {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
		}
		saga = &DispatchSaga{
			SagaID: s.id, RunID: s.run.Int64, Tag: s.tag, Task: s.task,
			Operation: s.operation, Status: s.status, RequestID: s.request,
			RecoveryGeneration: s.generation, BindingActive: s.active,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return saga, nil
}
