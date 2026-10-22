package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// SPEC §9.3A confined leaf handoff: the controller projection and token-free
// leaf brief a machine-oriented orchestration surface renders from one §9.4
// worktree saga. Both are reads: they create no receipt or authority, never
// launch a provider or process, and never persist an expiry judgment.

// MaxWorktreeRecoveryGeneration is SQLite's maximum signed integer recovery
// generation (SPEC §9.4). A nonterminal saga at this generation has no next
// recovery id, so rendering its controller projection fails closed instead of
// advertising a null or wrapped id (SPEC §9.3A).
const MaxWorktreeRecoveryGeneration = 9223372036854775807

// LeafBriefSchemaVersion is the §9.3A leaf brief schema version.
const LeafBriefSchemaVersion = 1

// ErrWorktreeRecoveryExhausted reports the §9.3A fail-closed exhaustion
// refusal: a nonterminal saga at the maximum recovery generation has no next
// id, so no controller projection may be rendered for it.
var ErrWorktreeRecoveryExhausted = errors.New("worktree recovery generation exhausted")

// HandoffClaim is the claim member of the controller projection: exactly
// token, expiresAt, and revision, in that order. Revision is the task revision
// recorded by the claim's acquisition receipt, not the task's current
// revision; the two are compared for leaf eligibility.
type HandoffClaim struct {
	Token     int64  `json:"token"`
	ExpiresAt string `json:"expiresAt"`
	Revision  int    `json:"revision"`
}

// HandoffRun is the run member of the controller projection: exactly runId,
// attempt, leaseGeneration, attemptToken, and expiresAt, in that order.
type HandoffRun struct {
	RunID           int64  `json:"runId"`
	Attempt         int64  `json:"attempt"`
	LeaseGeneration int64  `json:"leaseGeneration"`
	AttemptToken    int64  `json:"attemptToken"`
	ExpiresAt       string `json:"expiresAt"`
}

// HandoffBrokerRequest is the exact §9.4 non-secret broker mint input,
// rendered inside the controller projection only for a stored needs-broker
// status. It omits uid, gid, expiry, authentication, and every claim/run
// capability.
type HandoffBrokerRequest struct {
	FamilyID         string  `json:"familyId"`
	Verb             string  `json:"verb"`
	RegistrationRoot string  `json:"registrationRoot"`
	OwnerBoundary    string  `json:"ownerBoundary"`
	TargetPath       string  `json:"targetPath"`
	Branch           string  `json:"branch"`
	BaseCommit       *string `json:"baseCommit"`
	Force            bool    `json:"force"`
	RecoverRequestID string  `json:"recoverRequestId"`
}

// HandoffWorkspace is the workspace member of the controller projection:
// exactly sagaId, sagaToken, status, path, ownerBoundary, branch, baseCommit,
// recoveryGeneration, recoverRequestId, and brokerRequest, in that order. The
// path and boundary are the absolute host identities stored on the saga; this
// is controller-only material and never reaches a leaf brief.
type HandoffWorkspace struct {
	SagaID             int64                 `json:"sagaId"`
	SagaToken          int64                 `json:"sagaToken"`
	Status             string                `json:"status"`
	Path               string                `json:"path"`
	OwnerBoundary      string                `json:"ownerBoundary"`
	Branch             string                `json:"branch"`
	BaseCommit         *string               `json:"baseCommit"`
	RecoveryGeneration int64                 `json:"recoveryGeneration"`
	RecoverRequestID   *string               `json:"recoverRequestId"`
	BrokerRequest      *HandoffBrokerRequest `json:"brokerRequest"`
}

// ControllerProjection is the exact §9.3A controller projection: exactly
// owner, claim, run, and workspace, in that order. The owner is read from the
// referenced run; no owner or attempt lineage is stored on the saga.
type ControllerProjection struct {
	Owner     string           `json:"owner"`
	Claim     HandoffClaim     `json:"claim"`
	Run       HandoffRun       `json:"run"`
	Workspace HandoffWorkspace `json:"workspace"`
}

