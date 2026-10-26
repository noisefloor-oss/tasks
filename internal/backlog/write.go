package backlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	taskElemIndent  = "      "
	metaValueIndent = "    "
)

func (f *File) buildFileBytes() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, tag := range f.tags {
		blk := f.blocks[tag]
		body, err := formatTagBlock(blk)
		if err != nil {
			return nil, fmt.Errorf("tag %q: %w", tag, err)
		}
		fmt.Fprintf(&buf, "  %s: %s", jsonString(tag), body)
		if i < len(f.tags)-1 {
			buf.WriteString(",\n")
		} else {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("}\n")
	return buf.Bytes(), nil
}

func formatTagBlock(blk *tagBlock) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range blk.keys {
		var val []byte
		var err error
		switch key {
		case "tasks":
			val, err = formatTaskArray(blk)
		case "metadata":
			val, err = formatMetadata(blk)
		default:
			val = blk.values[key]
		}
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, "    %s: %s", jsonString(key), val)
		if i < len(blk.keys)-1 {
			buf.WriteString(",\n")
		} else {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("  }")
	return buf.Bytes(), nil
}

func formatTaskArray(blk *tagBlock) ([]byte, error) {
	if len(blk.tasks) == 0 {
		return []byte("[]"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, tr := range blk.tasks {
		buf.WriteString(taskElemIndent)
		buf.Write(tr.raw)
		if i < len(blk.tasks)-1 {
			buf.WriteString(",\n")
		} else {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("    ]")
	return buf.Bytes(), nil
}

func formatMetadata(blk *tagBlock) ([]byte, error) {
	raw := blk.values["metadata"]
	if !blk.metaDirty {
		return raw, nil
	}
	repl := map[string]json.RawMessage{
		"lastModified":   mustMarshalJSON(blk.metadata.LastModified),
		"completedCount": mustMarshalJSON(blk.metadata.CompletedCount),
	}
	spliced, ok, err := spliceObjectValues(raw, repl)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	if ok {
		return spliced, nil
	}
	obj, err := decodeOrderedObject(raw)
	if err != nil {
		return nil, fmt.Errorf("metadata: %w", err)
	}
	obj.setRaw("lastModified", repl["lastModified"])
	obj.setRaw("completedCount", repl["completedCount"])
	return obj.marshalJSON(metaValueIndent)
}

func spliceObjectValues(raw json.RawMessage, repl map[string]json.RawMessage) (json.RawMessage, bool, error) {
	src := bytes.TrimSpace(raw)
	if len(src) == 0 || src[0] != '{' {
		return nil, false, fmt.Errorf("expected JSON object")
	}

	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil {
		return nil, false, err
	}

	type span struct {
		start, end int64
		val        json.RawMessage
	}
	var spans []span
	found := 0
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, false, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, false, fmt.Errorf("expected string key, got %T", t)
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false, err
		}
		if newVal, ok := repl[key]; ok {
			end := dec.InputOffset()
			spans = append(spans, span{start: end - int64(len(val)), end: end, val: newVal})
			found++
		}
	}
	if found != len(repl) {
		return nil, false, nil
	}

	var buf bytes.Buffer
	prev := int64(0)
	for _, s := range spans {
		buf.Write(src[prev:s.start])
		buf.Write(s.val)
		prev = s.end
	}
	buf.Write(src[prev:])
	return buf.Bytes(), true, nil
}

func mustMarshalJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// MarshalTask serializes a Task into the §1 object shape used when the
// projection path creates tasks. The key order matches the §1 schema and
// string values are encoded without HTML escaping.
func MarshalTask(t Task) json.RawMessage {
	obj := &orderedObject{values: make(map[string]json.RawMessage)}
	obj.setRaw("id", mustMarshalJSON(t.ID))
	obj.setRaw("title", MarshalString(t.Title))
	obj.setRaw("description", MarshalString(t.Description))
	obj.setRaw("details", MarshalString(t.Details))
	obj.setRaw("status", mustMarshalJSON(t.Status))
	if t.Priority != "" {
		obj.setRaw("priority", mustMarshalJSON(t.Priority))
	}
	if t.Milestone != "" {
		obj.setRaw("milestone", MarshalString(t.Milestone))
	}
	if t.Link != nil {
		obj.setRaw("externalLink", MarshalExternalLink(*t.Link))
	}
	obj.setRaw("dependencies", mustMarshalJSON(t.Dependencies))
	subtasks := t.Subtasks
	if subtasks == nil {
		subtasks = []json.RawMessage{}
	}
	obj.setRaw("subtasks", mustMarshalJSON(subtasks))
	obj.setRaw("updatedAt", mustMarshalJSON(t.UpdatedAt))
	raw, err := obj.marshalJSON(taskElemIndent)
	if err != nil {
		panic(err)
	}
	return raw
}

// SpliceObjectValues replaces the named fields in a JSON object, preserving
// all other fields and key order verbatim. When a named field is absent the
// object is rebuilt with the missing fields appended in sorted order, so
// callers may add a field to a §1 task that does not yet carry it.
func SpliceObjectValues(raw json.RawMessage, repl map[string]json.RawMessage) (json.RawMessage, error) {
	spliced, ok, err := spliceObjectValues(raw, repl)
	if err != nil {
		return nil, err
	}
	if ok {
		return spliced, nil
	}
	obj, err := decodeOrderedObject(raw)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(repl))
	for key := range repl {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		obj.setRaw(key, repl[key])
	}
	return obj.marshalJSON(taskElemIndent)
}

// UpdateTaskObject applies field updates to a serialized §1 task object,
// preserving untouched fields verbatim.
func UpdateTaskObject(raw json.RawMessage, updates map[string]json.RawMessage) (json.RawMessage, error) {
	return SpliceObjectValues(raw, updates)
}

// MarshalString encodes s as a JSON string without HTML escaping, the
// form observed in real backlogs (`<info added ...>` blocks stay
// readable). Intended for details/title/description values passed to
// WriteMinimal and AddTask.
func MarshalString(s string) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		panic(err) // strings always encode
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func (f *File) Write(path string) error {
	data, err := f.buildFileBytes()
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

func (f *File) WriteMinimal(targetTag string, taskIndex int, updates map[string]json.RawMessage, newStatus string) error {
	blk, ok := f.blocks[targetTag]
	if !ok {
		return fmt.Errorf("tag %q not found", targetTag)
	}
	if taskIndex < 0 || taskIndex >= len(blk.tasks) {
		return fmt.Errorf("task index %d out of range", taskIndex)
	}
	tr := blk.tasks[taskIndex]
	if newStatus != "" && !ValidStatuses[newStatus] {
		return &ErrInvalidStatus{ID: tr.task.ID, Status: newStatus}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	repl := make(map[string]json.RawMessage, len(updates)+2)
	if newStatus != "" {
		repl["status"] = mustMarshalJSON(newStatus)
	}
	for key, val := range updates {
		repl[key] = val
	}
	repl["updatedAt"] = mustMarshalJSON(now)

	newRaw, spliced, err := spliceObjectValues(tr.raw, repl)
	if err != nil {
		return err
	}
	if !spliced {
		rebuilt := tr.obj.clone()
		if newStatus != "" {
			rebuilt.setRaw("status", repl["status"])
		}
		updateKeys := make([]string, 0, len(updates))
		for key := range updates {
			updateKeys = append(updateKeys, key)
		}
		sort.Strings(updateKeys)
		for _, key := range updateKeys {
			rebuilt.setRaw(key, updates[key])
		}
		rebuilt.setRaw("updatedAt", repl["updatedAt"])
		newRaw, err = rebuilt.marshalJSON(taskElemIndent)
		if err != nil {
			return err
		}
	}
	return f.commitTaskEdit(targetTag, blk, tr, newRaw, now)
}

// commitTaskEdit installs newRaw as the task record's serialized form,
// revalidates the tag, and writes the file with per-write metadata
// maintenance. It is the shared commit path for minimal task edits.
func (f *File) commitTaskEdit(targetTag string, blk *tagBlock, tr *taskRec, newRaw json.RawMessage, now string) error {
	newObj, err := decodeOrderedObject(newRaw)
	if err != nil {
		return err
	}
	updated, err := decodeTask(newRaw)
	if err != nil {
		return err
	}
	if err := validateTask(targetTag, updated); err != nil {
		return err
	}

	oldTask, oldRaw, oldObj := tr.task, tr.raw, tr.obj
	tr.task, tr.raw, tr.obj = *updated, newRaw, newObj
	if err := validateTasks(targetTag, blk.tasks); err != nil {
		tr.task, tr.raw, tr.obj = oldTask, oldRaw, oldObj
		return err
	}

	return f.writeTouched(blk, now)
}

// writeTouched maintains the per-write metadata invariants (SPEC §1:
// lastModified and completedCount on every write; taskCount is set on
// file creation and never trusted afterward) and writes the file.
func (f *File) writeTouched(blk *tagBlock, now string) error {
	if blk.metadata != nil {
		blk.metadata.LastModified = now
		completed := 0
		for _, t := range blk.tasks {
			if t.task.Status == "done" {
				completed++
			}
		}
		blk.metadata.CompletedCount = completed
		blk.metaDirty = true
	}

	return f.Write(f.path)
}

// initialVersion is the metadata version stamped on files this codec
// creates; it matches the shape observed in real backlogs.
const initialVersion = "1.0.0"

// InitFileWithParents creates the target directory and then delegates to
// InitFile. Existing final directory symlinks are refused so setup cannot be
// redirected through a pre-planted canonical tasks directory.
func InitFileWithParents(path, tag string) (*File, error) {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		info, err = os.Lstat(dir)
	case err != nil:
		return nil, err
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("backlog directory %s is not a regular directory", dir)
	}
	return InitFile(path, tag)
}

// InitFile creates a new backlog file at path containing a single empty
// tag, atomically. It refuses to overwrite an existing file.
func InitFile(path, tag string) (*File, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	now := time.Now().UTC().Format(time.RFC3339)
	meta := &orderedObject{values: make(map[string]json.RawMessage)}
	meta.setRaw("version", mustMarshalJSON(initialVersion))
	meta.setRaw("lastModified", mustMarshalJSON(now))
	meta.setRaw("taskCount", []byte("0"))
	meta.setRaw("completedCount", []byte("0"))
	meta.setRaw("tags", mustMarshalJSON([]string{tag}))
	metaRaw, err := meta.marshalJSON(metaValueIndent)
	if err != nil {
		return nil, err
	}

	blk := &tagBlock{
		keys: []string{"tasks", "metadata"},
		values: map[string]json.RawMessage{
			"tasks":    []byte("[]"),
			"metadata": metaRaw,
		},
		metadata: &Metadata{
			Version:      initialVersion,
			LastModified: now,
			Tags:         []string{tag},
		},
	}
	f := &File{
		path:   path,
		tags:   []string{tag},
		blocks: map[string]*tagBlock{tag: blk},
	}
	data, err := f.buildFileBytes()
	if err != nil {
		return nil, err
	}
	if _, err := Parse(data); err != nil {
		return nil, fmt.Errorf("internal error: generated file does not parse: %w", err)
	}
	if err := atomicWrite(path, data); err != nil {
		return nil, err
	}
	return f, nil
}

// AddTask appends one task through the atomic batch path.
func (f *File) AddTask(tag string, t Task) (string, error) {
	ids, err := f.AddTasks(tag, []Task{t})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// NextTaskIDs returns the next contiguous IDs for the current in-memory
// snapshot. It does not reserve them; callers that use the result to build a
// batch must keep the enclosing mutation lock held until AddTasks publishes.
func (f *File) NextTaskIDs(tag string, count int) ([]string, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	if count < 0 {
		return nil, fmt.Errorf("task id count must not be negative")
	}
	if count == 0 {
		return []string{}, nil
	}

	maxID := 0
	for _, tr := range blk.tasks {
		if n, err := strconv.Atoi(tr.task.ID); err == nil && n > maxID {
			maxID = n
		}
	}
	ids := make([]string, count)
	for i := range ids {
		ids[i] = strconv.Itoa(maxID + i + 1)
	}
	return ids, nil
}

// AddTasks assigns one contiguous id range and publishes the whole batch with
// one file replacement. Input IDs and UpdatedAt values are ignored.
func (f *File) AddTasks(tag string, tasks []Task) ([]string, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	ids, err := f.NextTaskIDs(tag, len(tasks))
	if err != nil {
		return nil, err
	}
	if len(tasks) == 0 {
		return ids, nil
	}

	now := time.Now().UTC().Format(time.RFC3339)
	records := make([]*taskRec, len(tasks))
	for i, input := range tasks {
		t := input
		t.ID = ids[i]
		t.UpdatedAt = now
		if t.Status == "" {
			t.Status = "pending"
		}
		if t.Dependencies == nil {
			t.Dependencies = []string{}
		}
		t.Subtasks = nil
		if err := validateTask(tag, &t); err != nil {
			return nil, err
		}

		raw := MarshalTask(t)
		obj, err := decodeOrderedObject(raw)
		if err != nil {
			return nil, err
		}
		records[i] = &taskRec{raw: raw, task: t, obj: obj}
	}

	previousTasks := blk.tasks
	combined := make([]*taskRec, 0, len(previousTasks)+len(records))
	combined = append(combined, previousTasks...)
	combined = append(combined, records...)
	if err := validateTasks(tag, combined); err != nil {
		return nil, err
	}
	blk.tasks = combined

	previousDirty := blk.metaDirty
	var previousMetadata Metadata
	if blk.metadata != nil {
		previousMetadata = *blk.metadata
	}
	if err := f.writeTouched(blk, now); err != nil {
		blk.tasks = previousTasks
		blk.metaDirty = previousDirty
		if blk.metadata != nil {
			*blk.metadata = previousMetadata
		}
		return nil, err
	}
	return ids, nil
}

// RemoveTask deletes the task with the given id from tag and writes the
// file. If other tasks in the tag depend on id it refuses unless
// stripRefs is set, in which case the dangling references are removed
// from those tasks (bumping their updatedAt) and their ids are returned.
func (f *File) RemoveTask(tag, id string, stripRefs bool) ([]string, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	idx := -1
	for i, tr := range blk.tasks {
		if tr.task.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("task %s not found in tag %q", id, tag)
	}

	var dependents []*taskRec
	for i, tr := range blk.tasks {
		if i == idx {
			continue
		}
		for _, d := range tr.task.Dependencies {
			if d == id {
				dependents = append(dependents, tr)
				break
			}
		}
	}
	if len(dependents) > 0 && !stripRefs {
		ids := make([]string, len(dependents))
		for i, tr := range dependents {
			ids[i] = tr.task.ID
		}
		return nil, fmt.Errorf("task %s is a dependency of task(s) %s", id, strings.Join(ids, ", "))
	}

	now := time.Now().UTC().Format(time.RFC3339)
	stripped := make([]string, 0, len(dependents))
	for _, tr := range dependents {
		if err := stripDependency(tag, tr, id, now); err != nil {
			return nil, err
		}
		stripped = append(stripped, tr.task.ID)
	}

	blk.tasks = append(blk.tasks[:idx], blk.tasks[idx+1:]...)
	if err := f.writeTouched(blk, now); err != nil {
		return nil, err
	}
	return stripped, nil
}

// stripDependency rewrites tr's dependencies without id, preserving the
// remaining entries' original bytes (string or number form), and bumps
// the task's updatedAt.
func stripDependency(tag string, tr *taskRec, id, now string) error {
	depsRaw, ok := tr.obj.values["dependencies"]
	if !ok {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(depsRaw, &elems); err != nil {
		return fmt.Errorf("task %s: parsing dependencies: %w", tr.task.ID, err)
	}
	keep := make([]string, 0, len(elems))
	for _, e := range elems {
		if idString(e) != id {
			keep = append(keep, string(bytes.TrimSpace(e)))
		}
	}
	newDeps := "[]"
	if len(keep) > 0 {
		newDeps = "[" + strings.Join(keep, ", ") + "]"
	}

	repl := map[string]json.RawMessage{
		"dependencies": json.RawMessage(newDeps),
		"updatedAt":    mustMarshalJSON(now),
	}
	newRaw, spliced, err := spliceObjectValues(tr.raw, repl)
	if err != nil {
		return err
	}
	if !spliced {
		rebuilt := tr.obj.clone()
		rebuilt.setRaw("dependencies", repl["dependencies"])
		rebuilt.setRaw("updatedAt", repl["updatedAt"])
		newRaw, err = rebuilt.marshalJSON(taskElemIndent)
		if err != nil {
			return err
		}
	}

	newObj, err := decodeOrderedObject(newRaw)
	if err != nil {
		return err
	}
	updated, err := decodeTask(newRaw)
	if err != nil {
		return err
	}
	if err := validateTask(tag, updated); err != nil {
		return err
	}
	tr.task, tr.raw, tr.obj = *updated, newRaw, newObj
	return nil
}

func (f *File) SetPath(path string) {
	f.path = path
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)

	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	f, err := os.CreateTemp(dir, ".noise-tasks-*.tmp")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := f.Name()

	defer func() {
		f.Close()
		os.Remove(tmpName)
	}()

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}

	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("setting temp file mode: %w", err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("fsyncing temp file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file: %w", err)
	}

	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening backlog directory for fsync: %w", err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("fsyncing backlog directory: %w", err)
	}
	if err := d.Close(); err != nil {
		return fmt.Errorf("closing backlog directory: %w", err)
	}

	return nil
}
