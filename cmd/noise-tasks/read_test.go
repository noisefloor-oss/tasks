package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// taskLit builds one task object literal for test fixtures.
func taskLit(id, title, status, priority string, deps ...string) string {
	depJSON, _ := json.Marshal(deps)
	if deps == nil {
		depJSON = []byte("[]")
	}
	prio := ""
	if priority != "" {
		prio = fmt.Sprintf(`"priority": %q,`, priority)
	}
	return fmt.Sprintf(`{
    "id": %q,
    "title": %q,
    "description": "desc of %s",
    "details": "",
    "status": %q,
    %s
    "dependencies": %s,
    "subtasks": [],
    "updatedAt": "2026-08-05T00:00:00Z"
  }`, id, title, id, status, prio, depJSON)
}

func backlogFile(t *testing.T, tag string, tasks ...string) string {
	t.Helper()
	content := fmt.Sprintf(`{
  %q: {
    "tasks": [%s],
    "metadata": {
      "version": "1.0.0",
      "lastModified": "2026-08-05T00:00:00Z",
      "taskCount": %d,
      "completedCount": 0,
      "tags": [%q]
    }
  }
}
`, tag, strings.Join(tasks, ", "), len(tasks), tag)
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runCmd(cmd func([]string, io.Writer, io.Writer) int, args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = cmd(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func nextID(t *testing.T, path string) string {
	t.Helper()
	code, stdout, stderr := runCmd(cmdNext, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("next exit %d, stderr: %s", code, stderr)
	}
	var got struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("next --json output not JSON: %v\n%s", err, stdout)
	}
	return got.ID
}

func TestNextPriorityOrder(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "low prio", "pending", "low"),
		taskLit("2", "medium prio", "pending", "medium"),
		taskLit("3", "high prio", "pending", "high"),
	)
	if id := nextID(t, path); id != "3" {
		t.Errorf("next = task %s, want 3 (high beats medium/low)", id)
	}

	path = backlogFile(t, "main",
		taskLit("1", "low prio", "pending", "low"),
		taskLit("2", "medium prio", "pending", "medium"),
	)
	if id := nextID(t, path); id != "2" {
		t.Errorf("next = task %s, want 2 (medium beats low)", id)
	}
}

func TestNextUnsetPriorityRanksAsMedium(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "low prio", "pending", "low"),
		taskLit("2", "no prio", "pending", ""),
	)
	if id := nextID(t, path); id != "2" {
		t.Errorf("next = task %s, want 2 (unset priority ranks as medium, above low)", id)
	}
}

func TestNextDanglingDependencyGates(t *testing.T) {
	// A dependency that resolves to no task in the tag counts as
	// unsatisfied (SPEC §4): the high-priority task stays gated and the
	// free low-priority task wins.
	path := backlogFile(t, "main",
		taskLit("1", "gated by ghost dep", "pending", "high", "99"),
		taskLit("2", "free low", "pending", "low"),
	)
	if id := nextID(t, path); id != "2" {
		t.Errorf("next = task %s, want 2 (dangling dep gates task 1)", id)
	}

	// With only the dangling-dep task in the tag, nothing is actionable.
	path = backlogFile(t, "main",
		taskLit("1", "gated by ghost dep", "pending", "high", "99"),
	)
	code, stdout, stderr := runCmd(cmdNext, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("next exit %d, stderr: %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "null" {
		t.Errorf("json output = %q, want null (dangling dep leaves nothing actionable)", stdout)
	}
}

func TestNextDependencyGating(t *testing.T) {
	// Task 2 is high priority but gated by a pending dependency;
	// the lower-priority task 3 is the only actionable one.
	path := backlogFile(t, "main",
		taskLit("1", "dep still open", "in-progress", "high"),
		taskLit("2", "gated high", "pending", "high", "1"),
		taskLit("3", "free medium", "pending", "medium"),
	)
	if id := nextID(t, path); id != "3" {
		t.Errorf("next = task %s, want 3 (task 2 gated by pending dep)", id)
	}
}

func TestNextDoneAndCancelledDepsSatisfy(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "finished", "done", "high"),
		taskLit("2", "abandoned", "cancelled", "high"),
		taskLit("3", "ready", "pending", "high", "1", "2"),
	)
	if id := nextID(t, path); id != "3" {
		t.Errorf("next = task %s, want 3 (done and cancelled deps both satisfy)", id)
	}
}

func TestNextOnlyPendingEligible(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "already running", "in-progress", "high"),
		taskLit("2", "under review", "review", "high"),
		taskLit("3", "waiting", "pending", "low"),
	)
	if id := nextID(t, path); id != "3" {
		t.Errorf("next = task %s, want 3 (only pending tasks are eligible)", id)
	}
}