// LeafBriefTask is the bounded task context of a leaf brief: exactly id,
// title, description, details, status, priority, milestone, dependencies,
// subtasks, and updatedAt, in that order — the show --json values and
// empty-array rules minus revision and claim.
type LeafBriefTask struct {
	ID           string          `json:"id"`
	Title        string          `json:"title"`
	Description  string          `json:"description"`
	Details      string          `json:"details"`
	Status       string          `json:"status"`
	Priority     string          `json:"priority"`
	Milestone    string          `json:"milestone"`
	Dependencies []string        `json:"dependencies"`
	Subtasks     json.RawMessage `json:"subtasks"`
	UpdatedAt    string          `json:"updatedAt"`
}

// LeafBriefRun names exactly runId and attempt.
type LeafBriefRun struct {
	RunID   int64 `json:"runId"`
	Attempt int64 `json:"attempt"`
}

// LeafBriefWorkspace carries exactly path, branch, and baseCommit. The path is
// the literal "." because the host starts the leaf with the registered
// workspace as its current directory; no absolute host path or owner boundary
// is ever exposed here.
type LeafBriefWorkspace struct {
	Path       string `json:"path"`
	Branch     string `json:"branch"`
	BaseCommit string `json:"baseCommit"`
}

// LeafBrief is the token-free §9.3A leaf brief: exactly schemaVersion, tag,
// task, run, and workspace, in that order. It contains no owner, revision,
// token, lease, acquisition, request or receipt id, saga id or fence, broker
// request or capability, family id, registration root, common-directory path,
// provider credential, or other task record.
type LeafBrief struct {
	SchemaVersion int                `json:"schemaVersion"`
	Tag           string             `json:"tag"`
	Task          LeafBriefTask      `json:"task"`
	Run           LeafBriefRun       `json:"run"`
	Workspace     LeafBriefWorkspace `json:"workspace"`
}

// HandoffConflict is the typed §9.3A refusal (exit 3, empty stdout): the
// selected saga's durable state does not yield a safe handoff, so a caller
// must reconcile durable state rather than hand an uncertain workspace to a
// leaf. The diagnostic names only non-secret identifiers.
type HandoffConflict struct {
	SagaID int64
	Reason string
}

func (e *HandoffConflict) Error() string {
	return fmt.Sprintf("worktree saga %d does not yield a safe handoff: %s", e.SagaID, e.Reason)
}

// handoffConflictf builds the typed refusal for sagaID.
func handoffConflictf(sagaID int64, format string, args ...any) error {
	return &HandoffConflict{SagaID: sagaID, Reason: fmt.Sprintf(format, args...)}
}

// leafBriefPriority applies the SPEC §1 absent-priority rule so the brief's
// task renders the same values as show --json.
func leafBriefPriority(p string) string {
	if p == "" {
		return "medium"
	}
	return p
}

