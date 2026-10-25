package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const outsideGitRehomingDiagnostic = "noise-tasks: existing outside-Git task store cannot be rehomed automatically; back it up and restore it into this repository family\n"

func TestOutsideGitStoreRefusesRehomingAfterGitInit(t *testing.T) {
	project := t.TempDir()
	t.Setenv("NOISE_PROJECT_DIR", project)
	if code, stdout, stderr := runCmd(cmdInit); code != 0 {
		t.Fatalf("outside-Git init = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if code, stdout, stderr := runCmd(cmdAdd, "--title", "preserve me"); code != 0 {
		t.Fatalf("outside-Git add = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	outsideStore := filepath.Join(project, ".tasks", "tasks.sqlite")
	bundle := filepath.Join(project, "before-git.ntbackup")
	if code, stdout, stderr := runCmd(cmdBackup, "create", "--output", bundle); code != 0 {
		t.Fatalf("outside-Git backup = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	before, err := os.ReadFile(outsideStore)
	if err != nil {
		t.Fatal(err)
	}
	ownerGit(t, project, "init")
	gitStore := projectStorePath(project)

	for _, tc := range []struct {
		name string
		run  func() (int, string, string)
	}{
		{name: "list", run: func() (int, string, string) { return runCmd(cmdList, "--json") }},
		{name: "add", run: func() (int, string, string) { return runCmd(cmdAdd, "--title", "second authority") }},
		{name: "init", run: func() (int, string, string) { return runCmd(cmdInit) }},
		{name: "setup", run: func() (int, string, string) {
			return runCmd(cmdSetup, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
		}},
		{name: "context", run: func() (int, string, string) { return runCmd(cmdContext, "--json") }},
		{name: "backup restore", run: func() (int, string, string) {
			return runCmd(cmdBackup, "restore", "--offline", "--request-id", "rehome-refusal", bundle)
		}},
		{name: "migrate apply", run: func() (int, string, string) {
			return runCmd(cmdMigrate, "apply", "--digest", "sha256:"+strings.Repeat("0", 64),
				"--offline", "--request-id", "rehome-refusal", bundle)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := tc.run()
			if code != 1 || stdout != "" || stderr != outsideGitRehomingDiagnostic {
				t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
			}
			after, err := os.ReadFile(outsideStore)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("outside-Git store changed")
			}
			if _, err := os.Lstat(gitStore); !os.IsNotExist(err) {
				t.Fatalf("Git-side store exists after refusal: %v", err)
			}
		})
	}
}

func TestOutsideGitRehomingPreservesMigrationRecoveryPrecedence(t *testing.T) {
	project := t.TempDir()
	t.Setenv("NOISE_PROJECT_DIR", project)
	if code, _, stderr := runCmd(cmdInit); code != 0 {
		t.Fatalf("outside-Git init = code %d stderr %q", code, stderr)
	}
	ownerGit(t, project, "init")
	writeRecoveryJournal(t, filepath.Join(project, ".git", "noise-tasks"), "")

	want := "noise-tasks: migration recovery required; rerun noise tasks migrate apply with the original plan and backup\n"
	for name, run := range map[string]func() (int, string, string){
		"list": func() (int, string, string) { return runCmd(cmdList, "--json") },
		"init": func() (int, string, string) { return runCmd(cmdInit) },
		"setup": func() (int, string, string) {
			return runCmd(cmdSetup, "--adapter", "anthropic-api", "--model", "m", "--yes", "--json")
		},
		"backup restore": func() (int, string, string) {
			return runCmd(cmdBackup, "restore", "--offline", "--request-id", "recover1", filepath.Join(project, "missing.ntbackup"))
		},
	} {
		code, stdout, stderr := run()
		if code != 1 || stdout != "" || stderr != want {
			t.Errorf("%s = code %d stdout %q stderr %q", name, code, stdout, stderr)
		}
	}
	code, stdout, stderr := runCmd(cmdContext, "--json")
	if code != 0 || stderr != "" || !strings.Contains(stdout, `"blockedReason": "migration-recovery-required"`) {
		t.Fatalf("context = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = runCmd(cmdMigrate, "apply", "--digest", "sha256:"+strings.Repeat("0", 64),
		"--offline", "--request-id", "recover1", filepath.Join(project, "missing.ntbackup"))
	if code != 1 || stdout != "" || stderr == "" || stderr == want || stderr == outsideGitRehomingDiagnostic {
		t.Fatalf("recovery apply = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	writeRecoveryJournal(t, filepath.Join(project, ".git", "noise-tasks"), "not json")
	code, stdout, stderr = runCmd(cmdBackup, "restore", "--offline", "--request-id", "recover1", filepath.Join(project, "missing.ntbackup"))
	if code != 1 || stdout != "" || !strings.HasPrefix(stderr, "noise-tasks: invalid migration recovery journal:") {
		t.Fatalf("restore with malformed journal = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !os.IsNotExist(err) {
		t.Fatalf("Git-side store exists during recovery: %v", err)
	}
}

func TestOutsideGitRehomingGateRejectsUnsafeFallbackEntries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		plant func(*testing.T, string)
	}{
		{name: "symlink", plant: func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(t.TempDir(), "target.sqlite"), path); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "directory", plant: func(t *testing.T, path string) {
			if err := os.MkdirAll(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := setupProject(t)
			fallback := filepath.Join(project, ".tasks", "tasks.sqlite")
			tc.plant(t, fallback)
			code, stdout, stderr := runCmd(cmdList)
			want := "noise-tasks: " + fallback + " is not a regular file\n"
			if code != 1 || stdout != "" || stderr != want {
				t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
			}
			if _, err := os.Lstat(projectStorePath(project)); !os.IsNotExist(err) {
				t.Fatalf("Git-side store exists after refusal: %v", err)
			}
		})
	}
}

func TestOutsideGitRehomingGatePropagatesLstatErrors(t *testing.T) {
	project := setupProject(t)
	if err := os.WriteFile(filepath.Join(project, ".tasks"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(cmdList)
	if code != 1 || stdout != "" || stderr == "" ||
		stderr == outsideGitRehomingDiagnostic || strings.Contains(stderr, "project is not initialized") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := os.Lstat(projectStorePath(project)); !os.IsNotExist(err) {
		t.Fatalf("Git-side store exists after refusal: %v", err)
	}
}