func TestNextTieBreakLowestID(t *testing.T) {
	// Numeric comparison: 9 < 10 even though "10" < "9" lexically.
	path := backlogFile(t, "main",
		taskLit("10", "ten", "pending", "high"),
		taskLit("9", "nine", "pending", "high"),
	)
	if id := nextID(t, path); id != "9" {
		t.Errorf("next = task %s, want 9 (numeric tie-break, not lexical)", id)
	}

	path = backlogFile(t, "main",
		taskLit("10", "ten", "pending", "high"),
		taskLit("2", "two", "pending", "high"),
	)
	if id := nextID(t, path); id != "2" {
		t.Errorf("next = task %s, want 2 (lowest id wins the tie)", id)
	}
}

func TestNextNoneActionable(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "finished", "done", "high"),
		taskLit("2", "gated forever", "pending", "high", "3"),
		taskLit("3", "stuck", "blocked", "high"),
	)

	code, stdout, stderr := runCmd(cmdNext, "--file", path)
	if code != 0 {
		t.Fatalf("next exit %d, stderr: %s", code, stderr)
	}
	if strings.TrimSpace(stdout) != "no actionable task" {
		t.Errorf("human output = %q, want %q", stdout, "no actionable task")
	}

	code, stdout, _ = runCmd(cmdNext, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("next --json exit %d", code)
	}
	if strings.TrimSpace(stdout) != "null" {
		t.Errorf("json output = %q, want null", stdout)
	}
}

func mixedBacklog(t *testing.T) string {
	t.Helper()
	return backlogFile(t, "main",
		taskLit("1", "done high", "done", "high"),
		taskLit("2", "pending high", "pending", "high"),
		taskLit("3", "pending low", "pending", "low"),
		taskLit("4", "blocked medium", "blocked", "medium"),
	)
}

func TestListStatusFilter(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, stderr := runCmd(cmdList, "--file", path, "--status", "pending")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{"pending high", "pending low", "2 tasks: 2 pending"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list output missing %q:\n%s", want, stdout)
		}
	}
	for _, reject := range []string{"done high", "blocked medium"} {
		if strings.Contains(stdout, reject) {
			t.Errorf("list output should not contain %q:\n%s", reject, stdout)
		}
	}
}

func TestListPriorityFilter(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, stderr := runCmd(cmdList, "--file", path, "--priority", "high", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var out listOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("list --json output not JSON: %v\n%s", err, stdout)
	}
	if out.Total != 2 || len(out.Tasks) != 2 {
		t.Fatalf("total = %d, tasks = %d, want 2 and 2", out.Total, len(out.Tasks))
	}
	if out.Tasks[0].ID != "1" || out.Tasks[1].ID != "2" {
		t.Errorf("task ids = %s, %s, want 1, 2", out.Tasks[0].ID, out.Tasks[1].ID)
	}
}

func TestListCombinedFilters(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, _ := runCmd(cmdList, "--file", path, "--status", "pending", "--priority", "high", "--json")
	if code != 0 {
		t.Fatalf("list exit %d", code)
	}
	var out listOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || out.Tasks[0].ID != "2" {
		t.Errorf("want exactly task 2, got total=%d tasks=%v", out.Total, out.Tasks)
	}
}

func TestListRefusesUnknownFilterValues(t *testing.T) {
	path := mixedBacklog(t)
	for _, args := range [][]string{
		{"--file", path, "--status", "finished"},
		{"--file", path, "--priority", "urgent"},
	} {
		code, stdout, stderr := runCmd(cmdList, args...)
		if code == 0 {
			t.Errorf("list %v: expected refusal, got exit 0", args)
		}
		if stdout != "" {
			t.Errorf("list %v: refusal must not write stdout, got %q", args, stdout)
		}
		if !strings.Contains(stderr, "unknown") {
			t.Errorf("list %v: stderr = %q, want an unknown-value message", args, stderr)
		}
	}
}

func TestListJSONShape(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, stderr := runCmd(cmdList, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}

	// Stdout must be exactly one JSON document, nothing else.
	dec := json.NewDecoder(strings.NewReader(stdout))
	var raw map[string]json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("stdout is not a JSON document: %v\n%s", err, stdout)
	}
	if dec.More() {
		t.Fatalf("stdout has trailing content beyond the JSON document:\n%s", stdout)
	}
	for _, key := range []string{"tag", "total", "counts", "tasks"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("list --json missing key %q", key)
		}
	}

	var out listOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Tag != "main" || out.Total != 4 {
		t.Errorf("tag=%q total=%d, want main and 4", out.Tag, out.Total)
	}
	want := map[string]int{"done": 1, "pending": 2, "blocked": 1}
	for s, n := range want {
		if out.Counts[s] != n {
			t.Errorf("counts[%s] = %d, want %d", s, out.Counts[s], n)
		}
	}
	// Dependencies and subtasks must serialize as [], never null.
	if strings.Contains(stdout, `"dependencies": null`) || strings.Contains(stdout, `"subtasks": null`) {
		t.Errorf("empty slices must serialize as [], not null:\n%s", stdout)
	}
}