// WorktreeHandoff renders the §9.3A controller projection for one §9.4
// worktree saga and, when every leaf-eligibility rule holds, the token-free
// leaf brief. One deferred transaction observes a single committed snapshot
// and one clock reading judges every expiry.
//
// The projection is renderable only while the saga belongs to the current
// live controller (SPEC §9.4: returning a saga token requires a currently
// live controller for the same run): the saga must name an existing run with
// its own tag and task, the task's stored claim must still be the run's
// captured acquisition (matching acquisition id and token) and be live —
// with the stored claim expiry parsed explicitly, so a malformed value is a
// refusal, never live — the run/attempt pair must be exactly
// running/running, and the current attempt's lease must be unexpired. The
// verified claim acquisition receipt is additionally bound to the controller
// identity it proves: its tag and task must name the saga's, and its owner
// and acquisition time must match the run's owner and the live claim's
// preserved acquisition. This gates every saga status — pending,
// needs-broker, cleanup-required, bound, failed, and removed alike — so no
// capability is ever disclosed from a stale, expired, terminal, or
// mis-attributed controller; the current attempt may differ from the one
// that planned the saga. A mismatch is a typed conflict. A nonterminal saga
// at the maximum recovery generation refuses fail-closed with
// ErrWorktreeRecoveryExhausted before any id is derived.
func (d *DB) WorktreeHandoff(sagaID int64) (*ControllerProjection, *LeafBrief, error) {
	var projection *ControllerProjection
	var brief *LeafBrief
	err := d.deferred(func(t *tx) error {
		now := time.Now().UTC()
		s, err := loadWorktreeSaga(t, sagaID)
		if err != nil {
			return err
		}
		if s == nil {
			return fmt.Errorf("worktree saga %d not found", sagaID)
		}
		if !isTerminalWorktreeStatus(s.status) && s.generation >= MaxWorktreeRecoveryGeneration {
			return ErrWorktreeRecoveryExhausted
		}
		projection, brief, err = renderWorktreeHandoff(t, s, now)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	return projection, brief, nil
}

// loadWorktreeSaga reads one saga row, or nil when the id is absent.
func loadWorktreeSaga(t *tx, sagaID int64) (*worktreeSagaState, error) {
	s, err := scanWorktreeSaga(t.QueryRow(`SELECT `+worktreeSagaColumns+`
		FROM worktree_sagas WHERE saga_id=?`, sagaID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read worktree saga %d: %w", sagaID, err)
	}
	return s, nil
}

// renderWorktreeHandoff builds both projections from the loaded saga inside
// the caller's snapshot.
func renderWorktreeHandoff(t *tx, s *worktreeSagaState, now time.Time) (*ControllerProjection, *LeafBrief, error) {
	// The projection's owner is read from the referenced run; a saga without
	// one has no controller to project.
	if !s.run.Valid {
		return nil, nil, handoffConflictf(s.id, "saga names no run")
	}
	var runTag, runTask, runOwner, runClaimID string
	var runClaimToken, runCurrentAttempt int64
	var runStatus string
	err := t.QueryRow(`SELECT tag,task_id,owner,claim_acquisition_id,claim_token,status,current_attempt
		FROM runs WHERE run_id=?`, s.run.Int64).
		Scan(&runTag, &runTask, &runOwner, &runClaimID, &runClaimToken, &runStatus, &runCurrentAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, handoffConflictf(s.id, "referenced run %d is missing", s.run.Int64)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read run %d for worktree saga %d: %w", s.run.Int64, s.id, err)
	}
	if runTag != s.tag || runTask != s.task {
		return nil, nil, handoffConflictf(s.id, "saga does not name run %d's tag and task", s.run.Int64)
	}

	task, err := t.getTask(s.tag, s.task)
	if err != nil {
		return nil, nil, err
	}
	if task == nil {
		return nil, nil, fmt.Errorf("worktree saga %d names missing task %s/%s", s.id, s.tag, s.task)
	}
	// The claim members must be the run's own acquisition: a task whose claim
	// was released and reacquired carries a different controller's capability.
	if task.Claim == nil || task.Claim.Token != runClaimToken || claimAcquisitionID(task.Claim) != runClaimID {
		return nil, nil, handoffConflictf(s.id, "task claim does not match run %d's captured acquisition", s.run.Int64)
	}
	acquisition, err := loadClaimAcquisition(t, runClaimID)
	if err != nil {
		return nil, nil, err
	}
	// The verified acquisition record must prove this controller: the saga's
	// tag and task, the run's owner, and the live claim's preserved
	// acquisition time. A digest-consistent receipt naming any other
	// controller identity is a linkage refusal, never a projection source.
	if acquisition.request.tag != s.tag || acquisition.request.task != s.task {
		return nil, nil, handoffConflictf(s.id, "claim acquisition record does not name the saga's tag and task")
	}
	if acquisition.request.owner != runOwner || acquisition.request.owner != task.Claim.Owner {
		return nil, nil, handoffConflictf(s.id, "claim acquisition owner does not match the run controller")
	}
	if acquisition.acquiredAt != task.Claim.AcquiredAt {
		return nil, nil, handoffConflictf(s.id, "claim acquisition time does not match the live claim")
	}

	var attemptToken, attemptGeneration int64
	var attemptStatus, attemptExpires string
	err = t.QueryRow(`SELECT attempt_token,lease_generation,status,expires_at FROM attempts
		WHERE run_id=? AND attempt_no=?`, s.run.Int64, runCurrentAttempt).
		Scan(&attemptToken, &attemptGeneration, &attemptStatus, &attemptExpires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, handoffConflictf(s.id, "run %d has no current attempt", s.run.Int64)
	}
	// The claim expiry is parsed explicitly: a malformed stored expiry is a
	// refusal, never ClaimExpired's malformed-as-live reading.
	claimExpiry, err := time.Parse(time.RFC3339, task.Claim.ExpiresAt)
	if err != nil || !now.Before(claimExpiry) {
		return nil, nil, handoffConflictf(s.id, "claim is not live")
	}
	// §9.4: returning a saga token requires a currently live controller for
	// the same run, so every projection member — claim and attempt
	// capabilities included — is disclosed only while the claim is the run's
	// own unexpired acquisition, the run/attempt pair is exactly
	// running/running, and the current attempt's lease is unexpired. This
	// gates every saga status, not only a bound provision; the current
	// attempt may differ from the one that planned the saga.
	if runStatus != "running" || attemptStatus != "running" {
		return nil, nil, handoffConflictf(s.id, "run/attempt pair is %s/%s", runStatus, attemptStatus)
	}
	lease, err := time.Parse(time.RFC3339, attemptExpires)
	if err != nil || !now.Before(lease) {
		return nil, nil, handoffConflictf(s.id, "attempt lease is not live")
	}

	familyID, _, err := t.requireIdentity()
	if err != nil {
		return nil, nil, err
	}
	var recoverRequestID *string
	if !isTerminalWorktreeStatus(s.status) {
		next, err := worktreeRecoveryRequestID(familyID, s.id, s.generation+1)
		if err != nil {
			return nil, nil, err
		}
		recoverRequestID = &next
	}
	var brokerRequest *HandoffBrokerRequest
	if s.status == "needs-broker" {
		request := &HandoffBrokerRequest{
			FamilyID:         familyID,
			Verb:             "create",
			RegistrationRoot: s.registration.String,
			OwnerBoundary:    s.boundary,
			TargetPath:       s.path,
			Branch:           s.branch,
			BaseCommit:       nullableStringPointer(s.base),
			Force:            s.force,
			RecoverRequestID: *recoverRequestID,
		}
		if s.operation == "remove" {
			request.Verb = "remove"
		}
		brokerRequest = request
	}

	projection := &ControllerProjection{
		Owner: runOwner,
		Claim: HandoffClaim{
			Token:     task.Claim.Token,
			ExpiresAt: task.Claim.ExpiresAt,
			Revision:  int(acquisition.revision),
		},
		Run: HandoffRun{
			RunID:           s.run.Int64,
			Attempt:         runCurrentAttempt,
			LeaseGeneration: attemptGeneration,
			AttemptToken:    attemptToken,
			ExpiresAt:       attemptExpires,
		},
		Workspace: HandoffWorkspace{
			SagaID:             s.id,
			SagaToken:          s.fence,
			Status:             s.status,
			Path:               s.path,
			OwnerBoundary:      s.boundary,
			Branch:             s.branch,
			BaseCommit:         nullableStringPointer(s.base),
			RecoveryGeneration: s.generation,
			RecoverRequestID:   recoverRequestID,
			BrokerRequest:      brokerRequest,
		},
	}
	// The v5 CHECK constraints make binding_active=1 exactly an active bound
	// provision: the sole leaf-brief candidate (SPEC §9.3A operation=provision,
	// status=bound, binding_active=1).
	if s.active {
		if err := s.leafEligibility(t, task, int(acquisition.revision)); err != nil {
			return nil, nil, err
		}
		brief, err := s.leafBrief(task, runCurrentAttempt)
		if err != nil {
			return nil, nil, err
		}
		return projection, brief, nil
	}
	if s.status == "bound" {
		// A bound row whose linked removal committed is no longer the active
		// binding; a caller must reconcile rather than treat the removed
		// workspace as handoff material.
		return nil, nil, handoffConflictf(s.id, "bound provision is no longer the active binding")
	}
	// A needs-broker or other non-bound projection is recovery material only
	// and is never accompanied by a leaf brief (SPEC §9.3A).
	return projection, nil, nil
}

// leafEligibility applies the §9.3A rules that go beyond the universal
// live-controller disclosure gate: an active bound provision yields a brief
// only while the task retains the revision recorded by its claim acquisition
// and no saga for the tag/task holds live pending, needs-broker, or
// cleanup-required status. Any mismatch is a typed refusal without a brief.
func (s *worktreeSagaState) leafEligibility(t *tx, task *Task, acquiredRevision int) error {
	if task.Revision != acquiredRevision {
		return handoffConflictf(s.id, "task revision %d drifted from claim acquisition revision %d", task.Revision, acquiredRevision)
	}
	var live int
	if err := t.QueryRow(`SELECT COUNT(*) FROM worktree_sagas
		WHERE tag=? AND task_id=? AND status IN ('pending','needs-broker','cleanup-required')`, s.tag, s.task).Scan(&live); err != nil {
		return fmt.Errorf("read live worktree sagas for %s/%s: %w", s.tag, s.task, err)
	}
	if live > 0 {
		return handoffConflictf(s.id, "a live worktree saga exists for task %s/%s", s.tag, s.task)
	}
	return nil
}

// leafBrief renders the token-free brief for an eligible bound provision.
func (s *worktreeSagaState) leafBrief(task *Task, attempt int64) (*LeafBrief, error) {
	if !s.base.Valid {
		return nil, handoffConflictf(s.id, "bound provision has no base commit")
	}
	subtasks := task.Subtasks
	if len(subtasks) == 0 {
		subtasks = json.RawMessage(`[]`)
	}
	dependencies := task.Dependencies
	if dependencies == nil {
		dependencies = []string{}
	}
	return &LeafBrief{
		SchemaVersion: LeafBriefSchemaVersion,
		Tag:           s.tag,
		Task: LeafBriefTask{
			ID:           task.ID,
			Title:        task.Title,
			Description:  task.Description,
			Details:      task.Details,
			Status:       task.Status,
			Priority:     leafBriefPriority(task.Priority),
			Milestone:    task.Milestone,
			Dependencies: dependencies,
			Subtasks:     subtasks,
			UpdatedAt:    task.UpdatedAt,
		},
		Run: LeafBriefRun{RunID: s.run.Int64, Attempt: attempt},
		Workspace: LeafBriefWorkspace{
			Path:       ".",
			Branch:     s.branch,
			BaseCommit: s.base.String,
		},
	}, nil
}

// claimAcquisitionRecord is the decoded, semantically validated fact set of
// one claim acquisition receipt: the reconstructed request (operation,
// request id, tag, task, owner, ttl), the recorded task revision, and the
// acquisition time the receipt proves.
type claimAcquisitionRecord struct {
	request    claimRequest
	revision   int64
	acquiredAt string
}

// loadClaimAcquisition loads the decoded facts of the claim acquisition
// receipt naming acquisitionID. Exactly one such receipt may exist; it must
// pass the stored-envelope verification and the claim-specific semantic
// validator (exact catalog, deterministic receipt/input/result identities and
// digests, timestamp/TTL/acquisition grammar, revision >= 1). A missing,
// duplicated, or invalid record fails closed rather than guessing from
// current task state; binding the returned facts to the controller they must
// prove is the caller's linkage check.
func loadClaimAcquisition(t *tx, acquisitionID string) (*claimAcquisitionRecord, error) {
	rows, err := t.Query(`SELECT request_id, body FROM receipts WHERE kind=?`, ReceiptKindClaim)
	if err != nil {
		return nil, fmt.Errorf("read claim receipts: %w", err)
	}
	var matches []string
	for rows.Next() {
		var requestID, body string
		if err := rows.Scan(&requestID, &body); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read claim receipt: %w", err)
		}
		decoded, err := decodeJSONObject([]byte(body))
		if err != nil {
			rows.Close()
			return nil, receiptIntegrityError(ReceiptKindClaim)
		}
		if id, ok := decoded["acquisitionId"].(string); ok && id == acquisitionID {
			matches = append(matches, requestID)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("read claim receipts: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("read claim receipts: %w", err)
	}
	switch len(matches) {
	case 1:
	case 0:
		return nil, fmt.Errorf("claim acquisition %s has no recorded receipt", acquisitionID)
	default:
		return nil, fmt.Errorf("claim acquisition %s is recorded by %d receipts", acquisitionID, len(matches))
	}
	// The verified loader re-reads and checks the stored envelope — canonical
	// body, row and embedded digests, family, kind, request id — before the
	// semantic validator proves the deterministic contract.
	receipt, err := t.findVerifiedReceipt(ReceiptKindClaim, matches[0])
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, fmt.Errorf("claim acquisition %s has no recorded receipt", acquisitionID)
	}
	if id, _ := receipt["acquisitionId"].(string); id != acquisitionID {
		return nil, receiptIntegrityError(ReceiptKindClaim)
	}
	request, revision, err := decodeClaimAcquisitionReceipt(receipt)
	if err != nil {
		return nil, err
	}
	acquiredAt, _ := receipt["acquiredAt"].(string)
	return &claimAcquisitionRecord{request: request, revision: revision, acquiredAt: acquiredAt}, nil
}
