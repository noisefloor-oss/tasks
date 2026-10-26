package backlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Subtask is the normalized read shape for one nested task. Its ID is relative
// to ParentID; callers address it as ParentID + "." + ID.
type Subtask struct {
	ID           string
	ParentID     string
	Title        string
	Description  string
	Details      string
	Status       string
	Dependencies []string
	UpdatedAt    string
}

type taskReference struct {
	parentID  string
	subtaskID string
}

func (r taskReference) nested() bool { return r.subtaskID != "" }

func (r taskReference) String() string {
	if !r.nested() {
		return r.parentID
	}
	return r.parentID + "." + r.subtaskID
}

// ParseTaskReference parses a top-level id or qualified subtask id and returns
// (parentID, subtaskID). subtaskID is empty for a top-level id.
func ParseTaskReference(raw string) (parentID, subtaskID string, err error) {
	ref, err := parseTaskReference(raw)
	if err != nil {
		return "", "", err
	}
	return ref.parentID, ref.subtaskID, nil
}

func parseTaskReference(raw string) (taskReference, error) {
	parts := strings.Split(raw, ".")
	if len(parts) < 1 || len(parts) > 2 {
		return taskReference{}, fmt.Errorf("invalid task reference %q", raw)
	}
	for _, part := range parts {
		if !isPositiveIntString(part) {
			return taskReference{}, fmt.Errorf("invalid task reference %q", raw)
		}
	}
	ref := taskReference{parentID: parts[0]}
	if len(parts) == 2 {
		ref.subtaskID = parts[1]
	}
	return ref, nil
}

type subtaskJSON struct {
	ID           json.RawMessage   `json:"id"`
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	Details      string            `json:"details"`
	Status       string            `json:"status"`
	Dependencies []json.RawMessage `json:"dependencies"`
	UpdatedAt    string            `json:"updatedAt"`
}

func decodeSubtask(parentID string, raw json.RawMessage) (*Subtask, error) {
	if _, err := decodeOrderedObject(raw); err != nil {
		return nil, err
	}
	var src subtaskJSON
	if err := json.Unmarshal(raw, &src); err != nil {
		return nil, err
	}
	subtask := &Subtask{
		ID:          idString(src.ID),
		ParentID:    parentID,
		Title:       src.Title,
		Description: src.Description,
		Details:     src.Details,
		Status:      src.Status,
		UpdatedAt:   src.UpdatedAt,
	}
	if !isPositiveIntString(subtask.ID) {
		return nil, fmt.Errorf("task %s: invalid subtask id %q", parentID, subtask.ID)
	}
	if !ValidStatuses[subtask.Status] {
		return nil, &ErrInvalidStatus{ID: parentID + "." + subtask.ID, Status: subtask.Status}
	}
	if src.Dependencies != nil {
		subtask.Dependencies = make([]string, len(src.Dependencies))
		for i, dependency := range src.Dependencies {
			subtask.Dependencies[i] = idString(dependency)
		}
	}
	return subtask, nil
}

func (f *File) findSubtask(tag string, ref taskReference) (*taskRec, *Subtask, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, nil, fmt.Errorf("tag %q not found", tag)
	}
	var parent *taskRec
	for _, candidate := range blk.tasks {
		if candidate.task.ID == ref.parentID {
			parent = candidate
			break
		}
	}
	if parent == nil {
		return nil, nil, fmt.Errorf("task %s not found in tag %q", ref.String(), tag)
	}

	var match *Subtask
	for _, raw := range parent.task.Subtasks {
		subtask, err := decodeSubtask(ref.parentID, raw)
		if err != nil {
			return nil, nil, err
		}
		if subtask.ID != ref.subtaskID {
			continue
		}
		if match != nil {
			return nil, nil, fmt.Errorf("duplicate subtask id %q under task %s", ref.subtaskID, ref.parentID)
		}
		match = subtask
	}
	if match == nil {
		return nil, nil, fmt.Errorf("task %s not found in tag %q", ref.String(), tag)
	}
	return parent, match, nil
}

