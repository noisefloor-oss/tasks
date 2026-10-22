package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// The §12 command surface: registry CRUD, set-milestone, --milestone
// filters, next's closed-milestone exclusion, and verify findings.

func milestoneListJSON(t *testing.T, path string) []milestoneRollup {
	t.Helper()
	code, stdout, stderr := runCmd(cmdMilestone, "list", "--file", path, "--json")
	if code != 0 {
		t.Fatalf("milestone list exit %d, stderr: %s", code, stderr)
	}
	var out struct {
		Tag        string            `json:"tag"`
		Milestones []milestoneRollup `json:"milestones"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("milestone list json: %v", err)
	}
	return out.Milestones
}

func TestMilestoneAddList(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))

	if code, _, stderr := runCmd(cmdMilestone, "add", "--file", path, "auth refactor"); code != 0 {
		t.Fatalf("add exit %d, stderr: %s", code, stderr)
	}
	if code, _, _ := runCmd(cmdMilestone, "add", "--file", path, "v1.3 release"); code != 0 {
		t.Fatal("second add failed")
	}
	// Duplicate name is a typed conflict (exit 3).
	code, _, stderr := runCmd(cmdMilestone, "add", "--file", path, "auth refactor")
	if code != 3 || !strings.Contains(stderr, "already exists") {
		t.Fatalf("duplicate add = (%d, %q)", code, stderr)
	}
	// Invalid grammar is a usage error (exit 2).
	if code, _, _ := runCmd(cmdMilestone, "add", "--file", path, " padded "); code != 2 {
		t.Fatalf("padded name exit = %d", code)
	}
	if code, _, _ := runCmd(cmdMilestone, "add", "--file", path, "-flaglike"); code != 2 {
		t.Fatalf("hyphen-leading name exit = %d", code)
	}

	entries := milestoneListJSON(t, path)
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Name != "auth refactor" || entries[0].Status != "open" || entries[0].Order != 0 {
		t.Fatalf("first entry = %+v", entries[0])
	}
	if entries[1].Order != 1 {
		t.Fatalf("second entry order = %+v", entries[1])
	}
}

func TestMilestoneCloseReopenRemaining(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "done one", "done", ""),
		taskLit("2", "open one", "pending", ""),
		taskLit("3", "unassigned", "pending", ""))
	for _, name := range []string{"m1"} {
		if code, _, stderr := runCmd(cmdMilestone, "add", "--file", path, name); code != 0 {
			t.Fatalf("add %s: %s", name, stderr)
		}
	}
	for _, id := range []string{"1", "2"} {
		if code, _, stderr := runCmd(cmdSetMilestone, "--file", path, id, "m1"); code != 0 {
			t.Fatalf("set-milestone %s: %s", id, stderr)
		}
	}

	code, stdout, stderr := runCmd(cmdMilestone, "close", "--file", path, "--json", "m1")
	if code != 0 {
		t.Fatalf("close exit %d, stderr: %s", code, stderr)
	}
	var flip struct {
		Status    string `json:"status"`
		Remaining int    `json:"remaining"`
	}
	if err := json.Unmarshal([]byte(stdout), &flip); err != nil {
		t.Fatal(err)
	}
	if flip.Status != "closed" || flip.Remaining != 1 {
		t.Fatalf("close output = %+v", flip)
	}
	// Unknown name is a usage error.
	if code, _, _ := runCmd(cmdMilestone, "close", "--file", path, "nope"); code != 2 {
		t.Fatalf("close unknown exit = %d", code)
	}
	// Reopen is a successful flip back.
	if code, _, stderr := runCmd(cmdMilestone, "reopen", "--file", path, "m1"); code != 0 {
		t.Fatalf("reopen: %s", stderr)
	}
}

func TestSetMilestoneSetClearNoop(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", "high"))
	runCmd(cmdMilestone, "add", "--file", path, "m1")

	if code, _, stderr := runCmd(cmdSetMilestone, "--file", path, "1", "m1"); code != 0 {
		t.Fatalf("set: %s", stderr)
	}
	f := readTasks(t, path)
	task, err := f.Task("main", "1")
	if err != nil || task.Milestone != "m1" {
		t.Fatalf("stored milestone = %q, err %v", task.Milestone, err)
	}

	// Unknown milestone refuses.
	if code, _, _ := runCmd(cmdSetMilestone, "--file", path, "1", "ghost"); code != 2 {
		t.Fatalf("unknown milestone exit = %d", code)
	}
	// Subtask ids refuse.
	if code, _, _ := runCmd(cmdSetMilestone, "--file", path, "1.1", "m1"); code != 2 {
		t.Fatalf("subtask id exit = %d", code)
	}
	// Name and --clear together refuse.
	if code, _, _ := runCmd(cmdSetMilestone, "--file", path, "1", "m1", "--clear"); code != 2 {
		t.Fatalf("both-forms exit = %d", code)
	}

	// No-op assignment writes nothing.
	before, _ := os.ReadFile(path)
	runCmd(cmdSetMilestone, "--file", path, "1", "m1")
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("no-op assignment rewrote the file")
	}
	// Clear removes the field (flags precede positionals).
	if code, _, stderr := runCmd(cmdSetMilestone, "--file", path, "--clear", "1"); code != 0 {
		t.Fatalf("clear: %s", stderr)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"milestone"`) {
		t.Fatal("milestone field survived clear")
	}
	// Clear again is a no-op.
	before, _ = os.ReadFile(path)
	runCmd(cmdSetMilestone, "--file", path, "--clear", "1")
	after, _ = os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("no-op clear rewrote the file")
	}
}

