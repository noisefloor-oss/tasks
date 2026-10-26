package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/buildinfo"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdBackup implements §8.5: backup create / verify / restore. Authority
// commands never accept --file.
func cmdBackup(args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks backup <create|verify|restore> [options]")
		return 2
	}
	switch args[0] {
	case "-h", "-help", "--help", "help":
		fmt.Fprintln(stdout, "usage: noise-tasks backup <create|verify|restore> [options]")
		return 0
	case "create":
		return cmdBackupCreate(args[1:], stdout, stderr)
	case "verify":
		return cmdBackupVerify(args[1:], stdout, stderr)
	case "restore":
		return cmdBackupRestore(args[1:], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "noise-tasks: unknown backup subcommand %q\n", args[0])
		fmt.Fprintln(stderr, "usage: noise-tasks backup <create|verify|restore> [options]")
		return 2
	}
}

func cmdBackupCreate(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("backup create", stderr)
	var output string
	var legacy bool
	var adoptPath string
	var jsonOut bool
	fs.StringVar(&output, "output", "", "backup bundle output path (must end in .ntbackup)")
	fs.BoolVar(&legacy, "legacy", false, "snapshot the exact legacy live.json instead of the SQLite authority")
	fs.StringVar(&adoptPath, "adopt", "", "adopt one explicitly named retired-layout document instead of the SQLite authority")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks backup create --output <bundle.ntbackup> [--legacy | --adopt <path>] [--json]")
		return 2
	}
	if output == "" {
		fmt.Fprintln(stderr, "noise-tasks: backup create requires --output <bundle.ntbackup>")
		return 2
	}
	if legacy && adoptPath != "" {
		fmt.Fprintln(stderr, "noise-tasks: backup create accepts one of --legacy or --adopt, not both")
		return 2
	}
	if legacy {
		return cmdBackupCreateLegacy(output, jsonOut, stdout, stderr)
	}
	if adoptPath != "" {
		return cmdBackupCreateAdopt(output, adoptPath, jsonOut, stdout, stderr)
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	// Backup create validates the store already carries its §8.2 identity;
	// it is never the fallback that repairs or mints one for an unupgraded
	// v1 store.
	if err := ps.db.CheckIdentity(ps.owner); err != nil {
		return fail(stderr, err)
	}
	manifest, err := ps.db.CreateBackup(output, "noise-tasks "+buildinfo.Version)
	if err != nil {
		return fail(stderr, err)
	}
	if jsonOut {
		if err := writeJSON(stdout, manifest); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "backup: %s\n", output)
	fmt.Fprintf(stdout, "familyId: %s\n", manifest.FamilyID)
	fmt.Fprintf(stdout, "payload: %s\n", manifest.PayloadDigest)
	fmt.Fprintf(stdout, "semantic: %s\n", manifest.SemanticDigest)
	fmt.Fprintf(stdout, "comparison: %s\n", manifest.ComparisonDigest)
	return 0
}

func cmdBackupVerify(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("backup verify", stderr)
	var jsonOut bool
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks backup verify <bundle.ntbackup> [--json]")
		return 2
	}
	// Context-free and read-only (SPEC §8.5): no project discovery, no store.
	manifest, err := store.VerifyBundle(fs.Arg(0))
	if err != nil {
		return fail(stderr, err)
	}
	if jsonOut {
		out := struct {
			Valid    bool            `json:"valid"`
			Manifest *store.Manifest `json:"manifest"`
		}{true, manifest}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "ok: %s (family %s, %d tasks)\n", fs.Arg(0), manifest.FamilyID, manifest.Inventory.Tasks)
	return 0
}