// TestListFixtureJSONEmitsPlainTaskShape pins the fixture surface: list --file
// --json emits the plain taskOut shape (like next), without the database-only
// revision and claim members.
func TestListFixtureJSONEmitsPlainTaskShape(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "fixture", "pending", ""))
	code, stdout, stderr := runCmd(cmdList, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("list --json output not JSON: %v\n%s", err, stdout)
	}
	if len(out.Tasks) != 1 {
		t.Fatalf("tasks = %d, want 1", len(out.Tasks))
	}
	for _, key := range []string{"revision", "claim"} {
		if _, ok := out.Tasks[0][key]; ok {
			t.Errorf("fixture task carries database-only key %q:\n%s", key, stdout)
		}
	}
}

func TestShowJSONShape(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, stderr := runCmd(cmdShow, "--file", path, "--json", "2")
	if code != 0 {
		t.Fatalf("show exit %d, stderr: %s", code, stderr)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("show --json output not JSON: %v\n%s", err, stdout)
	}
	for _, key := range []string{
		"id", "title", "description", "details", "status",
		"priority", "dependencies", "subtasks", "updatedAt",
	} {
		if _, ok := raw[key]; !ok {
			t.Errorf("show --json missing key %q", key)
		}
	}
	var got taskOut
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "2" || got.Title != "pending high" || got.Status != "pending" || got.Priority != "high" {
		t.Errorf("unexpected task payload: %+v", got)
	}
}

func TestShowHumanOutput(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, stderr := runCmd(cmdShow, "--file", path, "3")
	if code != 0 {
		t.Fatalf("show exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{"id:           3", "pending low", "status:       pending"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("show output missing %q:\n%s", want, stdout)
		}
	}
}

func TestShowNestedSubtask(t *testing.T) {
	parent := `{
    "id": "32",
    "title": "Diagnostics",
    "status": "pending",
    "dependencies": [],
    "subtasks": [{"id": 1, "title": "Health probes", "description": "legacy", "details": "keep", "status": "pending", "dependencies": [], "parentTaskId": 32, "updatedAt": "2026-08-01T00:00:00Z", "_custom": true}],
    "updatedAt": "2026-08-02T00:00:00Z"
  }`
	path := backlogFile(t, "master", parent)

	code, stdout, stderr := runCmd(cmdShow, "--file", path, "--json", "32.1")
	if code != 0 {
		t.Fatalf("show nested exit %d, stderr: %s", code, stderr)
	}
	var out subtaskOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "32.1" || out.ParentTaskID != "32" || out.Title != "Health probes" || out.Status != "pending" {
		t.Errorf("nested output = %+v", out)
	}

	code, stdout, stderr = runCmd(cmdShow, "--file", path, "32.1")
	if code != 0 {
		t.Fatalf("show nested human exit %d, stderr: %s", code, stderr)
	}
	for _, want := range []string{"id:           32.1", "parent:       32", "title:        Health probes"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output missing %q:\n%s", want, stdout)
		}
	}
}

func TestShowRefusesUnknownID(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, stderr := runCmd(cmdShow, "--file", path, "99")
	if code != 1 {
		t.Errorf("show 99 exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("refusal must not write stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "not found") {
		t.Errorf("stderr = %q, want a not-found message", stderr)
	}
}

func TestShowRequiresID(t *testing.T) {
	path := mixedBacklog(t)
	code, _, stderr := runCmd(cmdShow, "--file", path)
	if code != 2 {
		t.Errorf("show with no id exit = %d, want 2", code)
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("stderr = %q, want usage message", stderr)
	}
}

func TestFixtureSymlinkFailsClosed(t *testing.T) {
	tests := []struct {
		name string
		run  func([]string, io.Writer, io.Writer) int
		args func(string) []string
	}{
		{"read", cmdShow, func(path string) []string {
			return []string{"--file", path, "1"}
		}},
		{"mutation", cmdSetStatus, func(path string) []string {
			return []string{"--file", path, "1", "done"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))
			original, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(t.TempDir(), "tasks.json")
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}

			code, stdout, stderr := runCmd(tt.run, tt.args(link)...)
			wantStderr := "noise-tasks: backlog path is not a regular file: " + link + "\n"
			if code != 1 || stdout != "" || stderr != wantStderr {
				t.Fatalf("exit = %d, stdout = %q, stderr = %q; want 1, empty, %q", code, stdout, stderr, wantStderr)
			}
			info, err := os.Lstat(link)
			if err != nil || info.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("fixture after refusal = %v, %v; want symlink preserved", info, err)
			}
			if got, err := os.Readlink(link); err != nil || got != target {
				t.Fatalf("symlink target = %q, %v; want %q", got, err, target)
			}
			after, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, original) {
				t.Fatal("refused fixture command changed the symlink target")
			}
		})
	}
}

