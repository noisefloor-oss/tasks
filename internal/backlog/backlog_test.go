package backlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoundTripBasic(t *testing.T) {
	path := filepath.Join("testdata", "roundtrip", "basic.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	f, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	written, err := f.buildFileBytes()
	if err != nil {
		t.Fatalf("buildFileBytes: %v", err)
	}

	if !bytes.Equal(original, written) {
		t.Errorf("round-trip not byte-identical for basic.json")
		showDiff(t, original, written)
	}

	parsed, err := Parse(written)
	if err != nil {
		t.Fatalf("Parse of written bytes: %v", err)
	}
	tasks, err := parsed.Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 100 {
		t.Errorf("expected 100 tasks, got %d", len(tasks))
	}
}

func TestBuildFileRoundTripEquivalence(t *testing.T) {
	for _, name := range []string{"basic.json", "legacy_master.json", "two_tags.json", "unknown_fields.json", "legacy_meta.json"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join("testdata", "roundtrip", name)
			testRoundTrip(t, path)
		})
	}
}

func testRoundTrip(t *testing.T, path string) {
	t.Helper()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	f, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	written, err := f.buildFileBytes()
	if err != nil {
		t.Fatalf("buildFileBytes: %v", err)
	}

	if !bytes.Equal(original, written) {
		t.Errorf("round-trip not byte-identical for %s", filepath.Base(path))
		showDiff(t, original, written)
	}

	_, err = Parse(written)
	if err != nil {
		t.Fatalf("Parse of written bytes: %v", err)
	}
}

func TestRoundTripPreservesForeignFormatting(t *testing.T) {
	src := `{
  "main": {
    "tasks": [
      {
        "id": 1,
        "title": "Adopted task",
        "status": "pending",
        "dependencies": [2],
        "updatedAt": "2025-01-01T00:00:00Z"
      },
      {
        "id": 2,
        "title": "Other task with \u003cpre-escaped\u003e text",
        "status": "done",
        "dependencies": [],
        "updatedAt": "2025-01-01T00:00:00Z"
      }
    ],
    "metadata": {"version": "1.0.0", "lastModified": "2025-01-01T00:00:00Z", "taskCount": 2, "completedCount": 1, "tags": ["main"]}
  }
}
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tasks, err := f.Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].ID != "1" || tasks[1].ID != "2" {
		t.Errorf("int ids not normalized to strings: %q, %q", tasks[0].ID, tasks[1].ID)
	}
	if len(tasks[0].Dependencies) != 1 || tasks[0].Dependencies[0] != "2" {
		t.Errorf("int dependency not normalized: %v", tasks[0].Dependencies)
	}

	written, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != src {
		t.Error("foreign formatting (inline arrays, compact metadata, escapes) not preserved")
		showDiff(t, []byte(src), written)
	}
}

func TestRoundTripEmptyTasks(t *testing.T) {
	src := `{
  "main": {
    "tasks": [],
    "metadata": {
      "version": "1.0.0",
      "lastModified": "2025-01-01T00:00:00Z",
      "taskCount": 0,
      "completedCount": 0,
      "tags": ["main"]
    }
  }
}
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	written, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != src {
		t.Error("empty tasks array not preserved")
		showDiff(t, []byte(src), written)
	}
}

