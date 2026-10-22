package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

type decimalInt64Value struct{ target *int64 }

func (v decimalInt64Value) String() string {
	if v.target == nil {
		return "0"
	}
	return strconv.FormatInt(*v.target, 10)
}

func (v decimalInt64Value) Set(raw string) error {
	if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return fmt.Errorf("must be a base-10 integer")
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fmt.Errorf("must be a base-10 integer")
	}
	*v.target = parsed
	return nil
}

type decimalIntValue struct{ target *int }

func (v decimalIntValue) String() string {
	if v.target == nil {
		return "0"
	}
	return strconv.Itoa(*v.target)
}

func (v decimalIntValue) Set(raw string) error {
	if raw == "" || strings.IndexFunc(raw, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return fmt.Errorf("must be a base-10 integer")
	}
	parsed, err := strconv.ParseInt(raw, 10, 0)
	if err != nil {
		return fmt.Errorf("must be a base-10 integer")
	}
	*v.target = int(parsed)
	return nil
}

// tokenFlagNames are the fencing-capability flags that gain a -file twin
// (§4): supplying the capability as file content keeps it out of argv,
// process listings, and session transcripts.
var tokenFlagNames = map[string]bool{
	"token":         true,
	"claim-token":   true,
	"attempt-token": true,
	"saga-token":    true,
}

func decimalInt64Var(fs *flag.FlagSet, target *int64, name, usage string) {
	fs.Var(decimalInt64Value{target: target}, name, usage)
	if tokenFlagNames[name] {
		fs.String(name+"-file", "", "read "+usage+" from this file (mutually exclusive with --"+name+")")
	}
}

// resolveTokenFileFlags reads every explicitly supplied <token-flag>-file
// twin into its integer flag. The file must be a regular, non-symlink
// file of at most 64 bytes holding one trimmed base-10 integer; the
// integer flag and its file twin are mutually exclusive.
func resolveTokenFileFlags(fs *flag.FlagSet) error {
	var ferr error
	fs.Visit(func(f *flag.Flag) {
		if ferr != nil || !strings.HasSuffix(f.Name, "-file") {
			return
		}
		base := strings.TrimSuffix(f.Name, "-file")
		if !tokenFlagNames[base] {
			return
		}
		if flagExplicitlySet(fs, base) {
			ferr = fmt.Errorf("--%s and --%s are mutually exclusive", base, f.Name)
			return
		}
		path := f.Value.String()
		// Open without following a final symlink and validate on the
		// opened descriptor, so replacing the pathname between the
		// checks and the read cannot bypass the §4 guarantees.
		file, err := openTokenFile(path)
		if err != nil {
			ferr = fmt.Errorf("--%s: %v", f.Name, err)
			return
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			ferr = fmt.Errorf("--%s: %v", f.Name, err)
			return
		}
		if !info.Mode().IsRegular() {
			file.Close()
			ferr = fmt.Errorf("--%s: not a regular file: %s", f.Name, path)
			return
		}
		if info.Size() > 64 {
			file.Close()
			ferr = fmt.Errorf("--%s: file is too large", f.Name)
			return
		}
		buf := make([]byte, 65)
		n, err := file.Read(buf)
		file.Close()
		if err != nil {
			ferr = fmt.Errorf("--%s: %v", f.Name, err)
			return
		}
		if n > 64 {
			ferr = fmt.Errorf("--%s: file is too large", f.Name)
			return
		}
		trimmed := strings.TrimSpace(string(buf[:n]))
		if trimmed == "" {
			ferr = fmt.Errorf("--%s: file is empty", f.Name)
			return
		}
		if err := fs.Lookup(base).Value.Set(trimmed); err != nil {
			ferr = fmt.Errorf("--%s: %v", f.Name, err)
			return
		}
	})
	return ferr
}

// flagExplicitlySet reports whether the named flag itself was supplied,
// ignoring its -file twin (used for mutual exclusion).
func flagExplicitlySet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		set = set || f.Name == name
	})
	return set
}

func decimalIntVar(fs *flag.FlagSet, target *int, name, usage string) {
	fs.Var(decimalIntValue{target: target}, name, usage)
}

