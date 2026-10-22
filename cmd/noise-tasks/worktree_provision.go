package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// worktreeProvisionCapabilityFlags are the §9.4 caller capabilities: the
// command is invoked by the controller that owns the run's live claim.
type worktreeProvisionCapabilityFlags struct {
	owner        string
	claimToken   int64
	attempt      int64
	attemptToken int64
	requestID    string
}

func (f *worktreeProvisionCapabilityFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.owner, "owner", "", "run owner identity")
	decimalInt64Var(fs, &f.claimToken, "claim-token", "current claim fencing token")
	decimalInt64Var(fs, &f.attempt, "attempt", "current attempt number")
	decimalInt64Var(fs, &f.attemptToken, "attempt-token", "current attempt fencing token")
	fs.StringVar(&f.requestID, "request-id", "", "request id for replay")
}

func (f *worktreeProvisionCapabilityFlags) validate(fs *flag.FlagSet, stderr io.Writer) bool {
	if f.owner == "" {
		fmt.Fprintln(stderr, "noise-tasks worktree provision: --owner is required")
		return false
	}
	if err := store.ValidateOwnerLabel(f.owner); err != nil {
		fmt.Fprintf(stderr, "noise-tasks worktree provision: --owner %v\n", err)
		return false
	}
	if f.claimToken <= 0 || f.attempt <= 0 || f.attemptToken <= 0 {
		fmt.Fprintln(stderr, "noise-tasks worktree provision: --claim-token, --attempt, and --attempt-token must be positive integers")
		return false
	}
	if !store.ValidLocalKey(f.requestID) {
		fmt.Fprintln(stderr, "noise-tasks worktree provision: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return false
	}
	// §9.4: caller ids never use the automatic and recovery prefixes; this is
	// local grammar, validated before the store is opened.
	if strings.HasPrefix(f.requestID, "auto-") || strings.HasPrefix(f.requestID, "recover-") {
		fmt.Fprintln(stderr, `noise-tasks worktree provision: --request-id cannot begin with "auto-" or "recover-"`)
		return false
	}
	return true
}

func cmdWorktreeProvision(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("worktree provision", stderr, &cf)
	var capability worktreeProvisionCapabilityFlags
	capability.register(fs)
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks worktree provision <runId> --owner <owner> --claim-token <token> --attempt <n> --attempt-token <token> --request-id <id>")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "provision", stderr)
	if !ok || !capability.validate(fs, stderr) {
		return 2
	}
	// The live authority is the repository family; a fixture or caller-named
	// tag never selects it, and the tag derives from the run alone.
	if cf.file != "" {
		return worktreeUsageError(stderr, "--file is not accepted for worktree provision")
	}
	if flagWasSet(fs, "tag") {
		return worktreeUsageError(stderr, "--tag is not accepted for worktree provision; the run derives the tag")
	}

	// §9.4 preflight derives the immutable owner boundary before the store is
	// opened: a disappeared or differently-resolving owner root is the pinned
	// refusal — the family is never rehomed.
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	if _, derr := derivedOwnerBoundary(boundary); derr != nil {
		return fail(stderr, &store.WorktreeOwnerBoundaryUnavailable{Path: boundary})
	}
	ps, err := openProjectStoreAt(boundary)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()

	record, err := ps.db.GetRun(runID)
	if err != nil {
		return fail(stderr, err)
	}
	if record == nil {
		return reportWorktreeProvisionError(stderr, cf.json,
			&store.RunConflict{Operation: "provision", RunID: runID, Attempt: capability.attempt})
	}
	reserved, reservedBase, err := ps.db.WorktreeRequestReserved(capability.requestID)
	if err != nil {
		return fail(stderr, err)
	}
	identity, err := store.PreflightWorktreeProvision(ps.boundary, ps.owner, record.Tag, record.Task, reserved, reservedBase)
	if err != nil {
		return reportWorktreeProvisionError(stderr, cf.json, err)
	}
	plan, err := ps.db.PlanWorktreeProvision(store.WorktreeProvisionOptions{
		RunID: runID, Owner: capability.owner, ClaimToken: capability.claimToken,
		Attempt: capability.attempt, AttemptToken: capability.attemptToken, RequestID: capability.requestID,
		OwnerBoundary: identity.OwnerBoundary, RegistrationRoot: identity.RegistrationRoot,
		TargetPath: identity.TargetPath, Branch: identity.Branch, BaseCommit: identity.BaseCommit,
	})
	if err != nil {
		return reportWorktreeProvisionError(stderr, cf.json, err)
	}

	if plan.EffectRequired {
		// §9.4 phase 2: the effect runs only after the planning transaction
		// committed and closed, and the owner boundary is rederived
		// immediately before the effect.
		ownerNow, derr := derivedOwnerBoundary(ps.boundary)
		if derr != nil {
			return fail(stderr, &store.WorktreeOwnerBoundaryUnavailable{Path: ps.boundary})
		}
		outcome, err := store.EffectWorktreeProvision(ps.boundary, ownerNow, plan)
		if err != nil {
			return reportWorktreeProvisionError(stderr, cf.json, err)
		}
		result, ferr := finalizeWorktreeProvisionWithCompensation(ps, plan, outcome)
		if ferr != nil {
			return reportWorktreeProvisionError(stderr, cf.json, ferr)
		}
		return writeWorktreeProvisionResult(stdout, stderr, cf.json, result)
	}

	// Replay: the store says no effect may run; return the verified
	// historical result or current handoff unchanged.
	if plan.Result == nil {
		return fail(stderr, fmt.Errorf("worktree provision saga %d has no replayable result", plan.SagaID))
	}
	return writeWorktreeProvisionResult(stdout, stderr, cf.json, plan.Result)
}

// finalizeWorktreeProvisionWithCompensation records the observed §9.4
// outcome. When the observed create or confined handoff can no longer commit
// because the run or task became ineligible, it closes SQLite, compensates
// the exact immutable effect with no transaction open, and finalizes the same
// plan on a freshly opened store as failed (absence proved) or
// cleanup-required. The compensation path owns the close of ps.db; a later
// deferred close of that handle is a harmless no-op.
func finalizeWorktreeProvisionWithCompensation(ps *projectStore, plan *store.WorktreeProvisionPlan,
	outcome store.WorktreeProvisionOutcome) (*store.WorktreeMutationResult, error) {
	result, err := ps.db.FinalizeWorktreeProvision(plan, outcome)
	if err == nil {
		return result, nil
	}
	var compensation *store.WorktreeProvisionCompensationRequired
	if !errors.As(err, &compensation) {
		return nil, err
	}
	ps.db.Close()
	// §9.4: the compensation is itself an effect, so immediately before it —
	// after the close, with no SQLite transaction open — the trusted host
	// rederives the exact Git registration root and the current owner
	// boundary from repository state and requires both to equal the
	// immutable row; a mismatch fails closed with no effect and no row
	// rewrite, leaving the saga replayable.
	proved, cerr := store.CompensateWorktreeProvision(ps.boundary, plan)
	if cerr != nil {
		return nil, cerr
	}
	final := store.WorktreeProvisionFailed
	if !proved {
		final = store.WorktreeProvisionCleanupRequired
	}
	reopened, oerr := openProjectStoreAt(ps.boundary)
	if oerr != nil {
		return nil, oerr
	}
	defer reopened.db.Close()
	return reopened.db.FinalizeWorktreeProvision(plan, final)
}

// writeWorktreeProvisionResult composes the exact §9.4 provision output
// matrix, identically for a fresh result and an exact replay: a failed
// result is exit 1 with empty stdout in both modes with its diagnostic on
// stderr only; needs-broker and cleanup-required return the mutation
// result under --json and their pinned exit-1 diagnostics otherwise; a
// bound result renders the non-secret fields plus the manual-provision
// saga token.
func writeWorktreeProvisionResult(stdout, stderr io.Writer, jsonOut bool, result *store.WorktreeMutationResult) int {
	if result.Status == "failed" {
		// §9.4: failed is terminal and follows the exit-1 empty-stdout
		// contract in both output modes, fresh or replayed.
		fmt.Fprintln(stderr, "noise-tasks: worktree provision failed; the requested effect is proven absent")
		return 1
	}
	if result.Status == "needs-broker" {
		if jsonOut {
			return writeProvisionJSON(stdout, stderr, result)
		}
		fmt.Fprintln(stderr, "noise-tasks: worktree registration is confined and no broker is available")
		return 1
	}
	if result.Status == "cleanup-required" {
		if jsonOut {
			return writeProvisionJSON(stdout, stderr, result)
		}
		fmt.Fprintln(stderr, "noise-tasks: worktree cleanup is required")
		return 1
	}
	if jsonOut {
		return writeProvisionJSON(stdout, stderr, result)
	}
	runID, base := "-", "-"
	if result.RunID != nil {
		runID = fmt.Sprint(*result.RunID)
	}
	if result.BaseCommit != nil {
		base = *result.BaseCommit
	}
	fmt.Fprintf(stdout, "sagaId=%d operation=%s status=%s runId=%s ownerBoundary=%s path=%s branch=%s baseCommit=%s recoveryGeneration=%d",
		result.SagaID, result.Operation, result.Status, runID, result.OwnerBoundary, result.Path,
		result.Branch, base, result.RecoveryGeneration)
	if result.RecoverRequestID != nil {
		fmt.Fprintf(stdout, " recoverRequestId=%s", *result.RecoverRequestID)
	}
	fmt.Fprintf(stdout, " sagaToken=%d\n", result.SagaToken)
	return 0
}

func writeProvisionJSON(stdout, stderr io.Writer, result *store.WorktreeMutationResult) int {
	if err := writeJSON(stdout, result); err != nil {
		return fail(stderr, err)
	}
	return 0
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// reportWorktreeProvisionError maps the provision surface errors to exits:
// typed replay, run, and saga conflicts exit 3, worktree usage errors exit 2,
// worktree conflicts exit 3, and everything else is an operational exit 1.
func reportWorktreeProvisionError(stderr io.Writer, jsonOut bool, err error) int {
	var replay *store.ReplayConflict
	if errors.As(err, &replay) {
		return writeLiveConflict(stderr, jsonOut, replayConflictOut(replay), replay.Error())
	}
	var run *store.RunConflict
	if errors.As(err, &run) {
		return writeLiveConflict(stderr, jsonOut, runConflictOut(run), run.Error())
	}
	var saga *store.WorktreeSagaConflict
	if errors.As(err, &saga) {
		return writeLiveConflict(stderr, jsonOut, sagaConflictOut(saga), saga.Error())
	}
	return reportWorktreeError(stderr, jsonOut, err)
}

func sagaConflictOut(e *store.WorktreeSagaConflict) any {
	return struct {
		Kind   string `json:"kind"`
		SagaID int64  `json:"sagaId"`
	}{"worktree-saga-conflict", e.SagaID}
}