func TestMinimalWriteProperty(t *testing.T) {
	fixturePath := filepath.Join("testdata", "roundtrip", "basic.json")
	original, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.blocks["main"].tasks[0].task.Status; got != "pending" {
		t.Fatalf("fixture task 1 status = %q, want pending", got)
	}

	if err := f.WriteMinimal("main", 0, nil, "done"); err != nil {
		t.Fatalf("WriteMinimal: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(original, written) {
		t.Error("expected bytes to change after status mutation")
	}

	origLines := strings.Split(string(original), "\n")
	modLines := strings.Split(string(written), "\n")
	if len(origLines) != len(modLines) {
		t.Fatalf("line count changed: %d -> %d", len(origLines), len(modLines))
	}

	allowedPrefixes := []string{
		`        "status"`,
		`        "updatedAt"`,
		`      "completedCount"`,
		`      "lastModified"`,
	}
	diffCount := 0
	for i, line := range modLines {
		if line != origLines[i] {
			if !hasAllowedPrefix(line, allowedPrefixes) {
				t.Errorf("line %d changed unexpectedly: %q", i+1, line)
			}
			diffCount++
		}
	}
	if diffCount > 4 {
		t.Errorf("expected at most 4 changed lines, got %d", diffCount)
	}
	t.Logf("diffCount=%d (expected <=4)", diffCount)

	reparsed, err := ReadFile(path)
	if err != nil {
		t.Fatalf("reparse after minimal write: %v", err)
	}
	task, err := reparsed.Task("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "done" {
		t.Errorf("status after write = %q, want done", task.Status)
	}
	meta, err := reparsed.Metadata("main")
	if err != nil {
		t.Fatal(err)
	}
	if meta.CompletedCount != 13 {
		t.Errorf("completedCount after write = %d, want 13", meta.CompletedCount)
	}
}

func TestWriteMinimalUpdatesField(t *testing.T) {
	fixturePath := filepath.Join("testdata", "roundtrip", "basic.json")
	original, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	secondRawBefore := string(f.blocks["main"].tasks[1].raw)

	updates := map[string]json.RawMessage{
		"details": json.RawMessage(`"Reworked scope:\n<info added 2025-05-11T09:00:00Z>\nTrimmed to the v1 surface & deferred the rest.\n</info>"`),
	}
	if err := f.WriteMinimal("main", 0, updates, ""); err != nil {
		t.Fatalf("WriteMinimal: %v", err)
	}

	reparsed, err := ReadFile(path)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	task, err := reparsed.Task("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(task.Details, "<info added 2025-05-11T09:00:00Z>") {
		t.Errorf("details not updated verbatim: %q", task.Details)
	}
	if got := string(reparsed.blocks["main"].tasks[1].raw); got != secondRawBefore {
		t.Error("untouched task bytes changed")
	}
}

func TestWriteMinimalRejectsBadStatus(t *testing.T) {
	path := filepath.Join("testdata", "roundtrip", "basic.json")
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f.SetPath(filepath.Join(t.TempDir(), "tasks.json"))

	err = f.WriteMinimal("main", 0, nil, "not-a-status")
	var statusErr *ErrInvalidStatus
	if !errors.As(err, &statusErr) {
		t.Errorf("expected ErrInvalidStatus, got %v", err)
	}
}

func TestWriteMinimalRejectsDuplicateID(t *testing.T) {
	path := filepath.Join("testdata", "roundtrip", "basic.json")
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmpPath := filepath.Join(t.TempDir(), "tasks.json")
	f.SetPath(tmpPath)

	updates := map[string]json.RawMessage{"id": json.RawMessage(`"2"`)}
	err = f.WriteMinimal("main", 0, updates, "")
	var dupErr *ErrDuplicateID
	if !errors.As(err, &dupErr) {
		t.Errorf("expected ErrDuplicateID, got %v", err)
	}
	if got := f.blocks["main"].tasks[0].task.ID; got != "1" {
		t.Errorf("task id mutated despite refusal: %q", got)
	}
	if _, err := os.Stat(tmpPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("file written despite refusal")
	}
}

func TestAtomicWriteCrashSafety(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")

	fixturePath := filepath.Join("testdata", "roundtrip", "basic.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Write(path); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("file mode not preserved: %v", info.Mode().Perm())
	}

	tmpFiles, _ := filepath.Glob(filepath.Join(dir, ".noise-tasks-*.tmp"))
	if len(tmpFiles) != 0 {
		t.Errorf("leftover temp files found: %v", tmpFiles)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected 1 file in dir, got %d", len(entries))
	}
}

func TestAtomicWriteIgnoresLeftoverTemp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")

	fixturePath := filepath.Join("testdata", "roundtrip", "basic.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	tmpPath := filepath.Join(dir, ".noise-tasks-leftover.tmp")
	if err := os.WriteFile(tmpPath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}

	f1, err := ReadFile(path)
	if err != nil {
		t.Fatalf("read with leftover tmp: %v", err)
	}
	if f1 == nil {
		t.Fatal("expected non-nil file")
	}

	if _, err := os.Stat(tmpPath); err != nil {
		t.Error("temp file was removed (should be ignored, not deleted)")
	}

	_, err = Parse([]byte("garbage"))
	if err == nil {
		t.Error("expected error parsing garbage")
	}
}

func TestAtomicWriteEndToEnd(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")

	fixturePath := filepath.Join("testdata", "roundtrip", "basic.json")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := atomicWrite(path, data); err != nil {
		t.Fatal(err)
	}

	result, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, result) {
		t.Error("atomic write changed file content")
	}

	tmpFiles, _ := filepath.Glob(filepath.Join(dir, ".noise-tasks-*.tmp"))
	if len(tmpFiles) > 0 {
		t.Errorf("leftover temp files: %v", tmpFiles)
	}
}

