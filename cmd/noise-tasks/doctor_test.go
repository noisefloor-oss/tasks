package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

func runDoctor(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cmdDoctor(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func doctorGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// doctorWorkspace builds a throwaway workspace: an initialized family, a
// legacy-only family carrying all three retired layouts, a nested repo,
// and a bare directory that is no family at all.
func doctorWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	initRepo := func(name string) string {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		doctorGit(t, dir, "init", "-q", "-b", "main")
		return dir
	}

	famA := initRepo("fam-a")
	db, err := store.CreateWithOwner(filepath.Join(famA, ".git", "noise-tasks", "tasks.sqlite"), famA)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	famB := initRepo("fam-b")
	for _, rel := range []string{
		filepath.Join(".taskmaster", "tasks", "tasks.json"),
		filepath.Join(".noisetask", "tasks.json"),
		filepath.Join(".tasks", "tasks", "tasks.json"),
	} {
		full := filepath.Join(famB, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(`{"master":{"tasks":[]}}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	initRepo(filepath.Join("fam-b", "nested"))

	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root
}

func decodeDoctor(t *testing.T, stdout string) doctorReport {
	t.Helper()
	var report doctorReport
	dec := json.NewDecoder(strings.NewReader(stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("decode doctor JSON: %v\n%s", err, stdout)
	}
	return report
}

func TestDoctorCommandInventory(t *testing.T) {
	root := doctorWorkspace(t)

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if report.SchemaVersion != 1 || report.Root != root || report.Families == nil {
		t.Fatalf("report = %+v", report)
	}
	if len(report.Families) != 3 {
		t.Fatalf("families = %+v", report.Families)
	}
	famA, famB, nested := report.Families[0], report.Families[1], report.Families[2]
	if !strings.HasSuffix(famA.Boundary, "fam-a") || !famA.Initialized ||
		famA.StoreSchemaVersion != store.SchemaVersion ||
		famA.StoreOwnerBoundary != famA.Boundary || famA.AuthorityError != "" {
		t.Fatalf("fam-a = %+v", famA)
	}
	if !strings.HasSuffix(famB.Boundary, "fam-b") || famB.Initialized || len(famB.LegacyLayouts) != 3 {
		t.Fatalf("fam-b = %+v", famB)
	}
	for i := 1; i < len(famB.LegacyLayouts); i++ {
		if famB.LegacyLayouts[i-1].Path >= famB.LegacyLayouts[i].Path {
			t.Fatalf("legacy layouts not sorted: %+v", famB.LegacyLayouts)
		}
	}
	if !strings.HasSuffix(nested.Boundary, "nested") || nested.Initialized {
		t.Fatalf("nested = %+v", nested)
	}

	code, stdout, stderr = runDoctor("--root", root)
	if code != 0 || stderr != "" ||
		!strings.Contains(stdout, "store ok schema ") ||
		!strings.Contains(stdout, "no store") ||
		!strings.Contains(stdout, "legacy .taskmaster/tasks/tasks.json (") ||
		!strings.Contains(stdout, "3 families: 1 initialized, 2 without a store, 1 carrying retired layouts") {
		t.Fatalf("human doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestDoctorCommandWorktreesDedupToOneFamily(t *testing.T) {
	root := doctorWorkspace(t)
	famA := filepath.Join(root, "fam-a")
	doctorGit(t, famA, "config", "user.email", "t@example.com")
	doctorGit(t, famA, "config", "user.name", "t")
	doctorGit(t, famA, "commit", "-q", "--allow-empty", "-m", "seed")
	doctorGit(t, famA, "worktree", "add", "-q", filepath.Join(root, "lane"), "-b", "lane")

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 3 {
		t.Fatalf("linked worktree not deduplicated: %+v", report.Families)
	}
	for _, family := range report.Families {
		if strings.HasSuffix(family.Boundary, "lane") {
			t.Fatalf("worktree lane reported as its own family: %+v", family)
		}
	}
}

func TestDoctorCommandRelativeRootDeduplicates(t *testing.T) {
	root := doctorWorkspace(t)
	famA := filepath.Join(root, "fam-a")
	doctorGit(t, famA, "config", "user.email", "t@example.com")
	doctorGit(t, famA, "config", "user.name", "t")
	doctorGit(t, famA, "commit", "-q", "--allow-empty", "-m", "seed")
	doctorGit(t, famA, "worktree", "add", "-q", filepath.Join(root, "lane"), "-b", "lane")

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	code, stdout, stderr := runDoctor("--root", ".", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if report.Root != root {
		t.Fatalf("root = %q, want the canonical %q", report.Root, root)
	}
	if len(report.Families) != 3 {
		t.Fatalf("relative root broke deduplication: %+v", report.Families)
	}
}

func TestDoctorCommandNeverTouchesAValidStore(t *testing.T) {
	root := doctorWorkspace(t)
	storeDir := filepath.Join(root, "fam-a", ".git", "noise-tasks")
	before, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	hadBefore := map[string]bool{}
	for _, entry := range before {
		hadBefore[entry.Name()] = true
	}
	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 3 || !report.Families[0].Initialized ||
		report.Families[0].AuthorityError != "" ||
		report.Families[0].StoreSchemaVersion != store.SchemaVersion ||
		report.Families[0].StoreOwnerBoundary != report.Families[0].Boundary {
		t.Fatalf("fam-a facts = %+v", report.Families[0])
	}
	after, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range after {
		if !hadBefore[entry.Name()] {
			t.Errorf("doctor created %s beside a valid store", entry.Name())
		}
	}
}

func TestDoctorCommandPrimaryPreferenceRefreshesLegacyFacts(t *testing.T) {
	root := t.TempDir()
	primary := filepath.Join(root, "z-main")
	if err := os.MkdirAll(primary, 0o750); err != nil {
		t.Fatal(err)
	}
	doctorGit(t, primary, "init", "-q", "-b", "main")
	doctorGit(t, primary, "config", "user.email", "t@example.com")
	doctorGit(t, primary, "config", "user.name", "t")
	doctorGit(t, primary, "commit", "-q", "--allow-empty", "-m", "seed")
	// The lane sorts before the primary, so the worktree is discovered
	// first and the primary later replaces it by preference.
	doctorGit(t, primary, "worktree", "add", "-q", filepath.Join(root, "a-lane"), "-b", "lane")

	primaryOnly := filepath.Join(primary, ".taskmaster", "tasks")
	if err := os.MkdirAll(primaryOnly, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(primaryOnly, "tasks.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	laneOnly := filepath.Join(root, "a-lane", ".noisetask")
	if err := os.MkdirAll(laneOnly, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(laneOnly, "tasks.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 1 {
		t.Fatalf("families = %+v", report.Families)
	}
	family := report.Families[0]
	if family.Boundary != primary {
		t.Fatalf("boundary = %q, want the primary %q", family.Boundary, primary)
	}
	if len(family.LegacyLayouts) != 1 || family.LegacyLayouts[0].Path != ".taskmaster/tasks/tasks.json" {
		t.Fatalf("legacy layouts = %+v, want the primary checkout's", family.LegacyLayouts)
	}
}

func TestDoctorCommandSkipsUnreadableSubdirectories(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	root := doctorWorkspace(t)
	locked := filepath.Join(root, "fam-b", "locked")
	if err := os.MkdirAll(locked, 0o750); err != nil {
		t.Fatal(err)
	}
	doctorGit(t, locked, "init", "-q", "-b", "main")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o750)

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 3 {
		t.Fatalf("unreadable subdirectory aborted the walk: %+v", report.Families)
	}
	for _, family := range report.Families {
		if strings.HasSuffix(family.Boundary, "locked") {
			t.Fatalf("unreadable family reported: %+v", family)
		}
	}
}

func TestDoctorCommandIgnoresSymlinkedRepositories(t *testing.T) {
	outside := t.TempDir()
	doctorGit(t, outside, "init", "-q", "-b", "main")
	root := doctorWorkspace(t)
	if err := os.Symlink(outside, filepath.Join(root, "linked-repo")); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 3 {
		t.Fatalf("symlinked repository escaped the root: %+v", report.Families)
	}
	for _, family := range report.Families {
		if !strings.HasPrefix(family.Boundary, root) {
			t.Fatalf("family outside the root: %+v", family)
		}
	}
}

func TestDoctorCommandReportsUnlistableButTraversableRepo(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission bits")
	}
	root := doctorWorkspace(t)
	locked := filepath.Join(root, "fam-b", "locked")
	if err := os.MkdirAll(locked, 0o750); err != nil {
		t.Fatal(err)
	}
	doctorGit(t, locked, "init", "-q", "-b", "main")
	db, err := store.CreateWithOwner(filepath.Join(locked, ".git", "noise-tasks", "tasks.sqlite"), locked)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	// A nested repo inside the unlistable directory must not surface.
	nested := filepath.Join(locked, "hidden-repo")
	if err := os.MkdirAll(nested, 0o750); err != nil {
		t.Fatal(err)
	}
	doctorGit(t, nested, "init", "-q", "-b", "main")
	if err := os.Chmod(locked, 0o111); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o750)

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	var lockedFamily *doctorFamily
	for i := range report.Families {
		if strings.HasSuffix(report.Families[i].Boundary, "locked") {
			lockedFamily = &report.Families[i]
		}
		if strings.HasSuffix(report.Families[i].Boundary, "hidden-repo") {
			t.Fatalf("unlistable directory was descended into: %+v", report.Families[i])
		}
	}
	if lockedFamily == nil || !lockedFamily.Initialized ||
		lockedFamily.StoreSchemaVersion != store.SchemaVersion ||
		lockedFamily.StoreOwnerBoundary != lockedFamily.Boundary {
		t.Fatalf("traversable-but-unlistable family = %+v", lockedFamily)
	}
}
func TestDoctorCommandEscapesURIPathCharacters(t *testing.T) {
	for _, name := range []string{"hash#repo", "question?repo", "percent%repo"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			repo := filepath.Join(root, name)
			if err := os.MkdirAll(repo, 0o750); err != nil {
				t.Fatal(err)
			}
			doctorGit(t, repo, "init", "-q", "-b", "main")
			db, err := store.CreateWithOwner(filepath.Join(repo, ".git", "noise-tasks", "tasks.sqlite"), repo)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()

			before, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			code, stdout, stderr := runDoctor("--root", root, "--json")
			if code != 0 || stderr != "" {
				t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
			}
			report := decodeDoctor(t, stdout)
			if len(report.Families) != 1 || !report.Families[0].Initialized ||
				report.Families[0].AuthorityError != "" ||
				report.Families[0].StoreSchemaVersion != store.SchemaVersion {
				t.Fatalf("family = %+v", report.Families[0])
			}
			after, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != len(before) {
				t.Fatalf("doctor created files at the workspace root: %v", after)
			}
		})
	}
}
func TestDoctorCommandSeparateGitDirFindsFamilyStore(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	admin := filepath.Join(root, "admin")
	if err := os.MkdirAll(checkout, 0o750); err != nil {
		t.Fatal(err)
	}

	doctorGit(t, root, "init", "-q", "--separate-git-dir", admin, checkout)
	db, err := store.CreateWithOwner(filepath.Join(admin, "noise-tasks", "tasks.sqlite"), checkout)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 1 {
		t.Fatalf("families = %+v", report.Families)
	}
	family := report.Families[0]
	if family.Boundary != checkout || family.CommonDir != admin ||
		!family.Initialized || family.StorePath != filepath.Join(admin, "noise-tasks", "tasks.sqlite") ||
		family.StoreSchemaVersion != store.SchemaVersion || family.StoreOwnerBoundary != checkout {
		t.Fatalf("separate-git-dir family = %+v", family)
	}
}

func TestDoctorCommandUnreadableStoreReportsAuthorityError(t *testing.T) {
	root := doctorWorkspace(t)
	storeDir := filepath.Join(root, "fam-a", ".git", "noise-tasks")
	garbage := filepath.Join(storeDir, "tasks.sqlite")
	if err := os.WriteFile(garbage, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runDoctor("--root", root, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("doctor = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	report := decodeDoctor(t, stdout)
	if len(report.Families) != 3 || !report.Families[0].Initialized ||
		report.Families[0].AuthorityError == "" || report.Families[0].StoreSchemaVersion != 0 {
		t.Fatalf("fam-a = %+v", report.Families[0])
	}
	had := map[string]bool{}
	for _, entry := range before {
		had[entry.Name()] = true
	}
	after, err := os.ReadDir(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range after {
		if had[entry.Name()] {
			continue
		}
		t.Errorf("doctor created %s", entry.Name())
	}
	if t.Failed() {
		t.FailNow()
	}
}

func TestDoctorCommandUsageAndBadRoot(t *testing.T) {
	for _, args := range [][]string{{"--file", "x.json"}, {"--tag", "main"}, {"--json", "extra"}} {
		code, stdout, _ := runDoctor(args...)
		if code != 2 || stdout != "" {
			t.Errorf("args %q = code %d stdout %q", args, code, stdout)
		}
	}
	code, stdout, stderr := runDoctor("--root", "/nonexistent-doctor-root", "--json")
	if code != 1 || stdout != "" || stderr == "" {
		t.Fatalf("bad root = code %d stdout %q stderr %q", code, stdout, stderr)
	}
}
