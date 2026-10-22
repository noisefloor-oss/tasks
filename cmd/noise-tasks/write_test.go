package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// chdirGitDir creates a temp directory containing a .git marker (so
// discovery treats it as a repo root) and chdirs into it for the test.
func chdirGitDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readTasks(t *testing.T, path string) *backlog.File {
	t.Helper()
	f, err := backlog.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s back: %v", path, err)
	}
	return f
}

func TestInitCreatesEmptyMainTag(t *testing.T) {
	dir := chdirGitDir(t)

	code, stdout, stderr := runCmd(cmdInit)
	if code != 0 {
		t.Fatalf("init exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "tasks.sqlite") || !strings.Contains(stdout, `"main"`) {
		t.Errorf("init output should name the database and tag: %q", stdout)
	}

	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatalf("open created store: %v", err)
	}
	defer db.Close()
	tags, err := db.ListTags()
	if err != nil || len(tags) != 1 || tags[0] != "main" {
		t.Fatalf("tags = %v (err %v), want [main]", tags, err)
	}
	tasks, err := db.ListTasks("main")
	if err != nil || len(tasks) != 0 {
		t.Errorf("tasks = %v (err %v), want empty", tasks, err)
	}

	// The created database is discoverable: list works without --file.
	code, _, stderr = runCmd(cmdList)
	if code != 0 {
		t.Errorf("list after init exit %d, stderr: %s", code, stderr)
	}
}

