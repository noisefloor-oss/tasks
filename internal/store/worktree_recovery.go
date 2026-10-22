package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// §9.4 worktree saga recovery: the store-only attempt primitive. Recovery is
// authorized by the private saga fence alone — never a claim, attempt, lease,
// owner, or effect-path override — and is decided receipt-first through the
// common (kind="worktree", requestId) namespace. Every committed attempt
// consumes exactly one deterministic generation, writes exactly one immutable
// recovery receipt, and appends exactly one content-free worktree/recovering
// event; the sole exception is §9.4's unproved cleanup-required provision
// attempt, which performs no CAS and commits nothing. The trusted host
// performs the idempotent external effect or compensation between the read
// snapshot and the finalization CAS; this layer never holds a transaction
// across that boundary.

// WorktreeRecoveryOptions authorizes exactly one recovery attempt: the saga
// id, its private saga fence, and the deterministic request id advertised for
// the saga's current generation (SPEC §9.4).
type WorktreeRecoveryOptions struct {
	SagaID    int64
	SagaToken int64
	RequestID string
}

// WorktreeRecoveryAttempt is the committed-read handoff across the
// SQLite/external-effect boundary: only recorded identities, snapshot status,
// and the snapshot generation the finalization CAS compare-and-sets. It
// carries no claim, attempt, lease, or broker capability. RunID,
// SourceSagaID, RegistrationRoot, and BaseCommit are zero/empty when the saga
// row stores NULL. RequestID is the recovery request id this attempt commits
// under; OriginalRequestID is the saga row's own reserved request id.
// ObservedRegistrationRoot is the trusted host's effect-time-derived
// registration root for a runless legacy cleanup attempt, whose row cannot
// store one; it is empty for every other attempt and is never persisted.
type WorktreeRecoveryAttempt struct {
	SagaID                   int64
	SagaToken                int64
	SourceSagaID             int64
	RunID                    int64
	Tag                      string
	Task                     string
	Operation                string
	Status                   string
	OwnerBoundary            string
	RegistrationRoot         string
	TargetPath               string
	Branch                   string
	BaseCommit               string
	Force                    bool
	RecoveryGeneration       int64
	RequestID                string
	OriginalRequestID        string
	ObservedRegistrationRoot string
	EffectRequired           bool
	Result                   *WorktreeMutationResult
}

// WorktreeRecoveryOutcome is the trusted host's reconciled observation after
// the idempotent effect or compensation completed with no transaction open.
type WorktreeRecoveryOutcome string

const (
	WorktreeRecoveryBound           WorktreeRecoveryOutcome = "bound"
	WorktreeRecoveryRemoved         WorktreeRecoveryOutcome = "removed"
	WorktreeRecoveryNeedsBroker     WorktreeRecoveryOutcome = "needs-broker"
	WorktreeRecoveryFailed          WorktreeRecoveryOutcome = "failed"
	WorktreeRecoveryCleanupRequired WorktreeRecoveryOutcome = "cleanup-required"

	// WorktreeRecoveryCleanupConfined is the §9.4 confined-compensation
	// observation: the compensating registration write of a cleanup-required
	// provision produced the §9.5 permission-class confinement signal. Like
	// the unproved cleanup observation it commits nothing; unlike it, the
	// returned handoff also carries the non-secret broker mint bindings for
	// the still-advertised request id.
	WorktreeRecoveryCleanupConfined WorktreeRecoveryOutcome = "cleanup-confined"
)

// WorktreeRecoveryConflict reports the typed §9.4 exit-3 refusals around one
// recovery request that are not common §8.4 replay collisions: a wrong saga
// fence, a receipt-missing request against a terminal saga, or a request id
// that is not the one advertised for the saga's next recovery generation.
// The diagnostic names only non-secret identifiers.
type WorktreeRecoveryConflict struct {
	SagaID int64
	Reason string
}

func (e *WorktreeRecoveryConflict) Error() string {
	return fmt.Sprintf("worktree saga %d refused recovery: %s", e.SagaID, e.Reason)
}

// WorktreeRecoveryStatusError reports an invalid or state-machine-impossible
// trusted-host observation for one recovery attempt.
type WorktreeRecoveryStatusError struct {
	Status     WorktreeRecoveryOutcome
	SagaStatus string
}

