package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// liveFlags are the §8 live-scoped flags added to backlog commands.
type liveFlags struct {
	revision     int
	owner        string
	token        int64
	ttl          int
	renew        bool
	force        bool
	worktree     bool
	keepWorktree bool
}

type taskMutationRunFlags struct {
	run          int64
	attempt      int64
	attemptToken int64
}

func (rf *taskMutationRunFlags) register(fs *flag.FlagSet) {
	decimalInt64Var(fs, &rf.run, "run", "orchestrator run id")
	decimalInt64Var(fs, &rf.attempt, "attempt", "current run attempt")
	decimalInt64Var(fs, &rf.attemptToken, "attempt-token", "current attempt fencing token")
}

func taskMutationGate(fs *flag.FlagSet, lf *liveFlags, rf *taskMutationRunFlags,
	stderr io.Writer, command string) (*store.ClaimGate, bool) {
	ownerSet, tokenSet := flagWasSet(fs, "owner"), flagWasSet(fs, "token")
	runSet, attemptSet := flagWasSet(fs, "run"), flagWasSet(fs, "attempt")
	attemptTokenSet := flagWasSet(fs, "attempt-token")
	if ownerSet != tokenSet {
		fmt.Fprintf(stderr, "noise-tasks %s: --owner and --token must be supplied together\n", command)
		return nil, false
	}
	runCount := 0
	for _, supplied := range []bool{runSet, attemptSet, attemptTokenSet} {
		if supplied {
			runCount++
		}
	}
	if runCount != 0 && runCount != 3 {
		fmt.Fprintf(stderr, "noise-tasks %s: --run, --attempt, and --attempt-token must be supplied together\n", command)
		return nil, false
	}
	if runCount == 3 && !ownerSet {
		fmt.Fprintf(stderr, "noise-tasks %s: run gates require --owner and --token\n", command)
		return nil, false
	}
	if runCount == 3 && (rf.run <= 0 || rf.attempt <= 0 || rf.attemptToken <= 0) {
		fmt.Fprintf(stderr, "noise-tasks %s: --run, --attempt, and --attempt-token must be positive integers\n", command)
		return nil, false
	}
	if !ownerSet && runCount == 0 {
		return nil, true
	}
	gate := &store.ClaimGate{Owner: lf.owner, Token: lf.token, RunID: rf.run,
		Attempt: rf.attempt, AttemptToken: rf.attemptToken}
	if err := store.ValidateTaskRunGate(gate); err != nil {
		fmt.Fprintf(stderr, "noise-tasks %s: %v\n", command, err)
		return nil, false
	}
	return gate, true
}

func (rf *taskMutationRunFlags) rejectOnFixturePath(fs *flag.FlagSet, stderr io.Writer, command string) int {
	if !flagWasSet(fs, "run") && !flagWasSet(fs, "attempt") && !flagWasSet(fs, "attempt-token") {
		return -1
	}
	fmt.Fprintf(stderr, "noise-tasks %s: --run, --attempt, and --attempt-token are not accepted with --file\n", command)
	return 2
}

func (lf *liveFlags) register(fs *flag.FlagSet) {
	fs.IntVar(&lf.revision, "revision", 0, "expected task revision (required for mutations)")
	fs.StringVar(&lf.owner, "owner", "", "claim owner identity")
	decimalInt64Var(fs, &lf.token, "token", "claim fencing token")
	fs.IntVar(&lf.ttl, "ttl", 0, "claim time-to-live in seconds")
	fs.BoolVar(&lf.renew, "renew", false, "renew a claim by token")
	fs.BoolVar(&lf.force, "force", false, "force a release or top-level removal")
}

// registerRemovedWorktree registers the --worktree flag the §9.4 v5 cutover
// removed from claim. The flag still parses so its use is the guided usage
// error, never a bare flag-parser failure.
func (lf *liveFlags) registerRemovedWorktree(fs *flag.FlagSet) {
	fs.BoolVar(&lf.worktree, "worktree", false, "removed by the v5 cutover; use worktree provision")
}

// registerRemovedKeepWorktree registers the --keep-worktree flag the §9.4
// v5 cutover removed from release and set-status. Branch retention and
// explicit worktree remove --force replace keep flags.
func (lf *liveFlags) registerRemovedKeepWorktree(fs *flag.FlagSet) {
	fs.BoolVar(&lf.keepWorktree, "keep-worktree", false, "removed by the v5 cutover; branch retention replaces keep flags")
}

// liveFlagsSet reports whether any database-only flag was supplied.
func (lf *liveFlags) liveFlagsSet() bool {
	return lf.revision != 0 || lf.owner != "" || lf.token != 0 || lf.ttl != 0 || lf.renew || lf.worktree || lf.keepWorktree
}