func TestInitJSONAndCustomTag(t *testing.T) {
	dir := chdirGitDir(t)

	code, stdout, stderr := runCmd(cmdInit, "--tag", "backlog", "--json")
	if code != 0 {
		t.Fatalf("init exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		Path string `json:"path"`
		Tag  string `json:"tag"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("init --json output not JSON: %v\n%s", err, stdout)
	}
	if out.Tag != "backlog" || out.Path != projectStorePath(dir) {
		t.Errorf("json output = %+v", out)
	}
	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tags, _ := db.ListTags()
	if len(tags) != 1 || tags[0] != "backlog" {
		t.Errorf("tags = %v, want [backlog]", tags)
	}
}

func TestInitRefusesWhenStoreExists(t *testing.T) {
	dir := chdirGitDir(t)
	if code, _, stderr := runCmd(cmdInit); code != 0 {
		t.Fatalf("first init exit %d, stderr: %s", code, stderr)
	}

	// From the same directory.
	code, stdout, stderr := runCmd(cmdInit)
	if code != 1 {
		t.Errorf("second init exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("refusal must not write stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("stderr = %q, want already-exists message", stderr)
	}

	// From a subdirectory: the repository boundary still resolves the store.
	sub := filepath.Join(dir, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(cmdInit); code != 1 || !strings.Contains(stderr, "already exists") {
		t.Errorf("init from subdir exit = %d, stderr = %q, want refusal", code, stderr)
	}
}

// TestInitRefusesSymlinkAtStorePath pins §8.5: a dangling symlink at the
// database path is not "absent" — init refuses instead of creating the
// database through the link.
func TestInitRefusesSymlinkAtStorePath(t *testing.T) {
	dir := chdirGitDir(t)
	path := projectStorePath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "dangling.sqlite")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdInit)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "not a regular file") {
		t.Fatalf("init with symlink at store path = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("store path after refusal = %v, %v; want the symlink untouched", info, err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("init created the symlink target: %v", err)
	}
}

func TestInitRefusesWhenLegacyLiveStorePresent(t *testing.T) {
	dir := chdirGitDir(t)
	legacy := filepath.Join(dir, ".git", "noise-tasks", "live.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdInit)
	want := "noise-tasks: legacy live store found; create a verified backup and run noise tasks migrate plan\n"
	if code != 1 || stdout != "" || stderr != want {
		t.Errorf("init with legacy store exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestInitWithExplicitFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "tasks.json")
	code, _, stderr := runCmd(cmdInit, "--file", path)
	if code != 0 {
		t.Fatalf("init --file exit %d, stderr: %s", code, stderr)
	}
	readTasks(t, path)

	// Refuses to overwrite.
	code, stdout, stderr := runCmd(cmdInit, "--file", path)
	if code != 1 || !strings.Contains(stderr, "already exists") {
		t.Errorf("init over existing --file exit = %d, stderr = %q, want refusal", code, stderr)
	}
	if stdout != "" {
		t.Errorf("refusal must not write stdout, got %q", stdout)
	}
}

func TestInitOutsideGitUsesOnlyTheStartingDirectory(t *testing.T) {
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })

	code, _, stderr := runCmd(cmdInit)
	if code != 0 {
		t.Fatalf("init outside Git exit %d, stderr: %s", code, stderr)
	}
	db, err := store.Open(filepath.Join(dir, ".tasks", "tasks.sqlite"))
	if err != nil {
		t.Fatalf("open outside-Git store: %v", err)
	}
	db.Close()
}

func TestAddHappyPath(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "first", "done", "high"),
		taskLit("3", "third", "pending", "low"),
	)

	code, stdout, stderr := runCmd(cmdAdd, "--file", path,
		"--title", "new task",
		"--description", "the desc",
		"--details", "the details",
		"--priority", "high",
		"--dependencies", "1,3")
	if code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, stderr)
	}
	// Prints the new id: monotonic max+1 over existing ids.
	if strings.TrimSpace(stdout) != "4" {
		t.Errorf("add stdout = %q, want the new id 4", stdout)
	}

	f := readTasks(t, path)
	task, err := f.Task("main", "4")
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "new task" || task.Description != "the desc" ||
		task.Details != "the details" || task.Priority != "high" {
		t.Errorf("task fields = %+v", task)
	}
	if task.Status != "pending" {
		t.Errorf("new task status = %q, want pending", task.Status)
	}
	if strings.Join(task.Dependencies, ",") != "1,3" {
		t.Errorf("dependencies = %v, want [1 3]", task.Dependencies)
	}
	if task.UpdatedAt == "" {
		t.Error("new task has no updatedAt")
	}
}

func TestAddJSONOutput(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))
	code, stdout, stderr := runCmd(cmdAdd, "--file", path, "--json", "--title", "t")
	if code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("add --json output not JSON: %v\n%s", err, stdout)
	}
	if out.ID != "2" {
		t.Errorf("id = %q, want 2", out.ID)
	}
}

func TestAddAbsentPriorityNeverWritten(t *testing.T) {
	// SPEC §1: absent priority is medium everywhere and the file is never
	// rewritten to add it — including for newly created tasks.
	path := backlogFile(t, "main", taskLit("1", "first", "pending", ""))
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(cmdAdd, "--file", path, "--title", "no prio"); code != 0 {
		t.Fatalf("add exit stderr: %s", stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(after), `"priority"`) != strings.Count(string(before), `"priority"`) {
		t.Errorf("add without --priority must not write a priority key:\n%s", after)
	}
	f := readTasks(t, path)
	task, err := f.Task("main", "2")
	if err != nil {
		t.Fatal(err)
	}
	if task.Priority != "" {
		t.Errorf("stored priority = %q, want absent", task.Priority)
	}
}

func TestAddToEmptyBacklogStartsAtOne(t *testing.T) {
	path := backlogFile(t, "main")
	code, stdout, stderr := runCmd(cmdAdd, "--file", path, "--title", "first ever")
	if code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "1" {
		t.Errorf("first id = %q, want 1", stdout)
	}
}

func TestAddRefusals(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"missing title", []string{"--file", path}, 2, "--title is required"},
		{"unknown priority", []string{"--file", path, "--title", "t", "--priority", "urgent"}, 2, "unknown priority"},
		{"positional arg", []string{"--file", path, "--title", "t", "stray"}, 2, "unexpected argument"},
		{"nonexistent dep", []string{"--file", path, "--title", "t", "--dependencies", "99"}, 1, "dependency 99 does not exist"},
		{"unknown tag", []string{"--file", path, "--tag", "nope", "--title", "t"}, 1, "not found"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCmd(cmdAdd, tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit = %d, want %d", tc.name, code, tc.code)
		}
		if stdout != "" {
			t.Errorf("%s: refusal must not write stdout, got %q", tc.name, stdout)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: stderr = %q, want %q", tc.name, stderr, tc.want)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("refused add mutated the file")
	}
}

func TestSetStatusHappyPath(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "first", "pending", "high"),
		taskLit("2", "second", "pending", "low"),
	)

	code, stdout, stderr := runCmd(cmdSetStatus, "--file", path, "1", "done")
	if code != 0 {
		t.Fatalf("set-status exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "task 1: pending -> done") {
		t.Errorf("stdout = %q, want the transition", stdout)
	}

	f := readTasks(t, path)
	task, err := f.Task("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" {
		t.Errorf("status = %q, want done", task.Status)
	}
	// completedCount is maintained on every write (SPEC §1).
	meta, err := f.Metadata("main")
	if err != nil {
		t.Fatal(err)
	}
	if meta.CompletedCount != 1 {
		t.Errorf("completedCount = %d, want 1", meta.CompletedCount)
	}
}

func TestSetStatusJSONOutput(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))
	code, stdout, stderr := runCmd(cmdSetStatus, "--file", path, "--json", "1", "in-progress")
	if code != 0 {
		t.Fatalf("set-status exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Previous string `json:"previous"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("set-status --json output not JSON: %v\n%s", err, stdout)
	}
	if out.ID != "1" || out.Status != "in-progress" || out.Previous != "pending" {
		t.Errorf("json output = %+v", out)
	}
}

func TestSetStatusNestedLegacySubtask(t *testing.T) {
	parent := `{
    "id": "32",
    "title": "Diagnostics",
    "status": "pending",
    "dependencies": [],
    "subtasks": [{"id": 1, "title": "Health probes", "status": "pending", "dependencies": [], "parentTaskId": 32, "updatedAt": "2026-08-01T00:00:00Z", "_custom": true}],
    "updatedAt": "2026-08-02T00:00:00Z"
  }`
	path := backlogFile(t, "master", parent)

	code, stdout, stderr := runCmd(cmdSetStatus, "--file", path, "--json", "32.1", "done")
	if code != 0 {
		t.Fatalf("set-status nested exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		ID       string `json:"id"`
		Status   string `json:"status"`
		Previous string `json:"previous"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "32.1" || out.Status != "done" || out.Previous != "pending" {
		t.Errorf("nested transition = %+v", out)
	}

	f := readTasks(t, path)
	parentTask, subtask, err := f.ResolveTaskReference("master", "32.1")
	if err != nil {
		t.Fatal(err)
	}
	if parentTask != nil || subtask.Status != "done" || subtask.UpdatedAt != "2026-08-01T00:00:00Z" {
		t.Errorf("stored nested task = parent %+v, subtask %+v", parentTask, subtask)
	}
	top, err := f.Task("master", "32")
	if err != nil {
		t.Fatal(err)
	}
	if top.Status != "pending" || top.UpdatedAt != "2026-08-02T00:00:00Z" {
		t.Errorf("parent changed: %+v", top)
	}
}

func TestSetStatusRefusals(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"unknown id", []string{"--file", path, "99", "done"}, 1, "not found"},
		{"unknown status", []string{"--file", path, "1", "finished"}, 1, "unknown status"},
		{"missing args", []string{"--file", path, "1"}, 2, "usage"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runCmd(cmdSetStatus, tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit = %d, want %d", tc.name, code, tc.code)
		}
		if stdout != "" {
			t.Errorf("%s: refusal must not write stdout, got %q", tc.name, stdout)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: stderr = %q, want %q", tc.name, stderr, tc.want)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("refused set-status mutated the file")
	}
}

var infoBlockRe = regexp.MustCompile(
	`\n<info added \d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z>\nfrom the trenches\n</info>$`)

func TestAppendHappyPath(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))

	code, stdout, stderr := runCmd(cmdAppend, "--file", path, "1", "from the trenches")
	if code != 0 {
		t.Fatalf("append exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "appended to task 1") {
		t.Errorf("stdout = %q", stdout)
	}

	f := readTasks(t, path)
	task, err := f.Task("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !infoBlockRe.MatchString(task.Details) {
		t.Errorf("details = %q, want a deterministic timestamped info block", task.Details)
	}

	// A second append preserves the first block and adds another.
	if code, _, stderr := runCmd(cmdAppend, "--file", path, "1", "second note"); code != 0 {
		t.Fatalf("second append exit %d, stderr: %s", code, stderr)
	}
	task, err = readTasks(t, path).Task("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.Details, "from the trenches") || !strings.Contains(task.Details, "second note") {
		t.Errorf("details after two appends = %q", task.Details)
	}
	if strings.Count(task.Details, "<info added ") != 2 || strings.Count(task.Details, "</info>") != 2 {
		t.Errorf("want two info blocks, got %q", task.Details)
	}
}

func TestAppendJSONOutput(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))
	code, stdout, stderr := runCmd(cmdAppend, "--file", path, "--json", "1", "note")
	if code != 0 {
		t.Fatalf("append exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		ID        string `json:"id"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("append --json output not JSON: %v\n%s", err, stdout)
	}
	if out.ID != "1" || out.Timestamp == "" {
		t.Errorf("json output = %+v", out)
	}
}

func TestAppendRefusals(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))

	code, stdout, stderr := runCmd(cmdAppend, "--file", path, "99", "text")
	if code != 1 || !strings.Contains(stderr, "not found") {
		t.Errorf("append to unknown id: exit = %d, stderr = %q, want refusal 1", code, stderr)
	}
	if stdout != "" {
		t.Errorf("refusal must not write stdout, got %q", stdout)
	}

	code, _, stderr = runCmd(cmdAppend, "--file", path, "1")
	if code != 2 || !strings.Contains(stderr, "usage") {
		t.Errorf("append without text: exit = %d, stderr = %q, want usage 2", code, stderr)
	}
}