func (e *WorktreeRecoveryStatusError) Error() string {
	return fmt.Sprintf("worktree recovery outcome %q is invalid for saga status %q", e.Status, e.SagaStatus)
}

// worktreeRecoveryFaultHook pins the finalization crash boundary in tests.
// Production keeps the no-op. No external effect is ever invoked by a hook.
var worktreeRecoveryFaultHook = func(string) error { return nil }

func recoveryAttemptFromSaga(s *worktreeSagaState) *WorktreeRecoveryAttempt {
	a := &WorktreeRecoveryAttempt{SagaID: s.id, SagaToken: s.fence,
		Tag: s.tag, Task: s.task, Operation: s.operation, Status: s.status,
		OwnerBoundary: s.boundary, TargetPath: s.path, Branch: s.branch,
		Force: s.force, RecoveryGeneration: s.generation, OriginalRequestID: s.request}
	if s.run.Valid {
		a.RunID = s.run.Int64
	}
	if s.source.Valid {
		a.SourceSagaID = s.source.Int64
	}
	if s.registration.Valid {
		a.RegistrationRoot = s.registration.String
	}
	if s.base.Valid {
		a.BaseCommit = s.base.String
	}
	return a
}

// attachHistoricalRecoveryMembers reconstructs the derived members of one
// replayed nonterminal historical result at its recorded generation: the
// then-advertised deterministic request id for the next generation — null at
// the exhausted generation — and, for needs-broker, the broker mint bindings
// over the row's immutable identity, so an exact receipt replay returns the
// historical public mutation result rather than a bare projection (SPEC §9.4
// replay contract). A runless legacy row's effect-time root is not durable,
// so its replayed prune handoff keeps the row's empty root.
func attachHistoricalRecoveryMembers(t *tx, s *worktreeSagaState, result *WorktreeMutationResult) error {
	if isTerminalWorktreeStatus(result.Status) || result.RecoveryGeneration >= MaxWorktreeRecoveryGeneration {
		return nil
	}
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return err
	}
	recoverID, err := worktreeRecoveryRequestID(familyID, s.id, result.RecoveryGeneration+1)
	if err != nil {
		return err
	}
	result.RecoverRequestID = &recoverID
	if result.Status == "needs-broker" {
		result.BrokerRequest = worktreeBrokerRequest(familyID, s, result.BaseCommit, recoverID)
	}
	return nil
}

// RecoverWorktreeSaga performs the read half of one §9.4 recovery attempt in
// one closed read transaction: it validates the saga fence, decides the
// common receipt namespace receipt-first, gates a fresh attempt on the
// nonterminal state, the recovery-generation bound, and the advertised
// deterministic request id, and snapshots only recorded identities. An exact
// recovery receipt replays its historical public result with no effect even
// when the row is now at a later generation or terminal; the returned
// snapshot is then read-only. Recovery output never carries the saga token.
func (d *DB) RecoverWorktreeSaga(o WorktreeRecoveryOptions) (*WorktreeRecoveryAttempt, error) {
	if o.SagaID <= 0 || o.SagaToken <= 0 {
		return nil, fmt.Errorf("worktree recovery authorization is invalid")
	}
	var attempt *WorktreeRecoveryAttempt
	err := d.deferred(func(t *tx) error {
		s, err := loadWorktreeSaga(t, o.SagaID)
		if err != nil {
			return err
		}
		if s == nil {
			return fmt.Errorf("worktree saga %d not found", o.SagaID)
		}
		if s.fence != o.SagaToken {
			return &WorktreeRecoveryConflict{SagaID: s.id, Reason: "saga token does not match the saga fence"}
		}
		// Receipt-first: the common (kind="worktree", requestId) namespace is
		// decided before any current-status judgment (SPEC §9.4).
		receipt, err := t.findVerifiedReceipt(ReceiptKindWorktree, o.RequestID)
		if err != nil {
			return err
		}
		attempt = recoveryAttemptFromSaga(s)
		attempt.RequestID = o.RequestID
		if receipt != nil {
			if err := validateRecoveryWorktreeReceipt(t, receipt, s, o.RequestID); err != nil {
				return err
			}
			// Historical public result; the saga token never rides a
			// recovery result (SPEC §9.4 output contract).
			result, err := worktreeResultFromReceipt(receipt, 0)
			if err != nil {
				return err
			}
			if err := attachHistoricalRecoveryMembers(t, s, result); err != nil {
				return err
			}
			attempt.Result = result
			return nil
		}
		if isTerminalWorktreeStatus(s.status) {
			return &WorktreeRecoveryConflict{SagaID: s.id,
				Reason: fmt.Sprintf("saga is terminal and request %q has no receipt", o.RequestID)}
		}
		// The generation bound is checked before the next id is derived or
		// compared, and so before any Git, broker, filesystem, or
		// compensation effect can run (SPEC §9.4).
		if s.generation >= MaxWorktreeRecoveryGeneration {
			return ErrWorktreeRecoveryExhausted
		}
		familyID, _, err := t.requireIdentity()
		if err != nil {
			return err
		}
		advertised, err := worktreeRecoveryRequestID(familyID, s.id, s.generation+1)
		if err != nil {
			return err
		}
		if o.RequestID != advertised {
			return &WorktreeRecoveryConflict{SagaID: s.id,
				Reason: "request id is not the one advertised for the next recovery generation"}
		}
		attempt.EffectRequired = true
		return nil
	})
	if err != nil {
		return nil, err
	}
	return attempt, nil
}

