package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cleanBacklog = `{
  "main": {
    "tasks": [
      {"id": "1", "title": "one", "description": "", "details": "", "status": "done", "priority": "high", "dependencies": [], "subtasks": [], "updatedAt": "2026-01-01T00:00:00Z"},
      {"id": "2", "title": "two", "description": "", "details": "", "status": "pending", "priority": "medium", "dependencies": ["1"], "subtasks": [], "updatedAt": "2026-01-01T00:00:00Z"}
    ],
    "metadata": {"version": "1.0", "lastModified": "2026-01-01T00:00:00Z", "taskCount": 2, "completedCount": 1, "tags": ["main"]}
  }
}
`

func backlogWith(tasks ...string) string {
	return `{"main": {"tasks": [` + strings.Join(tasks, ", ") + `]}}`
}

func taskJSONFixture(id, status, deps string) string {
	return fmt.Sprintf(`{"id": %q, "title": "t%s", "status": %q, "dependencies": [%s], "subtasks": []}`, id, id, status, deps)
}

func writeTasks(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runVerifyCmd(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = verifyMain(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func TestVerifyClean(t *testing.T) {
	path := writeTasks(t, t.TempDir(), cleanBacklog)
	code, out, stderr := runVerifyCmd(t, "--file", path)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(out, "ok: 2 task(s) across 1 tag(s) verified") {
		t.Errorf("stdout = %q, want ok summary", out)
	}
}

func TestVerifyStructuralRefusals(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"duplicate id", backlogWith(taskJSONFixture("1", "pending", ""), taskJSONFixture("1", "pending", "")), "duplicate task id"},
		{"invalid id", backlogWith(taskJSONFixture("0", "pending", "")), "invalid task id"},
		{"invalid status", backlogWith(taskJSONFixture("1", "wat", "")), "invalid status"},
		{"invalid dependency id", backlogWith(taskJSONFixture("1", "pending", `"nope"`)), "invalid task id"},
		{"invalid priority", backlogWith(`{"id": "1", "title": "x", "status": "pending", "priority": "urgent", "dependencies": [], "subtasks": []}`), "invalid priority"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTasks(t, t.TempDir(), tc.content)
			code, out, stderr := runVerifyCmd(t, "--file", path)
			if code != 1 {
				t.Fatalf("exit = %d, want 1\nstdout: %s\nstderr: %s", code, out, stderr)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("stdout = %q, want it to contain %q", out, tc.want)
			}
		})
	}
}

func TestVerifyMissingDependency(t *testing.T) {
	path := writeTasks(t, t.TempDir(), backlogWith(taskJSONFixture("1", "pending", `"9"`)))
	code, out, _ := runVerifyCmd(t, "--file", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout: %s", code, out)
	}
	if !strings.Contains(out, "task 1: dependency 9 does not exist") {
		t.Errorf("stdout = %q, want missing-dependency finding", out)
	}
}

func TestVerifySelfDependency(t *testing.T) {
	path := writeTasks(t, t.TempDir(), backlogWith(taskJSONFixture("1", "pending", `"1"`)))
	code, out, _ := runVerifyCmd(t, "--file", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout: %s", code, out)
	}
	if !strings.Contains(out, "task 1 depends on itself") {
		t.Errorf("stdout = %q, want self-dependency finding", out)
	}
}

func TestVerifyLongerCycle(t *testing.T) {
	path := writeTasks(t, t.TempDir(), backlogWith(
		taskJSONFixture("1", "pending", `"2"`),
		taskJSONFixture("2", "pending", `"3"`),
		taskJSONFixture("3", "pending", `"1"`),
	))
	code, out, _ := runVerifyCmd(t, "--file", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout: %s", code, out)
	}
	if !strings.Contains(out, "dependency cycle: 1 -> 2 -> 3 -> 1") {
		t.Errorf("stdout = %q, want cycle finding", out)
	}
}

func TestVerifyCounterMismatch(t *testing.T) {
	content := strings.Replace(cleanBacklog, `"completedCount": 1`, `"completedCount": 5`, 1)
	path := writeTasks(t, t.TempDir(), content)
	code, out, _ := runVerifyCmd(t, "--file", path)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout: %s", code, out)
	}
	if !strings.Contains(out, "completedCount is 5, want 1") {
		t.Errorf("stdout = %q, want counter-mismatch finding", out)
	}
}

func TestVerifyTagFlagRestrictsChecks(t *testing.T) {
	content := `{"main": {"tasks": [` + taskJSONFixture("1", "pending", "") + `]}, "other": {"tasks": [` + taskJSONFixture("1", "pending", `"9"`) + `]}}`
	path := writeTasks(t, t.TempDir(), content)

	code, out, _ := runVerifyCmd(t, "--file", path)
	if code != 1 {
		t.Fatalf("no --tag: exit = %d, want 1\nstdout: %s", code, out)
	}
	if !strings.Contains(out, "finding: other:") {
		t.Errorf("no --tag stdout = %q, want finding in tag other", out)
	}

	code, out, _ = runVerifyCmd(t, "--file", path, "--tag", "main")
	if code != 0 {
		t.Fatalf("--tag main: exit = %d, want 0\nstdout: %s", code, out)
	}
}