func TestSetMilestoneMinimalWrite(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "first", "pending", ""),
		taskLit("2", "second", "pending", ""))
	runCmd(cmdMilestone, "add", "--file", path, "m1")

	before := readTasks(t, path)
	beforeRaw, err := before.TasksRaw("main")
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(cmdSetMilestone, "--file", path, "2", "m1"); code != 0 {
		t.Fatalf("set: %s", stderr)
	}
	after := readTasks(t, path)
	afterRaw, err := after.TasksRaw("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeRaw) != 2 || len(afterRaw) != 2 {
		t.Fatalf("task counts = %d -> %d", len(beforeRaw), len(afterRaw))
	}
	// §3 minimal write: the untouched task's serialized object is
	// byte-identical; only the target task and metadata change.
	if string(beforeRaw[0]) != string(afterRaw[0]) {
		t.Fatalf("untouched task changed:\n%s\n%s", beforeRaw[0], afterRaw[0])
	}
	if string(beforeRaw[1]) == string(afterRaw[1]) {
		t.Fatal("target task unchanged")
	}
}

func substringAfter(data []byte, marker string) string {
	s := string(data)
	idx := strings.Index(s, marker)
	if idx < 0 {
		return ""
	}
	end := strings.Index(s[idx:], `"id": "2"`)
	if end < 0 {
		return s[idx:]
	}
	return s[idx : idx+end]
}

func TestMilestoneRemoveRefusalAndForce(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", ""))
	runCmd(cmdMilestone, "add", "--file", path, "m1")
	runCmd(cmdSetMilestone, "--file", path, "1", "m1")

	code, _, stderr := runCmd(cmdMilestone, "remove", "--file", path, "m1")
	if code != 3 || !strings.Contains(stderr, "1") {
		t.Fatalf("in-use remove = (%d, %q)", code, stderr)
	}
	if code, _, stderr := runCmd(cmdMilestone, "remove", "--file", path, "--force", "m1"); code != 0 {
		t.Fatalf("force remove: %s", stderr)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "milestone") {
		t.Fatalf("registry or field survived force remove:\n%s", raw)
	}
	if code, _, _ := runCmd(cmdMilestone, "remove", "--file", path, "ghost"); code != 2 {
		t.Fatalf("remove unknown exit = %d", code)
	}
}

func TestListMilestoneFilter(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "in m1", "pending", ""),
		taskLit("2", "outside", "pending", ""))
	runCmd(cmdMilestone, "add", "--file", path, "m1")
	runCmd(cmdSetMilestone, "--file", path, "1", "m1")

	code, stdout, stderr := runCmd(cmdList, "--file", path, "--milestone", "m1", "--json")
	if code != 0 {
		t.Fatalf("list exit %d, stderr: %s", code, stderr)
	}
	var out listOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 1 || len(out.Tasks) != 1 || out.Tasks[0].Milestone != "m1" {
		t.Fatalf("filtered list = %+v", out)
	}
	if code, _, _ := runCmd(cmdList, "--file", path, "--milestone", "ghost"); code != 2 {
		t.Fatalf("unknown filter exit = %d", code)
	}
}

