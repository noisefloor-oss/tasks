package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// SPEC §9.3B replayable dispatch preparation and reconciliation: JSON-only
// machine surfaces for a trusted orchestrator. dispatch composes exactly one
// claim-next, one run start, and one worktree provision or recover lineage
// from one stable root request id and writes no dispatch table, cache,
// journal, or receipt: replay and crash recovery are derived from the
// underlying claim receipt, run receipt, worktree saga, and worktree receipts
// alone. A successful result is possible only for an active bound provision
// satisfying every §9.3A eligibility rule; a nonterminal workspace is the
// typed blocked controller handoff; operational failures and stale or
// mismatched capabilities keep the primitive exit 1 or exit 3 result with
// empty stdout.

// dispatchStepDomain prefixes the §9.3B step request id derivation: the
// literal bytes "noise-tasks-dispatch-step-v1" followed by one NUL.
var dispatchStepDomain = []byte("noise-tasks-dispatch-step-v1\x00")

// dispatchStepIDs carries the three derived §9.3B step request ids in their
// exact output order.
type dispatchStepIDs struct {
	Claim     string `json:"claim"`
	Start     string `json:"start"`
	Provision string `json:"provision"`
}

// dispatchStepRequestIDs derives the exact §9.3B step request ids
// dp1.<step>.<hex64> for the literal steps claim, start, and provision:
// hex64 is the lowercase SHA-256 of the domain prefix plus canonical JSON
// {"rootRequestId":"<root>","step":"<step>"}. The two-member object is
// emitted in canonical sorted key order, and the local-key grammar of the
// root excludes every byte JSON would escape, so the encoding is exact.
func dispatchStepRequestIDs(root string) (dispatchStepIDs, error) {
	var ids dispatchStepIDs
	for step, target := range map[string]*string{
		"claim": &ids.Claim, "start": &ids.Start, "provision": &ids.Provision,
	} {
		payload, err := json.Marshal(struct {
			RootRequestID string `json:"rootRequestId"`
			Step          string `json:"step"`
		}{root, step})
		if err != nil {
			return dispatchStepIDs{}, fmt.Errorf("derive dispatch %s request id: %w", step, err)
		}
		sum := sha256.Sum256(append(dispatchStepDomain, payload...))
		*target = "dp1." + step + "." + hex.EncodeToString(sum[:])
	}
	return ids, nil
}

// dispatchSuccess is the exact §9.3B success object: rootRequestId, task,
// stepRequestIds, controller, and leaf, in that order. controller and leaf
// are the exact §9.3A projections; dispatch adds no member to either.
type dispatchSuccess struct {
	RootRequestID  string                      `json:"rootRequestId"`
	Task           string                      `json:"task"`
	StepRequestIDs dispatchStepIDs             `json:"stepRequestIds"`
	Controller     *store.ControllerProjection `json:"controller"`
	Leaf           *store.LeafBrief            `json:"leaf"`
}

// dispatchBlocked is the exact §9.3B blocked controller result: kind,
// reason, rootRequestId, task, stepRequestIds, and controller, in that
// order. kind is dispatch-blocked; reason is needs-broker or needs-recovery.
type dispatchBlocked struct {
	Kind           string                      `json:"kind"`
	Reason         string                      `json:"reason"`
	RootRequestID  string                      `json:"rootRequestId"`
	Task           string                      `json:"task"`
	StepRequestIDs dispatchStepIDs             `json:"stepRequestIds"`
	Controller     *store.ControllerProjection `json:"controller"`
}

func cmdDispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks dispatch <prepare|reconcile> [options]")
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, "usage: noise-tasks dispatch <prepare|reconcile> [options]")
		return 0
	case "prepare":
		return cmdDispatchPrepare(args[1:], stdout, stderr)
	case "reconcile":
		return cmdDispatchReconcile(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "noise-tasks dispatch: unknown subcommand %q\n", args[0])
		return 2
	}
}

// requireDispatchJSON enforces the §9.3B machine-surface rule: --json is
// required and human output is not defined, so omitting it is a usage error.
func requireDispatchJSON(stderr io.Writer, command string, jsonOut bool) bool {
	if !jsonOut {
		fmt.Fprintf(stderr, "noise-tasks dispatch %s: --json is required for this machine surface\n", command)
		return false
	}
	return true
}