// cmdBackupCreateLegacy snapshots the exact derived legacy live.json (SPEC
// §8.5). It is context-aware — it resolves the repository family's Git
// common directory and owner boundary — but it never opens, requires, or
// publishes the current SQLite authority, and it never reads a --file or a
// projection.
func cmdBackupCreateLegacy(output string, jsonOut bool, stdout, stderr io.Writer) int {
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
	// Once apply has a durable journal, the original verified bundle and
	// accepted plan are recovery inputs. A new legacy backup is not allowed
	// to masquerade as recovery; only the matching apply advances the state,
	// and an adoption recovery journal fences this surface the same way
	// (§8.6A).
	if err := migrationJournalGate(filepath.Join(storeDir, store.StoreFile)); err != nil {
		return fail(stderr, err)
	}
	if err := adoptionJournalGate(filepath.Join(storeDir, store.StoreFile)); err != nil {
		return fail(stderr, err)
	}
	legacyPath := filepath.Join(commonCanon, "noise-tasks", store.LegacyFile)
	manifest, err := store.CreateLegacyBackup(output, legacyPath, commonCanon, owner, "noise-tasks "+buildinfo.Version)
	if err != nil {
		return fail(stderr, err)
	}
	if jsonOut {
		if err := writeJSON(stdout, manifest); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "backup: %s\n", output)
	fmt.Fprintf(stdout, "familyId: %s\n", manifest.FamilyID)
	fmt.Fprintf(stdout, "payload: %s\n", manifest.PayloadDigest)
	fmt.Fprintf(stdout, "semantic: %s\n", manifest.SemanticDigest)
	fmt.Fprintf(stdout, "comparison: %s\n", manifest.ComparisonDigest)
	return 0
}

// cmdBackupCreateAdopt snapshots one explicitly named retired-layout
// document (SPEC §8.6A). It is context-aware — it resolves the repository
// family's Git common directory and owner boundary — but it never opens,
// requires, or publishes the current SQLite authority, and it never reads a
// --file or a projection.
func cmdBackupCreateAdopt(output, adoptPath string, jsonOut bool, stdout, stderr io.Writer) int {
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
	// The verb refuses the states adoption never operates on (SPEC §8.6A):
	// an existing store, either recovery journal, or the derived live.json.
	if err := store.AdoptionFreshGate(storeDir); err != nil {
		return fail(stderr, err)
	}
	layoutCanonical, err := store.ResolveAdoptionSource(adoptPath, owner, storeDir)
	if err != nil {
		return fail(stderr, err)
	}
	manifest, err := store.CreateAdoptionBackup(output, layoutCanonical, commonCanon, owner, "noise-tasks "+buildinfo.Version)
	if err != nil {
		return fail(stderr, err)
	}
	if jsonOut {
		if err := writeJSON(stdout, manifest); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "backup: %s\n", output)
	fmt.Fprintf(stdout, "familyId: %s\n", manifest.FamilyID)
	fmt.Fprintf(stdout, "payload: %s\n", manifest.PayloadDigest)
	fmt.Fprintf(stdout, "semantic: %s\n", manifest.SemanticDigest)
	fmt.Fprintf(stdout, "comparison: %s\n", manifest.ComparisonDigest)
	return 0
}

func cmdBackupRestore(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("backup restore", stderr)
	var overwrite, offline, jsonOut bool
	var requestID string
	fs.BoolVar(&overwrite, "overwrite", false, "explicitly authorize replacing an existing same-family, same-boundary store")
	fs.BoolVar(&offline, "offline", false, "acknowledge that all unsupported writers are stopped")
	fs.StringVar(&requestID, "request-id", "", "operator request id for replay-safe restore")
	fs.BoolVar(&jsonOut, "json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks backup restore <bundle.ntbackup> [--overwrite] --offline --request-id <id> [--json]")
		return 2
	}
	if !offline {
		fmt.Fprintln(stderr, "noise-tasks: backup restore requires --offline (acknowledging all unsupported writers are stopped)")
		return 2
	}
	if requestID == "" {
		fmt.Fprintln(stderr, "noise-tasks: backup restore requires --request-id <id>")
		return 2
	}
	// Grammar is validated before any bundle or store effect (SPEC §9.1:
	// request ids use the §9.2 local-key grammar).
	if !store.ValidLocalKey(requestID) {
		fmt.Fprintf(stderr, "noise-tasks: backup restore --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}\n")
		return 2
	}
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return fail(stderr, err)
	}
	target, _ := store.Path(boundary)
	if err := migrationJournalGate(target); err != nil {
		return fail(stderr, err)
	}
	if err := adoptionJournalGate(target); err != nil {
		return fail(stderr, err)
	}
	if err := outsideGitRehomingGate(boundary); err != nil {
		return fail(stderr, err)
	}
	owner, err := derivedOwnerBoundary(boundary)
	if err != nil {
		return fail(stderr, err)
	}
	result, err := store.Restore(fs.Arg(0), target, store.RestoreOptions{
		Overwrite:     overwrite,
		RequestID:     requestID,
		OwnerBoundary: owner,
	})
	if err != nil {
		var conflict *store.ReplayConflict
		if errors.As(err, &conflict) {
			fmt.Fprintf(stderr, "noise-tasks: %v\n", err)
			return 3
		}
		return fail(stderr, err)
	}
	if jsonOut {
		out := struct {
			Receipt  map[string]any `json:"receipt"`
			Replayed bool           `json:"replayed"`
			Adopted  bool           `json:"adopted"`
		}{result.Receipt, result.Replayed, result.Adopted}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if result.Replayed {
		fmt.Fprintf(stdout, "replayed: receipt %s\n", result.Receipt["receiptId"])
		return 0
	}
	fmt.Fprintf(stdout, "restored: %s\n", target)
	fmt.Fprintf(stdout, "receipt: %s\n", result.Receipt["receiptId"])
	if result.Adopted {
		// The recovery fact is named: the bundle's family was adopted onto
		// this boundary because none existed here (SPEC §8.5).
		fmt.Fprintf(stdout, "adopted: family %s onto owner boundary %s\n", result.Receipt["familyId"], owner)
	}
	return 0
}

