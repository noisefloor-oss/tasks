package backlog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// §12 codec behavior: registry round-trip fidelity, unknown-field
// preservation on entries, task-field placement, and minimal writes.

const milestoneFixture = `{
  "main": {
    "tasks": [
      {
        "id": "1",
        "title": "first",
        "description": "",
        "details": "",
        "status": "pending",
        "priority": "high",
        "milestone": "m1",
        "dependencies": [],
        "subtasks": [],
        "updatedAt": "2026-08-05T00:00:00Z"
      }
    ],
    "milestones": [
      {
        "name": "m1",
        "status": "open",
        "customField": true
      },
      {
        "name": "m2",
        "status": "closed"
      }
    ],
    "metadata": {
      "version": "1.0.0",
      "lastModified": "2026-08-05T00:00:00Z",
      "taskCount": 1,
      "completedCount": 0,
      "tags": ["main"]
    }
  }
}
`

func milestoneFile(t *testing.T, content string) *File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return f
}

// A plain read+write round-trips the registry and the task field
// byte-identically: this is what a pre-§12 binary guarantees (§12).
func TestMilestoneRoundTripFidelity(t *testing.T) {
	f := milestoneFile(t, milestoneFixture)
	data, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, []byte(milestoneFixture)) {
		t.Fatalf("round-trip not byte-identical:\n%s", data)
	}
}

// Flipping one entry's status splices only that value: entry bytes,
// unknown fields, and every other registry byte stay identical.
func TestMilestoneCloseSplicesStatusOnly(t *testing.T) {
	f := milestoneFile(t, milestoneFixture)
	if err := f.SetMilestoneStatus("main", "m2", "open"); err != nil {
		t.Fatal(err)
	}
	data, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if !strings.Contains(out, `"name": "m2",
        "status": "open"`) {
		t.Fatalf("status not flipped in place:\n%s", out)
	}
	if !strings.Contains(out, `"customField": true`) {
		t.Fatal("unknown entry field lost")
	}
	// The untouched entry and the task field survive verbatim.
	if !strings.Contains(out, `"milestone": "m1"`) {
		t.Fatal("task milestone field lost")
	}
}

func TestMilestonesParseErrors(t *testing.T) {
	f := milestoneFile(t, milestoneFixture)
	entries, err := f.Milestones("main")
	if err != nil || len(entries) != 2 {
		t.Fatalf("entries = %+v, err = %v", entries, err)
	}
	if entries[0].Status != "open" || entries[1].Status != "closed" {
		t.Fatalf("decoded statuses = %+v", entries)
	}

	bad := strings.Replace(milestoneFixture, "\"status\": \"open\",\n        \"customField\": true", "\"status\": \"bogus\",\n        \"customField\": true", 1)
	f2 := milestoneFile(t, bad)
	if _, err := f2.Milestones("main"); err == nil {
		t.Fatal("invalid entry status accepted")
	}
	notArray := strings.Replace(milestoneFixture,
		"\"milestones\": [\n      {\n        \"name\": \"m1\",\n        \"status\": \"open\",\n        \"customField\": true\n      },\n      {\n        \"name\": \"m2\",\n        \"status\": \"closed\"\n      }\n    ]",
		"\"milestones\": {\"name\": \"m1\"}", 1)
	f3 := milestoneFile(t, notArray)
	if _, err := f3.Milestones("main"); err == nil {
		t.Fatal("non-array registry accepted")
	}
}

