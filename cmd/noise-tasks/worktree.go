package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdWorktree dispatches the §9 `worktree` subcommands.
func cmdWorktree(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks worktree <provision|list|remove|recover|prune> [options]")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "provision":
		return cmdWorktreeProvision(rest, stdout, stderr)
	case "recover":
		return cmdWorktreeRecover(rest, stdout, stderr)
	case "create":
		// §9.4 v5 cutover: worktree create is removed; the guided usage
		// error replaces the legacy surface.
		return worktreeCutoverRefusal(stderr, "worktree: create")
	case "list":
		return cmdWorktreeList(rest, stdout, stderr)
	case "remove":
		return cmdWorktreeRemove(rest, stdout, stderr)
	case "prune":
		return cmdWorktreePrune(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "noise-tasks worktree: unknown subcommand %q\n", sub)
		return 2
	}
}

// worktreeCutoverGuidance is the single §9.4 v5-cutover guidance sentence
// every removed legacy worktree surface reports.
const worktreeCutoverGuidance = "acquire a claim, start a run, and use worktree provision"

// worktreeCutoverRefusal reports the §9.4 v5-cutover usage error (exit 2)
// for one removed legacy surface, with the pinned guidance.
func worktreeCutoverRefusal(stderr io.Writer, surface string) int {
	fmt.Fprintf(stderr, "noise-tasks %s was removed by the v5 cutover; %s\n", surface, worktreeCutoverGuidance)
	return 2
}

// worktreeUsageError reports a §9 usage error (exit 2).
func worktreeUsageError(stderr io.Writer, msg string) int {
	fmt.Fprintln(stderr, "noise-tasks: "+msg)
	return 2
}

// reportWorktreeError maps store-layer worktree errors to exit codes: typed
// conflicts exit 3, usage errors exit 2, everything else exits 1.
func reportWorktreeError(stderr io.Writer, jsonOut bool, err error) int {
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
		out := struct {
			Kind   string `json:"kind"`
			SagaID int64  `json:"sagaId"`
		}{"worktree-saga-conflict", saga.SagaID}
		return writeLiveConflict(stderr, jsonOut, out, saga.Error())
	}
	var wc *store.WorktreeConflict
	if errors.As(err, &wc) {
		return writeLiveConflict(stderr, jsonOut, worktreeConflictOut(wc), wc.Error())
	}
	var ue *store.WorktreeUsageError
	if errors.As(err, &ue) {
		return worktreeUsageError(stderr, ue.Message)
	}
	return fail(stderr, err)
}

// boundedDiagnostic caps a stderr-only diagnostic at a fixed length so an
// effect-layer error stays a single bounded line.
func boundedDiagnostic(msg string) string {
	const max = 512
	if len(msg) > max {
		return msg[:max] + "..."
	}
	return msg
}

func worktreeConflictOut(e *store.WorktreeConflict) any {
	return struct {
		Kind     string `json:"kind"`
		Reason   string `json:"reason"`
		Tag      string `json:"tag"`
		Task     string `json:"task"`
		Path     string `json:"path,omitempty"`
		Branch   string `json:"branch,omitempty"`
		Boundary string `json:"boundary,omitempty"`
	}{"worktree-conflict", e.Kind, e.Tag, e.ID, e.Path, e.Branch, e.Boundary}
}

// resolveWorktreeSagaStore handles the shared §9.4 read-surface routing: the
// family-wide saga table accepts neither --file nor a tag selection.
func resolveWorktreeSagaStore(cf *commonFlags, fs *flag.FlagSet, stderr io.Writer, cmd string) (*projectStore, int) {
	if cf.file != "" {
		return nil, worktreeUsageError(stderr, "--file is not accepted for worktree "+cmd)
	}
	if flagWasSet(fs, "tag") {
		return nil, worktreeUsageError(stderr, "--tag is not accepted for worktree "+cmd+"; the saga table spans all tags")
	}
	ps, err := openProjectStore()
	if err != nil {
		return nil, fail(stderr, err)
	}
	return ps, -1
}

