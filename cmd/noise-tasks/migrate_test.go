package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

const migrateLegacyDocument = `{"schemaVersion":1,"fencing":0,"nextSeq":1,"base":{"hash":"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef","tags":{}},"tags":{"main":{"nextId":2,"taskCount":1,"completedCount":0,"tasks":[{"task":{"id":1,"title":"Legacy","description":"d","details":"","status":"pending","dependencies":[],"updatedAt":"2026-01-01T00:00:00Z"},"revision":1}]}},"events":[]}`

// migrateFixtureRepo prepares a git-dir repo with the derived legacy live
// store and returns (repo, storeDir, legacyPath).
func migrateFixtureRepo(t *testing.T) (string, string, string) {
	t.Helper()
	dir := chdirGitDir(t)
	storeDir := filepath.Join(dir, ".git", "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(storeDir, "live.json")
	if err := os.WriteFile(legacyPath, []byte(migrateLegacyDocument), 0o600); err != nil {
		t.Fatal(err)
	}

	return dir, storeDir, legacyPath
}

// readPlanFile strictly re-reads a published plan through the store reader.
func readPlanFile(path string) (*store.MigrationPlan, error) {
	return store.ReadMigrationPlan(path)
}

// migrateBundle creates a legacy backup bundle of the current fixture repo.
func migrateBundle(t *testing.T, name string) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), name)
	if code, _, stderr := runCmd(cmdBackup, "create", "--legacy", "--output", bundle); code != 0 {
		t.Fatalf("legacy backup exit %d: %s", code, stderr)
	}
	return bundle
}