func TestValidateMilestoneName(t *testing.T) {
	for _, bad := range []string{"", " padded", "padded ", "-leading", "ok\x00nul"} {
		if err := ValidateMilestoneName(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	for _, good := range []string{"m1", "auth refactor", "v1.3.0 — release", strings.Repeat("a", 256)} {
		if err := ValidateMilestoneName(good); err != nil {
			t.Fatalf("rejected %q: %v", good, err)
		}
	}
}

// AssignMilestone keeps MarshalTask's key order: the field lands after
// priority, and after status when priority is absent.
func TestAssignMilestoneKeyOrder(t *testing.T) {
	withPriority, err := AssignMilestone([]byte(`{"id":"1","title":"t","status":"pending","priority":"high","dependencies":[],"updatedAt":"x"}`), "m1")
	if err != nil {
		t.Fatal(err)
	}
	got := string(withPriority)
	if !strings.Contains(got, `"priority":"high","milestone":"m1","dependencies":[]`) {
		t.Fatalf("with-priority placement:\n%s", got)
	}

	withoutPriority, err := AssignMilestone([]byte(`{"id":"1","title":"t","status":"pending","dependencies":[],"updatedAt":"x"}`), "m1")
	if err != nil {
		t.Fatal(err)
	}
	got = string(withoutPriority)
	if !strings.Contains(got, `"status":"pending","milestone":"m1","dependencies":[]`) {
		t.Fatalf("without-priority placement:\n%s", got)
	}

	// Pretty files keep their own style: the insert reuses the file's
	// key/value and member separators instead of compacting.
	pretty := []byte("{\n  \"id\": \"1\",\n  \"status\": \"pending\",\n  \"updatedAt\": \"x\"\n}")
	prettyOut, err := AssignMilestone(pretty, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prettyOut), "\"status\": \"pending\",\n  \"milestone\": \"m1\",\n  \"updatedAt\"") {
		t.Fatalf("pretty placement:\n%s", string(prettyOut))
	}
}

func TestMilestoneSplicesPreserveFraming(t *testing.T) {
	nonCanonical := `{"main":{"tasks":[{"id":"1","title":"t","description":"","details":"","status":"pending","dependencies":[],"subtasks":[],"updatedAt":"x"}],"milestones":[{"name":"m1","status":"open"},{"name":"m2","status":"closed"}],"metadata":{"version":"1.0.0","lastModified":"x","taskCount":1,"completedCount":0,"tags":["main"]}}}`
	f := milestoneFile(t, nonCanonical)
	prefix := `[{"name":"m1","status":"open"},{"name":"m2","status":"closed"}`

	// Append: every existing byte stays; the new entry follows.
	if err := f.AddMilestone("main", "m3"); err != nil {
		t.Fatal(err)
	}
	data := string(f.blocks["main"].values["milestones"])
	if !strings.HasPrefix(data, prefix+",") {
		t.Fatalf("append touched existing bytes:\n%s", data)
	}
	if !strings.Contains(data, `"name": "m3"`) {
		t.Fatalf("appended entry missing:\n%s", data)
	}

	// Flip: only the status value inside m1's span changes.
	if err := f.SetMilestoneStatus("main", "m1", "closed"); err != nil {
		t.Fatal(err)
	}
	data = string(f.blocks["main"].values["milestones"])
	if !strings.HasPrefix(data, `[{"name":"m1","status":"closed"},`) {
		t.Fatalf("flip rewrote more than the status value:\n%s", data)
	}
	if !strings.Contains(data, `{"name":"m2","status":"closed"}`) {
		t.Fatalf("flip touched sibling entry:\n%s", data)
	}

	// Remove: m2's exact bytes vanish; m1 and m3 keep theirs.
	if _, err := f.RemoveMilestone("main", "m2", false); err != nil {
		t.Fatal(err)
	}
	data = string(f.blocks["main"].values["milestones"])
	if strings.Contains(data, `"name":"m2"`) {
		t.Fatalf("remove left the entry:\n%s", data)
	}
	if !strings.HasPrefix(data, `[{"name":"m1","status":"closed"},`) ||
		!strings.Contains(data, `"name": "m3"`) {
		t.Fatalf("remove disturbed untouched entries:\n%s", data)
	}
}

// Removing the last registry entry drops the milestones key entirely.
func TestRemoveLastMilestoneDropsKey(t *testing.T) {
	single := strings.Replace(milestoneFixture, `,
      {
        "name": "m2",
        "status": "closed"
      }`, "", 1)
	f := milestoneFile(t, single)
	if _, err := f.RemoveMilestone("main", "m1", true); err != nil {
		t.Fatal(err)
	}
	data, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "milestone") {
		t.Fatalf("registry key or field survived:\n%s", data)
	}
}
