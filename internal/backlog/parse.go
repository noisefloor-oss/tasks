package backlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

type tagBlock struct {
	keys      []string
	values    map[string]json.RawMessage
	tasks     []*taskRec
	metadata  *Metadata
	metaDirty bool
}

type taskRec struct {
	raw  json.RawMessage
	task Task
	obj  *orderedObject
}

type File struct {
	path   string
	tags   []string
	blocks map[string]*tagBlock
}

func (f *File) Tags() []string         { return append([]string(nil), f.tags...) }
func (f *File) HasTag(tag string) bool { _, ok := f.blocks[tag]; return ok }

func (f *File) Tasks(tag string) ([]Task, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	result := make([]Task, len(blk.tasks))
	for i, tr := range blk.tasks {
		result[i] = tr.task
	}
	return result, nil
}

func (f *File) Task(tag, id string) (*Task, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	for _, tr := range blk.tasks {
		if tr.task.ID == id {
			return &tr.task, nil
		}
	}
	return nil, fmt.Errorf("task %s not found in tag %q", id, tag)
}

// TasksRaw returns the byte-faithful serialized §1 objects for a tag in file
// order.
func (f *File) TasksRaw(tag string) ([]json.RawMessage, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	raws := make([]json.RawMessage, len(blk.tasks))
	for i, tr := range blk.tasks {
		raws[i] = append(json.RawMessage(nil), tr.raw...)
	}
	return raws, nil
}

// ReadTask parses a serialized §1 task object.
func ReadTask(raw json.RawMessage) (*Task, error) {
	return decodeTask(raw)
}

func (f *File) Metadata(tag string) (*Metadata, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	return blk.metadata, nil
}

func (f *File) TaskIndex(tag, id string) (int, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return -1, fmt.Errorf("tag %q not found", tag)
	}
	for i, tr := range blk.tasks {
		if tr.task.ID == id {
			return i, nil
		}
	}
	return -1, fmt.Errorf("task %s not found in tag %q", id, tag)
}

func ReadFile(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, err
	}
	f.path = path
	return f, nil
}

func Parse(data []byte) (*File, error) {
	top, err := decodeOrderedObject(data)
	if err != nil {
		return nil, fmt.Errorf("parsing backlog: %w", err)
	}

	f := &File{
		tags:   top.keys,
		blocks: make(map[string]*tagBlock, len(top.keys)),
	}

	for _, tag := range top.keys {
		raw := top.values[tag]
		blk, err := parseTagBlock(tag, raw)
		if err != nil {
			return nil, fmt.Errorf("tag %q: %w", tag, err)
		}
		f.blocks[tag] = blk
	}

	for _, tag := range f.tags {
		blk := f.blocks[tag]
		if err := validateTasks(tag, blk.tasks); err != nil {
			return nil, err
		}
	}

	return f, nil
}

func parseTagBlock(tag string, raw json.RawMessage) (*tagBlock, error) {
	obj, err := decodeOrderedObject(raw)
	if err != nil {
		return nil, err
	}

	blk := &tagBlock{
		keys:   obj.keys,
		values: obj.values,
	}

	tasksRaw, ok := obj.values["tasks"]
	if !ok {
		return nil, fmt.Errorf("missing \"tasks\" key")
	}

	var taskList []json.RawMessage
	if err := json.Unmarshal(tasksRaw, &taskList); err != nil {
		return nil, fmt.Errorf("parsing tasks array: %w", err)
	}

	blk.tasks = make([]*taskRec, len(taskList))
	for i, tRaw := range taskList {
		taskObj, err := decodeOrderedObject(tRaw)
		if err != nil {
			return nil, fmt.Errorf("task %d: %w", i, err)
		}
		t, err := decodeTask(tRaw)
		if err != nil {
			return nil, fmt.Errorf("task %d: %w", i, err)
		}
		if err := validateTask(tag, t); err != nil {
			return nil, err
		}
		blk.tasks[i] = &taskRec{
			raw:  tRaw,
			task: *t,
			obj:  taskObj,
		}
	}

	metaRaw, ok := obj.values["metadata"]
	if ok {
		if _, err := decodeOrderedObject(metaRaw); err != nil {
			return nil, fmt.Errorf("parsing metadata: %w", err)
		}
		var m Metadata
		if err := json.Unmarshal(metaRaw, &m); err != nil {
			return nil, fmt.Errorf("parsing metadata: %w", err)
		}
		blk.metadata = &m
	}

	return blk, nil
}

