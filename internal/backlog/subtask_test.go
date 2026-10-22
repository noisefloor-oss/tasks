package backlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nestedFixture = `{
  "master": {
    "tasks": [
      {
        "id": "32",
        "title": "Diagnostics",
        "status": "pending",
        "dependencies": [],
        "subtasks": [
          {"id": 1, "title": "Health probes", "description": "legacy", "details": "keep", "status": "pending", "dependencies": [], "parentTaskId": 32, "updatedAt": "2026-08-01T00:00:00Z", "_custom": {"keep": true}},
          {"id": "2", "title": "Support bundle", "status": "done", "dependencies": [1], "parentTaskId": "32", "spacing": [ 1,  2 ]}
        ],
        "updatedAt": "2026-08-02T00:00:00Z",
        "_parentCustom": "unchanged"
      }
    ],
    "metadata": {"version": "1.0.0", "lastModified": "2026-08-02T00:00:00Z", "taskCount": 1, "completedCount": 0, "tags": ["master"]}
  }
}
`

func nestedFixtureFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(nestedFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveTaskReferenceNormalizesLegacySubtaskID(t *testing.T) {
	f, err := ReadFile(nestedFixtureFile(t))
	if err != nil {
		t.Fatal(err)
	}
	task, subtask, err := f.ResolveTaskReference("master", "32.1")
	if err != nil {
		t.Fatal(err)
	}
	if task != nil {
		t.Fatalf("top-level task = %+v, want nil", task)
	}
	if subtask.ID != "1" || subtask.ParentID != "32" || subtask.Title != "Health probes" || subtask.Status != "pending" {
		t.Fatalf("subtask = %+v", subtask)
	}
}

func TestSetNestedStatusPreservesEveryOtherTaskByte(t *testing.T) {
	path := nestedFixtureFile(t)
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previous, err := f.SetStatusByReference("master", "32.1", "done")
	if err != nil {
		t.Fatal(err)
	}
	if previous != "pending" {
		t.Fatalf("previous = %q, want pending", previous)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(written)
	for _, unchanged := range []string{
		`"updatedAt": "2026-08-01T00:00:00Z", "_custom": {"keep": true}`,
		`{"id": "2", "title": "Support bundle", "status": "done", "dependencies": [1], "parentTaskId": "32", "spacing": [ 1,  2 ]}`,
		`"updatedAt": "2026-08-02T00:00:00Z",`,
		`"_parentCustom": "unchanged"`,
	} {
		if !strings.Contains(got, unchanged) {
			t.Errorf("write changed protected bytes %q", unchanged)
		}
	}
	if !strings.Contains(got, `"id": 1, "title": "Health probes", "description": "legacy", "details": "keep", "status": "done"`) {
		t.Error("target subtask status was not spliced in place")
	}

	reparsed, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, subtask, err := reparsed.ResolveTaskReference("master", "32.1")
	if err != nil {
		t.Fatal(err)
	}
	if subtask.Status != "done" || subtask.UpdatedAt != "2026-08-01T00:00:00Z" {
		t.Errorf("updated subtask = %+v", subtask)
	}
}

func TestNestedReferenceRefusalsDoNotWrite(t *testing.T) {
	path := nestedFixtureFile(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"32.9", "32.0", "32.01", "32.1.2", "missing"} {
		f, err := ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.SetStatusByReference("master", ref, "done"); err == nil {
			t.Errorf("reference %q unexpectedly succeeded", ref)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(original) {
			t.Fatalf("reference %q changed the file", ref)
		}
	}
}

func TestNestedReferenceRejectsDuplicateChildID(t *testing.T) {
	src := strings.Replace(nestedFixture,
		`{"id": "2", "title": "Support bundle", "status": "done", "dependencies": [1], "parentTaskId": "32", "spacing": [ 1,  2 ]}`,
		`{"id": "1", "title": "duplicate", "status": "done", "dependencies": [], "parentTaskId": "32"}`,
		1)
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.SetStatusByReference("master", "32.1", "done"); err == nil || !strings.Contains(err.Error(), "duplicate subtask") {
		t.Fatalf("duplicate error = %v", err)
	}
}

func TestSubtaskDependenciesNormalizeForOutput(t *testing.T) {
	f, err := ReadFile(nestedFixtureFile(t))
	if err != nil {
		t.Fatal(err)
	}
	_, subtask, err := f.ResolveTaskReference("master", "32.2")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal([]string{"1"})
	got, _ := json.Marshal(subtask.Dependencies)
	if string(got) != string(want) {
		t.Errorf("dependencies = %s, want %s", got, want)
	}
}

func TestRemoveNestedLeafPreservesParentAndSiblingBytes(t *testing.T) {
	path := nestedFixtureFile(t)
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.RemoveTaskByReference("master", "32.2", false); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(written)
	if strings.Contains(got, `"id": "2", "title": "Support bundle"`) {
		t.Fatal("target subtask remains after removal")
	}
	for _, unchanged := range []string{
		`{"id": 1, "title": "Health probes", "description": "legacy", "details": "keep", "status": "pending", "dependencies": [], "parentTaskId": 32, "updatedAt": "2026-08-01T00:00:00Z", "_custom": {"keep": true}}`,
		`"updatedAt": "2026-08-02T00:00:00Z"`,
		`"_parentCustom": "unchanged"`,
	} {
		if !strings.Contains(got, unchanged) {
			t.Errorf("removal changed protected bytes %q", unchanged)
		}
	}
	if _, err := ReadFile(path); err != nil {
		t.Fatalf("removed backlog no longer parses: %v", err)
	}
}

func TestRemoveNestedRefusesPossibleDependentAndForce(t *testing.T) {
	path := nestedFixtureFile(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		f, err := ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.RemoveTaskByReference("master", "32.1", force)
		if err == nil {
			t.Fatalf("force=%v unexpectedly removed depended-on subtask", force)
		}
		if force && !strings.Contains(err.Error(), "--force is not supported") {
			t.Fatalf("force error = %v", err)
		}
		if !force && !strings.Contains(err.Error(), "32.2") {
			t.Fatalf("dependent error = %v", err)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || string(after) != string(original) {
			t.Fatalf("refusal changed file: read=%v", readErr)
		}
	}
}

func TestRemoveNestedFirstMiddleLastAndOnlyRemainValid(t *testing.T) {
	for _, target := range []string{"32.1", "32.2", "32.3"} {
		t.Run(target, func(t *testing.T) {
			src := strings.Replace(nestedFixture,
				`{"id": "2", "title": "Support bundle", "status": "done", "dependencies": [1], "parentTaskId": "32", "spacing": [ 1,  2 ]}`,
				`{"id": "2", "title": "middle", "status": "done", "dependencies": [], "parentTaskId": "32"},
          {"id": 3, "title": "last", "status": "done", "dependencies": [], "parentTaskId": 32}`,
				1)
			path := filepath.Join(t.TempDir(), "tasks.json")
			if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
				t.Fatal(err)
			}
			f, err := ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.RemoveTaskByReference("master", target, false); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadFile(path); err != nil {
				t.Fatalf("result does not parse: %v", err)
			}
		})
	}

	src := strings.Replace(nestedFixture,
		`,
          {"id": "2", "title": "Support bundle", "status": "done", "dependencies": [1], "parentTaskId": "32", "spacing": [ 1,  2 ]}`,
		"", 1)
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.RemoveTaskByReference("master", "32.1", false); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path); err != nil {
		t.Fatalf("only-element result does not parse: %v", err)
	}
}