func TestVerifyErrorsExitTwo(t *testing.T) {
	cleanPath := writeTasks(t, t.TempDir(), cleanBacklog)
	malformedPath := writeTasks(t, t.TempDir(), "{not json")
	cases := []struct {
		name string
		args []string
	}{
		{"missing file", []string{"--file", filepath.Join(t.TempDir(), "nope.json")}},
		{"malformed json", []string{"--file", malformedPath}},
		{"unknown flag", []string{"--file", cleanPath, "--nope"}},
		{"positional arg", []string{"--file", cleanPath, "extra"}},
		{"unknown tag", []string{"--file", cleanPath, "--tag", "nope"}},
		{"against outside git repo", []string{"--file", cleanPath, "--against", "HEAD"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, stderr := runVerifyCmd(t, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2\nstdout: %s\nstderr: %s", code, out, stderr)
			}
		})
	}
}

func TestVerifyJSONOutput(t *testing.T) {
	path := writeTasks(t, t.TempDir(), cleanBacklog)
	code, out, _ := runVerifyCmd(t, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s", code, out)
	}
	var report verifyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out)
	}
	if !report.Clean || len(report.Findings) != 0 {
		t.Errorf("report = %+v, want clean with no findings", report)
	}

	dupPath := writeTasks(t, t.TempDir(), backlogWith(taskJSONFixture("1", "pending", ""), taskJSONFixture("1", "pending", "")))
	code, out, _ = runVerifyCmd(t, "--file", dupPath, "--json")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout: %s", code, out)
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out)
	}
	if report.Clean || len(report.Findings) != 1 || report.Findings[0].Kind != "duplicate-id" {
		t.Errorf("report = %+v, want one duplicate-id finding", report)
	}
}

// --- git-backed tests for --against ---