// findSubtaskRec locates the referenced subtask inside a single parent taskRec.
func findSubtaskRec(parent *taskRec, ref taskReference) (*Subtask, error) {
	var match *Subtask
	for _, raw := range parent.task.Subtasks {
		subtask, err := decodeSubtask(ref.parentID, raw)
		if err != nil {
			return nil, err
		}
		if subtask.ID != ref.subtaskID {
			continue
		}
		if match != nil {
			return nil, fmt.Errorf("duplicate subtask id %q under task %s", ref.subtaskID, ref.parentID)
		}
		match = subtask
	}
	if match == nil {
		return nil, fmt.Errorf("task %s not found", ref.String())
	}
	return match, nil
}

// DecodeSubtask decodes one serialized subtask with the given parent id.
func DecodeSubtask(parentID string, raw json.RawMessage) (*Subtask, error) {
	return decodeSubtask(parentID, raw)
}

// ResolveTaskReference returns exactly one of task or subtask.
func (f *File) ResolveTaskReference(tag, raw string) (task *Task, subtask *Subtask, err error) {
	ref, err := parseTaskReference(raw)
	if err != nil {
		return nil, nil, err
	}
	if !ref.nested() {
		task, err = f.Task(tag, ref.parentID)
		return task, nil, err
	}
	_, subtask, err = f.findSubtask(tag, ref)
	return nil, subtask, err
}

// SetStatusByReference updates a top-level task or one nested subtask and
// returns the previous status.
func (f *File) SetStatusByReference(tag, raw, status string) (string, error) {
	if !ValidStatuses[status] {
		return "", &ErrInvalidStatus{ID: raw, Status: status}
	}
	ref, err := parseTaskReference(raw)
	if err != nil {
		return "", err
	}
	if ref.nested() {
		return f.setSubtaskStatus(tag, ref, status)
	}

	index, err := f.TaskIndex(tag, ref.parentID)
	if err != nil {
		return "", err
	}
	task, err := f.Task(tag, ref.parentID)
	if err != nil {
		return "", err
	}
	previous := task.Status
	if err := f.WriteMinimal(tag, index, nil, status); err != nil {
		return "", err
	}
	return previous, nil
}

// UpdateSubtaskStatusRaw replaces the status of the referenced subtask inside
// a serialized §1 task object, preserving every other field and sibling
// byte-for-byte. It returns the updated raw object and the previous status.
func UpdateSubtaskStatusRaw(raw json.RawMessage, parentID, subtaskID, status string) (json.RawMessage, string, error) {
	ref := taskReference{parentID: parentID, subtaskID: subtaskID}
	obj, err := decodeOrderedObject(raw)
	if err != nil {
		return nil, "", err
	}
	parent := &taskRec{raw: raw, obj: obj}
	task, err := decodeTask(raw)
	if err != nil {
		return nil, "", err
	}
	parent.task = *task
	subtask, err := findSubtaskRec(parent, ref)
	if err != nil {
		return nil, "", err
	}
	newSubtasks, err := replaceSubtaskStatus(obj.values["subtasks"], ref, status)
	if err != nil {
		return nil, "", err
	}
	newRaw, replaced, err := spliceObjectValues(raw, map[string]json.RawMessage{"subtasks": newSubtasks})
	if err != nil {
		return nil, "", err
	}
	if !replaced {
		return nil, "", fmt.Errorf("task %s has no subtasks field", ref.parentID)
	}
	return newRaw, subtask.Status, nil
}