// derivedOwnerBoundary computes the canonical §8.1 owner boundary: inside Git,
// the primary working-tree root registered for the common directory — the
// <primary>/.git parent for an ordinary repository, and the Git-registered
// primary worktree for a common directory that is not a plain .git directory
// (a submodule's <superproject>/.git/modules/... gitdir). Outside Git, the
// boundary itself is the boundary. An absent or ambiguous root refuses;
// nothing is guessed from path spelling.
func derivedOwnerBoundary(boundary string) (string, error) {
	canon, err := canonicalizePath(boundary)
	if err != nil {
		return "", err
	}
	common, ok := backlog.GitCommonDir(canon)
	if !ok {
		return canon, nil
	}
	commonCanon, err := canonicalizePath(common)
	if err != nil {
		return "", err
	}
	if filepath.Base(commonCanon) == ".git" {
		return filepath.Dir(commonCanon), nil
	}
	return registeredWorktreeRoot(commonCanon)
}

// registeredWorktreeRoot resolves the primary working tree Git registered for
// a non-.git common directory (a submodule's gitdir under a superproject's
// .git/modules path). It reads Git's own registration — core.worktree, whose
// relative form Git defines relative to the common directory — with a
// no-shell invocation, never path spelling, and validates that the resolved
// root really is registered against that common directory before accepting
// it. An absent registration or a failed validation refuses rather than
// guessing.
func registeredWorktreeRoot(commonDir string) (string, error) {
	refuse := func(reason string) (string, error) {
		return "", fmt.Errorf("cannot determine the primary working tree for Git common directory %s (%s); refusing to mint or match an owner boundary", commonDir, reason)
	}
	// Read only this repository's common config. In particular, ambient
	// GIT_CONFIG_* / GIT_DIR / GIT_WORK_TREE values must not redirect the
	// immutable owner identity derived by §8.1.
	cmd := exec.Command("git", "config", "--file="+filepath.Join(commonDir, "config"), "--get", "core.worktree")
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return refuse("no working tree registered")
		}
		return refuse(fmt.Sprintf("git config core.worktree failed: %v", err))
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return refuse("no working tree registered")
	}
	if !filepath.IsAbs(root) {
		root = filepath.Join(commonDir, root)
	}
	rootCanon, err := canonicalizePath(root)
	if err != nil {
		return refuse(fmt.Sprintf("canonicalize %s: %v", root, err))
	}
	resolved, ok := backlog.GitCommonDir(rootCanon)
	if !ok {
		return refuse(fmt.Sprintf("%s has no resolvable .git marker", rootCanon))
	}
	resolvedCanon, err := canonicalizePath(resolved)
	if err != nil || resolvedCanon != commonDir {
		return refuse(fmt.Sprintf("%s is registered against common directory %s", rootCanon, resolvedCanon))
	}
	return rootCanon, nil
}