func newRunFlagSet(name string, stderr io.Writer, cf *commonFlags, allowTag bool) *flag.FlagSet {
	fs := newCommandFlagSet(name, stderr)
	if allowTag {
		fs.StringVar(&cf.tag, "tag", "", `tag to operate on (default: sole tag, else "main")`)
	}
	fs.BoolVar(&cf.json, "json", false, "machine-readable JSON on stdout")
	return fs
}

func cmdRun(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks run <start|retry|renew|finish|cancel|show|list> [options]")
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, "usage: noise-tasks run <start|retry|renew|finish|cancel|show|list> [options]")
		return 0
	case "start":
		return cmdRunStart(args[1:], stdout, stderr)
	case "retry":
		return cmdRunRetry(args[1:], stdout, stderr)
	case "renew":
		return cmdRunRenew(args[1:], stdout, stderr)
	case "finish":
		return cmdRunFinish(args[1:], stdout, stderr)
	case "cancel":
		return cmdRunCancel(args[1:], stdout, stderr)
	case "show":
		return cmdRunShow(args[1:], stdout, stderr)
	case "list":
		return cmdRunList(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "noise-tasks run: unknown subcommand %q\n", args[0])
		return 2
	}
}

type runCapabilityFlags struct {
	owner        string
	claimToken   int64
	attempt      int64
	attemptToken int64
	requestID    string
	ttl          int
}

// flagWasSet reports whether the named flag was supplied explicitly. A
// supplied <token-flag>-file twin satisfies its integer flag.
func flagWasSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		set = set || f.Name == name || (tokenFlagNames[name] && f.Name == name+"-file")
	})
	return set
}

func (f *runCapabilityFlags) register(fs *flag.FlagSet, withTTL bool) {
	fs.StringVar(&f.owner, "owner", "", "run owner identity")
	decimalInt64Var(fs, &f.claimToken, "claim-token", "current claim fencing token")
	decimalInt64Var(fs, &f.attempt, "attempt", "current attempt number")
	decimalInt64Var(fs, &f.attemptToken, "attempt-token", "current attempt fencing token")
	fs.StringVar(&f.requestID, "request-id", "", "request id for replay")
	if withTTL {
		decimalIntVar(fs, &f.ttl, "ttl", "attempt lease time-to-live in seconds")
	}
}

func (f *runCapabilityFlags) validate(fs *flag.FlagSet, stderr io.Writer, command string, withTTL bool) bool {
	if f.owner == "" {
		fmt.Fprintf(stderr, "noise-tasks %s: --owner is required\n", command)
		return false
	}
	if err := store.ValidateOwnerLabel(f.owner); err != nil {
		fmt.Fprintf(stderr, "noise-tasks %s: --owner %v\n", command, err)
		return false
	}
	if f.claimToken <= 0 || f.attempt <= 0 || f.attemptToken <= 0 {
		fmt.Fprintf(stderr, "noise-tasks %s: --claim-token, --attempt, and --attempt-token must be positive integers\n", command)
		return false
	}
	if !store.ValidLocalKey(f.requestID) {
		fmt.Fprintf(stderr, "noise-tasks %s: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}\n", command)
		return false
	}
	if withTTL && flagWasSet(fs, "ttl") && (f.ttl < 1 || f.ttl > store.MaxRunTTL) {
		fmt.Fprintf(stderr, "noise-tasks %s: --ttl must be between 1 and %d seconds when supplied\n", command, store.MaxRunTTL)
		return false
	}
	return true
}

func parsePositiveRunID(raw, command string, stderr io.Writer) (int64, bool) {
	runID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || runID <= 0 {
		fmt.Fprintf(stderr, "noise-tasks %s: run id must be a positive integer\n", command)
		return 0, false
	}
	return runID, true
}

func reportRunError(stderr io.Writer, jsonOut bool, err error) int {
	var replay *store.ReplayConflict
	if errors.As(err, &replay) {
		return writeLiveConflict(stderr, jsonOut, replayConflictOut(replay), replay.Error())
	}
	var conflict *store.RunConflict
	if errors.As(err, &conflict) {
		return writeLiveConflict(stderr, jsonOut, runConflictOut(conflict), conflict.Error())
	}
	return fail(stderr, err)
}