func TestValidationDuplicateIDs(t *testing.T) {
	path := filepath.Join("testdata", "validation", "dup_ids.json")
	_, err := ReadFile(path)
	if err == nil {
		t.Fatal("expected error for duplicate IDs")
	}
	var dupErr *ErrDuplicateID
	if !errors.As(err, &dupErr) {
		t.Errorf("expected ErrDuplicateID, got %T: %v", err, err)
	}
}

func TestValidationBadStatus(t *testing.T) {
	path := filepath.Join("testdata", "validation", "bad_status.json")
	_, err := ReadFile(path)
	if err == nil {
		t.Fatal("expected error for bad status")
	}
	var statusErr *ErrInvalidStatus
	if !errors.As(err, &statusErr) {
		t.Errorf("expected ErrInvalidStatus, got %T: %v", err, err)
	}
}

func TestValidationBadID(t *testing.T) {
	path := filepath.Join("testdata", "validation", "bad_id.json")
	_, err := ReadFile(path)
	if err == nil {
		t.Fatal("expected error for bad ID")
	}
	var idErr *ErrInvalidID
	if !errors.As(err, &idErr) {
		t.Errorf("expected ErrInvalidID, got %T: %v", err, err)
	}
}

func TestValidationBadDepID(t *testing.T) {
	path := filepath.Join("testdata", "validation", "bad_dep_id.json")
	_, err := ReadFile(path)
	if err == nil {
		t.Fatal("expected error for bad dependency ID")
	}
	var idErr *ErrInvalidID
	if !errors.As(err, &idErr) {
		t.Errorf("expected ErrInvalidID, got %T: %v", err, err)
	}
}

func TestValidationNonPositiveIDs(t *testing.T) {
	for _, id := range []string{"0", "01", "-1", ""} {
		src := `{"main":{"tasks":[{"id":` + string(jsonString(id)) + `,"title":"t","status":"pending"}]}}`
		_, err := Parse([]byte(src))
		var idErr *ErrInvalidID
		if !errors.As(err, &idErr) {
			t.Errorf("id %q: expected ErrInvalidID, got %v", id, err)
		}
	}
}

func TestParseRefusesTrailingGarbage(t *testing.T) {
	src := `{"main":{"tasks":[]}} {"again":true}`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected error for trailing data")
	}
}

func TestParseRefusesDuplicateKey(t *testing.T) {
	src := `{"main":{"tasks":[],"tasks":[]}}`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected error for duplicate object key")
	}
}

func TestParseRealBacklog(t *testing.T) {
	path := filepath.Join("..", "..", ".tasks/tasks", "tasks.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("repo backlog not found at %s", path)
	}

	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f, err := Parse(original)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !f.HasTag("main") {
		t.Error("expected 'main' tag")
	}
	tasks, err := f.Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) < 1 {
		t.Error("expected at least 1 task")
	}

	written, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, written) {
		t.Error("repo backlog round-trip not byte-identical")
		showDiff(t, original, written)
	}
}

func TestParseSubtaskWithParentTaskId(t *testing.T) {
	path := filepath.Join("testdata", "roundtrip", "unknown_fields.json")
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := f.Tasks("main")
	if err != nil {
		t.Fatal(err)
	}

	found := false
	for _, task := range tasks {
		for _, subRaw := range task.Subtasks {
			var obj map[string]any
			if err := json.Unmarshal(subRaw, &obj); err != nil {
				t.Fatalf("subtask not an object: %v", err)
			}
			if _, ok := obj["parentTaskId"]; ok {
				found = true
			}
			if _, ok := obj["_custom_field"]; ok {
				t.Logf("subtask carries unknown field: %v", obj["_custom_field"])
			}
		}
	}
	if !found {
		t.Error("expected a subtask with parentTaskId in fixture")
	}
}

func TestLegacyMasterIntIDs(t *testing.T) {
	path := filepath.Join("testdata", "roundtrip", "legacy_master.json")
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !f.HasTag("master") {
		t.Fatal("expected 'master' tag")
	}
	tasks, err := f.Tasks("master")
	if err != nil {
		t.Fatal(err)
	}
	if tasks[0].ID != "1" {
		t.Errorf("int id not normalized to string: %q", tasks[0].ID)
	}
	for _, task := range tasks {
		for _, dep := range task.Dependencies {
			if !isPositiveIntString(dep) {
				t.Errorf("task %s: bad normalized dependency %q", task.ID, dep)
			}
		}
	}
}