// rejectOnFixturePath returns exit code 2 when database-only flags appear on
// a --file fixture command: a standalone document supports no claims,
// worktree bindings, revisions, or feed (SPEC §8.1).
func (lf *liveFlags) rejectOnFixturePath(stderr io.Writer, cmd string) int {
	if !lf.liveFlagsSet() {
		return -1
	}
	fmt.Fprintf(stderr, "noise-tasks %s: --revision, --owner, --token, --ttl, --renew, --worktree, and --keep-worktree are not accepted with --file\n", cmd)
	return 2
}

// requireRevision enforces the §8.2 CAS contract: every mutation of an
// existing task's §1 fields or status supplies --revision.
func (lf *liveFlags) requireRevision(stderr io.Writer) bool {
	if lf.revision <= 0 {
		fmt.Fprintln(stderr, "noise-tasks: mutations require --revision <n>")
		return false
	}
	return true
}

// liveTaskOut is the database task output shape: §1 fields plus revision and
// claim (owner/acquiredAt/expiresAt only — never the token).
type liveTaskOut struct {
	taskOut
	Revision int       `json:"revision"`
	Claim    *claimOut `json:"claim,omitempty"`
}

type claimOut struct {
	Owner      string `json:"owner"`
	AcquiredAt string `json:"acquiredAt"`
	ExpiresAt  string `json:"expiresAt"`
}

func toLiveTaskOut(t *store.Task) liveTaskOut {
	task, err := storeToBacklogTask(t)
	if err != nil {
		task = &backlog.Task{ID: t.ID}
	}
	out := liveTaskOut{taskOut: toTaskOut(task), Revision: t.Revision}
	if t.Claim != nil {
		out.Claim = &claimOut{
			Owner:      t.Claim.Owner,
			AcquiredAt: t.Claim.AcquiredAt,
			ExpiresAt:  t.Claim.ExpiresAt,
		}
	}
	return out
}

// writeLiveConflict writes the §8 exit-3 conflict diagnostics. stdout stays
// empty; under --json a single JSON value goes to stderr, else a
// human-readable diagnostic.
func writeLiveConflict(stderr io.Writer, jsonOut bool, value any, human string) int {
	if jsonOut {
		data, err := json.MarshalIndent(value, "", "  ")
		if err == nil {
			fmt.Fprintf(stderr, "%s\n", data)
		}
	} else {
		fmt.Fprintln(stderr, "noise-tasks: "+human)
	}
	return 3
}

func staleConflictOut(e *store.StaleWriteConflict) any {
	return struct {
		Kind         string `json:"kind"`
		Tag          string `json:"tag"`
		Task         string `json:"task"`
		Supplied     int    `json:"supplied"`
		Current      int    `json:"current"`
		Status       string `json:"status"`
		ClaimOwner   string `json:"claimOwner,omitempty"`
		ClaimExpires string `json:"claimExpires,omitempty"`
	}{"stale-write", e.Tag, e.ID, e.Supplied, e.Current, e.Status, e.ClaimOwner, e.ClaimExpiry}
}

func claimConflictOut(e *store.ClaimConflict) any {
	// The fencing token is a write capability and is redacted from every
	// surface except claim acquisition/renew output (SPEC §8.2).
	return struct {
		Kind      string `json:"kind"`
		Tag       string `json:"tag"`
		Task      string `json:"task"`
		Owner     string `json:"owner,omitempty"`
		ExpiresAt string `json:"expiresAt,omitempty"`
	}{e.Kind, e.Tag, e.ID, e.Owner, e.Expiry}
}

func replayConflictOut(e *store.ReplayConflict) any {
	return struct {
		Kind      string `json:"kind"`
		Receipt   string `json:"receiptKind"`
		RequestID string `json:"requestId"`
	}{"replay-conflict", e.Kind, e.RequestID}
}

// cmdLive dispatches the surviving `live` subcommands (SPEC §8): the §8.4
// change feed. The §8.6 migration shipped only in the v1.4.0
// migration-capable release, and the retired projection-synchronization
// subcommands do not exist.
func cmdLive(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks live <feed> [options]")
		return 2
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "feed":
		return cmdLiveFeed(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "noise-tasks live: unknown subcommand %q\n", sub)
		return 2
	}
}

