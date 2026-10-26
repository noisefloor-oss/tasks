package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// doctorFamilyFor returns the report's family whose boundary is dir.
func doctorFamilyFor(t *testing.T, report doctorReport, dir string) doctorFamily {
	t.Helper()
	for _, family := range report.Families {
		if family.Boundary == dir {
			return family
		}
	}
	t.Fatalf("family %s not reported: %+v", dir, report.Families)
	return doctorFamily{}
}

// doctorUnboundPaths returns the family's unbound-worktree rows keyed by
// path.
func doctorUnboundPaths(t *testing.T, family doctorFamily) map[string]HygieneWorktreeRow {
	t.Helper()
	rows := make(map[string]HygieneWorktreeRow)
	for _, row := range family.UnboundWorktrees {
		rows[row.Path] = row
	}
	return rows
}

func TestDoctorReportsUnboundRegisteredWorktrees(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	hygieneProvisionOwned(t, dir)

	root := filepath.Dir(dir)
	// A clean registered worktree no saga row names.
	reviewClean := filepath.Join(root, "review-clean")
	hygieneGit(t, dir, "worktree", "add", "-q", reviewClean, "-b", "review-clean")
	// A dirty one: the finding must name its dirtiness.
	reviewDirty := filepath.Join(root, "review-dirty")
	hygieneGit(t, dir, "worktree", "add", "-q", reviewDirty, "-b", "review-dirty")
	if err := os.WriteFile(filepath.Join(reviewDirty, "local.txt"), []byte("wip"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A detached one: no branch, the head names the checkout.
	reviewDetached := filepath.Join(root, "review-detached")
	hygieneGit(t, dir, "worktree", "add", "-q", "--detach", reviewDetached)

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	family := doctorFamilyFor(t, report, dir)
	rows := doctorUnboundPaths(t, family)
	if len(rows) != 3 {
		t.Fatalf("unbound worktrees = %+v", family.UnboundWorktrees)
	}
	if _, ok := rows[canonTestPath(t, dir, "main-1")]; ok {
		t.Fatalf("owned worktree reported unbound: %+v", family.UnboundWorktrees)
	}
	row, ok := rows[reviewClean]
	if !ok || row.Branch != "review-clean" || row.Detached || !row.Exists || row.Dirty || row.Head == "" {
		t.Fatalf("clean review row = %+v", row)
	}
	row, ok = rows[reviewDirty]
	if !ok || row.Branch != "review-dirty" || !row.Exists || !row.Dirty {
		t.Fatalf("dirty review row = %+v", row)
	}
	row, ok = rows[reviewDetached]
	if !ok || !row.Detached || row.Branch != "" || !row.Exists || row.Head == "" {
		t.Fatalf("detached review row = %+v", row)
	}

	code, stdout, _ = runDoctor("--root", root)
	if code != 0 ||
		!strings.Contains(stdout, "hard finding: unbound worktree "+reviewClean+" branch review-clean exists") ||
		!strings.Contains(stdout, "hard finding: unbound worktree "+reviewDirty+" branch review-dirty exists dirty") ||
		!strings.Contains(stdout, "hard finding: unbound worktree "+reviewDetached+" detached at ") ||
		!strings.Contains(stdout, "3 unbound registered worktree(s)") ||
		strings.Contains(stdout, canonTestPath(t, dir, "main-1")) {
		t.Fatalf("human doctor = code %d stdout %q", code, stdout)
	}
}

func TestDoctorSagaBoundWorktreeIsNotAFinding(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	hygieneProvisionOwned(t, dir)

	assertNoFinding := func() {
		t.Helper()
		code, stdout, stderr := runDoctor("--root", filepath.Dir(dir), "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
		}
		family := doctorFamilyFor(t, decodeDoctor(t, stdout), dir)
		if len(family.UnboundWorktrees) != 0 || family.UnboundWorktrees == nil ||
			family.UnboundWorktreesError != "" {
			t.Fatalf("unbound worktrees = %+v error %q", family.UnboundWorktrees, family.UnboundWorktreesError)
		}
	}

	// An active provision saga owns its path.
	assertNoFinding()

	// A terminal saga row owns its path too (§8.9/§9.4): removal of the
	// binding evidence is never inferred from status.
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	if _, err := sqlDB.Exec(`UPDATE worktree_sagas SET status='failed', binding_active=0`); err != nil {
		t.Fatal(err)
	}
	assertNoFinding()

	// Human output carries the zero count and no finding line.
	code, stdout, _ := runDoctor("--root", filepath.Dir(dir))
	if code != 0 ||
		strings.Contains(stdout, "hard finding") ||
		!strings.Contains(stdout, "0 unbound registered worktree(s)") {
		t.Fatalf("human doctor = code %d stdout %q", code, stdout)
	}
}

func TestDoctorPreSagaStorePrintsEmptyArray(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main", "pre saga state")
	hygieneGit(t, dir, "worktree", "add", "-q",
		filepath.Join(filepath.Dir(dir), "lane"), "-b", "lane")
	// A store older than the saga surface carries no ownership
	// evidence: no findings, and the JSON array stays present and empty.
	sqlDB, err := sql.Open("sqlite", "file:"+projectStorePath(dir)+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`PRAGMA user_version=4`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runDoctor("--root", filepath.Dir(dir), "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"unboundWorktrees": []`) || strings.Contains(stdout, "null") {
		t.Fatalf("raw family json = %s", stdout)
	}
	family := doctorFamilyFor(t, decodeDoctor(t, stdout), dir)
	if len(family.UnboundWorktrees) != 0 || family.UnboundWorktreesError != "" {
		t.Fatalf("unbound worktrees = %+v error %q", family.UnboundWorktrees, family.UnboundWorktreesError)
	}
}

func TestDoctorContainsSagaReadFailurePerFamily(t *testing.T) {
	root := doctorWorkspace(t)
	famA := filepath.Join(root, "fam-a")
	doctorGit(t, famA, "config", "user.email", "t@example.com")
	doctorGit(t, famA, "config", "user.name", "t")
	doctorGit(t, famA, "commit", "-q", "--allow-empty", "-m", "seed")
	doctorGit(t, famA, "worktree", "add", "-q", filepath.Join(root, "lane"), "-b", "lane")
	// A family whose saga evidence cannot be read — here a saga table
	// the store's schema version promises but the file lacks — is
	// contained on that family's row, never fatal to the walk.
	sqlDB, err := sql.Open("sqlite", "file:"+filepath.Join(famA, ".git", "noise-tasks", "tasks.sqlite")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`DROP TABLE worktree_sagas`); err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	family := doctorFamilyFor(t, report, famA)
	if family.AuthorityError != "" || family.UnboundWorktreesError == "" ||
		len(family.UnboundWorktrees) != 0 {
		t.Fatalf("fam-a = %+v", family)
	}

	code, stdout, _ = runDoctor("--root", root)
	if code != 0 ||
		!strings.Contains(stdout, "store ok schema ") ||
		!strings.Contains(stdout, "unbound worktree check unavailable: ") ||
		strings.Contains(stdout, "hard finding") {
		t.Fatalf("human doctor = code %d stdout %q", code, stdout)
	}
}

func TestDoctorContainsGitFailurePerFamily(t *testing.T) {
	root := doctorWorkspace(t)
	famA := filepath.Join(root, "fam-a")
	famB := filepath.Join(root, "fam-b")
	for _, fam := range []string{famA, famB} {
		doctorGit(t, fam, "config", "user.email", "t@example.com")
		doctorGit(t, fam, "config", "user.name", "t")
		doctorGit(t, fam, "commit", "-q", "--allow-empty", "-m", "seed")
	}
	laneA := filepath.Join(root, "lane-a")
	doctorGit(t, famA, "worktree", "add", "-q", laneA, "-b", "lane-a")
	laneB := filepath.Join(root, "lane-b")
	doctorGit(t, famB, "worktree", "add", "-q", laneB, "-b", "lane-b")
	db, err := store.CreateWithOwner(filepath.Join(famB, ".git", "noise-tasks", "tasks.sqlite"), famB)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	// A family whose worktree listing Git cannot serve — a corrupt HEAD
	// — is contained on its row, and the healthy initialized family's
	// real finding still reports.
	if err := os.WriteFile(filepath.Join(famA, ".git", "HEAD"), []byte("garbage\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"unboundWorktrees": []`) || strings.Contains(stdout, `"unboundWorktrees": null`) {
		t.Fatalf("raw family json = %s", stdout)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 3 {
		t.Fatalf("families = %+v", report.Families)
	}
	family := doctorFamilyFor(t, report, famA)
	if family.AuthorityError != "" || family.UnboundWorktreesError == "" ||
		len(family.UnboundWorktrees) != 0 || family.UnboundWorktrees == nil {
		t.Fatalf("fam-a = %+v", family)
	}
	family = doctorFamilyFor(t, report, famB)
	if family.UnboundWorktreesError != "" || len(family.UnboundWorktrees) != 1 ||
		family.UnboundWorktrees[0].Path != laneB || family.UnboundWorktrees[0].Branch != "lane-b" {
		t.Fatalf("fam-b = %+v", family)
	}

	code, stdout, _ = runDoctor("--root", root)
	if code != 0 ||
		!strings.Contains(stdout, "unbound worktree check unavailable: ") ||
		!strings.Contains(stdout, "hard finding: unbound worktree "+laneB+" branch lane-b exists") {
		t.Fatalf("human doctor = code %d stdout %q", code, stdout)
	}
}

func TestDoctorRootAtLinkedWorktreeReportsOneFamily(t *testing.T) {
	root := doctorWorkspace(t)
	famA := filepath.Join(root, "fam-a")
	doctorGit(t, famA, "config", "user.email", "t@example.com")
	doctorGit(t, famA, "config", "user.name", "t")
	doctorGit(t, famA, "commit", "-q", "--allow-empty", "-m", "seed")
	lane := filepath.Join(root, "lane")
	doctorGit(t, famA, "worktree", "add", "-q", lane, "-b", "lane")

	code, stdout, stderr := runDoctor("--root", lane, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 1 {
		t.Fatalf("families = %+v", report.Families)
	}
	family := report.Families[0]
	// The family is keyed by the common directory, reported once at the
	// walked boundary, and still finds the family's real store.
	if family.Boundary != lane || family.CommonDir != filepath.Join(famA, ".git") ||
		!family.Initialized || family.UnboundWorktreesError != "" {
		t.Fatalf("family = %+v", family)
	}
	// The primary checkout fam-a is Git's main worktree and never a
	// finding; the lane the walk started from is.
	if len(family.UnboundWorktrees) != 1 || family.UnboundWorktrees[0].Path != lane ||
		family.UnboundWorktrees[0].Branch != "lane" {
		t.Fatalf("unbound worktrees = %+v", family.UnboundWorktrees)
	}

	// Human output is exactly one family line, one finding line, one
	// summary line: newline regressions cannot pass a Contains check.
	code, stdout, _ = runDoctor("--root", lane)
	if code != 0 {
		t.Fatalf("human doctor = code %d stdout %q", code, stdout)
	}
	want := []string{
		fmt.Sprintf("%s: store ok schema %d", lane, family.StoreSchemaVersion),
		"  hard finding: unbound worktree " + lane + " branch lane exists",
		"1 families: 1 initialized, 0 without a store, 0 carrying retired layouts, 1 unbound registered worktree(s)",
		"",
	}
	got := strings.Split(stdout, "\n")
	if len(got) != len(want) {
		t.Fatalf("human lines = %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("human line %d = %q want %q", i, got[i], want[i])
		}
	}
}

func TestDoctorAndHygieneNeverRunConfiguredFsmonitor(t *testing.T) {
	dir := chdirWorktreeRepo(t)
	seedProjectStore(t, dir, "main", "fsmonitor state")
	review := filepath.Join(filepath.Dir(dir), "review")
	hygieneGit(t, dir, "worktree", "add", "-q", review, "-b", "review")
	// A walked family's repo config is not trusted enough to run its
	// code: the dirtiness probe must disable a configured fsmonitor
	// helper instead of executing it.
	marker := filepath.Join(filepath.Dir(dir), "fsmonitor-ran")
	script := filepath.Join(filepath.Dir(dir), "fsmonitor.sh")
	body := "#!/bin/sh\ntouch '" + marker + "'\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	hygieneGit(t, dir, "config", "core.fsmonitor", script)

	code, stdout, stderr := runDoctor("--root", filepath.Dir(dir), "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("doctor executed the configured fsmonitor helper")
	}
	code, stdout, stderr = runHygiene("--json")
	if code != 0 || stderr != "" {
		t.Fatalf("hygiene = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("hygiene executed the configured fsmonitor helper")
	}
	if !strings.Contains(stdout, `"path": "`+review+`"`) {
		t.Fatalf("hygiene report missing review row: %s", stdout)
	}
}