func TestRemoveHappyPath(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "keeper", "pending", "high"),
		taskLit("2", "leaf", "done", "low"),
	)

	code, stdout, stderr := runCmd(cmdRemove, "--file", path, "2")
	if code != 0 {
		t.Fatalf("remove exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "removed task 2") {
		t.Errorf("stdout = %q", stdout)
	}
	if strings.Contains(stdout, "stripped") {
		t.Errorf("no references were stripped, output should not claim so: %q", stdout)
	}

	f := readTasks(t, path)
	tasks, err := f.Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].ID != "1" {
		t.Errorf("remaining tasks = %v, want just task 1", tasks)
	}
	// The removed task was done; completedCount follows.
	meta, err := f.Metadata("main")
	if err != nil {
		t.Fatal(err)
	}
	if meta.CompletedCount != 0 {
		t.Errorf("completedCount = %d, want 0", meta.CompletedCount)
	}
}

func TestRemoveQualifiedSubtask(t *testing.T) {
	parent := `{
    "id": "32",
    "title": "Diagnostics",
    "status": "done",
    "dependencies": [],
    "subtasks": [
      {"id": 1, "title": "implemented", "status": "done", "dependencies": [], "parentTaskId": 32, "_keep": true},
      {"id": "2", "title": "obsolete leaf", "status": "pending", "dependencies": [], "parentTaskId": "32"}
    ],
    "updatedAt": "2026-08-02T00:00:00Z",
    "_parent": "keep"
  }`
	path := backlogFile(t, "master", parent)
	code, stdout, stderr := runCmd(cmdRemove, "--file", path, "--json", "32.2")
	if code != 0 {
		t.Fatalf("remove nested exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		ID       string   `json:"id"`
		Stripped []string `json:"stripped"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "32.2" || len(out.Stripped) != 0 {
		t.Fatalf("output = %+v", out)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(written)
	if strings.Contains(got, "obsolete leaf") || !strings.Contains(got, `"_keep": true`) ||
		!strings.Contains(got, `"updatedAt": "2026-08-02T00:00:00Z"`) || !strings.Contains(got, `"_parent": "keep"`) {
		t.Fatalf("unexpected nested removal bytes:\n%s", got)
	}
}

func TestRemoveQualifiedSubtaskRefusalsDoNotWrite(t *testing.T) {
	parent := `{
    "id": "32", "title": "Diagnostics", "status": "done", "dependencies": [],
    "subtasks": [
      {"id": 1, "title": "base", "status": "pending", "dependencies": [], "parentTaskId": 32},
      {"id": 2, "title": "dependent", "status": "pending", "dependencies": [1], "parentTaskId": 32}
    ]
  }`
	path := backlogFile(t, "master", parent)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		code int
	}{
		{[]string{"--file", path, "32.1"}, 1},
		{[]string{"--file", path, "--force", "32.1"}, 2},
		{[]string{"--file", path, "32.9"}, 1},
	} {
		code, stdout, stderr := runCmd(cmdRemove, tc.args...)
		if code != tc.code || stdout != "" || stderr == "" {
			t.Fatalf("args %v: code=%d stdout=%q stderr=%q", tc.args, code, stdout, stderr)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || string(after) != string(original) {
			t.Fatalf("args %v changed file: %v", tc.args, readErr)
		}
	}
}

func TestRemoveRefusesWhenDependedUpon(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "foundation", "done", "high"),
		taskLit("2", "built on 1", "pending", "high", "1"),
		taskLit("3", "also on 1", "pending", "low", "1", "2"),
	)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdRemove, "--file", path, "1")
	if code != 1 {
		t.Errorf("remove exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("refusal must not write stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "task 1 is a dependency of task(s) 2, 3") {
		t.Errorf("stderr = %q, want the dependent ids", stderr)
	}
	if !strings.Contains(stderr, "--force") {
		t.Errorf("stderr = %q, want a --force hint", stderr)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("refused remove mutated the file")
	}

	// Unknown id refuses too.
	code, _, stderr = runCmd(cmdRemove, "--file", path, "99")
	if code != 1 || !strings.Contains(stderr, "not found") {
		t.Errorf("remove unknown id: exit = %d, stderr = %q", code, stderr)
	}
}

func TestRemoveForceStripsReferences(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "foundation", "done", "high"),
		taskLit("2", "built on 1", "pending", "high", "1", "3"),
		taskLit("3", "independent", "pending", "low"),
	)

	code, stdout, stderr := runCmd(cmdRemove, "--file", path, "--force", "1")
	if code != 0 {
		t.Fatalf("remove --force exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "removed task 1") {
		t.Errorf("stdout = %q", stdout)
	}
	// --force says so when it strips references.
	if !strings.Contains(stdout, "stripped") || !strings.Contains(stdout, "task(s) 2") {
		t.Errorf("stdout = %q, want a stripped-references note naming task 2", stdout)
	}

	f := readTasks(t, path)
	if _, err := f.Task("main", "1"); err == nil {
		t.Error("task 1 still present after remove --force")
	}
	task, err := f.Task("main", "2")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(task.Dependencies, ",") != "3" {
		t.Errorf("task 2 dependencies = %v, want only [3] (ref to 1 stripped)", task.Dependencies)
	}
	if task.UpdatedAt == "2026-08-05T00:00:00Z" {
		t.Error("stripped task's updatedAt not bumped")
	}
}

func TestRemoveForceJSONListsStripped(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "foundation", "done", "high"),
		taskLit("2", "built on 1", "pending", "high", "1"),
	)
	code, stdout, stderr := runCmd(cmdRemove, "--file", path, "--force", "--json", "1")
	if code != 0 {
		t.Fatalf("remove --force exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		ID       string   `json:"id"`
		Stripped []string `json:"stripped"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("remove --json output not JSON: %v\n%s", err, stdout)
	}
	if out.ID != "1" || len(out.Stripped) != 1 || out.Stripped[0] != "2" {
		t.Errorf("json output = %+v, want id 1 stripped [2]", out)
	}

	// Without strips, the list is [] and never null.
	path = backlogFile(t, "main", taskLit("1", "leaf", "pending", "high"))
	code, stdout, _ = runCmd(cmdRemove, "--file", path, "--json", "1")
	if code != 0 {
		t.Fatalf("remove exit %d", code)
	}
	if !strings.Contains(stdout, `"stripped": []`) {
		t.Errorf("stripped must serialize as [], got %q", stdout)
	}
}

// foreignBacklog is a fixture in a foreign format: an inline task, a
// deeply indented task, number ids and deps, compact metadata. The
// minimal-write property says untouched lines survive mutations byte
// for byte.
const foreignBacklog = `{
  "main": {
    "tasks": [
      {"id": 1, "title": "Inline task", "details": "", "status": "pending", "dependencies": [2], "subtasks": [], "updatedAt": "2025-01-01T00:00:00Z"},
      {
            "id": 2,
            "title": "Deep task with <angles> & ampersands",
            "status": "done",
            "dependencies": [],
            "updatedAt": "2025-01-01T00:00:00Z"
      }
    ],
    "metadata": {"version": "1.0.0", "lastModified": "2025-01-01T00:00:00Z", "taskCount": 2, "completedCount": 1, "tags": ["main"]}
  }
}
`

func writeForeignBacklog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(foreignBacklog), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// changedLines returns 1-based indices of lines that differ. A -1 entry
// means the line counts differ.
func changedLines(orig, mod string) []int {
	origLines := strings.Split(orig, "\n")
	modLines := strings.Split(mod, "\n")
	if len(origLines) != len(modLines) {
		return []int{-1}
	}
	var out []int
	for i := range modLines {
		if modLines[i] != origLines[i] {
			out = append(out, i+1)
		}
	}
	return out
}

func TestSetStatusMinimalDiffOnForeignFile(t *testing.T) {
	path := writeForeignBacklog(t)
	if code, _, stderr := runCmd(cmdSetStatus, "--file", path, "1", "in-progress"); code != 0 {
		t.Fatalf("set-status exit stderr: %s", stderr)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Only the inline target task (line 4) and the metadata line (13) may change.
	changed := changedLines(foreignBacklog, string(written))
	if len(changed) != 2 || changed[0] != 4 || changed[1] != 13 {
		t.Errorf("changed lines = %v, want [4 13]\n%s", changed, written)
	}
	if !strings.Contains(string(written), `"dependencies": [2]`) {
		t.Error("untouched fields of the target task were reformatted")
	}
}

func TestAppendMinimalDiffOnForeignFile(t *testing.T) {
	path := writeForeignBacklog(t)
	if code, _, stderr := runCmd(cmdAppend, "--file", path, "1", "note"); code != 0 {
		t.Fatalf("append exit stderr: %s", stderr)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := changedLines(foreignBacklog, string(written))
	if len(changed) != 2 || changed[0] != 4 || changed[1] != 13 {
		t.Errorf("changed lines = %v, want [4 13]\n%s", changed, written)
	}
	if !strings.Contains(string(written), `\n<info added `) {
		t.Error("info block missing from the spliced details")
	}
}

func TestAddMinimalDiffOnForeignFile(t *testing.T) {
	path := writeForeignBacklog(t)
	code, stdout, stderr := runCmd(cmdAdd, "--file", path, "--title", "third")
	if code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "3" {
		t.Errorf("new id = %q, want 3 (max+1 over number ids)", stdout)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Every original line survives byte-for-byte except the last task's
	// closing brace (gains a comma) and the metadata line.
	origLines := strings.Split(strings.TrimSuffix(foreignBacklog, "\n"), "\n")
	writtenStr := string(written)
	for i, line := range origLines {
		if strings.TrimSpace(line) == "}" && i == 10 { // task 2's close, line 11
			continue
		}
		if strings.Contains(line, `"metadata"`) {
			continue
		}
		if !strings.Contains(writtenStr, line+"\n") {
			t.Errorf("original line %d not preserved verbatim: %q", i+1, line)
		}
	}
	if !strings.Contains(writtenStr, "      },\n") {
		t.Error("previous last task should gain a trailing comma")
	}

	f := readTasks(t, path)
	task, err := f.Task("main", "3")
	if err != nil {
		t.Fatal(err)
	}
	if task.Title != "third" || task.Status != "pending" {
		t.Errorf("appended task = %+v", task)
	}
}

// TestHelpExitsZeroAcrossCommands pins the -help exit-code alignment:
// asking for help is not an error, so every command exits 0.
func TestHelpExitsZeroAcrossCommands(t *testing.T) {
	type commandCase struct {
		run         func([]string, io.Writer, io.Writer) int
		commonFlags bool
	}
	cmds := map[string]commandCase{
		"setup":      {cmdSetup, false},
		"init":       {cmdInit, true},
		"list":       {cmdList, true},
		"next":       {cmdNext, true},
		"show":       {cmdShow, true},
		"add":        {cmdAdd, true},
		"set-status": {cmdSetStatus, true},
		"append":     {cmdAppend, true},
		"remove":     {cmdRemove, true},
		"verify":     {verifyMain, true},
		"generate":   {cmdGenerate, true},
		"version":    {cmdVersion, false},
	}
	for name, tc := range cmds {
		code, stdout, stderr := runCmd(tc.run, "-help")
		if code != 0 {
			t.Errorf("%s -help exit = %d, want 0", name, code)
		}
		if stdout != "" {
			t.Errorf("%s -help wrote stdout: %q", name, stdout)
		}
		if tc.commonFlags && !strings.Contains(stderr, "-file") {
			t.Errorf("%s -help should document the common flags, stderr = %q", name, stderr)
		}
		if !tc.commonFlags && strings.Contains(stderr, "-file") {
			t.Errorf("%s -help must not document project flags, stderr = %q", name, stderr)
		}
		// A genuinely bad flag still exits 2.
		if code, _, _ := runCmd(tc.run, "-no-such-flag"); code != 2 {
			t.Errorf("%s with a bad flag exit = %d, want 2", name, code)
		}
	}
}

// TestAddMilestoneNeverDanglesAcrossConcurrentRemove races add --milestone
// against milestone remove --force; whichever order the store serializes
// (§8.2), no task may end up referencing a removed milestone.
func TestAddMilestoneNeverDanglesAcrossConcurrentRemove(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main")
	reseed := func() {
		db, err := store.Open(projectStorePath(dir))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var exists *backlog.ErrMilestoneExists
		if err := db.AddMilestone("main", "v1"); err != nil && !errors.As(err, &exists) {
			t.Fatal(err)
		}
	}
	reseed()

	for i := 0; i < 12; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			runCmd(cmdAdd, "--milestone", "v1", "--title", fmt.Sprintf("t%d", i))
		}(i)
		go func() {
			defer wg.Done()
			runCmd(cmdMilestone, "remove", "--force", "v1")
		}()
		wg.Wait()
		reseed()
	}

	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	entries, err := db.MilestoneEntries("main")
	if err != nil {
		t.Fatal(err)
	}
	known := make(map[string]bool, len(entries))
	for _, e := range entries {
		known[e.Name] = true
	}
	tasks, err := db.ListTasks("main")
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range tasks {
		if task.Milestone != "" && !known[task.Milestone] {
			t.Errorf("task %s references removed milestone %q", task.ID, task.Milestone)
		}
	}
}