func TestRefusesUnknownTag(t *testing.T) {
	path := mixedBacklog(t)
	for name, cmd := range map[string]func([]string, io.Writer, io.Writer) int{
		"list": cmdList,
		"next": cmdNext,
	} {
		code, stdout, stderr := runCmd(cmd, "--file", path, "--tag", "nope")
		if code != 1 {
			t.Errorf("%s --tag nope exit = %d, want 1", name, code)
		}
		if stdout != "" {
			t.Errorf("%s refusal must not write stdout, got %q", name, stdout)
		}
		if !strings.Contains(stderr, `"nope" not found`) {
			t.Errorf("%s stderr = %q, want tag-not-found message", name, stderr)
		}
	}
}

func TestDefaultTagSoleTag(t *testing.T) {
	// A legacy file whose only tag is "master" resolves without --tag.
	path := backlogFile(t, "master",
		taskLit("1", "legacy task", "pending", "high"),
	)
	code, stdout, stderr := runCmd(cmdList, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var out listOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Tag != "master" {
		t.Errorf("tag = %q, want master (sole tag)", out.Tag)
	}
}

func TestDefaultTagMainAmongMany(t *testing.T) {
	content := fmt.Sprintf(`{
  "featureX": { "tasks": [%s] },
  "main": { "tasks": [%s] }
}
`, taskLit("1", "feature task", "pending", "high"), taskLit("1", "main task", "pending", "high"))
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runCmd(cmdList, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var out listOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Tag != "main" || out.Tasks[0].Title != "main task" {
		t.Errorf("defaulted to tag %q with task %q, want main / main task", out.Tag, out.Tasks[0].Title)
	}

	// An explicit --tag still selects the other tag.
	code, stdout, _ = runCmd(cmdList, "--file", path, "--tag", "featureX", "--json")
	if code != 0 {
		t.Fatalf("list --tag featureX exit %d", code)
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Tag != "featureX" {
		t.Errorf("tag = %q, want featureX", out.Tag)
	}
}

func TestAbsentPriorityIsMediumEverywhere(t *testing.T) {
	// SPEC §1: an absent priority is medium — ranking, filtering, and
	// output all agree.
	path := backlogFile(t, "main",
		taskLit("1", "explicit low", "pending", "low"),
		taskLit("2", "no priority", "pending", ""),
	)

	// list --priority medium matches the unset-priority task.
	code, stdout, stderr := runCmd(cmdList, "--file", path, "--priority", "medium", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var out listOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || out.Tasks[0].ID != "2" {
		t.Fatalf("list --priority medium: total=%d, want exactly task 2:\n%s", out.Total, stdout)
	}
	// ...and JSON reports the effective priority, not "".
	if out.Tasks[0].Priority != "medium" {
		t.Errorf("json priority = %q, want medium", out.Tasks[0].Priority)
	}

	// The table renders medium for the unset-priority row, not a dash.
	code, stdout, _ = runCmd(cmdList, "--file", path)
	if code != 0 {
		t.Fatalf("list exit %d", code)
	}
	row := ""
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "no priority") {
			row = line
		}
	}
	if row == "" || !strings.Contains(row, "medium") {
		t.Errorf("table row for the unset-priority task should render medium:\n%s", stdout)
	}

	// show renders medium too.
	code, stdout, _ = runCmd(cmdShow, "--file", path, "2")
	if code != 0 {
		t.Fatalf("show exit %d", code)
	}
	if !strings.Contains(stdout, "priority:     medium") {
		t.Errorf("show should render medium for absent priority:\n%s", stdout)
	}

	// next ranks it as medium, above low (the reviewer's repro).
	if id := nextID(t, path); id != "2" {
		t.Errorf("next = task %s, want 2 (medium beats low)", id)
	}
}

func TestListEmptyResult(t *testing.T) {
	path := mixedBacklog(t)
	code, stdout, stderr := runCmd(cmdList, "--file", path, "--status", "deferred")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "0 tasks") {
		t.Errorf("empty result should report %q:\n%s", "0 tasks", stdout)
	}
}