func cmdRunStart(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("run start", stderr, &cf, true)
	owner := fs.String("owner", "", "run owner identity")
	var claimToken int64
	decimalInt64Var(fs, &claimToken, "claim-token", "current claim fencing token")
	requestID := fs.String("request-id", "", "request id for run replay")
	var ttl int
	decimalIntVar(fs, &ttl, "ttl", "attempt lease time-to-live in seconds")
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks run start <task> --owner <owner> --claim-token <token> --request-id <id> [--ttl <seconds>]")
		return 2
	}
	if *owner == "" {
		fmt.Fprintln(stderr, "noise-tasks run start: --owner is required")
		return 2
	}
	if err := store.ValidateOwnerLabel(*owner); err != nil {
		fmt.Fprintf(stderr, "noise-tasks run start: --owner %v\n", err)
		return 2
	}
	if claimToken <= 0 {
		fmt.Fprintln(stderr, "noise-tasks run start: --claim-token must be a positive integer")
		return 2
	}
	if *requestID == "" {
		fmt.Fprintln(stderr, "noise-tasks run start: --request-id is required")
		return 2
	}
	if !store.ValidLocalKey(*requestID) {
		fmt.Fprintln(stderr, "noise-tasks run start: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return 2
	}
	ttlSupplied := false
	fs.Visit(func(f *flag.Flag) { ttlSupplied = ttlSupplied || f.Name == "ttl" })
	if ttlSupplied && (ttl < 1 || ttl > store.MaxRunTTL) {
		fmt.Fprintf(stderr, "noise-tasks run start: --ttl must be between 1 and %d seconds when supplied\n", store.MaxRunTTL)
		return 2
	}
	_, subtask, err := backlog.ParseTaskReference(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "noise-tasks run start: invalid task: %v\n", err)
		return 2
	}
	if subtask != "" {
		fmt.Fprintln(stderr, "noise-tasks run start: task must be a top-level id")
		return 2
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	result, _, err := ps.db.StartRun(tag, fs.Arg(0), *owner, claimToken, *requestID, ttl)
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
	if cf.json {
		if err := writeJSON(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "runId=%d attempt=%d leaseGeneration=%d attemptToken=%d expiresAt=%s\n",
		result.RunID, result.Attempt, result.LeaseGeneration, result.AttemptToken, result.ExpiresAt)
	return 0
}

func cmdRunRetry(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("run retry", stderr, &cf, false)
	var capability runCapabilityFlags
	capability.register(fs, true)
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks run retry <runId> --owner <owner> --claim-token <token> --attempt <n> --attempt-token <token> --request-id <id> [--ttl <seconds>]")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "run retry", stderr)
	if !ok || !capability.validate(fs, stderr, "run retry", true) {
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	result, _, err := ps.db.RetryRun(runID, capability.owner, capability.claimToken, capability.attempt,
		capability.attemptToken, capability.requestID, capability.ttl)
	if err != nil {
		return reportRunError(stderr, cf.json, err)
	}
	return writeRunStartResult(stdout, stderr, cf.json, result)
}

func cmdRunRenew(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("run renew", stderr, &cf, false)
	var capability runCapabilityFlags
	capability.register(fs, true)
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks run renew <runId> --owner <owner> --claim-token <token> --attempt <n> --attempt-token <token> --request-id <id> [--ttl <seconds>]")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "run renew", stderr)
	if !ok || !capability.validate(fs, stderr, "run renew", true) {
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	result, _, err := ps.db.RenewRun(runID, capability.owner, capability.claimToken, capability.attempt,
		capability.attemptToken, capability.requestID, capability.ttl)
	if err != nil {
		return reportRunError(stderr, cf.json, err)
	}
	if cf.json {
		if err := writeJSON(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "runId=%d attempt=%d leaseGeneration=%d claimToken=%d claimExpiresAt=%s attemptToken=%d attemptExpiresAt=%s\n",
		result.RunID, result.Attempt, result.LeaseGeneration, result.ClaimToken, result.ClaimExpiresAt,
		result.AttemptToken, result.AttemptExpiresAt)
	return 0
}

func cmdRunFinish(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("run finish", stderr, &cf, false)
	var capability runCapabilityFlags
	capability.register(fs, false)
	outcome := fs.String("outcome", "", "terminal outcome: succeeded or failed")
	taskStatus := fs.String("task-status", "", "optional task status to publish")
	var revision int
	decimalIntVar(fs, &revision, "revision", "task revision for --task-status")
	resultDigest := fs.String("result-digest", "", "optional lowercase sha256 digest")
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks run finish <runId> --owner <owner> --claim-token <token> --attempt <n> --attempt-token <token> --outcome <succeeded|failed> --request-id <id> [--task-status <status> --revision <n>] [--result-digest <digest>]")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "run finish", stderr)
	if !ok || !capability.validate(fs, stderr, "run finish", false) {
		return 2
	}
	if *outcome != "succeeded" && *outcome != "failed" {
		fmt.Fprintln(stderr, "noise-tasks run finish: --outcome must be succeeded or failed")
		return 2
	}
	statusSet, revisionSet := flagWasSet(fs, "task-status"), flagWasSet(fs, "revision")
	if statusSet != revisionSet {
		fmt.Fprintln(stderr, "noise-tasks run finish: --task-status and --revision must be supplied together")
		return 2
	}
	if statusSet && (!backlog.ValidStatuses[*taskStatus] || revision <= 0) {
		fmt.Fprintln(stderr, "noise-tasks run finish: --task-status must be valid and --revision must be positive")
		return 2
	}
	if *outcome == "failed" && *taskStatus == "done" {
		fmt.Fprintln(stderr, "noise-tasks run finish: failed outcome cannot publish task status done")
		return 2
	}
	if flagWasSet(fs, "result-digest") && !store.ValidDigest(*resultDigest) {
		fmt.Fprintln(stderr, "noise-tasks run finish: --result-digest must be lowercase sha256:<64-hex>")
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	result, _, err := ps.db.FinishRun(runID, capability.owner, capability.claimToken, capability.attempt,
		capability.attemptToken, *outcome, capability.requestID, *taskStatus, revision, *resultDigest)
	if err != nil {
		return reportRunError(stderr, cf.json, err)
	}
	if cf.json {
		if err := writeJSON(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	task := "-"
	if result.Task != nil {
		task = fmt.Sprintf("%s/%s/%d", result.Task.ID, result.Task.Status, result.Task.Revision)
	}
	digest := "-"
	if result.TerminalResultDigest != nil {
		digest = *result.TerminalResultDigest
	}
	fmt.Fprintf(stdout, "runId=%d runStatus=%s attempt=%d attemptStatus=%s task=%s terminalResultDigest=%s\n",
		result.RunID, result.RunStatus, result.Attempt, result.AttemptStatus, task, digest)
	return 0
}

func cmdRunCancel(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("run cancel", stderr, &cf, false)
	var capability runCapabilityFlags
	capability.register(fs, false)
	force := fs.Bool("force", false, "operator cancellation without run capabilities")
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks run cancel <runId> (--force | --owner <owner> --claim-token <token> --attempt <n> --attempt-token <token>) --request-id <id>")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "run cancel", stderr)
	if !ok {
		return 2
	}
	if !store.ValidLocalKey(capability.requestID) {
		fmt.Fprintln(stderr, "noise-tasks run cancel: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return 2
	}
	if *force {
		for _, name := range []string{"owner", "claim-token", "attempt", "attempt-token"} {
			if flagWasSet(fs, name) {
				fmt.Fprintln(stderr, "noise-tasks run cancel: --force accepts no owner or capability flags")
				return 2
			}
		}
	} else if !capability.validate(fs, stderr, "run cancel", false) {
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	result, _, err := ps.db.CancelRun(runID, capability.owner, capability.claimToken, capability.attempt,
		capability.attemptToken, *force, capability.requestID)
	if err != nil {
		return reportRunError(stderr, cf.json, err)
	}
	if cf.json {
		if err := writeJSON(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "runId=%d runStatus=%s attempt=%d attemptStatus=%s forced=%t\n",
		result.RunID, result.RunStatus, result.Attempt, result.AttemptStatus, result.Forced)
	return 0
}

func cmdRunShow(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("run show", stderr, &cf, false)
	if code, ok := parseFlags(fs, runIDFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks run show <runId> [--json]")
		return 2
	}
	runID, ok := parsePositiveRunID(fs.Arg(0), "run show", stderr)
	if !ok {
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	record, err := ps.db.GetRun(runID)
	if err != nil {
		return fail(stderr, err)
	}
	if record == nil {
		return fail(stderr, fmt.Errorf("run %d not found", runID))
	}
	if cf.json {
		if err := writeJSON(stdout, record); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	attempts, err := json.Marshal(record.Attempts)
	if err != nil {
		return fail(stderr, err)
	}
	receipts, err := json.Marshal(record.Receipts)
	if err != nil {
		return fail(stderr, err)
	}
	terminalAt, terminalDigest := "null", "null"
	if record.TerminalAt != nil {
		terminalAt = *record.TerminalAt
	}
	if record.TerminalResultDigest != nil {
		terminalDigest = *record.TerminalResultDigest
	}
	fmt.Fprintf(stdout, "runId=%d tag=%s task=%s owner=%s status=%s currentAttempt=%d startedAt=%s updatedAt=%s terminalAt=%s terminalResultDigest=%s attempts=%s receipts=%s\n",
		record.RunID, record.Tag, record.Task, record.Owner, record.Status, record.CurrentAttempt,
		record.StartedAt, record.UpdatedAt, terminalAt, terminalDigest, attempts, receipts)
	return 0
}

func cmdRunList(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newRunFlagSet("run list", stderr, &cf, false)
	task := fs.String("task", "", "filter by top-level task id")
	status := fs.String("status", "", "filter by run status")
	owner := fs.String("owner", "", "filter by owner")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks run list [--task <id>] [--status <status>] [--owner <owner>] [--json]")
		return 2
	}
	if *task != "" {
		_, subtask, err := backlog.ParseTaskReference(*task)
		if err != nil || subtask != "" {
			fmt.Fprintln(stderr, "noise-tasks run list: --task must be a top-level task id")
			return 2
		}
	}
	if *status != "" && *status != "running" && *status != "retryable" && *status != "succeeded" && *status != "cancelled" {
		fmt.Fprintln(stderr, "noise-tasks run list: --status is invalid")
		return 2
	}
	if *owner != "" {
		if err := store.ValidateOwnerLabel(*owner); err != nil {
			fmt.Fprintf(stderr, "noise-tasks run list: --owner %v\n", err)
			return 2
		}
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	runs, err := ps.db.ListRuns(*task, *status, *owner)
	if err != nil {
		return fail(stderr, err)
	}
	if cf.json {
		if err := writeJSON(stdout, runs); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, run := range runs {
		fmt.Fprintf(stdout, "runId=%d tag=%s task=%s owner=%s status=%s currentAttempt=%d updatedAt=%s\n",
			run.RunID, run.Tag, run.Task, run.Owner, run.Status, run.CurrentAttempt, run.UpdatedAt)
	}
	return 0
}

func writeRunStartResult(stdout, stderr io.Writer, jsonOut bool, result *store.RunStartResult) int {
	if jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "runId=%d attempt=%d leaseGeneration=%d attemptToken=%d expiresAt=%s\n",
		result.RunID, result.Attempt, result.LeaseGeneration, result.AttemptToken, result.ExpiresAt)
	return 0
}

func runConflictOut(e *store.RunConflict) any {
	return struct {
		Kind      string `json:"kind"`
		Operation string `json:"operation"`
		Tag       string `json:"tag,omitempty"`
		Task      string `json:"task,omitempty"`
		RunID     int64  `json:"runId,omitempty"`
		Attempt   int64  `json:"attempt,omitempty"`
	}{"run-conflict", e.Operation, e.Tag, e.Task, e.RunID, e.Attempt}
}

// runIDFirstArgs admits the documented positional-first spelling while also
// accepting flag-first input from scripts using Go's standard flag style.
func runIDFirstArgs(args []string) []string {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
		return args
	}
	out := append([]string(nil), args[1:]...)
	return append(out, args[0])
}