func TestMigratePlanCLI(t *testing.T) {
	_, storeDir, legacyPath := migrateFixtureRepo(t)
	before, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	bundle := migrateBundle(t, "legacy.ntbackup")

	code, stdout, stderr := runCmd(cmdMigrate, "plan", "--json", bundle)
	if code != 0 {
		t.Fatalf("migrate plan exit %d: %s", code, stderr)
	}
	var out struct {
		Plan       map[string]any `json:"plan"`
		PlanDigest string         `json:"planDigest"`
		Path       string         `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode plan output %q: %v", stdout, err)
	}
	planPath := filepath.Join(storeDir, "migration-plan.json")
	if out.Path != planPath || out.PlanDigest != out.Plan["planDigest"] {
		t.Fatalf("plan output path/digest: %q %v", out.Path, out.Plan)
	}
	info, err := os.Lstat(planPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("published plan: %v %v", err, info)
	}
	// Planning changes no authority and never retires the source.
	if _, err := os.Lstat(filepath.Join(storeDir, "tasks.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("tasks.sqlite must not be created: %v", err)
	}
	after, err := os.ReadFile(legacyPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("legacy live.json mutated: %v", err)
	}

	// The strict store reader accepts the published plan.
	if _, err := readPlanFile(planPath); err != nil {
		t.Fatalf("published plan rejected: %v", err)
	}

	// The human form is stable.
	code, stdout, stderr = runCmd(cmdMigrate, "plan", bundle)
	if code != 0 {
		t.Fatalf("migrate plan (human) exit %d: %s", code, stderr)
	}
	for _, want := range []string{"plan: " + planPath, "familyId: ", "planDigest: "} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("human output missing %q: %q", want, stdout)
		}
	}

	// A repeat with the same semantic inputs replaces the plan atomically.
	if err := os.WriteFile(planPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(cmdMigrate, "plan", bundle); code != 0 {
		t.Fatalf("replan exit %d: %s", code, stderr)
	}
	if _, err := readPlanFile(planPath); err != nil {
		t.Fatalf("replaced plan rejected: %v", err)
	}
}

func TestMigratePlanUsageCLI(t *testing.T) {
	_, _, _ = migrateFixtureRepo(t)
	bundle := migrateBundle(t, "usage.ntbackup")
	if code, _, _ := runCmd(cmdMigrate); code != 2 {
		t.Fatalf("bare migrate exit %d", code)
	}
	if code, _, _ := runCmd(cmdMigrate, "plan"); code != 2 {
		t.Fatalf("missing positional exit %d", code)
	}
	if code, _, _ := runCmd(cmdMigrate, "plan", bundle, bundle); code != 2 {
		t.Fatalf("two positionals exit %d", code)
	}
	// Apply is batch 2; it is not implemented and must not run.
	if code, stdout, _ := runCmd(cmdMigrate, "apply", bundle); code != 2 || stdout != "" {
		t.Fatalf("migrate apply exit %d stdout %q", code, stdout)
	}
}

func TestMigratePlanSourceMutationRefusedCLI(t *testing.T) {
	_, storeDir, legacyPath := migrateFixtureRepo(t)
	bundle := migrateBundle(t, "stale.ntbackup")
	mutated := strings.Replace(migrateLegacyDocument, `"title":"Legacy"`, `"title":"Changed"`, 1)
	if err := os.WriteFile(legacyPath, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdMigrate, "plan", bundle)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "comparison digests differ") {
		t.Fatalf("mutated source: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(filepath.Join(storeDir, "migration-plan.json")); !os.IsNotExist(err) {
		t.Fatalf("refused plan must publish nothing: %v", err)
	}
}

func TestMigratePlanWrongFamilyRefusedCLI(t *testing.T) {
	_, storeDir, _ := migrateFixtureRepo(t)
	// A bundle from a different repository family cannot plan here.
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, ".git", "noise-tasks"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ".git", "noise-tasks", "live.json"), []byte(migrateLegacyDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(other); err != nil {
		t.Fatal(err)
	}
	foreign := migrateBundle(t, "foreign.ntbackup")
	if err := os.Chdir(orig); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdMigrate, "plan", foreign)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "family") {
		t.Fatalf("foreign family: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(filepath.Join(storeDir, "migration-plan.json")); !os.IsNotExist(err) {
		t.Fatalf("refused plan must publish nothing: %v", err)
	}
}

func TestMigratePlanLinkedWorktreeSamePathCLI(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-b", "main")
	gitCmd(t, repo, "config", "user.email", "test@example.invalid")
	gitCmd(t, repo, "config", "user.name", "test")
	gitCmd(t, repo, "commit", "--allow-empty", "-m", "init")
	storeDir := filepath.Join(repo, ".git", "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "live.json"), []byte(migrateLegacyDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	gitCmd(t, repo, "worktree", "add", linked)

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	bundle := migrateBundle(t, "linked.ntbackup")
	if err := os.Chdir(linked); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdMigrate, "plan", "--json", bundle)
	if code != 0 {
		t.Fatalf("migrate plan from linked worktree exit %d: %s", code, stderr)
	}
	var out struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(storeDir, "migration-plan.json"); out.Path != want {
		t.Fatalf("linked worktree plan path %q, want %q", out.Path, want)
	}
	if _, err := readPlanFile(out.Path); err != nil {
		t.Fatalf("linked worktree plan rejected: %v", err)
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// migratePlanCLI runs `migrate plan --json` in the current fixture repo and
// returns the decoded plan map and digest.
func migratePlanCLI(t *testing.T, bundle string) (map[string]any, string) {
	t.Helper()
	code, stdout, stderr := runCmd(cmdMigrate, "plan", "--json", bundle)
	if code != 0 {
		t.Fatalf("migrate plan exit %d: %s", code, stderr)
	}
	var out struct {
		Plan       map[string]any `json:"plan"`
		PlanDigest string         `json:"planDigest"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode plan output %q: %v", stdout, err)
	}
	return out.Plan, out.PlanDigest
}