func cmdClaim(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("claim", stderr, &cf)
	var lf liveFlags
	lf.register(fs)
	lf.registerRemovedWorktree(fs)
	requestID := fs.String("request-id", "", "request id for §8.4 acquisition replay")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks claim [options] <id>")
		return 2
	}
	id := fs.Arg(0)
	if cf.file != "" {
		fmt.Fprintln(stderr, "noise-tasks claim: --file is not accepted for claim")
		return 2
	}
	// §9.4 v5 cutover: claim --worktree is removed; the guided usage error
	// replaces the legacy compound surface.
	if lf.worktree {
		return worktreeCutoverRefusal(stderr, "claim: --worktree")
	}
	if lf.owner == "" {
		fmt.Fprintln(stderr, "noise-tasks claim: --owner is required")
		return 2
	}
	// One owner-label grammar governs claim owners, claim-next owners and
	// its --assignment filter, and §9.2 plan owners (SPEC §8.3); it is
	// enforced before any store is opened.
	if err := store.ValidateOwnerLabel(lf.owner); err != nil {
		fmt.Fprintf(stderr, "noise-tasks claim: --owner %v\n", err)
		return 2
	}
	if lf.renew && lf.token == 0 {
		fmt.Fprintln(stderr, "noise-tasks claim: --token is required with --renew")
		return 2
	}
	if lf.renew && *requestID != "" {
		fmt.Fprintln(stderr, "noise-tasks claim: --request-id is not accepted with --renew (renewal is token CAS)")
		return 2
	}
	if !lf.renew {
		if *requestID == "" {
			fmt.Fprintln(stderr, "noise-tasks claim: --request-id is required to acquire a claim")
			return 2
		}
		if !store.ValidLocalKey(*requestID) {
			fmt.Fprintf(stderr, "noise-tasks claim: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}\n")
			return 2
		}
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()

	var claim *store.Claim
	var revision int
	if lf.renew {
		claim, revision, err = ps.db.RenewClaim(tag, id, lf.owner, lf.token, lf.ttl)
	} else {
		// Replay returns the originally recorded acquisition's task and
		// token in the acquisition's own output shape (§8.4).
		claim, revision, _, err = ps.db.AcquireClaim(tag, id, lf.owner, lf.ttl, *requestID)
	}
	if err != nil {
		var rc *store.ReplayConflict
		if errors.As(err, &rc) {
			return writeLiveConflict(stderr, cf.json, replayConflictOut(rc), rc.Error())
		}
		var cc *store.ClaimConflict
		if errors.As(err, &cc) {
			return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
		}
		return fail(stderr, err)
	}

	if cf.json {
		out := struct {
			Token     int64  `json:"token"`
			ExpiresAt string `json:"expiresAt"`
			Revision  int    `json:"revision"`
		}{claim.Token, claim.ExpiresAt, revision}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "token=%d expiresAt=%s revision=%d\n", claim.Token, claim.ExpiresAt, revision)
	return 0
}

func cmdRelease(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("release", stderr, &cf)
	var lf liveFlags
	lf.register(fs)
	lf.registerRemovedKeepWorktree(fs)
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks release [options] <id>")
		return 2
	}
	id := fs.Arg(0)
	if cf.file != "" {
		fmt.Fprintln(stderr, "noise-tasks release: --file is not accepted for release")
		return 2
	}
	// §9.4 v5 cutover: --keep-worktree is removed; the guided usage error
	// replaces the keep flag.
	if lf.keepWorktree {
		return worktreeCutoverRefusal(stderr, "release: --keep-worktree")
	}
	if lf.owner == "" {
		fmt.Fprintln(stderr, "noise-tasks release: --owner is required")
		return 2
	}
	if err := store.ValidateOwnerLabel(lf.owner); err != nil {
		fmt.Fprintf(stderr, "noise-tasks release: --owner %v\n", err)
		return 2
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()

	if err := ps.db.ReleaseClaim(tag, id, lf.owner, lf.token, lf.force); err != nil {
		var cc *store.ClaimConflict
		if errors.As(err, &cc) {
			return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
		}
		return fail(stderr, err)
	}

	if cf.json {
		out := struct {
			ID string `json:"id"`
		}{id}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "released claim on task %s\n", id)
	return 0
}

func cmdLiveFeed(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("live feed", stderr, &cf)
	since := fs.Int64("since", 0, "replay events after this sequence")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "noise-tasks live feed: unexpected argument")
		return 2
	}
	if cf.file != "" {
		fmt.Fprintln(stderr, "noise-tasks live feed: --file is not accepted")
		return 2
	}

	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	feed, err := ps.db.Feed(*since)
	if err != nil {
		return fail(stderr, err)
	}
	if feed.Truncated {
		if !cf.json {
			fmt.Fprintln(stderr, "noise-tasks: live feed truncated; full resync required")
		}
	}
	if cf.json {
		if err := writeJSON(stdout, feed); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "oldest retained sequence: %d\n", feed.OldestRetainedSeq)
	for _, e := range feed.Events {
		fmt.Fprintf(stdout, "%d %s %s", e.Seq, e.Kind, e.Tag)
		if e.Task != "" {
			fmt.Fprintf(stdout, " task=%s", e.Task)
		}
		if e.Status != "" {
			fmt.Fprintf(stdout, " status=%s", e.Status)
		}
		if e.Owner != "" {
			fmt.Fprintf(stdout, " owner=%s", e.Owner)
		}
		if e.Count > 0 {
			fmt.Fprintf(stdout, " count=%d", e.Count)
		}
		fmt.Fprintln(stdout)
	}
	return 0
}