func gitEnv() []string {
	return append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// initRepoWithBacklog creates a git repo containing .tasks/tasks/tasks.json
// with the given content, committed as HEAD, and returns the file path.
func initRepoWithBacklog(t *testing.T, content string) (repo, path string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo = t.TempDir()
	mustGit(t, repo, "init", "-q")
	sub := filepath.Join(repo, ".tasks/tasks")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	path = writeTasks(t, sub, content)
	mustGit(t, repo, "add", ".")
	mustGit(t, repo, "commit", "-q", "-m", "seed")
	return repo, path
}

func TestVerifyAgainstRemovalIsFatal(t *testing.T) {
	_, path := initRepoWithBacklog(t, cleanBacklog)
	// Drop task 2, keep the file structurally clean, and add task 3 to
	// prove an addition cannot mask a removal.
	rewritten := `{
  "main": {
    "tasks": [
      {"id": "1", "title": "one", "description": "", "details": "", "status": "done", "priority": "high", "dependencies": [], "subtasks": [], "updatedAt": "2026-01-01T00:00:00Z"},
      {"id": "3", "title": "three", "description": "", "details": "", "status": "pending", "priority": "low", "dependencies": [], "subtasks": [], "updatedAt": "2026-01-02T00:00:00Z"}
    ],
    "metadata": {"version": "1.0", "lastModified": "2026-01-02T00:00:00Z", "taskCount": 2, "completedCount": 1, "tags": ["main"]}
  }
}
`
	if err := os.WriteFile(path, []byte(rewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runVerifyCmd(t, "--file", path, "--against", "HEAD")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(out, `task 2 "two" removed (present in HEAD)`) {
		t.Errorf("stdout = %q, want removal finding", out)
	}
	if !strings.Contains(out, `task 3 "three" added`) {
		t.Errorf("stdout = %q, want addition still reported", out)
	}
}

func TestVerifyAgainstQualifiedSubtaskRemovalIsNamedAndFatal(t *testing.T) {
	withSubtasks := strings.Replace(cleanBacklog, `"subtasks": []`,
		`"subtasks": [
        {"id": 1, "title": "kept", "status": "done", "dependencies": [], "parentTaskId": "1"},
        {"id": "2", "title": "obsolete", "status": "pending", "dependencies": [], "parentTaskId": 1}
      ]`, 1)
	_, path := initRepoWithBacklog(t, withSubtasks)
	withoutObsolete := strings.Replace(withSubtasks,
		`,
        {"id": "2", "title": "obsolete", "status": "pending", "dependencies": [], "parentTaskId": 1}`,
		"", 1)
	if err := os.WriteFile(path, []byte(withoutObsolete), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runVerifyCmd(t, "--file", path, "--against", "HEAD")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(out, `subtask 1.2 "obsolete" removed (present in HEAD)`) {
		t.Errorf("stdout = %q, want qualified removal finding", out)
	}

	code, out, stderr = runVerifyCmd(t, "--file", path, "--against", "HEAD", "--json")
	if code != 1 {
		t.Fatalf("json exit = %d, want 1; stderr=%s", code, stderr)
	}
	var report verifyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, finding := range report.Findings {
		if finding.Kind == "subtask-removed" && finding.Task == "1.2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("report did not name qualified removal: %+v", report.Findings)
	}
}

func TestVerifyAgainstAdditionIsClean(t *testing.T) {
	_, path := initRepoWithBacklog(t, cleanBacklog)
	added := strings.Replace(cleanBacklog,
		`"dependencies": ["1"], "subtasks": [], "updatedAt": "2026-01-01T00:00:00Z"}
    ],`,
		`"dependencies": ["1"], "subtasks": [], "updatedAt": "2026-01-01T00:00:00Z"},
      {"id": "3", "title": "three", "description": "", "details": "", "status": "pending", "priority": "low", "dependencies": [], "subtasks": [], "updatedAt": "2026-01-02T00:00:00Z"}
    ],`, 1)
	if err := os.WriteFile(path, []byte(added), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runVerifyCmd(t, "--file", path, "--against", "HEAD")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(out, `task 3 "three" added (not in HEAD)`) {
		t.Errorf("stdout = %q, want addition report", out)
	}
	if !strings.Contains(out, "no tasks removed (1 added, 0 changed)") {
		t.Errorf("stdout = %q, want against summary", out)
	}
}

func TestVerifyAgainstFieldChanges(t *testing.T) {
	_, path := initRepoWithBacklog(t, cleanBacklog)
	changed := strings.Replace(cleanBacklog, `"title": "two", "description": "", "details": "", "status": "pending"`,
		`"title": "two", "description": "", "details": "", "status": "in-progress"`, 1)
	changed = strings.Replace(changed, `"title": "one"`, `"title": "one renamed"`, 1)
	if err := os.WriteFile(path, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, stderr := runVerifyCmd(t, "--file", path, "--against", "HEAD")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(out, `task 2 status: "pending" -> "in-progress"`) {
		t.Errorf("stdout = %q, want status change report", out)
	}
	if !strings.Contains(out, `task 1 title: "one" -> "one renamed"`) {
		t.Errorf("stdout = %q, want title change report", out)
	}

	code, out, _ = runVerifyCmd(t, "--file", path, "--against", "HEAD", "--json")
	if code != 0 {
		t.Fatalf("json: exit = %d, want 0\nstdout: %s", code, out)
	}
	var report verifyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("stdout is not valid JSON: %v\n%s", err, out)
	}
	if !report.Clean || len(report.Changes) != 2 {
		t.Fatalf("report = %+v, want clean with 2 changes", report)
	}
	var sawStatus bool
	for _, c := range report.Changes {
		if c.Kind == "field-change" && c.Task == "2" && c.Field == "status" && c.Old == "pending" && c.New == "in-progress" {
			sawStatus = true
		}
	}
	if !sawStatus {
		t.Errorf("changes = %+v, want structured status change", report.Changes)
	}
}

func TestVerifyAgainstUnchangedIsClean(t *testing.T) {
	_, path := initRepoWithBacklog(t, cleanBacklog)
	code, out, stderr := runVerifyCmd(t, "--file", path, "--against", "HEAD")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(out, "against HEAD: no tasks removed (0 added, 0 changed)") {
		t.Errorf("stdout = %q, want clean against summary", out)
	}
}

func TestVerifyAgainstBadRefExitsTwo(t *testing.T) {
	_, path := initRepoWithBacklog(t, cleanBacklog)
	code, out, stderr := runVerifyCmd(t, "--file", path, "--against", "no-such-ref")
	if code != 2 {
		t.Fatalf("exit = %d, want 2\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(stderr, "no-such-ref") {
		t.Errorf("stderr = %q, want mention of the bad ref", stderr)
	}
}

func TestVerifyAgainstInLinkedWorktree(t *testing.T) {
	repo, _ := initRepoWithBacklog(t, cleanBacklog)
	wt := filepath.Join(t.TempDir(), "wt")
	mustGit(t, repo, "worktree", "add", "-q", "--detach", wt)

	// Sanity: in a linked worktree .git is a file, not a directory.
	info, err := os.Stat(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		t.Fatal(".git in linked worktree is a directory, expected a file")
	}

	wtPath := filepath.Join(wt, ".tasks/tasks", "tasks.json")
	code, out, stderr := runVerifyCmd(t, "--file", wtPath, "--against", "HEAD")
	if code != 0 {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, out, stderr)
	}
	if !strings.Contains(out, "no tasks removed") {
		t.Errorf("stdout = %q, want clean against summary", out)
	}
}
