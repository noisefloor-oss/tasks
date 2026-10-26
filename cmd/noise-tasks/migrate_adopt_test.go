package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// adoptionLayoutDocument is a retired-layout §1 document at the repo root
// (`.tasks/tasks/tasks.json`) with a sole legacy `master` tag, a non-dense
// id range, a subtask, and an unknown field.
const adoptionLayoutDocument = `{
  "master": {
    "tasks": [
      {"id": "19", "title": "Relay cutover", "description": "d", "details": "", "status": "pending", "dependencies": ["38"], "subtasks": [{"id": 1, "title": "sub", "description": "", "details": "", "status": "done", "dependencies": [], "parentTaskId": "19"}], "updatedAt": "2026-10-01T10:00:00Z", "triage": {"score": 7}},
      {"id": "38", "title": "Research agent", "description": "", "details": "", "status": "in-progress", "dependencies": [], "subtasks": [], "updatedAt": "2026-10-02T11:00:00Z"}
    ],
    "metadata": {"version": "1", "lastModified": "2026-10-02T11:00:00Z", "taskCount": 2, "completedCount": 0, "tags": ["master"]}
  }
}`

// adoptionFixtureRepo prepares a git-dir repo whose only task history is the
// retired layout, and returns (repo, layoutPath).
func adoptionFixtureRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := chdirGitDir(t)
	layoutPath := filepath.Join(dir, ".tasks", "tasks", "tasks.json")
	if err := os.MkdirAll(filepath.Dir(layoutPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(layoutPath, []byte(adoptionLayoutDocument), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, layoutPath
}

// adoptionCLIBundle runs backup create --adopt and backup verify.
func adoptionCLIBundle(t *testing.T, layoutPath, name string) string {
	t.Helper()
	bundle := filepath.Join(t.TempDir(), name)
	code, stdout, stderr := runCmd(cmdBackup, "create", "--adopt", layoutPath, "--output", bundle, "--json")
	if code != 0 {
		t.Fatalf("backup create --adopt exit %d: %s", code, stderr)
	}
	var manifest store.Manifest
	if err := json.Unmarshal([]byte(stdout), &manifest); err != nil {
		t.Fatalf("backup create --adopt stdout not JSON: %s", stdout)
	}
	code, _, stderr = runCmd(cmdBackup, "verify", bundle)
	if code != 0 {
		t.Fatalf("backup verify exit %d: %s", code, stderr)
	}
	return bundle
}

// TestMigrateAdoptCLI walks the whole §8.6A flow through the command layer:
// bundle, plan, apply, list/show probes, replay, and the layout's survival.
func TestMigrateAdoptCLI(t *testing.T) {
	repo, layoutPath := adoptionFixtureRepo(t)
	bundle := adoptionCLIBundle(t, layoutPath, "adopted.ntbackup")

	code, stdout, stderr := runCmd(cmdMigrate, "adopt", "plan", "--layout", layoutPath, "--json", bundle)
	if code != 0 {
		t.Fatalf("migrate adopt plan exit %d: %s", code, stderr)
	}
	var planOut struct {
		Plan       *store.MigrationPlan `json:"plan"`
		PlanPath   string               `json:"planPath"`
		FamilyID   string               `json:"familyId"`
		PlanDigest string               `json:"planDigest"`
	}
	if err := json.Unmarshal([]byte(stdout), &planOut); err != nil {
		t.Fatalf("plan stdout not JSON: %s", stdout)
	}
	if planOut.Plan.Source.Kind != store.SourceKindAdoptedJSON {
		t.Fatalf("plan source kind %q", planOut.Plan.Source.Kind)
	}
	if planOut.PlanPath != filepath.Join(repo, ".git", "noise-tasks", store.AdoptionPlanFile) {
		t.Fatalf("plan path %s", planOut.PlanPath)
	}
	code, _, stderr = runCmd(cmdMigrate, "adopt", "apply", "--digest", planOut.PlanDigest, "--offline", "--request-id", "adopt-cli-1", "--json", bundle)
	if code != 0 {
		t.Fatalf("migrate adopt apply exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git", "noise-tasks", "tasks.sqlite")); err != nil {
		t.Fatalf("adopted store missing: %v", err)
	}
	// The layout document is never removed.
	if _, err := os.Stat(layoutPath); err != nil {
		t.Fatalf("layout document removed by adoption: %v", err)
	}

	// The adopted store serves reads with full fidelity.
	code, stdout, stderr = runCmd(cmdList, "--tag", "master", "--json")
	if code != 0 {
		t.Fatalf("list exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"id": "38"`) || !strings.Contains(stdout, "Research agent") {
		t.Fatalf("adopted tasks missing from list: %s", stdout)
	}
	if !strings.Contains(stdout, `"tag": "master"`) {
		t.Fatalf("master tag not adopted: %s", stdout)
	}
	code, stdout, stderr = runCmd(cmdShow, "--tag", "master", "--json", "19")
	if code != 0 {
		t.Fatalf("show exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"subtasks"`) || !strings.Contains(stdout, "2026-10-01T10:00:00Z") {
		t.Fatalf("subtasks or verbatim updatedAt lost: %s", stdout)
	}

	// Replay of the same request id returns the same receipt.
	code, stdout, stderr = runCmd(cmdMigrate, "adopt", "apply", "--digest", planOut.PlanDigest, "--offline", "--request-id", "adopt-cli-1", "--json", bundle)
	if code != 0 {
		t.Fatalf("replay exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, `"replayed": true`) {
		t.Fatalf("replay not reported: %s", stdout)
	}

	// Every adoption step after initialization fails closed (exit 1).
	second := filepath.Join(t.TempDir(), "adopted-2.ntbackup")
	code, _, stderr = runCmd(cmdBackup, "create", "--adopt", layoutPath, "--output", second)
	if code != 1 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("bundle over existing store exit %d: %s", code, stderr)
	}
	code, _, stderr = runCmd(cmdMigrate, "adopt", "plan", "--layout", layoutPath, bundle)
	if code != 1 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("plan over existing store exit %d: %s", code, stderr)
	}
	// Replay still works after the operator removes the layout (§8.6A).
	if err := os.Remove(layoutPath); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCmd(cmdMigrate, "adopt", "apply", "--digest", planOut.PlanDigest, "--offline", "--request-id", "adopt-cli-1", "--json", bundle)
	if code != 0 || !strings.Contains(stdout, `"replayed": true`) {
		t.Fatalf("replay with source removed exit %d: %s %s", code, stdout, stderr)
	}
}

// TestMigrateAdoptRecoveryDiagnosticCLI proves an adoption recovery journal
// with no store gates normal commands with the recovery diagnostic (§8.7).
func TestMigrateAdoptRecoveryDiagnosticCLI(t *testing.T) {
	repo, layoutPath := adoptionFixtureRepo(t)
	bundle := adoptionCLIBundle(t, layoutPath, "diag.ntbackup")
	code, stdout, stderr := runCmd(cmdMigrate, "adopt", "plan", "--layout", layoutPath, "--json", bundle)
	if code != 0 {
		t.Fatalf("plan exit %d: %s", code, stderr)
	}
	var planOut struct {
		Plan *store.MigrationPlan `json:"plan"`
	}
	if err := json.Unmarshal([]byte(stdout), &planOut); err != nil {
		t.Fatal(err)
	}
	commonDir := filepath.Join(repo, ".git")
	storeDir := filepath.Join(commonDir, "noise-tasks")
	j, err := store.JournalBytes(&store.MigrationJournal{
		SchemaVersion:          store.JournalSchemaVersion,
		RequestID:              "adopt-diag-1",
		FamilyID:               planOut.Plan.FamilyID,
		PlanDigest:             planOut.Plan.PlanDigest,
		SourceComparisonDigest: planOut.Plan.Source.ComparisonDigest,
		BackupPayloadDigest:    planOut.Plan.Backup.PayloadDigest,
		TargetSchemaVersion:    store.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, store.AdoptionJournalFile), j, 0o600); err != nil {
		t.Fatal(err)
	}
	want := "adoption recovery required; rerun noise tasks migrate adopt apply with the original plan and backup"
	code, stdout, stderr = runCmd(cmdList, "--tag", "master")
	if code != 1 || stdout != "" || !strings.Contains(stderr, want) {
		t.Fatalf("list exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// The matching apply completes recovery and removes the journal.
	code, _, stderr = runCmd(cmdMigrate, "adopt", "apply", "--digest", planOut.Plan.PlanDigest, "--offline", "--request-id", "adopt-diag-1", bundle)
	if code != 0 {
		t.Fatalf("completing apply exit %d: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(storeDir, store.AdoptionJournalFile)); !os.IsNotExist(err) {
		t.Fatal("journal not removed after recovery completion")
	}
	code, _, stderr = runCmd(cmdList, "--tag", "master")
	if code != 0 {
		t.Fatalf("list after recovery exit %d: %s", code, stderr)
	}
}

// TestMigrateAdoptUsageCLI pins the usage errors (exit 2).
func TestMigrateAdoptUsageCLI(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"adopt bare", []string{"adopt"}},
		{"adopt unknown verb", []string{"adopt", "sync"}},
		{"apply without offline", []string{"adopt", "apply", "--digest", "sha256:" + strings.Repeat("0", 64), "--request-id", "r1", "b.ntbackup"}},
		{"apply without digest", []string{"adopt", "apply", "--offline", "--request-id", "r1", "b.ntbackup"}},
		{"apply without request id", []string{"adopt", "apply", "--offline", "--digest", "sha256:" + strings.Repeat("0", 64), "b.ntbackup"}},
		{"plan without layout", []string{"adopt", "plan", "b.ntbackup"}},
		{"both provenance flags", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.args == nil {
				if code, _, _ := runCmd(cmdBackup, "create", "--legacy", "--adopt", "x.json", "--output", "b.ntbackup"); code != 2 {
					t.Fatalf("exit %d, want 2", code)
				}
				return
			}
			if code, _, _ := runCmd(cmdMigrate, tc.args...); code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
		})
	}
}

// TestMigrateAdoptionJournalSurfacesCLI covers the round-2 fences: the §8.6
// compatibility verbs and restore refuse with the stable adoption
// diagnostic under an adoption recovery journal, context reports the §2.2
// blocked contract, doctor's inventory names the journal, a malformed
// journal is an operational failure, and a published target beside a valid
// journal is served.
func TestMigrateAdoptionJournalSurfacesCLI(t *testing.T) {
	repo, layoutPath := adoptionFixtureRepo(t)
	storeDir := filepath.Join(repo, ".git", "noise-tasks")
	journalPath := filepath.Join(storeDir, store.AdoptionJournalFile)
	bundle := adoptionCLIBundle(t, layoutPath, "fences.ntbackup")
	code, stdout, stderr := runCmd(cmdMigrate, "adopt", "plan", "--layout", layoutPath, "--json", bundle)
	if code != 0 {
		t.Fatalf("plan exit %d: %s", code, stderr)
	}
	var planOut struct {
		Plan *store.MigrationPlan `json:"plan"`
	}
	if err := json.Unmarshal([]byte(stdout), &planOut); err != nil {
		t.Fatal(err)
	}
	j, err := store.JournalBytes(&store.MigrationJournal{
		SchemaVersion:          store.JournalSchemaVersion,
		RequestID:              "adopt-fence-cli",
		FamilyID:               planOut.Plan.FamilyID,
		PlanDigest:             planOut.Plan.PlanDigest,
		SourceComparisonDigest: planOut.Plan.Source.ComparisonDigest,
		BackupPayloadDigest:    planOut.Plan.Backup.PayloadDigest,
		TargetSchemaVersion:    store.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journalPath, j, 0o600); err != nil {
		t.Fatal(err)
	}
	want := "adoption recovery required; rerun noise tasks migrate adopt apply with the original plan and backup"

	// The §8.6 compatibility verbs refuse with the adoption diagnostic.
	legacyPath := filepath.Join(storeDir, "live.json")
	if err := os.WriteFile(legacyPath, []byte(migrateLegacyDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	legacyBundle := filepath.Join(t.TempDir(), "legacy-fence.ntbackup")
	code, _, stderr = runCmd(cmdBackup, "create", "--legacy", "--output", legacyBundle)
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("backup --legacy under adoption journal exit %d: %s", code, stderr)
	}
	code, _, stderr = runCmd(cmdMigrate, "plan", legacyBundle)
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("migrate plan under adoption journal exit %d: %s", code, stderr)
	}
	code, _, stderr = runCmd(cmdMigrate, "apply", "--digest", "sha256:"+strings.Repeat("0", 64), "--offline", "--request-id", "mig-adopt-fence", legacyBundle)
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("migrate apply under adoption journal exit %d: %s", code, stderr)
	}
	// With the layout still on disk (always true in state 2), setup names
	// the real remedy, not pre-cutover retired-layout guidance.
	code, _, stderr = runSetup("", false)
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("setup under adoption journal exit %d: %s", code, stderr)
	}
	// Restore keeps the stable diagnostic rather than a generic store error.
	code, _, stderr = runCmd(cmdBackup, "restore", "--offline", "--request-id", "restore-under-adopt", bundle)
	if code != 1 || !strings.Contains(stderr, want) {
		t.Fatalf("restore under adoption journal exit %d: %s", code, stderr)
	}

	// context reports the blocked contract (§2.2) and stays exit 0.
	code, stdout, stderr = runCmd(cmdContext, "--json")
	if code != 0 || !strings.Contains(stdout, `"blocked": true`) ||
		!strings.Contains(stdout, `"adoption-recovery-required"`) {
		t.Fatalf("context exit %d stdout %s stderr %s", code, stdout, stderr)
	}

	// doctor's per-family inventory names the journal.
	code, stdout, stderr = runDoctor("--root", repo, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor exit %d: %s", code, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 1 || !report.Families[0].RecoveryJournal {
		t.Fatalf("doctor families = %+v", report.Families)
	}

	// A malformed journal is an operational failure: exit 1, empty stdout.
	if err := os.WriteFile(journalPath, []byte(`{"not":"strict"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runCmd(cmdContext, "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "invalid adoption recovery journal") {
		t.Fatalf("malformed journal context exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// Restore the strict journal, clear the §8.6 source, and complete the
	// adoption; then a valid journal beside the published target (state 3)
	// leaves normal commands serving the proven store.
	if err := os.WriteFile(journalPath, j, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runCmd(cmdMigrate, "adopt", "apply", "--digest", planOut.Plan.PlanDigest, "--offline", "--request-id", "adopt-fence-cli", bundle)
	if code != 0 {
		t.Fatalf("completing apply exit %d: %s", code, stderr)
	}
	if err := os.WriteFile(journalPath, j, 0o600); err != nil {
		t.Fatal(err)
	}
	code, _, stderr = runCmd(cmdList, "--tag", "master")
	if code != 0 {
		t.Fatalf("list beside published target and journal exit %d: %s", code, stderr)
	}
}

// TestMigrateAdoptionMidflightFencesCLI proves §8.6 artifacts appearing
// between adoption plan and apply fence both adoption verbs at the command
// layer (§8.6A).
func TestMigrateAdoptionMidflightFencesCLI(t *testing.T) {
	repo, layoutPath := adoptionFixtureRepo(t)
	storeDir := filepath.Join(repo, ".git", "noise-tasks")
	bundle := adoptionCLIBundle(t, layoutPath, "midflight.ntbackup")
	code, stdout, stderr := runCmd(cmdMigrate, "adopt", "plan", "--layout", layoutPath, "--json", bundle)
	if code != 0 {
		t.Fatalf("plan exit %d: %s", code, stderr)
	}
	var planOut struct {
		Plan *store.MigrationPlan `json:"plan"`
	}
	if err := json.Unmarshal([]byte(stdout), &planOut); err != nil {
		t.Fatal(err)
	}
	apply := func() (int, string) {
		c, _, e := runCmd(cmdMigrate, "adopt", "apply", "--digest", planOut.Plan.PlanDigest, "--offline", "--request-id", "adopt-mid-cli", bundle)
		return c, e
	}

	// The derived live.json appears mid-adoption.
	legacyPath := filepath.Join(storeDir, "live.json")
	if err := os.WriteFile(legacyPath, []byte(migrateLegacyDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	if c, e := apply(); c != 1 || !strings.Contains(e, "belongs to migrate plan and migrate apply") {
		t.Fatalf("apply under mid-flight live.json exit %d: %s", c, e)
	}
	second := filepath.Join(t.TempDir(), "mid.ntbackup")
	if c, _, e := runCmd(cmdBackup, "create", "--adopt", layoutPath, "--output", second); c != 1 || !strings.Contains(e, "belongs to migrate plan and migrate apply") {
		t.Fatalf("bundle under mid-flight live.json exit %d: %s", c, e)
	}
	if err := os.Remove(legacyPath); err != nil {
		t.Fatal(err)
	}

	// A §8.6 recovery journal appears mid-adoption.
	j, err := store.JournalBytes(&store.MigrationJournal{
		SchemaVersion:          store.JournalSchemaVersion,
		RequestID:              "stuck-mid-cli",
		FamilyID:               planOut.Plan.FamilyID,
		PlanDigest:             "sha256:" + strings.Repeat("5", 64),
		SourceComparisonDigest: "sha256:" + strings.Repeat("6", 64),
		BackupPayloadDigest:    "sha256:" + strings.Repeat("7", 64),
		TargetSchemaVersion:    store.SchemaVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, store.MigrationJournalFile), j, 0o600); err != nil {
		t.Fatal(err)
	}
	if c, e := apply(); c != 1 || !strings.Contains(e, "§8.6 migration recovery journal exists") {
		t.Fatalf("apply under mid-flight migration journal exit %d: %s", c, e)
	}
	if c, _, e := runCmd(cmdBackup, "create", "--adopt", layoutPath, "--output", second); c != 1 || !strings.Contains(e, "§8.6 migration recovery journal exists") {
		t.Fatalf("bundle under mid-flight migration journal exit %d: %s", c, e)
	}
}
