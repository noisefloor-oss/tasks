package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/buildinfo"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdMigrateAdopt implements §8.6A: migrate adopt plan and migrate adopt
// apply, the one-way adoption of one explicitly named retired layout into a
// fresh store. Adoption commands never accept --file and never consult a
// projection.
func cmdMigrateAdopt(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks migrate adopt <plan|apply> ...")
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, "usage: noise-tasks migrate adopt <plan|apply> ...")
		return 0
	case "plan":
		return cmdMigrateAdoptPlan(args[1:], stdout, stderr)
	case "apply":
		return cmdMigrateAdoptApply(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: noise-tasks migrate adopt <plan|apply> ...")
		return 2
	}
}

// cmdMigrateAdoptPlan verifies the §8.5 adoption bundle against the exact
// named layout document and atomically publishes the strict plan at
// <storeDir>/adoption-plan.json. It is context-aware but never opens,
// requires, or publishes the SQLite authority.
func cmdMigrateAdoptPlan(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("migrate adopt plan", stderr)
	var layout string
	var jsonOut bool
	fs.StringVar(&layout, "layout", "", "explicit retired-layout document to adopt (inside the family boundary)")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 1 || layout == "" {
		fmt.Fprintln(stderr, "usage: noise-tasks migrate adopt plan --layout <path> [--json] <bundle.ntbackup>")
		return 2
	}
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	common, ok := backlog.GitCommonDir(boundary)
	if !ok {
		fmt.Fprintln(stderr, "noise-tasks: no Git repository; adoption requires a repository family")
		return 1
	}
	commonCanon, err := canonicalizePath(common)
	if err != nil {
		return fail(stderr, err)
	}
	owner, err := derivedOwnerBoundary(boundary)
	if err != nil {
		return fail(stderr, err)
	}
	storeDir := filepath.Join(commonCanon, "noise-tasks")
	plan, err := store.CreateAdoptionPlan(fs.Arg(0), layout, commonCanon, owner, storeDir, "noise-tasks "+buildinfo.Version)
	if err != nil {
		return fail(stderr, err)
	}
	planPath := filepath.Join(storeDir, store.AdoptionPlanFile)
	if jsonOut {
		out := struct {
			Plan       *store.MigrationPlan `json:"plan"`
			PlanPath   string               `json:"planPath"`
			FamilyID   string               `json:"familyId"`
			PlanDigest string               `json:"planDigest"`
		}{plan, planPath, plan.FamilyID, plan.PlanDigest}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "plan: %s\n", planPath)
	fmt.Fprintf(stdout, "familyId: %s\n", plan.FamilyID)
	fmt.Fprintf(stdout, "planDigest: %s\n", plan.PlanDigest)
	return 0
}

// cmdMigrateAdoptApply performs the §8.6A cutover: it requires the derived
// adoption plan, its accepted --digest, the same verified bundle, --offline,
// and a request id, and publishes the fresh current SQLite target from the
// bundle under the exclusive maintenance lock. The layout document is never
// removed.
func cmdMigrateAdoptApply(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("migrate adopt apply", stderr)
	var digest, requestID string
	var offline, jsonOut bool
	fs.StringVar(&digest, "digest", "", "accepted plan digest from migrate adopt plan")
	fs.BoolVar(&offline, "offline", false, "acknowledge that all unsupported writers are stopped")
	fs.StringVar(&requestID, "request-id", "", "operator request id for replay-safe adoption")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	usage := func() int {
		fmt.Fprintln(stderr, "usage: noise-tasks migrate adopt apply --digest <planDigest> --offline --request-id <id> [--json] <bundle.ntbackup>")
		return 2
	}
	if fs.NArg() != 1 {
		return usage()
	}
	// All required flags validate before any effect (§8.6A follows §8.6).
	if !offline {
		fmt.Fprintln(stderr, "noise-tasks: migrate adopt apply requires --offline (acknowledging all unsupported writers are stopped)")
		return 2
	}
	if digest == "" {
		fmt.Fprintln(stderr, "noise-tasks: migrate adopt apply requires --digest <planDigest>")
		return 2
	}
	if !store.ValidDigest(digest) {
		fmt.Fprintln(stderr, "noise-tasks: migrate adopt apply --digest must be a lowercase sha256:<hex> digest")
		return 2
	}
	if requestID == "" {
		fmt.Fprintln(stderr, "noise-tasks: migrate adopt apply requires --request-id <id>")
		return 2
	}
	if !store.ValidLocalKey(requestID) {
		fmt.Fprintln(stderr, "noise-tasks: migrate adopt apply --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return 2
	}
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	common, ok := backlog.GitCommonDir(boundary)
	if !ok {
		fmt.Fprintln(stderr, "noise-tasks: no Git repository; adoption requires a repository family")
		return 1
	}
	commonCanon, err := canonicalizePath(common)
	if err != nil {
		return fail(stderr, err)
	}
	owner, err := derivedOwnerBoundary(boundary)
	if err != nil {
		return fail(stderr, err)
	}
	recovery, err := store.ReadAdoptionRecovery(filepath.Join(commonCanon, "noise-tasks"))
	if err != nil {
		return fail(stderr, fmt.Errorf("invalid adoption recovery journal: %w", err))
	}
	if recovery != store.RecoveryRequired {
		if err := outsideGitRehomingGate(boundary); err != nil {
			return fail(stderr, err)
		}
	}
	result, err := store.ApplyAdoption(fs.Arg(0), commonCanon, owner, store.ApplyOptions{
		RequestID:  requestID,
		PlanDigest: digest,
	})
	if err != nil {
		var conflict *store.ReplayConflict
		if errors.As(err, &conflict) {
			fmt.Fprintf(stderr, "noise-tasks: %v\n", err)
			return 3
		}
		return fail(stderr, err)
	}
	target := filepath.Join(commonCanon, "noise-tasks", "tasks.sqlite")
	if jsonOut {
		out := struct {
			Receipt  map[string]any `json:"receipt"`
			Replayed bool           `json:"replayed"`
			Resumed  bool           `json:"resumed"`
		}{result.Receipt, result.Replayed, result.Resumed}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	switch {
	case result.Replayed:
		fmt.Fprintf(stdout, "replayed: receipt %s\n", result.Receipt["receiptId"])
	case result.Resumed:
		fmt.Fprintf(stdout, "resumed: receipt %s\n", result.Receipt["receiptId"])
	default:
		fmt.Fprintf(stdout, "adopted: %s\n", target)
		fmt.Fprintf(stdout, "receipt: %s\n", result.Receipt["receiptId"])
	}
	return 0
}
