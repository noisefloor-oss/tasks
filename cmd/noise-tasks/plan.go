package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdPlan implements the authoritative §9.2 plan surface. Unlike migration
// planning, this command reads a caller-supplied structured orchestration
// proposal; validate is read-only and apply is its only publication path.
func cmdPlan(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks plan <validate|apply> ...")
		return 2
	}
	switch args[0] {
	case "validate":
		return cmdPlanValidate(args[1:], stdout, stderr)
	case "apply":
		return cmdPlanApply(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "noise-tasks plan: unknown subcommand %q\n", args[0])
		return 2
	}
}

func cmdPlanValidate(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("plan validate", stderr)
	offline := fs.Bool("offline", false, "validate shape and local graph without opening a store")
	jsonOut := fs.Bool("json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, planPathFirstArgs(args)); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks plan validate [--offline] [--json] <plan.json>")
		return 2
	}
	plan, err := store.ReadOrchestratorPlan(fs.Arg(0))
	if err != nil {
		return fail(stderr, err)
	}
	var validation *store.OrchestratorPlanValidation
	if *offline {
		validation = store.ValidateOrchestratorPlanOffline(plan)
	} else {
		ps, err := openProjectStore()
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		validation, err = ps.db.ValidateOrchestratorPlan(plan)
		if err != nil {
			return fail(stderr, err)
		}
	}
	if *jsonOut {
		if err := writeJSON(stdout, validation); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "schemaVersion: %d\n", validation.SchemaVersion)
	fmt.Fprintf(stdout, "planId: %s\n", validation.PlanID)
	fmt.Fprintf(stdout, "tag: %s\n", validation.Tag)
	fmt.Fprintf(stdout, "planDigest: %s\n", validation.PlanDigest)
	fmt.Fprintf(stdout, "offline: %t\n", validation.Offline)
	fmt.Fprintf(stdout, "inventory: milestones=%d tasks=%d assignments=%d dependencies=%d\n",
		validation.Inventory.Milestones, validation.Inventory.Tasks,
		validation.Inventory.Assignments, validation.Inventory.Dependencies)
	return 0
}

func cmdPlanApply(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("plan apply", stderr)
	digest := fs.String("digest", "", "accepted plan digest")
	requestID := fs.String("request-id", "", "operator request id for replay-safe apply")
	jsonOut := fs.Bool("json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, planPathFirstArgs(args)); !ok {
		return code
	}
	usage := func() int {
		fmt.Fprintln(stderr, "usage: noise-tasks plan apply --digest <planDigest> --request-id <id> [--json] <plan.json>")
		return 2
	}
	if fs.NArg() != 1 {
		return usage()
	}
	if *digest == "" {
		fmt.Fprintln(stderr, "noise-tasks: plan apply requires --digest <planDigest>")
		return 2
	}
	if !store.ValidDigest(*digest) {
		fmt.Fprintln(stderr, "noise-tasks: plan apply --digest must be a lowercase sha256:<hex> digest")
		return 2
	}
	if *requestID == "" {
		fmt.Fprintln(stderr, "noise-tasks: plan apply requires --request-id <id>")
		return 2
	}
	if !store.ValidLocalKey(*requestID) {
		fmt.Fprintln(stderr, "noise-tasks: plan apply --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return 2
	}
	plan, err := store.ReadOrchestratorPlan(fs.Arg(0))
	if err != nil {
		return fail(stderr, err)
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	result, err := ps.db.ApplyOrchestratorPlan(plan, *digest, *requestID)
	if err != nil {
		var replay *store.ReplayConflict
		var planID *store.PlanIDConflict
		if errors.As(err, &replay) || errors.As(err, &planID) {
			fmt.Fprintf(stderr, "noise-tasks: %v\n", err)
			return 3
		}
		return fail(stderr, err)
	}
	if *jsonOut {
		if err := writeJSON(stdout, result); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if result.Replayed {
		fmt.Fprintf(stdout, "replayed: receipt %s\n", result.Receipt["receiptId"])
	} else {
		fmt.Fprintf(stdout, "applied: receipt %s\n", result.Receipt["receiptId"])
	}
	fmt.Fprintf(stdout, "planDigest: %s\n", result.Receipt["planDigest"])
	return 0
}

// planPathFirstArgs admits the §4 spelling (`plan.json --json`) as well as
// the flag-first spelling required by Go's standard flag parser.
func planPathFirstArgs(args []string) []string {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") {
		return args
	}
	out := append([]string(nil), args[1:]...)
	return append(out, args[0])
}