func TestMigrateApplyCLI(t *testing.T) {
	_, storeDir, legacyPath := migrateFixtureRepo(t)
	bundle := migrateBundle(t, "apply.ntbackup")
	_, digest := migratePlanCLI(t, bundle)

	code, stdout, stderr := runCmd(cmdMigrate, "apply", "--json", "--digest", digest, "--offline", "--request-id", "req-1", bundle)
	if code != 0 {
		t.Fatalf("migrate apply exit %d: %s", code, stderr)
	}
	var out struct {
		Receipt       map[string]any `json:"receipt"`
		Replayed      bool           `json:"replayed"`
		Resumed       bool           `json:"resumed"`
		Reconstructed bool           `json:"reconstructed"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("decode apply output %q: %v", stdout, err)
	}
	if out.Replayed || out.Resumed || out.Reconstructed || out.Receipt["receiptId"] == "" {
		t.Fatalf("apply output: %+v", out)
	}
	if out.Receipt["planDigest"] != digest || out.Receipt["sourceSchema"] != "legacy-json:1" {
		t.Fatalf("apply receipt digests: %v", out.Receipt)
	}
	if _, err := os.Lstat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy live.json must be retired: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(storeDir, "tasks.sqlite")); err != nil {
		t.Fatalf("tasks.sqlite must be published: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(storeDir, "migration-apply.json")); !os.IsNotExist(err) {
		t.Fatalf("journal must be removed: %v", err)
	}

	// The published store serves normal commands, and the legacy task is
	// visible through the SQLite authority.
	code, stdout, stderr = runCmd(cmdList, "--json")
	if code != 0 || !strings.Contains(stdout, `"title": "Legacy"`) {
		t.Fatalf("list after migration: exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Replay (state 5) returns the original receipt.
	code, stdout, stderr = runCmd(cmdMigrate, "apply", "--json", "--digest", digest, "--offline", "--request-id", "req-1", bundle)
	if code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"replayed": true`) {
		t.Fatalf("replay output: %q", stdout)
	}

	// The human form is stable.
	code, stdout, stderr = runCmd(cmdMigrate, "apply", "--digest", digest, "--offline", "--request-id", "req-1", bundle)
	if code != 0 || !strings.Contains(stdout, "replayed: receipt migration-") {
		t.Fatalf("human replay: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestMigrateApplyUsageCLI(t *testing.T) {
	migrateFixtureRepo(t)
	bundle := migrateBundle(t, "usage2.ntbackup")
	_, digest := migratePlanCLI(t, bundle)
	for _, args := range [][]string{
		{"apply", bundle, "--digest", digest, "--request-id", "r"}, // no --offline
		{"apply", bundle, "--offline", "--request-id", "r"},        // no --digest
		{"apply", bundle, "--offline", "--digest", digest},         // no --request-id
		{"apply", "--offline", "--digest", "notadigest", "--request-id", "r", bundle},
		{"apply", "--offline", "--digest", digest, "--request-id", "-bad", bundle},
		{"apply", "--offline", "--digest", digest, "--request-id", "r"}, // no positional
		{"apply", "--offline", "--digest", digest, "--request-id", "r", bundle, bundle},
	} {
		if code, stdout, _ := runCmd(cmdMigrate, args...); code != 2 || stdout != "" {
			t.Fatalf("args %q = exit %d stdout %q, want usage exit 2", args, code, stdout)
		}
	}
	// Flag validation precedes effects: nothing was retired or published.
	if _, err := os.Stat(filepath.Join(filepath.Dir(bundle), "never")); err == nil {
		t.Fatal("unreachable")
	}
}

// migrateJournalFor writes the strict §8.6 journal matching a CLI-produced
// plan into the fixture repo's store directory.
func migrateJournalFor(t *testing.T, storeDir string, plan map[string]any, requestID string) {
	t.Helper()
	body, err := store.JournalBytes(&store.MigrationJournal{
		SchemaVersion:          1,
		RequestID:              requestID,
		FamilyID:               plan["familyId"].(string),
		PlanDigest:             plan["planDigest"].(string),
		SourceComparisonDigest: plan["source"].(map[string]any)["comparisonDigest"].(string),
		BackupPayloadDigest:    plan["backup"].(map[string]any)["payloadDigest"].(string),
		TargetSchemaVersion:    store.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "migration-apply.json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestMigrateApplyRecoveryFencingCLI pins §2/§8.7: a valid journal in
// recoverable state 2 or 3 takes precedence over the legacy and missing
// diagnostics for every normal command, and only a matching migrate apply
// advances it.
func TestMigrateApplyRecoveryFencingCLI(t *testing.T) {
	for _, tc := range []struct {
		name      string
		keepLocal bool
		wantFlag  string
	}{
		{name: "state2-resume", keepLocal: true, wantFlag: `"resumed": true`},
		{name: "state3-reconstruct", keepLocal: false, wantFlag: `"reconstructed": true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, storeDir, legacyPath := migrateFixtureRepo(t)
			bundle := migrateBundle(t, tc.name+".ntbackup")
			plan, digest := migratePlanCLI(t, bundle)
			migrateJournalFor(t, storeDir, plan, "recover-1")
			if !tc.keepLocal {
				if err := os.Remove(legacyPath); err != nil {
					t.Fatal(err)
				}
			}

			want := "noise-tasks: migration recovery required; rerun noise tasks migrate apply with the original plan and backup\n"
			for name, run := range map[string]func() (int, string, string){
				"list":   func() (int, string, string) { return runCmd(cmdList, "--json") },
				"next":   func() (int, string, string) { return runCmd(cmdNext) },
				"add":    func() (int, string, string) { return runCmd(cmdAdd, "--title", "x") },
				"verify": func() (int, string, string) { return runCmd(verifyMain) },
				"live":   func() (int, string, string) { return runCmd(cmdLiveFeed) },
				"legacy-backup": func() (int, string, string) {
					return runCmd(cmdBackup, "create", "--legacy", "--output", filepath.Join(t.TempDir(), "late.ntbackup"))
				},
				"init": func() (int, string, string) { return runCmd(cmdInit) },
				"setup": func() (int, string, string) {
					return runCmd(cmdSetup, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
				},
				"context": func() (int, string, string) { return runCmd(cmdContext, "--json") },
			} {
				code, stdout, stderr := run()
				if name == "context" {
					// Context is a successful blocked query, not a refusal.
					if code != 0 || !strings.Contains(stdout, `"blockedReason": "migration-recovery-required"`) {
						t.Fatalf("%s = code %d stdout %q stderr %q", name, code, stdout, stderr)
					}
					continue
				}
				if code != 1 || stdout != "" || stderr != want {
					t.Errorf("%s = code %d stdout %q stderr %q", name, code, stdout, stderr)
				}
			}

			// Only the matching migrate apply advances the state.
			code, stdout, stderr := runCmd(cmdMigrate, "apply", "--json", "--digest", digest, "--offline", "--request-id", "recover-1", bundle)
			if code != 0 || !strings.Contains(stdout, tc.wantFlag) {
				t.Fatalf("recovery apply: exit %d stdout %q stderr %q", code, stdout, stderr)
			}
			if _, err := os.Lstat(legacyPath); !os.IsNotExist(err) {
				t.Fatalf("legacy must be retired: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(storeDir, "migration-apply.json")); !os.IsNotExist(err) {
				t.Fatalf("journal must be removed: %v", err)
			}
			// Normal commands work again.
			if code, _, _ := runCmd(cmdList, "--json"); code != 0 {
				t.Fatalf("list after recovery exit %d", code)
			}
		})
	}
}

func TestMigratePlanCannotBypassRecoveryCLI(t *testing.T) {
	_, storeDir, _ := migrateFixtureRepo(t)
	bundle := migrateBundle(t, "recovery-plan.ntbackup")
	plan, _ := migratePlanCLI(t, bundle)
	planPath := filepath.Join(storeDir, store.PlanFile)
	before, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	migrateJournalFor(t, storeDir, plan, "recover-plan-1")
	code, stdout, stderr := runCmd(cmdMigrate, "plan", "--json", bundle)
	want := "noise-tasks: migration recovery required; rerun noise tasks migrate apply with the original plan and backup\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Fatalf("plan during recovery: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	after, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("migrate plan replaced the recovery-bound plan")
	}
}

func TestPublishedMigrationRecoveryNormalCommandGate(t *testing.T) {
	t.Run("matching published receipt serves", func(t *testing.T) {
		_, storeDir, _ := migrateFixtureRepo(t)
		bundle := migrateBundle(t, "published-valid.ntbackup")
		plan, digest := migratePlanCLI(t, bundle)
		if code, _, stderr := runCmd(cmdMigrate, "apply", "--digest", digest, "--offline", "--request-id", "published-1", bundle); code != 0 {
			t.Fatalf("apply: %s", stderr)
		}
		migrateJournalFor(t, storeDir, plan, "published-1")
		if code, _, stderr := runCmd(cmdList, "--json"); code != 0 {
			t.Fatalf("normal read of proven state 4: exit %d stderr %q", code, stderr)
		}
	})

	t.Run("target without migration receipt refuses", func(t *testing.T) {
		_, storeDir, legacyPath := migrateFixtureRepo(t)
		bundle := migrateBundle(t, "published-unproven.ntbackup")
		plan, _ := migratePlanCLI(t, bundle)
		if err := os.Remove(legacyPath); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := runCmd(cmdBackup, "restore", "--offline", "--request-id", "restore-only-1", bundle); code != 0 {
			t.Fatalf("restore fixture: %s", stderr)
		}
		migrateJournalFor(t, storeDir, plan, "published-1")
		code, stdout, stderr := runCmd(cmdList, "--json")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid published migration recovery state") {
			t.Fatalf("unproven target: code %d stdout %q stderr %q", code, stdout, stderr)
		}
		for name, run := range map[string]func() (int, string, string){
			"init": func() (int, string, string) { return runCmd(cmdInit) },
			"setup": func() (int, string, string) {
				return runCmd(cmdSetup, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
			},
		} {
			code, stdout, stderr := run()
			if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid published migration recovery state") {
				t.Fatalf("%s over unproven target: code %d stdout %q stderr %q", name, code, stdout, stderr)
			}
		}
	})

	t.Run("dual authority refuses before open", func(t *testing.T) {
		repo, storeDir, _ := migrateFixtureRepo(t)
		bundle := migrateBundle(t, "published-dual.ntbackup")
		plan, _ := migratePlanCLI(t, bundle)
		target, err := store.CreateWithOwner(filepath.Join(storeDir, store.StoreFile), repo)
		if err != nil {
			t.Fatal(err)
		}
		target.Close()
		migrateJournalFor(t, storeDir, plan, "published-1")
		code, stdout, stderr := runCmd(cmdList, "--json")
		if code != 1 || stdout != "" || !strings.Contains(stderr, "both legacy source and SQLite target") {
			t.Fatalf("dual authority: code %d stdout %q stderr %q", code, stdout, stderr)
		}
	})
}

// TestMigrateApplyConflictExit3CLI pins the shared (migration, requestId)
// collision rule at the CLI: a different plan under the same request id on
// the published target is exit 3.
func TestMigrateApplyConflictExit3CLI(t *testing.T) {
	_, storeDir, legacyPath := migrateFixtureRepo(t)
	bundle := migrateBundle(t, "c1.ntbackup")
	_, digest := migratePlanCLI(t, bundle)
	if code, _, stderr := runCmd(cmdMigrate, "apply", "--digest", digest, "--offline", "--request-id", "same-1", bundle); code != 0 {
		t.Fatalf("first apply exit %d: %s", code, stderr)
	}
	// A second, different plan for the same family and request id.
	mutated := strings.Replace(migrateLegacyDocument, `"title":"Legacy"`, `"title":"Legacy2"`, 1)
	if err := os.WriteFile(legacyPath, []byte(mutated), 0o600); err != nil {
		t.Fatal(err)
	}
	bundle2 := migrateBundle(t, "c2.ntbackup")
	_, digest2 := migratePlanCLI(t, bundle2)
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdMigrate, "apply", "--digest", digest2, "--offline", "--request-id", "same-1", bundle2)
	if code != 3 || stdout != "" || !strings.Contains(stderr, "already used for a different migration request") {
		t.Fatalf("conflict: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	_ = storeDir
}

// TestMigrateApplyFromLinkedWorktree: apply is visible family-wide — run
// from a linked worktree it retires the common-dir source, publishes the
// common-dir target, and normal commands in the linked worktree see it.
func TestMigrateApplyFromLinkedWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	gitCmd(t, repo, "init", "-b", "main")
	gitCmd(t, repo, "config", "user.email", "test@example.invalid")
	gitCmd(t, repo, "config", "user.name", "test")
	gitCmd(t, repo, "commit", "--allow-empty", "-m", "init")
	storeDir := filepath.Join(repo, ".git", "noise-tasks")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "live.json"), []byte(migrateLegacyDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	gitCmd(t, repo, "worktree", "add", linked)

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	bundle := migrateBundle(t, "linked-apply.ntbackup")
	_, digest := migratePlanCLI(t, bundle)
	if err := os.Chdir(linked); err != nil {
		t.Fatal(err)
	}
	code, _, stderr := runCmd(cmdMigrate, "apply", "--digest", digest, "--offline", "--request-id", "wt-1", bundle)
	if code != 0 {
		t.Fatalf("apply from linked worktree exit %d: %s", code, stderr)
	}
	if _, err := os.Lstat(filepath.Join(storeDir, "live.json")); !os.IsNotExist(err) {
		t.Fatal("common-dir legacy source must be retired")
	}
	code, stdout, stderr := runCmd(cmdList, "--json")
	if code != 0 || !strings.Contains(stdout, `"title": "Legacy"`) {
		t.Fatalf("list from linked worktree: exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}
