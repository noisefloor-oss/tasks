package main

import (
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdWorktreeRecover drives one §9.4 saga recovery:
// `worktree recover <saga> --saga-token <f> --request-id <id>`. Recovery is
// authorized by the private saga fence and the deterministic advertised
// request id alone — the command accepts no owner, run, attempt, path,
// branch, base, or force override, so every such flag is an unknown-flag
// usage error and only --saga-token, --request-id, and the common --json
// exist. The read half (RecoverWorktreeSaga) validates the fence, decides
// the common receipt namespace receipt-first, gates the generation bound,
// and snapshots only recorded identities; an exact recovery receipt replays
// its historical public result with no effect. Otherwise the trusted host
// reconciles the idempotent effect or compensation and finalizes through the
// generation CAS.
func cmdWorktreeRecover(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("worktree recover", stderr, &cf, false)
	// Registered only so the family-pinned refusal applies; --file is never
	// accepted for worktree recover.
	fs.StringVar(&cf.file, "file", "", "path to a standalone JSON fixture document (never authoritative)")
	var sagaToken int64
	decimalInt64Var(fs, &sagaToken, "saga-token", "private saga fence token")
	var requestID string
	fs.StringVar(&requestID, "request-id", "", "deterministic recovery request id")
	if code, ok := parseFlags(fs, stdout, runIDFirstArgs(args)); !ok {
		return code
	}
	if cf.file != "" {
		return worktreeUsageError(stderr, "--file is not accepted for worktree recover")
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks worktree recover <saga> --saga-token <token> --request-id <id>")
		return 2
	}
	sagaID, err := strconv.ParseInt(fs.Arg(0), 10, 64)
	if err != nil || sagaID <= 0 {
		fmt.Fprintln(stderr, "noise-tasks worktree recover: saga id must be a positive integer")
		return 2
	}
	if sagaToken <= 0 {
		fmt.Fprintln(stderr, "noise-tasks worktree recover: --saga-token must be a positive integer")
		return 2
	}
	if !store.ValidLocalKey(requestID) {
		fmt.Fprintln(stderr, "noise-tasks worktree recover: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return 2
	}

	// The live authority is the repository family; a fixture never selects
	// it, and the boundary is rederived from repository state, never accepted
	// from an argument.
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	ps, err := openProjectStoreAt(boundary)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()

	attempt, err := ps.db.RecoverWorktreeSaga(store.WorktreeRecoveryOptions{
		SagaID: sagaID, SagaToken: sagaToken, RequestID: requestID})
	if err != nil {
		return reportWorktreeRecoverError(stderr, cf.json, err)
	}
	if !attempt.EffectRequired {
		// Exact receipt replay: the verified historical public result, with
		// its mode-specific output class preserved and no effect.
		if attempt.Result == nil {
			return fail(stderr, fmt.Errorf("worktree saga %d has no replayable result", sagaID))
		}
		return writeWorktreeRecoverResult(stdout, stderr, cf.json, attempt.Result, nil)
	}
	// §9.4 phase 2: with no SQLite transaction open, the trusted host
	// performs the idempotent effect or compensation the snapshot status
	// requires and finalizes through the generation CAS. Broker work that
	// reaches no reconciled outcome commits nothing and returns the §9.4
	// taxonomy error instead.
	reconciliation, err := store.ReconcileWorktreeRecoveryEffect(ps.db, ps.boundary, attempt)
	if err != nil {
		return reportWorktreeRecoverError(stderr, cf.json, err)
	}
	if reconciliation == nil || reconciliation.Result == nil {
		return fail(stderr, errors.New("worktree recovery produced no result"))
	}
	return writeWorktreeRecoverResult(stdout, stderr, cf.json, reconciliation.Result, reconciliation.Cause)
}

// worktreeRecoverResult is the exact §9.4 recover JSON projection: the
// provision/remove result shape with the saga token omitted entirely —
// recover JSON never carries sagaToken in either direction.
type worktreeRecoverResult struct {
	SagaID             int64                        `json:"sagaId"`
	Operation          string                       `json:"operation"`
	Status             string                       `json:"status"`
	RunID              *int64                       `json:"runId"`
	OwnerBoundary      string                       `json:"ownerBoundary"`
	Path               string                       `json:"path"`
	Branch             string                       `json:"branch"`
	BaseCommit         *string                      `json:"baseCommit"`
	RecoveryGeneration int64                        `json:"recoveryGeneration"`
	RecoverRequestID   *string                      `json:"recoverRequestId"`
	BrokerRequest      *store.WorktreeBrokerRequest `json:"brokerRequest"`
}

// writeWorktreeRecoverResult composes the exact §9.4 recover output matrix,
// identically for a fresh result and an exact receipt replay: failed is exit
// 1 with empty stdout in both modes with its bounded diagnostic on stderr
// only; needs-broker and cleanup-required return the exit-0 mutation result
// under --json and their pinned exit-1 diagnostics otherwise; a terminal
// result exits 0 rendering the non-secret fields — recover never renders a
// token or a broker request in human output.
func writeWorktreeRecoverResult(stdout, stderr io.Writer, jsonOut bool, result *store.WorktreeMutationResult, cause error) int {
	if result == nil {
		if cause == nil {
			cause = errors.New("worktree recovery produced no result")
		}
		return fail(stderr, cause)
	}
	switch result.Status {
	case "failed":
		// §9.4: every committed failed outcome — fresh or replay — is exit 1
		// with empty stdout in both modes; the bounded observation cause is
		// a stderr-only diagnostic and never a typed exit-3 conflict.
		if cause == nil {
			cause = errors.New("worktree recovery failed; the requested effect is proven absent")
		}
		return fail(stderr, errors.New(boundedDiagnostic(cause.Error())))
	case "needs-broker":
		// §9.4/§9.5: under --json the recoverable confinement outcome is the
		// exit-0 mutation result with brokerRequest and recoverRequestId;
		// any diagnostic is stderr-only. Human invocation is the pinned
		// exit-1 line with empty stdout.
		if jsonOut {
			if err := writeRecoverJSON(stdout, result); err != nil {
				return fail(stderr, err)
			}
			fmt.Fprintln(stderr, "noise-tasks: "+store.ErrWorktreeRegistrationConfined.Error())
			return 0
		}
		fmt.Fprintln(stderr, "noise-tasks: worktree registration is confined and no broker is available")
		return 1
	case "cleanup-required":
		// §9.4: a cleanup-required result — fresh, no-burn handoff, or exact
		// replay — is the exit-0 mutation result under --json; human
		// invocation is exit 1 with empty stdout and exactly the pinned line
		// on stderr. Under --json a bounded observation cause — e.g. a
		// residual quarantine path with its permission error — is
		// stderr-only.
		if jsonOut {
			if err := writeRecoverJSON(stdout, result); err != nil {
				return fail(stderr, err)
			}
			if cause != nil {
				fmt.Fprintln(stderr, "noise-tasks: "+boundedDiagnostic(cause.Error()))
			}
			return 0
		}
		fmt.Fprintln(stderr, "noise-tasks: worktree cleanup is required")
		return 1
	case "bound", "removed":
		if jsonOut {
			if err := writeRecoverJSON(stdout, result); err != nil {
				return fail(stderr, err)
			}
			return 0
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
		fmt.Fprintln(stdout)
		return 0
	default:
		return fail(stderr, fmt.Errorf("worktree recovery returned invalid status %q", result.Status))
	}
}

func writeRecoverJSON(stdout io.Writer, result *store.WorktreeMutationResult) error {
	return writeJSON(stdout, worktreeRecoverResult{
		SagaID:             result.SagaID,
		Operation:          result.Operation,
		Status:             result.Status,
		RunID:              result.RunID,
		OwnerBoundary:      result.OwnerBoundary,
		Path:               result.Path,
		Branch:             result.Branch,
		BaseCommit:         result.BaseCommit,
		RecoveryGeneration: result.RecoveryGeneration,
		RecoverRequestID:   result.RecoverRequestID,
		BrokerRequest:      result.BrokerRequest,
	})
}

// reportWorktreeRecoverError maps the recover surface errors to exits: the
// common §8.4 replay collision, the §9.4 recovery refusals, and the §9.5
// broker capability conflict are typed exit-3 conflicts; worktree usage
// errors exit 2; a broker refusal or uncertain result and every other
// failure — including a malformed or inconsistent stored receipt — is an
// operational exit 1.
func reportWorktreeRecoverError(stderr io.Writer, jsonOut bool, err error) int {
	var replay *store.ReplayConflict
	if errors.As(err, &replay) {
		return writeLiveConflict(stderr, jsonOut, replayConflictOut(replay), replay.Error())
	}
	var recovery *store.WorktreeRecoveryConflict
	if errors.As(err, &recovery) {
		return writeLiveConflict(stderr, jsonOut, recoveryConflictOut(recovery), recovery.Error())
	}
	var capability *store.WorktreeRecoveryCapabilityConflict
	if errors.As(err, &capability) {
		return writeLiveConflict(stderr, jsonOut, recoveryCapabilityConflictOut(capability), capability.Error())
	}
	var saga *store.WorktreeSagaConflict
	if errors.As(err, &saga) {
		return writeLiveConflict(stderr, jsonOut, sagaConflictOut(saga), saga.Error())
	}
	return reportWorktreeError(stderr, jsonOut, err)
}

func recoveryConflictOut(e *store.WorktreeRecoveryConflict) any {
	return struct {
		Kind   string `json:"kind"`
		SagaID int64  `json:"sagaId"`
		Reason string `json:"reason"`
	}{"worktree-recovery-conflict", e.SagaID, e.Reason}
}

func recoveryCapabilityConflictOut(e *store.WorktreeRecoveryCapabilityConflict) any {
	return struct {
		Kind   string `json:"kind"`
		SagaID int64  `json:"sagaId"`
	}{"worktree-recovery-capability-conflict", e.SagaID}
}