func hasAllowedPrefix(line string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

func showDiff(t *testing.T, original, modified []byte) {
	t.Helper()
	origLines := strings.Split(string(original), "\n")
	modLines := strings.Split(string(modified), "\n")
	maxLen := len(origLines)
	if len(modLines) > maxLen {
		maxLen = len(modLines)
	}
	diffCount := 0
	for i := 0; i < maxLen; i++ {
		var oLine, mLine string
		if i < len(origLines) {
			oLine = origLines[i]
		}
		if i < len(modLines) {
			mLine = modLines[i]
		}
		if oLine != mLine {
			if diffCount < 10 {
				t.Logf("line %d:\n  - %q\n  + %q", i+1, oLine, mLine)
			}
			diffCount++
		}
	}
	if diffCount > 10 {
		t.Logf("... and %d more differences", diffCount-10)
	}
}

func TestMinimalWriteForeignFormatting(t *testing.T) {
	src := `{
  "main": {
    "tasks": [
      {"id": 1, "title": "Inline task", "status": "pending", "dependencies": [2], "updatedAt": "2025-01-01T00:00:00Z"},
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
	dir := t.TempDir()
	path := filepath.Join(dir, "tasks.json")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WriteMinimal("main", 0, nil, "in-progress"); err != nil {
		t.Fatalf("WriteMinimal: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	origLines := strings.Split(src, "\n")
	modLines := strings.Split(string(written), "\n")
	if len(origLines) != len(modLines) {
		t.Fatalf("line count changed: %d -> %d", len(origLines), len(modLines))
	}
	var changed []int
	for i := range modLines {
		if modLines[i] != origLines[i] {
			changed = append(changed, i+1)
		}
	}
	if len(changed) != 2 || changed[0] != 4 || changed[1] != 13 {
		t.Errorf("expected exactly lines 4 (target task) and 13 (metadata) to change, got %v", changed)
	}

	taskLine := modLines[3]
	if !strings.Contains(taskLine, `"status": "in-progress"`) || !strings.Contains(taskLine, `"dependencies": [2]`) {
		t.Errorf("target task not spliced in place: %q", taskLine)
	}
	metaLine := modLines[12]
	if !strings.Contains(metaLine, `"tags": ["main"]`) || !strings.Contains(metaLine, `"completedCount": 1`) {
		t.Errorf("untouched metadata fields reformatted: %q", metaLine)
	}
	if !strings.Contains(string(written), "            \"title\": \"Deep task with <angles> & ampersands\",") {
		t.Error("untouched task's foreign indentation or escaping changed")
	}

	reparsed, err := ReadFile(path)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	task, err := reparsed.Task("main", "1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "in-progress" {
		t.Errorf("status = %q, want in-progress", task.Status)
	}
}

func TestParseRefusesDuplicateKeyInTask(t *testing.T) {
	src := `{"main":{"tasks":[{"id":"1","title":"a","title":"b","status":"pending"}]}}`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected error for duplicate key inside task object")
	}
}

func TestParseRefusesDuplicateKeyInMetadata(t *testing.T) {
	src := `{"main":{"tasks":[],"metadata":{"version":"1","version":"2"}}}`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected error for duplicate key inside metadata")
	}
}

func TestParseRefusesNonObjectMetadata(t *testing.T) {
	src := `{"main":{"tasks":[],"metadata":null}}`
	if _, err := Parse([]byte(src)); err == nil {
		t.Error("expected error for non-object metadata")
	}
}

func TestParseToleratesDuplicateKeyInSubtask(t *testing.T) {
	src := `{"main":{"tasks":[{"id":"1","title":"a","status":"pending","subtasks":[{"id":1,"note":"x","note":"y"}]}]}}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("subtask contents are preserved verbatim, not deep-validated: %v", err)
	}
	written, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"note":"x","note":"y"`) {
		t.Error("subtask bytes not preserved verbatim")
	}
}

func TestInitFileCreatesEmptyBacklog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if _, err := InitFile(path, "main"); err != nil {
		t.Fatalf("InitFile: %v", err)
	}

	f, err := ReadFile(path)
	if err != nil {
		t.Fatalf("created file does not parse: %v", err)
	}
	tasks, err := f.Tasks("main")
	if err != nil || len(tasks) != 0 {
		t.Errorf("tasks = %v (err %v), want empty", tasks, err)
	}
	meta, err := f.Metadata("main")
	if err != nil || meta == nil {
		t.Fatalf("metadata: %v", err)
	}
	if meta.TaskCount != 0 || meta.CompletedCount != 0 || meta.LastModified == "" {
		t.Errorf("metadata = %+v", meta)
	}

	// Deterministic shape: trailing newline, and a read+write round-trip
	// is byte-identical.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("}\n")) {
		t.Error("file must end with a trailing newline")
	}
	rebuilt, err := f.buildFileBytes()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, rebuilt) {
		t.Error("init output is not round-trip stable")
	}
}

func TestInitFileRefusesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InitFile(path, "main"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want already-exists refusal", err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "{}" {
		t.Error("existing file was overwritten")
	}
}

func TestAddTaskAssignsMonotonicID(t *testing.T) {
	src := `{"main":{"tasks":[{"id":"2","title":"a","status":"pending"},{"id":"7","title":"b","status":"done"}]}}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	f.SetPath(filepath.Join(t.TempDir(), "tasks.json"))

	id, err := f.AddTask("main", Task{Title: "c"})
	if err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	if id != "8" {
		t.Errorf("id = %q, want 8 (max+1, not gap-filling)", id)
	}
	task, err := f.Task("main", "8")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" || task.UpdatedAt == "" {
		t.Errorf("task = %+v, want pending status and an updatedAt", task)
	}
}