// validateRecoveryWorktreeReceipt verifies one exact recovery receipt for
// replay: the §8.4 field set, the deterministic generation id, the input and
// result digests, the saga/source identity, and an outcome the state machine
// allows at that generation. A receipt found under the request id that encodes
// a different normalized input — another saga, an original operation, or
// another generation — is the common typed §8.4 collision; anything
// inconsistent is corruption and fails closed.
func validateRecoveryWorktreeReceipt(t *tx, receipt map[string]any, s *worktreeSagaState, request string) error {
	if len(receipt) != len(worktreeReceiptFields) {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	for key := range receipt {
		if !worktreeReceiptFields[key] {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
	}
	sagaID, ok := receiptInt64(receipt["sagaId"])
	if !ok {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	if sagaID != s.id {
		return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: request}
	}
	generation, ok := receiptInt64(receipt["recoveryGeneration"])
	if !ok || generation > s.generation {
		// A receipt claiming a generation the row never reached is corruption.
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	status, ok := receipt["status"].(string)
	if !ok || !worktreeReceiptStatusAllowed(s.operation, status) {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return err
	}
	// A receipt found under this request id that is not this saga's
	// deterministic recovery receipt at its recorded generation — an
	// original-operation receipt, or another generation's input vector — is a
	// different normalized input: the common typed §8.4 collision.
	if want, err := worktreeRecoveryRequestID(familyID, s.id, generation); err != nil || generation <= 0 || request != want {
		return &ReplayConflict{Kind: ReceiptKindWorktree, RequestID: request}
	}
	// A terminal outcome is final, so an exact terminal receipt must be the
	// saga's current generation and status; a nonterminal one is admitted
	// only as a state-machine-authorized historical difference.
	if isTerminalWorktreeStatus(status) {
		if generation != s.generation || status != s.status {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
	} else if err := validateRecoveryReceiptHistory(t, familyID, s, generation, status); err != nil {
		return err
	}
	return validateWorktreeReceipt(t, receipt, request, s, generation, false)
}

// validateRecoveryReceiptHistory admits only §9.4's state-machine-authorized
// historical differences for one nonterminal recovery receipt by walking the
// entire chain from that receipt's generation to the row's current
// generation: every later generation's deterministic receipt must exist, be
// exactly that generation's receipt, and record a transition the recovery
// table authorizes — absorbing run/task terminalization's
// needs-broker→cleanup-required provision flip, where the row changed
// without a generation advance, as the sole divergence — and the generation
// the row rests on must be justified by its own receipt. Anything else is an
// impossible history and fails closed as corruption.
func validateRecoveryReceiptHistory(t *tx, familyID string, s *worktreeSagaState, generation int64, status string) error {
	previous := status
	// The walk starts only strictly behind the row, so generation+1 can
	// never overflow the maximum signed generation a row may hold.
	for step := generation; step < s.generation; step++ {
		committed, err := verifyWorktreeRecoveryStep(t, familyID, s, step+1)
		if err != nil {
			return err
		}
		// Run/task terminalization may flip a needs-broker provision row to
		// cleanup-required without advancing the generation, so one step out
		// of needs-broker is authorized out of the cleanup-required the flip
		// left exactly as out of needs-broker itself.
		if !worktreeRecoveryTransitionAllowed(s.operation, previous, committed) &&
			!(s.operation == "provision" && previous == "needs-broker" &&
				worktreeRecoveryTransitionAllowed(s.operation, "cleanup-required", committed)) {
			return receiptIntegrityError(ReceiptKindWorktree)
		}
		previous = committed
	}
	if previous != s.status &&
		!(s.operation == "provision" && previous == "needs-broker" && s.status == "cleanup-required") {
		return receiptIntegrityError(ReceiptKindWorktree)
	}
	return nil
}

// verifyWorktreeRecoveryStep loads and verifies one behind-generation step's
// exact recovery receipt and returns the status it recorded. The
// deterministic request id of that generation must resolve to a receipt — a
// missing one means the row advanced without the receipt every generation
// must own — whose §8.4 field set, saga identity, and recorded generation are
// exact, whose outcome the state machine allows, and which passes the same
// identity and digest verification as every other worktree receipt.
func verifyWorktreeRecoveryStep(t *tx, familyID string, s *worktreeSagaState, step int64) (string, error) {
	request, err := worktreeRecoveryRequestID(familyID, s.id, step)
	if err != nil {
		return "", err
	}
	receipt, err := t.findVerifiedReceipt(ReceiptKindWorktree, request)
	if err != nil {
		return "", err
	}
	if receipt == nil {
		// The row claims this generation without its deterministic receipt:
		// the history is missing a link.
		return "", receiptIntegrityError(ReceiptKindWorktree)
	}
	if len(receipt) != len(worktreeReceiptFields) {
		return "", receiptIntegrityError(ReceiptKindWorktree)
	}
	for key := range receipt {
		if !worktreeReceiptFields[key] {
			return "", receiptIntegrityError(ReceiptKindWorktree)
		}
	}
	if sagaID, ok := receiptInt64(receipt["sagaId"]); !ok || sagaID != s.id {
		return "", receiptIntegrityError(ReceiptKindWorktree)
	}
	if recorded, ok := receiptInt64(receipt["recoveryGeneration"]); !ok || recorded != step {
		return "", receiptIntegrityError(ReceiptKindWorktree)
	}
	status, ok := receipt["status"].(string)
	if !ok || !worktreeReceiptStatusAllowed(s.operation, status) {
		return "", receiptIntegrityError(ReceiptKindWorktree)
	}
	if err := validateWorktreeReceipt(t, receipt, request, s, step, false); err != nil {
		return "", err
	}
	return status, nil
}

func recoveryAttemptMatchesSaga(a *WorktreeRecoveryAttempt, s *worktreeSagaState) bool {
	return a != nil && s != nil && a.SagaID == s.id && a.SagaToken == s.fence &&
		a.RecoveryGeneration == s.generation && a.Status == s.status &&
		a.Operation == s.operation && a.Tag == s.tag && a.Task == s.task &&
		a.OriginalRequestID == s.request && a.OwnerBoundary == s.boundary &&
		a.TargetPath == s.path && a.Branch == s.branch && a.Force == s.force &&
		(s.run.Valid == (a.RunID != 0)) && (!s.run.Valid || s.run.Int64 == a.RunID) &&
		(s.source.Valid == (a.SourceSagaID != 0)) && (!s.source.Valid || s.source.Int64 == a.SourceSagaID) &&
		(s.registration.Valid == (a.RegistrationRoot != "")) &&
		(!s.registration.Valid || s.registration.String == a.RegistrationRoot) &&
		(s.base.Valid == (a.BaseCommit != "")) && (!s.base.Valid || s.base.String == a.BaseCommit)
}

// worktreeRecoveryTransitionAllowed is the exact §9.4 committed-recovery
// transition table: provision pending|needs-broker may commit
// needs-broker|bound|failed|cleanup-required, a cleanup-required provision
// commits failed only, and removal pending|needs-broker|cleanup-required may
// commit needs-broker|removed|failed|cleanup-required. Every terminal
// snapshot status and every other pair — including terminal-to-nonterminal —
// is refused.
func worktreeRecoveryTransitionAllowed(operation, from, to string) bool {
	head := from == "pending" || from == "needs-broker"
	switch operation {
	case "provision":
		if head {
			return to == "needs-broker" || to == "bound" || to == "failed" || to == "cleanup-required"
		}
		return from == "cleanup-required" && to == "failed"
	case "remove":
		return (head || from == "cleanup-required") &&
			(to == "needs-broker" || to == "removed" || to == "failed" || to == "cleanup-required")
	}
	return false
}

// validateWorktreeRecoveryOutcome admits only observations the §9.4 state
// machine allows for the attempt's snapshot operation and status, enforced
// exactly through the committed-recovery transition table. A cleanup-required
// provision stays cleanup-only: it can commit failed only, never binds or
// returns to needs-broker, while its unproved cleanup and confined-compensation
// observations commit nothing and are handled by the no-burn exception in
// CommitWorktreeRecovery.
func validateWorktreeRecoveryOutcome(a *WorktreeRecoveryAttempt, outcome WorktreeRecoveryOutcome) error {
	invalid := &WorktreeRecoveryStatusError{Status: outcome, SagaStatus: a.Status}
	switch {
	case outcome == WorktreeRecoveryCleanupConfined:
		// §9.4's confined-compensation observation: valid only for the
		// unproved cleanup-required provision attempt; it commits nothing.
		if a.Operation != "provision" || a.Status != "cleanup-required" {
			return invalid
		}
	case outcome == WorktreeRecoveryCleanupRequired &&
		a.Operation == "provision" && a.Status == "cleanup-required":
		// §9.4's unproved cleanup observation: the no-burn exception, which
		// commits nothing and leaves the generation unburned.
	case worktreeRecoveryTransitionAllowed(a.Operation, a.Status, string(outcome)):
		// The exact §9.4 committed-recovery transition table.
	default:
		return invalid
	}
	return nil
}

// recoveryRemovalSource loads and verifies the bound provision source of one
// linked removal for recovery. Unlike the manual-removal validation it admits
// §9.4's runless automatic cleanup rows (run NULL, auto- request id), whose
// authority is the parent's committed cleanup decision rather than a live
// run. A runless sourceless (legacy) row has no source edge at all.
func recoveryRemovalSource(t *tx, s *worktreeSagaState) (*worktreeSagaState, string, string, string, error) {
	if !s.source.Valid {
		return nil, "", "", "", nil
	}
	source, err := loadWorktreeSaga(t, s.source.Int64)
	if err != nil {
		return nil, "", "", "", err
	}
	if source == nil || source.id >= s.id || source.operation != "provision" || source.status != "bound" ||
		source.tag != s.tag || source.task != s.task || source.boundary != s.boundary ||
		source.registration != s.registration || source.path != s.path || source.branch != s.branch ||
		source.base != s.base || (s.run.Valid && (!source.run.Valid || s.run.Int64 != source.run.Int64)) {
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	// A live removal needs its active source and no removed sibling; the
	// unique indexes make anything else corruption.
	active := true
	owner, runStatus, taskStatus, err := validateBoundProvisionSource(t, source, &active)
	if err != nil {
		return nil, "", "", "", err
	}
	var removedID sql.NullInt64
	if err := t.QueryRow(`SELECT saga_id FROM worktree_sagas
		WHERE source_saga_id=? AND status='removed'`, source.id).Scan(&removedID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", "", "", err
	}
	if removedID.Valid || !source.active {
		return nil, "", "", "", receiptIntegrityError(ReceiptKindWorktree)
	}
	return source, owner, runStatus, taskStatus, nil
}

// attachCleanupCompensationBrokerRequest carries the §9.4 deterministic
// non-secret broker mint bindings on the unproved cleanup attempt's handoff:
// the remove verb over the same immutable identity, under the
// still-advertised recover request id. It persists nothing.
func attachCleanupCompensationBrokerRequest(t *tx, s *worktreeSagaState, result *WorktreeMutationResult) error {
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return err
	}
	recoverID, err := worktreeRecoveryRequestID(familyID, s.id, s.generation+1)
	if err != nil {
		return err
	}
	base := s.base.String
	result.BrokerRequest = &WorktreeBrokerRequest{FamilyID: familyID, Verb: "remove",
		RegistrationRoot: s.registration.String, OwnerBoundary: s.boundary,
		TargetPath: s.path, Branch: s.branch, BaseCommit: &base, Force: s.force,
		RecoverRequestID: recoverID}
	return nil
}

// applyLegacyPruneRegistrationRoot threads the trusted host's effect-time
// derived registration root onto one runless legacy cleanup handoff: the
// schema cannot store that root, so only the observed value the attempt
// carries can make the §9.4 prune-registration mint binding exact. Every
// other handoff — including all non-legacy ones — is unchanged, and nothing
// is persisted.
func applyLegacyPruneRegistrationRoot(s *worktreeSagaState, result *WorktreeMutationResult, observed string) {
	if observed != "" && result.BrokerRequest != nil && result.BrokerRequest.Verb == "prune-registration" &&
		s.operation == "remove" && !s.run.Valid && !s.source.Valid {
		result.BrokerRequest.RegistrationRoot = observed
	}
}

// CommitWorktreeRecovery conditionally records one observed recovery outcome.
// Finalization is a CAS on the unchanged saga fence, status, and
// recovery_generation=g: it sets the generation to g+1, writes exactly one
// immutable recovery receipt with one content-free worktree/recovering
// event, and — when the attempt terminalizes — follows with the original
// operation's terminal event and receipt in the same transaction. The sole
// exception is §9.4's unproved cleanup-required provision attempt, which
// performs no CAS, commits nothing, and returns the unchanged handoff. It
// never performs external work and never revalidates a claim, attempt, or
// lease; recovery results never carry the saga token.
func (d *DB) CommitWorktreeRecovery(a *WorktreeRecoveryAttempt, outcome WorktreeRecoveryOutcome) (*WorktreeMutationResult, error) {
	if a == nil || a.SagaID <= 0 || a.SagaToken <= 0 || a.RequestID == "" || a.OriginalRequestID == "" {
		return nil, fmt.Errorf("worktree recovery attempt is invalid")
	}
	if a.ObservedRegistrationRoot != "" && !canonicalAbsolutePath(a.ObservedRegistrationRoot) {
		return nil, fmt.Errorf("worktree recovery attempt is invalid")
	}
	if !a.EffectRequired {
		return nil, fmt.Errorf("worktree recovery attempt requires an effect")
	}
	if err := validateWorktreeRecoveryOutcome(a, outcome); err != nil {
		return nil, err
	}
	if a.RecoveryGeneration >= MaxWorktreeRecoveryGeneration {
		return nil, ErrWorktreeRecoveryExhausted
	}
	var result *WorktreeMutationResult
	err := d.immediate(func(t *tx) error {
		s, err := loadWorktreeSaga(t, a.SagaID)
		if err != nil {
			return err
		}
		if !recoveryAttemptMatchesSaga(a, s) {
			return &WorktreeSagaConflict{SagaID: a.SagaID}
		}
		// The committed request id is re-derived from the row and required
		// equal to the deterministic id advertised for the generation this
		// attempt commits: a mutated id must not commit an arbitrary
		// receipt identity under the burned generation (SPEC §9.4).
		familyID, _, err := t.requireIdentity()
		if err != nil {
			return err
		}
		advertised, err := worktreeRecoveryRequestID(familyID, s.id, s.generation+1)
		if err != nil {
			return err
		}
		if a.RequestID != advertised {
			return &WorktreeRecoveryConflict{SagaID: s.id,
				Reason: "request id is not the one advertised for the next recovery generation"}
		}
		// §9.4's no-burn exception: an unproved cleanup-required provision
		// attempt commits no transition, receipt, event, or generation and
		// returns the current handoff so the identical request may retry.
		if a.Operation == "provision" && a.Status == "cleanup-required" &&
			(outcome == WorktreeRecoveryCleanupRequired || outcome == WorktreeRecoveryCleanupConfined) {
			result, err = worktreeResultFromSaga(t, s, 0)
			if err != nil {
				return err
			}
			if outcome == WorktreeRecoveryCleanupConfined {
				if err := attachCleanupCompensationBrokerRequest(t, s, result); err != nil {
					return err
				}
			}
			return nil
		}
		owner := ""
		if s.run.Valid {
			if err := t.QueryRow(`SELECT owner FROM runs WHERE run_id=?`, s.run.Int64).Scan(&owner); err != nil {
				return err
			}
		}
		source, _, runStatus, taskStatus, err := recoveryRemovalSource(t, s)
		if err != nil {
			return err
		}
		if source == nil && !s.run.Valid {
			// A runless sourceless legacy row has no run or source edge; only
			// its task status can forbid the failed transition.
			if err := t.QueryRow(`SELECT status FROM tasks WHERE tag=? AND id=?`, s.tag, s.task).Scan(&taskStatus); err != nil {
				return err
			}
		}
		switch {
		case s.operation == "provision" && (outcome == WorktreeRecoveryBound || outcome == WorktreeRecoveryNeedsBroker):
			// A provisioning recovery rechecks only the immutable task/run
			// relationship, task status, and run status: an ineligible
			// create or confined handoff must be compensated and finalized
			// as failed or cleanup-required instead (SPEC §9.4).
			eligible, err := provisionStillEligible(t, s)
			if err != nil {
				return err
			}
			if !eligible {
				return &WorktreeProvisionCompensationRequired{SagaID: s.id}
			}
		case outcome == WorktreeRecoveryFailed && s.operation == "remove" &&
			(taskStatus == "done" || source != nil && runStatus != "running" && runStatus != "retryable"):
			// The failed removal transition is forbidden while its source run
			// is terminal or its task remains done; a runless sourceless
			// legacy row has no source run, so only its task status gates.
			return &WorktreeRemovalFailedForbidden{SagaID: s.id}
		}
		at := nowUTC()
		next := s.generation + 1
		active := s.operation == "provision" && outcome == WorktreeRecoveryBound
		updated, err := t.Exec(`UPDATE worktree_sagas SET status=?, recovery_generation=?, binding_active=?, updated_at=?
			WHERE saga_id=? AND saga_fence=? AND status=? AND recovery_generation=? AND operation=?
			AND ifnull(run_id,0)=? AND ifnull(source_saga_id,0)=? AND tag=? AND task_id=? AND request_id=?
			AND owner_boundary=? AND ifnull(registration_root,'')=? AND target_path=? AND branch=?
			AND ifnull(base_commit,'')=? AND force=?`,
			string(outcome), next, active, at, s.id, s.fence, s.status, s.generation, s.operation,
			s.run.Int64, s.source.Int64, s.tag, s.task, s.request, s.boundary,
			s.registration.String, s.path, s.branch, s.base.String, s.force)
		if err != nil {
			return fmt.Errorf("commit worktree recovery saga %d: %w", s.id, err)
		}
		if n, err := updated.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return &WorktreeSagaConflict{SagaID: s.id}
		}
		s.status, s.generation, s.active, s.updated = string(outcome), next, active, at
		if outcome == WorktreeRecoveryRemoved && source != nil {
			deactivated, err := t.Exec(`UPDATE worktree_sagas SET binding_active=0
				WHERE saga_id=? AND saga_fence=? AND operation='provision' AND status='bound'
				AND binding_active=1 AND run_id=? AND source_saga_id IS NULL AND tag=? AND task_id=?
				AND owner_boundary=? AND registration_root=? AND target_path=? AND branch=? AND base_commit=?
				AND NOT EXISTS(SELECT 1 FROM worktree_sagas child WHERE child.source_saga_id=?
					AND child.status='removed' AND child.saga_id<>?)`,
				source.id, source.fence, source.run.Int64, source.tag, source.task,
				source.boundary, source.registration.String, source.path, source.branch,
				source.base.String, source.id, s.id)
			if err != nil {
				return fmt.Errorf("deactivate worktree provision saga %d: %w", source.id, err)
			}
			if n, err := deactivated.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return &WorktreeSagaConflict{SagaID: s.id}
			}
			source.active = false
		}
		if outcome == WorktreeRecoveryRemoved && source == nil && !s.run.Valid {
			// Successful legacy cleanup clears the retained legacy binding in
			// this same finalization transaction (SPEC §9.4 schema v5).
			cleared, err := t.Exec(`UPDATE tasks SET wt_boundary=NULL, wt_path=NULL, wt_branch=NULL,
				wt_created_at=NULL, wt_keep=NULL, wt_extra=NULL
				WHERE tag=? AND id=? AND wt_boundary=? AND wt_path=? AND wt_branch=? AND wt_created_at=?`,
				s.tag, s.task, s.boundary, s.path, s.branch, s.created)
			if err != nil {
				return fmt.Errorf("clear legacy worktree binding for saga %d: %w", s.id, err)
			}
			if n, err := cleared.RowsAffected(); err != nil || n != 1 {
				if err != nil {
					return err
				}
				return receiptIntegrityError(ReceiptKindWorktree)
			}
		}
		terminal := isTerminalWorktreeStatus(s.status)
		if err := writeWorktreeRecoveryReceipt(t, s, owner, a.RequestID, s.status, at, s.generation, terminal); err != nil {
			return err
		}
		if terminal {
			// The original operation's receipt is written if absent; a
			// previously nonterminal row cannot already hold one.
			existing, err := t.findVerifiedReceipt(ReceiptKindWorktree, s.request)
			if err != nil {
				return err
			}
			if existing != nil {
				return receiptIntegrityError(ReceiptKindWorktree)
			}
			if err := writeOriginalWorktreeReceipt(t, s, owner, at); err != nil {
				return err
			}
		}
		if err := worktreeRecoveryFaultHook("recovered-before-commit"); err != nil {
			return err
		}
		result, err = worktreeResultFromSaga(t, s, 0)
		if err != nil {
			return err
		}
		applyLegacyPruneRegistrationRoot(s, result, a.ObservedRegistrationRoot)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// writeWorktreeRecoveryReceipt writes one committed recovery attempt's
// immutable receipt and its single covering content-free worktree/recovering
// event. The receipt's input digest is the §9.4 recovery shape over exactly
// force, operation "recover", the committed generation, the request id,
// runId 0, and the saga id; its event sequence range covers exactly the one
// recovering event. terminalFollows records that the same transaction also
// appends the original terminal event and receipt, so both §8.5 inventories
// are computed from the final transaction state (SPEC §8.5/§9.4).
func writeWorktreeRecoveryReceipt(t *tx, s *worktreeSagaState, owner, requestID, status, committedAt string, generation int64, terminalFollows bool) error {
	familyID, _, err := t.requireIdentity()
	if err != nil {
		return err
	}
	inputDigest, err := canonicalDigest(map[string]any{
		"force": s.force, "operation": "recover", "recoveryGeneration": generation,
		"requestId": requestID, "runId": int64(0), "sagaId": s.id,
	})
	if err != nil {
		return err
	}
	receiptID := worktreeReceiptID(requestID, inputDigest)
	active := worktreeReceiptActive(s, status)
	resultDigest, err := worktreeResultDigest(s, status, active, committedAt, generation)
	if err != nil {
		return err
	}
	inv, err := countClassInventory(t)
	if err != nil {
		return err
	}
	following := 1
	if terminalFollows {
		following = 2
	}
	for range following {
		if inv.Events < MaxRetainedEvents {
			inv.Events++
		}
		inv.Receipts++
	}
	var seq int64
	if err := t.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='events'`).Scan(&seq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read worktree recovery event sequence: %w", err)
	}
	seq++
	receipt := map[string]any{
		"schemaVersion": int64(ReceiptSchemaVersion), "kind": ReceiptKindWorktree,
		"receiptId": receiptID, "familyId": familyID, "requestId": requestID,
		"inputDigest": inputDigest, "resultDigest": resultDigest,
		"operation": s.operation, "sagaId": s.id, "sourceSagaId": nullableInt64(s.source),
		"tag": s.tag, "task": s.task, "runId": nullableInt64(s.run), "owner": owner,
		"status": status, "active": active, "ownerBoundary": s.boundary,
		"path": s.path, "branch": s.branch, "baseCommit": nullableString(s.base),
		"force": s.force, "recoveryGeneration": generation,
		"inventory": inventoryRecord(inv), "eventSeq": map[string]any{"first": seq, "last": seq},
		"committedAt": committedAt,
	}
	digest, err := receiptDigest(receipt)
	if err != nil {
		return err
	}
	if err := t.appendEvent(Event{Task: s.task, Tag: s.tag, Kind: ReceiptKindWorktree,
		Action: "recovering", Owner: owner, At: committedAt,
		Extra: worktreeEventExtra(s.id, receiptID, digest)}); err != nil {
		return err
	}
	var actual int64
	if err := t.QueryRow(`SELECT last_insert_rowid()`).Scan(&actual); err != nil || actual != seq {
		if err != nil {
			return err
		}
		return fmt.Errorf("worktree recovery event sequence changed from %d to %d", seq, actual)
	}
	_, err = t.insertReceipt(ReceiptKindWorktree, receiptID, requestID, receipt, committedAt)
	return err
}
