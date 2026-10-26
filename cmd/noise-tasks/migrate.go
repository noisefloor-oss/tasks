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

// cmdMigrate implements §8.6: migrate plan and migrate apply. Migration
// commands never accept --file and never consult a projection.
func cmdMigrate(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks migrate <plan|apply|adopt> ...")
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, "usage: noise-tasks migrate <plan|apply|adopt> ...")
		return 0
	case "plan":
		return cmdMigratePlan(args[1:], stdout, stderr)
	case "apply":
		return cmdMigrateApply(args[1:], stdout, stderr)
	case "adopt":
		return cmdMigrateAdopt(args[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: noise-tasks migrate <plan|apply|adopt> ...")
		return 2
	}
}

// cmdMigratePlan plans the one-way §8.6 migration: it verifies the §8.5
// legacy bundle against the exact derived legacy live.json and atomically
// publishes the strict plan at <storeDir>/migration-plan.json. It is
// context-aware — it resolves the repository family's Git common directory
// and owner boundary — but it never opens, requires, or publishes the current
// SQLite authority, and it changes no authority.
func cmdMigratePlan(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("migrate plan", stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks migrate plan [--json] <bundle.ntbackup>")
		return 2
	}
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	common, ok := backlog.GitCommonDir(boundary)
	if !ok {
		fmt.Fprintln(stderr, "noise-tasks: no Git repository; the legacy live store lives at <git-common-dir>/noise-tasks/live.json")
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
	// A recovery journal means apply has crossed its durable boundary. Per
	// §2/§8.7 only the matching migrate apply may advance that state; plan
	// must not replace the digest-bearing plan recovery depends on. An
	// adoption recovery journal fences this surface the same way (§8.6A).
	if err := migrationJournalGate(filepath.Join(storeDir, store.StoreFile)); err != nil {
		return fail(stderr, err)
	}
	if err := adoptionJournalGate(filepath.Join(storeDir, store.StoreFile)); err != nil {
		return fail(stderr, err)
	}
	legacyPath := filepath.Join(storeDir, store.LegacyFile)
	plan, err := store.CreateMigrationPlan(fs.Arg(0), legacyPath, commonCanon, owner, storeDir, "noise-tasks "+buildinfo.Version)
	if err != nil {
		return fail(stderr, err)
	}
	planPath := filepath.Join(storeDir, store.PlanFile)
	if jsonOut {
		out := struct {
			Plan       *store.MigrationPlan `json:"plan"`
			PlanDigest string               `json:"planDigest"`
			Path       string               `json:"path"`
		}{plan, plan.PlanDigest, planPath}
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

// cmdMigrateApply performs the §8.6 cutover: it requires the derived plan,
// its accepted --digest, the same verified bundle, --offline, and a request
// id, and publishes the current SQLite target from the bundle under the
// exclusive maintenance lock. The plan is always the derived
// <storeDir>/migration-plan.json.
func cmdMigrateApply(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("migrate apply", stderr)
	var digest, requestID string
	var offline, jsonOut bool
	fs.StringVar(&digest, "digest", "", "accepted plan digest from migrate plan")
	fs.BoolVar(&offline, "offline", false, "acknowledge that all pre-v2.5 writers are stopped")
	fs.StringVar(&requestID, "request-id", "", "operator request id for replay-safe migration")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	usage := func() int {
		fmt.Fprintln(stderr, "usage: noise-tasks migrate apply --digest <planDigest> --offline --request-id <id> [--json] <bundle.ntbackup>")
		return 2
	}
	if fs.NArg() != 1 {
		return usage()
	}
	// All required flags validate before any effect (§8.6).
	if !offline {
		fmt.Fprintln(stderr, "noise-tasks: migrate apply requires --offline (acknowledging all pre-v2.5 writers are stopped)")
		return 2
	}
	if digest == "" {
		fmt.Fprintln(stderr, "noise-tasks: migrate apply requires --digest <planDigest>")
		return 2
	}
	if !store.ValidDigest(digest) {
		fmt.Fprintln(stderr, "noise-tasks: migrate apply --digest must be a lowercase sha256:<hex> digest")
		return 2
	}
	if requestID == "" {
		fmt.Fprintln(stderr, "noise-tasks: migrate apply requires --request-id <id>")
		return 2
	}
	if !store.ValidLocalKey(requestID) {
		fmt.Fprintln(stderr, "noise-tasks: migrate apply --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")
		return 2
	}
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	common, ok := backlog.GitCommonDir(boundary)
	if !ok {
		fmt.Fprintln(stderr, "noise-tasks: no Git repository; the legacy live store lives at <git-common-dir>/noise-tasks/live.json")
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
	recovery, err := store.ReadMigrationRecovery(filepath.Join(commonCanon, "noise-tasks"))
	if err != nil {
		return fail(stderr, fmt.Errorf("invalid migration recovery journal: %w", err))
	}
	if recovery != store.RecoveryRequired {
		if err := adoptionJournalGate(filepath.Join(commonCanon, "noise-tasks", store.StoreFile)); err != nil {
			return fail(stderr, err)
		}
		if err := outsideGitRehomingGate(boundary); err != nil {
			return fail(stderr, err)
		}
	}
	result, err := store.ApplyMigration(fs.Arg(0), commonCanon, owner, store.ApplyOptions{
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
			Receipt       map[string]any `json:"receipt"`
			Replayed      bool           `json:"replayed"`
			Resumed       bool           `json:"resumed"`
			Reconstructed bool           `json:"reconstructed"`
		}{result.Receipt, result.Replayed, result.Resumed, result.Reconstructed}
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
	case result.Reconstructed:
		fmt.Fprintf(stdout, "reconstructed: receipt %s\n", result.Receipt["receiptId"])
	default:
		fmt.Fprintf(stdout, "migrated: %s\n", target)
		fmt.Fprintf(stdout, "receipt: %s\n", result.Receipt["receiptId"])
	}
	return 0
}