func TestNextMilestoneExclusion(t *testing.T) {
	path := backlogFile(t, "main",
		taskLit("1", "closed milestone task", "pending", "high"),
		taskLit("2", "open milestone task", "pending", "low", "4"),
		taskLit("3", "unassigned task", "pending", "low"),
		taskLit("4", "closed dependency", "done", "low"))
	runCmd(cmdMilestone, "add", "--file", path, "closed-m")
	runCmd(cmdMilestone, "add", "--file", path, "open-m")
	runCmd(cmdSetMilestone, "--file", path, "1", "closed-m")
	runCmd(cmdSetMilestone, "--file", path, "2", "open-m")
	runCmd(cmdSetMilestone, "--file", path, "4", "closed-m")
	runCmd(cmdMilestone, "close", "--file", path, "closed-m")

	// The high-priority task sits in a closed milestone, so next skips it.
	// Task 2 remains actionable because its done dependency (task 4) stays in
	// the status universe even though that dependency's milestone is closed.
	code, stdout, stderr := runCmd(cmdNext, "--file", path, "--json")
	if code != 0 {
		t.Fatalf("next exit %d, stderr: %s", code, stderr)
	}
	var next taskOut
	if err := json.Unmarshal([]byte(stdout), &next); err != nil {
		t.Fatal(err)
	}
	if next.ID != "2" {
		t.Fatalf("next = %+v, want task 2", next)
	}

	// --milestone restricts to that milestone.
	code, stdout, _ = runCmd(cmdNext, "--file", path, "--milestone", "open-m", "--json")
	if code != 0 {
		t.Fatalf("next --milestone exit %d", code)
	}
	if err := json.Unmarshal([]byte(stdout), &next); err != nil {
		t.Fatal(err)
	}
	if next.ID != "2" {
		t.Fatalf("filtered next = %+v", next)
	}

	// A closed milestone yields no candidate.
	code, stdout, _ = runCmd(cmdNext, "--file", path, "--milestone", "closed-m", "--json")
	if code != 0 {
		t.Fatalf("closed next exit %d", code)
	}
	if strings.TrimSpace(stdout) != "null" {
		t.Fatalf("closed next stdout = %q", stdout)
	}
	// Unknown milestone name refuses.
	if code, _, _ := runCmd(cmdNext, "--file", path, "--milestone", "ghost"); code != 2 {
		t.Fatalf("unknown milestone next exit = %d", code)
	}
}
func TestVerifyMilestoneFindings(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", ""))
	runCmd(cmdMilestone, "add", "--file", path, "m1")
	runCmd(cmdSetMilestone, "--file", path, "1", "m1")
	// Commands refuse an unknown milestone, so the unresolvable-reference
	// finding is reached only through a hand-edited file.
	raw, _ := os.ReadFile(path)
	broken := strings.Replace(string(raw), `"milestone": "m1"`, `"milestone": "ghost"`, 1)
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdoutV, _ := runVerifyCmd(t, "--file", path)
	if code != 1 || !strings.Contains(stdoutV, "does not exist in the registry") {
		t.Fatalf("unresolved ref verify = (%d, %q)", code, stdoutV)
	}
	// Break the registry entry's status.
	broken = strings.Replace(string(raw), `"status": "open"`, `"status": "weird"`, 1)
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	code, stdoutV, _ = runVerifyCmd(t, "--file", path)
	if code != 1 || !strings.Contains(stdoutV, "invalid status") {
		t.Fatalf("bad status verify = (%d, %q)", code, stdoutV)
	}
	// A clean registry verifies.
	broken = strings.Replace(string(raw), `"status": "open"`, `"status": "closed"`, 1)
	if err := os.WriteFile(path, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, stdoutV, _ := runVerifyCmd(t, "--file", path); code != 0 {
		t.Fatalf("clean verify = (%d, %q)", code, stdoutV)
	}
}

func TestShowReportsMilestone(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "first", "pending", ""))
	runCmd(cmdMilestone, "add", "--file", path, "m1")
	runCmd(cmdSetMilestone, "--file", path, "1", "m1")

	code, stdout, stderr := runCmd(cmdShow, "--file", path, "1")
	if code != 0 {
		t.Fatalf("show exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "milestone:    m1") {
		t.Fatalf("show output missing milestone:\n%s", stdout)
	}
}

// TestMilestoneRegistryLifecycleOnStore exercises the store-mode registry
// (add/list/set/close/remove) starting from an empty registry — the empty
// registry must read as "no entries", not a malformed document.
func TestMilestoneRegistryLifecycleOnStore(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "seed")

	// An unknown milestone refuses on an empty registry.
	if code, _, _ := runCmd(cmdMilestone, "close", "ghost"); code != 2 {
		t.Fatalf("close ghost on empty registry = %d, want 2", code)
	}
	if code, _, stderr := runCmd(cmdMilestone, "add", "v1"); code != 0 {
		t.Fatalf("milestone add exit %d, stderr: %s", code, stderr)
	}
	if code, _, stderr := runCmd(cmdAdd, "--milestone", "v1", "--title", "assigned"); code != 0 {
		t.Fatalf("add --milestone exit %d, stderr: %s", code, stderr)
	}
	if code, _, _ := runCmd(cmdAdd, "--milestone", "ghost", "--title", "nope"); code != 2 {
		t.Fatalf("add --milestone ghost = %d, want 2", code)
	}

	code, stdout, stderr := runCmd(cmdMilestone, "close", "--json", "v1")
	if code != 0 || !strings.Contains(stdout, `"remaining": 1`) {
		t.Fatalf("close = code %d stdout %q stderr %q", code, stdout, stderr)
	}

	// In use: unforced remove is a §6 conflict; forced clears the field.
	if code, _, _ := runCmd(cmdMilestone, "remove", "v1"); code != 3 {
		t.Fatalf("unforced remove = %d, want 3", code)
	}
	if code, _, stderr := runCmd(cmdMilestone, "remove", "--force", "v1"); code != 0 {
		t.Fatalf("forced remove exit %d, stderr: %s", code, stderr)
	}

	db, err := store.Open(projectStorePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	task, err := db.GetTask("main", "2")
	if err != nil || task == nil {
		t.Fatalf("task 2: %v", err)
	}
	if task.Milestone != "" {
		t.Fatalf("forced remove left milestone %q on task 2", task.Milestone)
	}
	// The registry is empty again: the next unknown name refuses cleanly.
	if code, _, _ := runCmd(cmdMilestone, "close", "v1"); code != 2 {
		t.Fatalf("close after removal = %d, want 2", code)
	}
}