// RemoveSubtaskRaw removes the referenced subtask from a serialized §1 task
// object, preserving every other field and sibling byte-for-byte.
func RemoveSubtaskRaw(raw json.RawMessage, parentID, subtaskID string) (json.RawMessage, error) {
	ref := taskReference{parentID: parentID, subtaskID: subtaskID}
	obj, err := decodeOrderedObject(raw)
	if err != nil {
		return nil, err
	}
	parent := &taskRec{raw: raw, obj: obj}
	task, err := decodeTask(raw)
	if err != nil {
		return nil, err
	}
	parent.task = *task
	if _, err := findSubtaskRec(parent, ref); err != nil {
		return nil, err
	}
	newSubtasks, err := removeSubtaskElement(obj.values["subtasks"], ref)
	if err != nil {
		return nil, err
	}
	newRaw, replaced, err := spliceObjectValues(raw, map[string]json.RawMessage{"subtasks": newSubtasks})
	if err != nil {
		return nil, err
	}
	if !replaced {
		return nil, fmt.Errorf("task %s has no subtasks field", ref.parentID)
	}
	return newRaw, nil
}

// RemoveTaskByReference removes a top-level task or one nested subtask. A
// nested removal is deliberately conservative: legacy bare dependency ids do
// not encode whether they name a sibling or a top-level task, so every possible
// inbound reference blocks removal and --force is refused.
func (f *File) RemoveTaskByReference(tag, raw string, force bool) ([]string, error) {
	ref, err := parseTaskReference(raw)
	if err != nil {
		return nil, err
	}
	if !ref.nested() {
		return f.RemoveTask(tag, ref.parentID, force)
	}
	if _, _, err := f.findSubtask(tag, ref); err != nil {
		return nil, err
	}
	if force {
		return nil, fmt.Errorf("--force is not supported for hierarchical task %s", ref.String())
	}
	return nil, f.removeSubtask(tag, ref)
}

func (f *File) removeSubtask(tag string, ref taskReference) error {
	parent, _, err := f.findSubtask(tag, ref)
	if err != nil {
		return err
	}
	dependents, err := f.possibleSubtaskDependents(tag, ref)
	if err != nil {
		return err
	}
	if len(dependents) > 0 {
		return fmt.Errorf("task %s is a possible dependency of task(s) %s; remove dependent subtasks first", ref.String(), strings.Join(dependents, ", "))
	}

	newSubtasks, err := removeSubtaskElement(parent.obj.values["subtasks"], ref)
	if err != nil {
		return err
	}
	newRaw, replaced, err := spliceObjectValues(parent.raw, map[string]json.RawMessage{
		"subtasks": newSubtasks,
	})
	if err != nil {
		return err
	}
	if !replaced {
		return fmt.Errorf("task %s has no subtasks field", ref.parentID)
	}
	newObj, err := decodeOrderedObject(newRaw)
	if err != nil {
		return err
	}
	newTask, err := decodeTask(newRaw)
	if err != nil {
		return err
	}
	if err := validateTask(tag, newTask); err != nil {
		return err
	}
	parent.raw = newRaw
	parent.obj = newObj
	parent.task = *newTask
	return f.writeTouched(f.blocks[tag], time.Now().UTC().Format(time.RFC3339))
}

func (f *File) possibleSubtaskDependents(tag string, target taskReference) ([]string, error) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, fmt.Errorf("tag %q not found", tag)
	}
	qualified := target.String()
	var dependents []string
	for _, task := range blk.tasks {
		for _, raw := range task.task.Subtasks {
			subtask, err := decodeSubtask(task.task.ID, raw)
			if err != nil {
				return nil, err
			}
			if subtask.ParentID == target.parentID && subtask.ID == target.subtaskID {
				continue
			}
			for _, dependency := range subtask.Dependencies {
				if dependency == qualified || (subtask.ParentID == target.parentID && dependency == target.subtaskID) {
					dependents = append(dependents, subtask.ParentID+"."+subtask.ID)
					break
				}
			}
		}
	}
	return dependents, nil
}