func cmdDispatchPrepare(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("dispatch prepare", stderr, &cf)
	ownerF := fs.String("owner", "", "claim and run owner identity")
	claimTTLF := fs.Int("claim-ttl", 0, "claim time-to-live in seconds")
	runTTLF := fs.Int("run-ttl", 0, "attempt lease time-to-live in seconds")
	milestoneF := fs.String("milestone", "", "restrict candidates to this milestone")
	assignmentF := fs.String("assignment", "", "restrict candidates to tasks assigned to this owner")
	rootF := fs.String("request-id", "", "root request id derivation seed")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks dispatch prepare --owner <owner> --request-id <root> [--claim-ttl <seconds>] [--run-ttl <seconds>] [--milestone <m>] [--assignment <owner>] --json")
		return 2
	}
	if cf.file != "" {
		fmt.Fprintln(stderr, "noise-tasks dispatch prepare: --file is not accepted for dispatch prepare")
		return 2
	}
	if !requireDispatchJSON(stderr, "prepare", cf.json) {
		return 2
	}
	if *ownerF == "" {
		fmt.Fprintln(stderr, "noise-tasks dispatch prepare: --owner is required")
		return 2
	}
	if err := store.ValidateOwnerLabel(*ownerF); err != nil {
		fmt.Fprintf(stderr, "noise-tasks dispatch prepare: --owner %v\n", err)
		return 2
	}
	if *assignmentF != "" {
		if err := store.ValidateOwnerLabel(*assignmentF); err != nil {
			fmt.Fprintf(stderr, "noise-tasks dispatch prepare: --assignment %v\n", err)
			return 2
		}
	}
	if *rootF == "" {
		fmt.Fprintln(stderr, "noise-tasks dispatch prepare: --request-id is required")
		return 2
	}
	if !store.ValidLocalKey(*rootF) {
		fmt.Fprintln(stderr, "noise-tasks dispatch prepare: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return 2
	}
	if flagWasSet(fs, "run-ttl") && (*runTTLF < 1 || *runTTLF > store.MaxRunTTL) {
		fmt.Fprintf(stderr, "noise-tasks dispatch prepare: --run-ttl must be between 1 and %d seconds when supplied\n", store.MaxRunTTL)
		return 2
	}
	if *milestoneF != "" {
		if err := backlog.ValidateMilestoneName(*milestoneF); err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	// The tag resolves exactly as claim-next resolves it, including the
	// unknown-milestone usage error (§12.2).
	entries, err := ps.db.MilestoneEntries(tag)
	if err != nil {
		return reportMilestoneError(stderr, cf.json, err)
	}
	if code := requireKnownMilestone(entries, *milestoneF, stderr); code != 0 {
		return code
	}

	steps, err := dispatchStepRequestIDs(*rootF)
	if err != nil {
		return fail(stderr, err)
	}

	// Step 1: claim-next under the derived claim request id. An identical
	// replay returns the originally recorded task and the live claim token;
	// the successful empty result passes through with no reservation.
	task, claim, _, err := ps.db.ClaimNext(tag, *ownerF, *claimTTLF, *milestoneF, *assignmentF, steps.Claim)
	if err != nil {
		var replay *store.ReplayConflict
		if errors.As(err, &replay) {
			return writeLiveConflict(stderr, cf.json, replayConflictOut(replay), replay.Error())
		}
		var conflict *store.ClaimConflict
		if errors.As(err, &conflict) {
			return writeLiveConflict(stderr, cf.json, claimConflictOut(conflict), conflict.Error())
		}
		return fail(stderr, err)
	}
	if task == nil {
		if err := writeJSON(stdout, nil); err != nil {
			return fail(stderr, err)
		}
		return 0
	}

	// Step 2: run start under the derived start request id, decided
	// receipt-first. A committed receipt proves the same run and normalized
	// inputs; a renewed or retried controller is then read from
	// authoritative state rather than the receipt's historical tokens, so
	// prepare never invents a second run for the root.
	startReceipt, err := ps.db.FindDispatchStartReceipt(steps.Start, tag, task.ID, *ownerF, *runTTLF)
	if err != nil {
		var replay *store.ReplayConflict
		if errors.As(err, &replay) {
			return writeLiveConflict(stderr, cf.json, replayConflictOut(replay), replay.Error())
		}
		return fail(stderr, err)
	}
	var runID int64
	if startReceipt != nil {
		runID = startReceipt.RunID
	} else {
		result, _, err := ps.db.StartRun(tag, task.ID, *ownerF, claim.Token, steps.Start, *runTTLF)
		if err != nil {
			var replay *store.ReplayConflict
			if errors.As(err, &replay) {
				return writeLiveConflict(stderr, cf.json, replayConflictOut(replay), replay.Error())
			}
			var conflict *store.RunConflict
			if errors.As(err, &conflict) {
				return writeLiveConflict(stderr, cf.json, runConflictOut(conflict), conflict.Error())
			}
			return fail(stderr, err)
		}
		runID = result.RunID
	}
	// The authoritative current live claim and attempt capabilities: these,
	// not any receipt's historical tokens, authorize the provision leg.
	controller, err := ps.db.DispatchLiveController(runID)
	if err != nil {
		return fail(stderr, err)
	}

	// Step 3: worktree provision under the derived provision request id,
	// composed exactly as the manual §9.4 surface — preflight, plan, effect
	// after commit, finalize through the compensation protocol.
	record, err := ps.db.GetRun(runID)
	if err != nil {
		return fail(stderr, err)
	}
	if record == nil {
		return reportWorktreeProvisionError(stderr, cf.json,
			&store.RunConflict{Operation: "provision", RunID: runID, Attempt: controller.Attempt})
	}
	if _, derr := derivedOwnerBoundary(ps.boundary); derr != nil {
		return fail(stderr, &store.WorktreeOwnerBoundaryUnavailable{Path: ps.boundary})
	}
	reserved, reservedBase, err := ps.db.WorktreeRequestReserved(steps.Provision)
	if err != nil {
		return fail(stderr, err)
	}
	identity, err := store.PreflightWorktreeProvision(ps.boundary, ps.owner, record.Tag, record.Task, reserved, reservedBase)
	if err != nil {
		return reportWorktreeProvisionError(stderr, cf.json, err)
	}
	plan, err := ps.db.PlanWorktreeProvision(store.WorktreeProvisionOptions{
		RunID: runID, Owner: *ownerF, ClaimToken: controller.ClaimToken,
		Attempt: controller.Attempt, AttemptToken: controller.AttemptToken, RequestID: steps.Provision,
		OwnerBoundary: identity.OwnerBoundary, RegistrationRoot: identity.RegistrationRoot,
		TargetPath: identity.TargetPath, Branch: identity.Branch, BaseCommit: identity.BaseCommit,
	})
	if err != nil {
		return reportWorktreeProvisionError(stderr, cf.json, err)
	}
	var result *store.WorktreeMutationResult
	if plan.EffectRequired {
		ownerNow, derr := derivedOwnerBoundary(ps.boundary)
		if derr != nil {
			return fail(stderr, &store.WorktreeOwnerBoundaryUnavailable{Path: ps.boundary})
		}
		outcome, eerr := store.EffectWorktreeProvision(ps.boundary, ownerNow, plan)
		if eerr != nil {
			return reportWorktreeProvisionError(stderr, cf.json, eerr)
		}
		result, err = finalizeWorktreeProvisionWithCompensation(ps, plan, outcome)
		if err != nil {
			return reportWorktreeProvisionError(stderr, cf.json, err)
		}
		// The compensation protocol owns the close of ps.db, so the live
		// authority is reopened before the projection reads that follow.
		if code := dispatchReopenStore(ps, stderr); code != 0 {
			return code
		}
	} else {
		if plan.Result == nil {
			return fail(stderr, fmt.Errorf("worktree provision saga %d has no replayable result", plan.SagaID))
		}
		result = plan.Result
	}
	if result.Status == "failed" {
		// §9.4 primitive contract: a terminal failed provision is exit 1
		// with empty stdout in every mode, never a blocked handoff.
		fmt.Fprintln(stderr, "noise-tasks: worktree provision failed; the requested effect is proven absent")
		return 1
	}
	return writeDispatchHandoff(ps, stdout, stderr, *rootF, task.ID, steps, result.SagaID)
}

// dispatchReopenStore refreshes one invocation's store handle after the
// §9.4 compensation protocol closed it, keeping the same resolved boundary;
// closing the superseded handle is a no-op when it was already closed.
func dispatchReopenStore(ps *projectStore, stderr io.Writer) int {
	reopened, err := openProjectStoreAt(ps.boundary)
	if err != nil {
		return fail(stderr, err)
	}
	superseded := ps.db
	ps.db = reopened.db
	superseded.Close()
	return 0
}

func cmdDispatchReconcile(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("dispatch reconcile", stderr, &cf, false)
	fs.StringVar(&cf.file, "file", "", "path to a standalone JSON fixture document (never authoritative)")
	ownerF := fs.String("owner", "", "run owner identity")
	var claimToken, attempt, attemptToken int64
	decimalInt64Var(fs, &claimToken, "claim-token", "current claim fencing token")
	decimalInt64Var(fs, &attempt, "attempt", "current attempt number")
	decimalInt64Var(fs, &attemptToken, "attempt-token", "current attempt fencing token")
	var sagaID, sagaToken int64
	decimalInt64Var(fs, &sagaID, "saga", "provisioning saga id")
	decimalInt64Var(fs, &sagaToken, "saga-token", "private saga fence token")
	recoverF := fs.String("recover-request-id", "", "advertised or terminalizing recovery request id")
	rootF := fs.String("root-request-id", "", "root request id derivation seed")
	if code, ok := parseFlags(fs, stdout, runIDFirstArgs(args)); !ok {
		return code
	}
	if cf.file != "" {
		return worktreeUsageError(stderr, "--file is not accepted for dispatch reconcile")
	}
	if !requireDispatchJSON(stderr, "reconcile", cf.json) {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks dispatch reconcile <runId> --owner <owner> --claim-token <token> --attempt <n> --attempt-token <token> --saga <saga> --saga-token <token> --recover-request-id <id> --root-request-id <root> --json")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "dispatch reconcile", stderr)
	if !ok {
		return 2
	}
	if *ownerF == "" {
		fmt.Fprintln(stderr, "noise-tasks dispatch reconcile: --owner is required")
		return 2
	}
	if err := store.ValidateOwnerLabel(*ownerF); err != nil {
		fmt.Fprintf(stderr, "noise-tasks dispatch reconcile: --owner %v\n", err)
		return 2
	}
	if claimToken <= 0 || attempt <= 0 || attemptToken <= 0 || sagaToken <= 0 {
		fmt.Fprintln(stderr, "noise-tasks dispatch reconcile: --claim-token, --attempt, --attempt-token, and --saga-token must be positive integers")
		return 2
	}
	if sagaID <= 0 {
		fmt.Fprintln(stderr, "noise-tasks dispatch reconcile: --saga must be a positive integer")
		return 2
	}
	for flagName, value := range map[string]string{"--recover-request-id": *recoverF, "--root-request-id": *rootF} {
		if value == "" || !store.ValidLocalKey(value) {
			fmt.Fprintf(stderr, "noise-tasks dispatch reconcile: %s is required and must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}\n", flagName)
			return 2
		}
	}

	// The live authority is the repository family; a fixture never selects
	// it, and the boundary is rederived from repository state.
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	ps, err := openProjectStoreAt(boundary)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()

	steps, err := dispatchStepRequestIDs(*rootF)
	if err != nil {
		return fail(stderr, err)
	}
	// One lineage, proven from the committed records: the derived claim,
	// start, and provision identities must name exactly the supplied
	// currently live controller for the named provisioning saga's run.
	// Anything else is the typed §9.3B conflict — never a fallback to
	// claim-next or a search for a convenient saga.
	saga, err := ps.db.ProveDispatchLineage(steps.Claim, steps.Start, steps.Provision,
		runID, sagaID, *ownerF, claimToken, attempt, attemptToken)
	if err != nil {
		var conflict *store.DispatchConflict
		if errors.As(err, &conflict) {
			return writeLiveConflict(stderr, cf.json, dispatchConflictOut(conflict), conflict.Error())
		}
		return fail(stderr, err)
	}
	if saga.Status == "bound" || saga.Status == "failed" || saga.Status == "removed" {
		return dispatchReconcileTerminal(ps, stdout, stderr, &cf, *rootF, *recoverF, steps, saga, sagaID, sagaToken)
	}
	return dispatchReconcileNonterminal(ps, stdout, stderr, &cf, *rootF, *recoverF, steps, saga, sagaID, sagaToken)
}

// dispatchReconcileNonterminal resumes a nonterminal provisioning saga: the
// recovery request id must equal the current controller projection's
// advertised next id — a historical receipt id is rejected even though raw
// §9.4 recover would replay it — and reconcile then delegates exactly to one
// worktree recover attempt and lets that primitive commit and verify its own
// receipt. The §9.5 broker capability on fd 3 is inherited unchanged by the
// delegation.
func dispatchReconcileNonterminal(ps *projectStore, stdout, stderr io.Writer, cf *commonFlags,
	root, recoverID string, steps dispatchStepIDs, saga *store.DispatchSaga, sagaID, sagaToken int64) int {
	projection, _, err := ps.db.WorktreeHandoff(sagaID)
	if err != nil {
		return reportDispatchHandoffError(stderr, cf.json, sagaID, err)
	}
	if projection.Workspace.RecoverRequestID == nil || *projection.Workspace.RecoverRequestID == "" {
		// A nonterminal projection without an advertised id is impossible
		// below the exhausted generation, which the render already refused.
		return fail(stderr, fmt.Errorf("worktree saga %d advertises no recovery request id", sagaID))
	}
	if recoverID != *projection.Workspace.RecoverRequestID {
		conflict := &store.DispatchConflict{Reason: fmt.Sprintf(
			"recovery request %q is not the id advertised for saga %d's next recovery generation",
			recoverID, sagaID)}
		return writeLiveConflict(stderr, cf.json, dispatchConflictOut(conflict), conflict.Error())
	}
	result, code, ok := dispatchPerformRecovery(ps, stderr, cf, sagaID, sagaToken, recoverID)
	if !ok {
		return code
	}
	if result.Status == "failed" {
		return fail(stderr, errors.New(boundedDiagnostic("worktree recovery failed; the requested effect is proven absent")))
	}
	return writeDispatchHandoff(ps, stdout, stderr, root, saga.Task, steps, sagaID)
}

// dispatchReconcileTerminal is crash-before-output replay only: it accepts
// exactly the terminalizing recovery receipt's request id — the projection's
// recoverRequestId is null for a terminal saga, and every new id is exit 3 —
// and requires the saga's original provision receipt (verified by the
// lineage proof) before dispatch returns a leaf brief. The primitive
// exit-1 failure of a terminalizing failed receipt is reachable only from a
// proven-live controller: the §9.3A projection read is the canonical
// liveness gate, so a stale claim, run, or attempt keeps the typed exit-3
// conflict with empty stdout (SPEC §9.3B).
func dispatchReconcileTerminal(ps *projectStore, stdout, stderr io.Writer, cf *commonFlags,
	root, recoverID string, steps dispatchStepIDs, saga *store.DispatchSaga, sagaID, sagaToken int64) int {
	if saga.RecoveryGeneration == 0 {
		// The original provision terminalized this saga; no recovery
		// receipt exists to replay (§9.3B).
		conflict := &store.DispatchConflict{Reason: fmt.Sprintf(
			"terminal saga %d has no recovery receipt to replay", sagaID)}
		return writeLiveConflict(stderr, cf.json, dispatchConflictOut(conflict), conflict.Error())
	}
	result, code, ok := dispatchPerformRecovery(ps, stderr, cf, sagaID, sagaToken, recoverID)
	if !ok {
		return code
	}
	// Only the exact receipt that terminalized the saga is accepted, never
	// an earlier historical recovery receipt (§9.3B).
	if result.Status != saga.Status || result.RecoveryGeneration != saga.RecoveryGeneration {
		conflict := &store.DispatchConflict{Reason: fmt.Sprintf(
			"recovery request %q is not the receipt that terminalized saga %d", recoverID, sagaID)}
		return writeLiveConflict(stderr, cf.json, dispatchConflictOut(conflict), conflict.Error())
	}
	if result.Status == "failed" {
		// §9.3A liveness is proven before the terminal-failure exit:
		// WorktreeHandoff refuses a stale controller — expired claim or
		// attempt lease, or a run/attempt pair no longer running — as the
		// typed conflict instead of the primitive failure.
		if _, _, err := ps.db.WorktreeHandoff(sagaID); err != nil {
			return reportDispatchHandoffError(stderr, cf.json, sagaID, err)
		}
		return fail(stderr, errors.New(boundedDiagnostic("worktree recovery failed; the requested effect is proven absent")))
	}
	return writeDispatchHandoff(ps, stdout, stderr, root, saga.Task, steps, sagaID)
}

// dispatchPerformRecovery delegates exactly to one §9.4 worktree recover
// attempt under the supplied saga fence and request id: RecoverWorktreeSaga
// decides the common receipt namespace receipt-first and validates the fence,
// and an effect-requiring attempt is reconciled by the trusted host half
// with no transaction open. The fd-3 broker capability, when inherited, is
// consumed unchanged by that host half.
func dispatchPerformRecovery(ps *projectStore, stderr io.Writer, cf *commonFlags,
	sagaID, sagaToken int64, recoverID string) (*store.WorktreeMutationResult, int, bool) {
	attempt, err := ps.db.RecoverWorktreeSaga(store.WorktreeRecoveryOptions{
		SagaID: sagaID, SagaToken: sagaToken, RequestID: recoverID})
	if err != nil {
		return nil, reportWorktreeRecoverError(stderr, cf.json, err), false
	}
	if !attempt.EffectRequired {
		// Exact receipt replay: the verified historical public result.
		if attempt.Result == nil {
			return nil, fail(stderr, fmt.Errorf("worktree saga %d has no replayable result", sagaID)), false
		}
		return attempt.Result, 0, true
	}
	reconciliation, err := store.ReconcileWorktreeRecoveryEffect(ps.db, ps.boundary, attempt)
	if err != nil {
		return nil, reportWorktreeRecoverError(stderr, cf.json, err), false
	}
	if reconciliation == nil || reconciliation.Result == nil {
		return nil, fail(stderr, errors.New("worktree recovery produced no result")), false
	}
	return reconciliation.Result, 0, true
}

// writeDispatchHandoff renders the §9.3A projections for the saga the
// composed lineage produced and writes the exact §9.3B result: the success
// object only for an active bound provision passing every leaf-eligibility
// rule, and otherwise the typed blocked controller result for a nonterminal
// workspace. A typed §9.3A refusal keeps exit 3 with empty stdout, and the
// exhausted-generation refusal keeps its pinned exit-1 diagnostic.
func writeDispatchHandoff(ps *projectStore, stdout, stderr io.Writer, root, task string,
	steps dispatchStepIDs, sagaID int64) int {
	projection, brief, err := ps.db.WorktreeHandoff(sagaID)
	if err != nil {
		return reportDispatchHandoffError(stderr, true, sagaID, err)
	}
	if brief != nil {
		if err := writeJSON(stdout, dispatchSuccess{
			RootRequestID: root, Task: task, StepRequestIDs: steps,
			Controller: projection, Leaf: brief,
		}); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	switch projection.Workspace.Status {
	case "needs-broker":
		return writeDispatchBlocked(stdout, stderr, root, task, steps, projection, "needs-broker")
	case "pending", "cleanup-required":
		return writeDispatchBlocked(stdout, stderr, root, task, steps, projection, "needs-recovery")
	default:
		// A terminal workspace without a brief is never handoff material;
		// the failed primitive contract is handled by the caller.
		return fail(stderr, fmt.Errorf("worktree saga %d is %s and yields no dispatch handoff",
			sagaID, projection.Workspace.Status))
	}
}

func writeDispatchBlocked(stdout, stderr io.Writer, root, task string, steps dispatchStepIDs,
	projection *store.ControllerProjection, reason string) int {
	if err := writeJSON(stdout, dispatchBlocked{
		Kind: "dispatch-blocked", Reason: reason, RootRequestID: root, Task: task,
		StepRequestIDs: steps, Controller: projection,
	}); err != nil {
		return fail(stderr, err)
	}
	return 0
}

// reportDispatchHandoffError maps the §9.3A render refusals: the exhausted
// nonterminal generation keeps its pinned fail-closed exit 1, and a typed
// handoff conflict keeps exit 3 with empty stdout.
func reportDispatchHandoffError(stderr io.Writer, jsonOut bool, sagaID int64, err error) int {
	if errors.Is(err, store.ErrWorktreeRecoveryExhausted) {
		fmt.Fprintln(stderr, "noise-tasks: "+store.ErrWorktreeRecoveryExhausted.Error())
		return 1
	}
	var conflict *store.HandoffConflict
	if errors.As(err, &conflict) {
		return writeLiveConflict(stderr, jsonOut, handoffConflictOut(conflict), conflict.Error())
	}
	return fail(stderr, err)
}

func dispatchConflictOut(e *store.DispatchConflict) any {
	return struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	}{"dispatch-conflict", e.Reason}
}

func handoffConflictOut(e *store.HandoffConflict) any {
	return struct {
		Kind   string `json:"kind"`
		SagaID int64  `json:"sagaId"`
		Reason string `json:"reason"`
	}{"worktree-handoff-conflict", e.SagaID, e.Reason}
}