func cmdWorktreeRemove(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("worktree remove", stderr, &cf, false)
	// Registered only so the family-pinned refusal applies; --file is never
	// accepted for worktree remove.
	fs.StringVar(&cf.file, "file", "", "path to a standalone JSON fixture document (never authoritative)")
	var capability runCapabilityFlags
	capability.register(fs, false)
	force := fs.Bool("force", false, "remove a dirty worktree")
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if cf.file != "" {
		return worktreeUsageError(stderr, "--file is not accepted for worktree remove")
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks worktree remove <run> --owner <owner> --claim-token <token> --attempt <n> --attempt-token <token> --request-id <id> [--force]")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "worktree remove", stderr)
	if !ok || !capability.validate(fs, stderr, "worktree remove", false) {
		return 2
	}
	if strings.HasPrefix(capability.requestID, "auto-") || strings.HasPrefix(capability.requestID, "recover-") {
		fmt.Fprintln(stderr, "noise-tasks worktree remove: --request-id uses a reserved prefix")
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()

	options := store.WorktreeRemoveOptions{RunID: runID, Owner: capability.owner,
		ClaimToken: capability.claimToken, Attempt: capability.attempt,
		AttemptToken: capability.attemptToken, RequestID: capability.requestID, Force: *force}
	inspected, err := ps.db.InspectWorktreeRemoval(options)
	if err != nil {
		return reportWorktreeError(stderr, cf.json, err)
	}
	if inspected.EffectRequired {
		if err := store.PreflightWorktreeRemovalEffect(inspected); err != nil {
			return reportWorktreeError(stderr, cf.json, err)
		}
	}

	plan, err := ps.db.PlanWorktreeRemoval(options)
	if err != nil {
		return reportWorktreeError(stderr, cf.json, err)
	}
	if !plan.EffectRequired {
		return writeWorktreeRemoveResult(stdout, stderr, cf.json, plan.Result, nil)
	}
	if !sameRemovalEffectIdentity(inspected, plan) {
		return fail(stderr, &store.WorktreeEffectIdentityMismatch{SagaID: plan.SagaID})
	}
	outcome, effectErr := store.PerformWorktreeRemovalEffect(plan)
	if outcome == "" {
		return fail(stderr, effectErr)
	}
	result, err := ps.db.FinalizeWorktreeRemoval(plan, outcome)
	if err != nil && outcome == store.WorktreeRemoveFailed {
		var forbidden *store.WorktreeRemovalFailedForbidden
		if errors.As(err, &forbidden) {
			result, err = ps.db.FinalizeWorktreeRemoval(plan, store.WorktreeRemoveCleanupRequired)
		}
	}
	if err != nil {
		return reportWorktreeError(stderr, cf.json, err)
	}
	return writeWorktreeRemoveResult(stdout, stderr, cf.json, result, effectErr)
}

func sameRemovalEffectIdentity(before, after *store.WorktreeRemovePlan) bool {
	if before == nil || after == nil {
		return false
	}
	return (before.SagaID == 0 || before.SagaID == after.SagaID) &&
		(before.SagaToken == 0 || before.SagaToken == after.SagaToken) &&
		before.SourceSagaID == after.SourceSagaID && before.RunID == after.RunID &&
		before.Tag == after.Tag && before.Task == after.Task && before.Owner == after.Owner &&
		before.RequestID == after.RequestID && before.OwnerBoundary == after.OwnerBoundary &&
		before.RegistrationRoot == after.RegistrationRoot && before.TargetPath == after.TargetPath &&
		before.Branch == after.Branch && before.BaseCommit == after.BaseCommit && before.Force == after.Force &&
		before.RecoveryGeneration == after.RecoveryGeneration
}

func writeWorktreeRemoveResult(stdout, stderr io.Writer, jsonOut bool, result *store.WorktreeMutationResult, cause error) int {
	if result == nil {
		if cause == nil {
			cause = errors.New("worktree removal produced no result")
		}
		return fail(stderr, cause)
	}
	if result.Status != "failed" {
		if jsonOut {
			if err := writeJSON(stdout, result); err != nil {
				return fail(stderr, err)
			}
		} else if result.Status == "removed" {
			runID, base, recoverID := "null", "null", "null"
			if result.RunID != nil {
				runID = fmt.Sprint(*result.RunID)
			}
			if result.BaseCommit != nil {
				base = *result.BaseCommit
			}
			if result.RecoverRequestID != nil {
				recoverID = *result.RecoverRequestID
			}
			fmt.Fprintf(stdout, "sagaId=%d operation=%s status=%s runId=%s ownerBoundary=%s path=%s branch=%s baseCommit=%s sagaToken=%d recoveryGeneration=%d recoverRequestId=%s\n",
				result.SagaID, result.Operation, result.Status, runID, result.OwnerBoundary, result.Path,
				result.Branch, base, result.SagaToken, result.RecoveryGeneration, recoverID)
		}
	}
	switch result.Status {
	case "removed":
		return 0
	case "needs-broker":
		// §9.5: under --json an exact recoverable confinement outcome is the
		// exit-0 saga mutation result on stdout; any diagnostic is stderr-only.
		if jsonOut {
			fmt.Fprintln(stderr, "noise-tasks: "+store.ErrWorktreeRegistrationConfined.Error())
			return 0
		}
		return fail(stderr, store.ErrWorktreeRegistrationConfined)
	case "cleanup-required":
		// §9.4: a cleanup-required result — fresh or exact replay — is the
		// exit-0 mutation result under --json; human invocation is exit 1
		// with empty stdout and exactly the pinned line on stderr.
		if jsonOut {
			return 0
		}
		fmt.Fprintln(stderr, "noise-tasks: worktree cleanup is required")
		return 1
	case "failed":
		// §9.4: every committed failed outcome — fresh or replay — is exit 1
		// with empty stdout in both modes; the cause is a bounded
		// stderr-only diagnostic and never routes through typed conflicts.
		if cause == nil {
			cause = errors.New("worktree removal failed")
		}
		return fail(stderr, errors.New(boundedDiagnostic(cause.Error())))
	default:
		return fail(stderr, fmt.Errorf("worktree removal returned invalid status %q", result.Status))
	}
}

// cmdWorktreeList renders the §9.4 saga read surface: every saga row ordered
// by saga id plus the owner boundary's unknown filesystem entries (SPEC
// §9.4). No request id, registration root, generation, fence, or capability
// is ever listed.
func cmdWorktreeList(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("worktree list", stderr, &cf)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "noise-tasks worktree list: unexpected argument")
		return 2
	}
	ps, code := resolveWorktreeSagaStore(&cf, fs, stderr, "list")
	if ps == nil {
		return code
	}
	defer ps.db.Close()

	report, err := ps.db.ListWorktreeSagas(ps.owner)
	if err != nil {
		return fail(stderr, err)
	}

	if cf.json {
		if err := writeJSON(stdout, report); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, e := range report.Entries {
		runID, base := "null", "null"
		if e.RunID != nil {
			runID = fmt.Sprint(*e.RunID)
		}
		if e.BaseCommit != nil {
			base = *e.BaseCommit
		}
		fmt.Fprintf(stdout, "sagaId=%d runId=%s tag=%s task=%s operation=%s status=%s active=%t ownerBoundary=%s path=%s branch=%s baseCommit=%s force=%t updatedAt=%s\n",
			e.SagaID, runID, e.Tag, e.Task, e.Operation, e.Status, e.Active,
			e.OwnerBoundary, e.Path, e.Branch, base, e.Force, e.UpdatedAt)
	}
	for _, p := range report.Unknown {
		fmt.Fprintf(stdout, "unknown path=%s\n", p.Path)
	}
	return 0
}

// pruneBrokerRequestOut is one §9.4 trusted-prune controller handoff:
// sagaId and the stored fence — exposed only on this explicit
// controller-only JSON surface — with the exact broker request shape laid
// flat in its spec order.
type pruneBrokerRequestOut struct {
	SagaID    int64 `json:"sagaId"`
	SagaToken int64 `json:"sagaToken"`
	*store.WorktreeBrokerRequest
}

// pruneResultOut is one §9.4 prune result: the saga id and its committed
// status after this prune run's recovery attempt.
type pruneResultOut struct {
	SagaID int64  `json:"sagaId"`
	Status string `json:"status"`
}

// cmdWorktreePrune is the §9.4 Trusted-Orchestrator prune surface. It
// snapshots the eligible nonterminal rows, closes that read transaction,
// and then invokes recovery for each — shape alone is never ownership
// proof, so unknown filesystem entries are reported and never touched. The
// JSON handoff returns exactly results, brokerRequests, and unknown; it is
// the sole prune surface that may expose a stored fence (sagaToken). A saga
// already committed needs-broker re-emits its mint at the currently
// advertised generation even when this run cannot reconcile the broker
// step, so a human prune or a lost first response never strands the saga
// without its only token-bearing handoff. Human prune never prints a saga
// token or mint binding (SPEC §9.4).
func cmdWorktreePrune(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("worktree prune", stderr, &cf)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "noise-tasks worktree prune: unexpected argument")
		return 2
	}
	ps, code := resolveWorktreeSagaStore(&cf, fs, stderr, "prune")
	if ps == nil {
		return code
	}
	defer ps.db.Close()

	snapshot, err := ps.db.SnapshotWorktreePrune(ps.owner)
	if err != nil {
		return fail(stderr, err)
	}

	var brokerRequests []pruneBrokerRequestOut
	ids := make([]int64, 0, len(snapshot.Eligible))
	for _, row := range snapshot.Eligible {
		ids = append(ids, row.SagaID)
		if row.RequestID == "" {
			// §9.4 exhaustion: the broker handoff refuses before any
			// effect; the diagnostic stays stderr-only.
			fmt.Fprintln(stderr, "noise-tasks: worktree recovery generation exhausted")
			continue
		}
		attempt, err := ps.db.RecoverWorktreeSaga(store.WorktreeRecoveryOptions{
			SagaID: row.SagaID, SagaToken: row.SagaToken, RequestID: row.RequestID})
		if err != nil {
			reportWorktreePruneRefusal(stderr, row.SagaID, err)
			continue
		}
		if !attempt.EffectRequired {
			// An exact receipt replay: no effect may run and this run
			// commits nothing new to hand off.
			continue
		}
		reconciled, err := store.ReconcileWorktreeRecoveryEffect(ps.db, ps.boundary, attempt)
		if err != nil {
			reportWorktreePruneRefusal(stderr, row.SagaID, err)
			// §9.4: a saga already committed needs-broker still hands off
			// its exact mint at the currently advertised generation — a
			// refused reconciliation (no inherited fd-3 capability, an
			// unavailable broker) commits nothing but must not strand the
			// saga without its recovery handoff. The refusal stays
			// stderr-only.
			if row.BrokerRequest != nil {
				brokerRequests = append(brokerRequests, pruneBrokerRequestOut{
					SagaID: row.SagaID, SagaToken: row.SagaToken,
					WorktreeBrokerRequest: row.BrokerRequest,
				})
			}
			continue
		}
		if reconciled.Result != nil && reconciled.Result.BrokerRequest != nil {
			brokerRequests = append(brokerRequests, pruneBrokerRequestOut{
				SagaID: row.SagaID, SagaToken: row.SagaToken,
				WorktreeBrokerRequest: reconciled.Result.BrokerRequest,
			})
		}
	}

	statuses, err := ps.db.ReadWorktreeSagaStatuses(ids)
	if err != nil {
		return fail(stderr, err)
	}
	results := make([]pruneResultOut, 0, len(statuses))
	for _, s := range statuses {
		results = append(results, pruneResultOut{SagaID: s.SagaID, Status: s.Status})
	}
	if brokerRequests == nil {
		brokerRequests = []pruneBrokerRequestOut{}
	}

	if cf.json {
		out := struct {
			Results        []pruneResultOut             `json:"results"`
			BrokerRequests []pruneBrokerRequestOut      `json:"brokerRequests"`
			Unknown        []store.WorktreeUnknownEntry `json:"unknown"`
		}{results, brokerRequests, snapshot.Unknown}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, r := range results {
		fmt.Fprintf(stdout, "sagaId=%d status=%s\n", r.SagaID, r.Status)
	}
	for _, p := range snapshot.Unknown {
		fmt.Fprintf(stdout, "unknown path=%s\n", p.Path)
	}
	fmt.Fprintf(stdout, "pruned: %d sagas, %d broker handoffs\n", len(results), len(brokerRequests))
	return 0
}

// reportWorktreePruneRefusal reports one saga's refused recovery as a
// bounded stderr-only diagnostic; the prune run itself continues so the
// remaining sagas still reach their recoveries (SPEC §6/§9.4).
func reportWorktreePruneRefusal(stderr io.Writer, sagaID int64, err error) {
	fmt.Fprintf(stderr, "noise-tasks: worktree saga %d prune recovery: %s\n", sagaID, boundedDiagnostic(err.Error()))
}