func (f *File) setSubtaskStatus(tag string, ref taskReference, status string) (string, error) {
	parent, subtask, err := f.findSubtask(tag, ref)
	if err != nil {
		return "", err
	}
	newSubtasks, err := replaceSubtaskStatus(parent.obj.values["subtasks"], ref, status)
	if err != nil {
		return "", err
	}
	newRaw, replaced, err := spliceObjectValues(parent.raw, map[string]json.RawMessage{
		"subtasks": newSubtasks,
	})
	if err != nil {
		return "", err
	}
	if !replaced {
		return "", fmt.Errorf("task %s has no subtasks field", ref.parentID)
	}

	newObj, err := decodeOrderedObject(newRaw)
	if err != nil {
		return "", err
	}
	newTask, err := decodeTask(newRaw)
	if err != nil {
		return "", err
	}
	if err := validateTask(tag, newTask); err != nil {
		return "", err
	}

	parent.raw = newRaw
	parent.obj = newObj
	parent.task = *newTask
	if err := f.writeTouched(f.blocks[tag], time.Now().UTC().Format(time.RFC3339)); err != nil {
		return "", err
	}
	return subtask.Status, nil
}

func replaceSubtaskStatus(raw json.RawMessage, ref taskReference, status string) (json.RawMessage, error) {
	src := bytes.TrimSpace(raw)
	dec := json.NewDecoder(bytes.NewReader(src))
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("task %s: subtasks is not an array", ref.parentID)
	}

	matchStart, matchEnd := int64(-1), int64(-1)
	var replacement json.RawMessage
	for dec.More() {
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return nil, err
		}
		subtask, err := decodeSubtask(ref.parentID, element)
		if err != nil {
			return nil, err
		}
		if subtask.ID != ref.subtaskID {
			continue
		}
		if matchStart >= 0 {
			return nil, fmt.Errorf("duplicate subtask id %q under task %s", ref.subtaskID, ref.parentID)
		}
		matchEnd = dec.InputOffset()
		matchStart = matchEnd - int64(len(element))
		var replaced bool
		replacement, replaced, err = spliceObjectValues(element, map[string]json.RawMessage{
			"status": mustMarshalJSON(status),
		})
		if err != nil {
			return nil, err
		}
		if !replaced {
			return nil, fmt.Errorf("task %s has no unambiguous status field", ref.String())
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if matchStart < 0 {
		return nil, fmt.Errorf("task %s not found", ref.String())
	}

	size, err := checkedSpliceSize(len(src)-int(matchEnd-matchStart), len(replacement))
	if err != nil {
		return nil, err
	}
	result := make([]byte, 0, size)
	result = append(result, src[:matchStart]...)
	result = append(result, replacement...)
	result = append(result, src[matchEnd:]...)
	return result, nil
}

type subtaskSpan struct {
	start int64
	end   int64
}

func removeSubtaskElement(raw json.RawMessage, ref taskReference) (json.RawMessage, error) {
	src := bytes.TrimSpace(raw)
	dec := json.NewDecoder(bytes.NewReader(src))
	token, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return nil, fmt.Errorf("task %s: subtasks is not an array", ref.parentID)
	}

	var spans []subtaskSpan
	match := -1
	for dec.More() {
		var element json.RawMessage
		if err := dec.Decode(&element); err != nil {
			return nil, err
		}
		end := dec.InputOffset()
		spans = append(spans, subtaskSpan{start: end - int64(len(element)), end: end})
		subtask, err := decodeSubtask(ref.parentID, element)
		if err != nil {
			return nil, err
		}
		if subtask.ID == ref.subtaskID {
			if match >= 0 {
				return nil, fmt.Errorf("duplicate subtask id %q under task %s", ref.subtaskID, ref.parentID)
			}
			match = len(spans) - 1
		}
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if match < 0 {
		return nil, fmt.Errorf("task %s not found", ref.String())
	}

	removeStart, removeEnd := spans[match].start, spans[match].end
	switch {
	case len(spans) == 1:
	case match < len(spans)-1:
		removeEnd = spans[match+1].start
	default:
		removeStart = spans[match-1].end
	}
	result := make([]byte, 0, len(src)-int(removeEnd-removeStart))
	result = append(result, src[:removeStart]...)
	result = append(result, src[removeEnd:]...)
	return result, nil
}