type taskJSON struct {
	ID           json.RawMessage   `json:"id"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Details      string            `json:"details"`
	Status       string            `json:"status"`
	Priority     string            `json:"priority"`
	Dependencies []json.RawMessage `json:"dependencies"`
	Milestone    string            `json:"milestone"`
	ExternalLink json.RawMessage   `json:"externalLink"`
	Subtasks     []json.RawMessage `json:"subtasks"`
	UpdatedAt    string            `json:"updatedAt"`
}

func decodeTask(raw json.RawMessage) (*Task, error) {
	var aux taskJSON
	if err := json.Unmarshal(raw, &aux); err != nil {
		return nil, err
	}
	t := &Task{
		ID:          idString(aux.ID),
		Title:       aux.Title,
		Description: aux.Description,
		Details:     aux.Details,
		Status:      aux.Status,
		Priority:    aux.Priority,
		Milestone:   aux.Milestone,
		Subtasks:    aux.Subtasks,
		UpdatedAt:   aux.UpdatedAt,
	}
	// A malformed externalLink member decodes to a nil Link — the raw
	// object keeps the member byte-faithfully and verify reports it
	// (SPEC §1); it is never a parse error.
	if len(aux.ExternalLink) > 0 {
		if link, err := DecodeExternalLink(aux.ExternalLink); err == nil {
			t.Link = link
		}
	}
	if aux.Dependencies != nil {
		t.Dependencies = make([]string, len(aux.Dependencies))
		for i, d := range aux.Dependencies {
			t.Dependencies[i] = idString(d)
		}
	}
	return t, nil
}

func idString(raw json.RawMessage) string {
	b := bytes.TrimSpace(raw)
	if len(b) >= 2 && b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return s
		}
	}
	return string(b)
}

func validateTask(tag string, t *Task) error {
	if !isPositiveIntString(t.ID) {
		return &ErrInvalidID{Tag: tag, ID: t.ID}
	}
	if !ValidStatuses[t.Status] {
		return &ErrInvalidStatus{ID: t.ID, Status: t.Status}
	}
	if t.Priority != "" && !ValidPriorities[t.Priority] {
		return &ErrInvalidPriority{ID: t.ID, Priority: t.Priority}
	}
	for _, dep := range t.Dependencies {
		if !isPositiveIntString(dep) {
			return &ErrInvalidID{Tag: tag, ID: dep}
		}
	}
	return nil
}

func validateTasks(tag string, tasks []*taskRec) error {
	seen := make(map[string]bool)
	for _, tr := range tasks {
		if seen[tr.task.ID] {
			return &ErrDuplicateID{Tag: tag, ID: tr.task.ID}
		}
		seen[tr.task.ID] = true
	}
	return nil
}

func decodeOrderedObject(data []byte) (*orderedObject, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return nil, fmt.Errorf("expected JSON object")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()

	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := t.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("expected object")
	}

	obj := &orderedObject{
		values: make(map[string]json.RawMessage),
	}

	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("reading key: %w", err)
		}
		key, ok := t.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", t)
		}
		if _, exists := obj.values[key]; exists {
			return nil, fmt.Errorf("duplicate key %q", key)
		}
		obj.keys = append(obj.keys, key)

		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, fmt.Errorf("reading value for %q: %w", key, err)
		}
		obj.values[key] = raw
	}

	t, err = dec.Token()
	if err != nil {
		return nil, err
	}
	if _, ok := t.(json.Delim); !ok {
		return nil, fmt.Errorf("expected closing delimiter")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON object")
	}

	return obj, nil
}

func (o *orderedObject) marshalJSON(indent string) ([]byte, error) {
	if len(o.keys) == 0 {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("{\n")
	for i, key := range o.keys {
		raw := o.values[key]
		indented, err := reindentRaw(raw, indent+"  ")
		if err != nil {
			return nil, fmt.Errorf("reindenting key %q: %w", key, err)
		}
		fmt.Fprintf(&buf, "%s  %s: %s", indent, jsonString(key), indented)
		if i < len(o.keys)-1 {
			buf.WriteString(",\n")
		} else {
			buf.WriteString("\n")
		}
	}
	buf.WriteString(indent)
	buf.WriteString("}")
	return buf.Bytes(), nil
}

func reindentRaw(raw json.RawMessage, baseIndent string) ([]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return raw, nil
	}

	switch raw[0] {
	case '{', '[':
		return reindentContainer(raw, baseIndent)
	default:
		return raw, nil
	}
}

func reindentContainer(raw json.RawMessage, baseIndent string) ([]byte, error) {
	compact := new(bytes.Buffer)
	if err := json.Compact(compact, raw); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, compact.Bytes(), baseIndent, "  "); err != nil {
		return nil, err
	}
	result := buf.Bytes()
	if len(result) > 0 && result[len(result)-1] == '\n' {
		result = result[:len(result)-1]
	}
	return result, nil
}

func jsonString(s string) []byte {
	enc, _ := json.Marshal(s)
	return enc
}