func TestNextTaskIDsPreviewsContiguousRangeWithoutMutation(t *testing.T) {
	f, err := Parse([]byte(`{"main":{"tasks":[{"id":"2","title":"a","status":"pending"},{"id":"7","title":"b","status":"done"}]}}`))
	if err != nil {
		t.Fatal(err)
	}

	ids, err := f.NextTaskIDs("main", 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids, ","); got != "8,9,10" {
		t.Fatalf("ids = %s, want 8,9,10", got)
	}
	tasks, err := f.Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 {
		t.Fatalf("preview mutated the backlog: %d tasks", len(tasks))
	}
	if _, err := f.NextTaskIDs("main", -1); err == nil {
		t.Error("negative preview count accepted")
	}
}

func TestAddTaskRefusesInvalidInput(t *testing.T) {
	f, err := Parse([]byte(`{"main":{"tasks":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	f.SetPath(path)

	if _, err := f.AddTask("main", Task{Title: "t", Priority: "urgent"}); err == nil {
		t.Error("expected refusal for invalid priority")
	}
	if _, err := f.AddTask("main", Task{Title: "t", Status: "finished"}); err == nil {
		t.Error("expected refusal for invalid status")
	}
	if _, err := f.AddTask("nope", Task{Title: "t"}); err == nil {
		t.Error("expected refusal for unknown tag")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("file written despite refusals")
	}
}

func TestRemoveTaskRefusesWithoutStripRefs(t *testing.T) {
	src := `{"main":{"tasks":[{"id":"1","title":"a","status":"pending"},{"id":"2","title":"b","status":"pending","dependencies":["1"]}]}}`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	f.SetPath(path)

	if _, err := f.RemoveTask("main", "1", false); err == nil || !strings.Contains(err.Error(), "dependency of") {
		t.Errorf("err = %v, want dependent-protected refusal", err)
	}
	if _, err := f.Task("main", "1"); err != nil {
		t.Error("task removed despite refusal")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("file written despite refusal")
	}
}

func TestRemoveTaskStripPreservesNumberDeps(t *testing.T) {
	// Foreign number-typed deps: stripping one entry keeps the others'
	// original bytes (5 stays a number, not restringified).
	src := `{
  "main": {
    "tasks": [
      {"id": 1, "title": "doomed", "status": "pending"},
      {"id": 2, "title": "dependent", "status": "pending", "dependencies": [1, 5], "updatedAt": "2025-01-01T00:00:00Z"},
      {"id": 5, "title": "other", "status": "pending"}
    ]
  }
}
`
	f, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "tasks.json")
	f.SetPath(path)

	stripped, err := f.RemoveTask("main", "1", true)
	if err != nil {
		t.Fatalf("RemoveTask: %v", err)
	}
	if len(stripped) != 1 || stripped[0] != "2" {
		t.Errorf("stripped = %v, want [2]", stripped)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), `"dependencies": [5]`) {
		t.Errorf("remaining dep should keep its number form:\n%s", written)
	}
	reparsed, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reparsed.Task("main", "1"); err == nil {
		t.Error("task 1 still present")
	}
	task, err := reparsed.Task("main", "2")
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Dependencies) != 1 || task.Dependencies[0] != "5" {
		t.Errorf("dependencies = %v, want [5]", task.Dependencies)
	}
}
