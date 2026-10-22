package backlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Milestones (SPEC §12): a per-tag registry carried on the tag block's
// optional "milestones" key, and the optional task field naming one entry.
// Registry writes splice or drop single entries; every other byte of the
// tag block stays identical, and unknown fields on surviving entries are
// preserved verbatim.

const milestonesKey = "milestones"

// ValidMilestoneStatuses are the §12.1 entry statuses.
var ValidMilestoneStatuses = map[string]bool{
	"open":   true,
	"closed": true,
}

// MilestoneEntry is one registry entry. Raw is the byte-faithful entry
// object; Name and Status are its decoded values.
type MilestoneEntry struct {
	Name   string
	Status string
	Raw    json.RawMessage
}

// ErrMilestoneName reports a name that violates the §12.1 grammar.
type ErrMilestoneName struct {
	Name   string
	Reason string
}

func (e *ErrMilestoneName) Error() string {
	return fmt.Sprintf("invalid milestone name %q: %s", e.Name, e.Reason)
}

// ErrMilestoneRegistry reports a registry value that is not §12.1-shaped.
type ErrMilestoneRegistry struct {
	Tag    string
	Reason string
}

func (e *ErrMilestoneRegistry) Error() string {
	return fmt.Sprintf("tag %q: invalid milestones registry: %s", e.Tag, e.Reason)
}

// ErrMilestoneNotFound reports a registry name that does not exist.
type ErrMilestoneNotFound struct {
	Tag  string
	Name string
}

func (e *ErrMilestoneNotFound) Error() string {
	return fmt.Sprintf("milestone %q does not exist in tag %q", e.Name, e.Tag)
}

// ErrMilestoneExists reports a duplicate registry name (a typed conflict).
type ErrMilestoneExists struct {
	Tag  string
	Name string
}

func (e *ErrMilestoneExists) Error() string {
	return fmt.Sprintf("milestone %q already exists in tag %q", e.Name, e.Tag)
}

// ErrMilestoneInUse reports a removal blocked by referencing tasks.
type ErrMilestoneInUse struct {
	Tag     string
	Name    string
	TaskIDs []string
}

func (e *ErrMilestoneInUse) Error() string {
	return fmt.Sprintf("milestone %q still assigned to tasks %s in tag %q", e.Name, strings.Join(e.TaskIDs, ","), e.Tag)
}

// ValidateMilestoneName enforces the §12.1 grammar: trimmed, nonempty,
// valid UTF-8 of at most 256 bytes, no ASCII control character, and no
// leading hyphen.
func ValidateMilestoneName(name string) error {
	trimmed := strings.TrimSpace(name)
	if name != trimmed {
		return &ErrMilestoneName{Name: name, Reason: "leading or trailing whitespace"}
	}
	if name == "" {
		return &ErrMilestoneName{Name: name, Reason: "empty"}
	}
	if len(name) > 256 {
		return &ErrMilestoneName{Name: name, Reason: "longer than 256 bytes"}
	}
	if !utf8Valid(name) {
		return &ErrMilestoneName{Name: name, Reason: "not valid UTF-8"}
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return &ErrMilestoneName{Name: name, Reason: "contains an ASCII control character"}
		}
	}
	if strings.HasPrefix(name, "-") {
		return &ErrMilestoneName{Name: name, Reason: "begins with '-'"}
	}
	return nil
}

func utf8Valid(s string) bool {
	return strings.ToValidUTF8(s, string(rune(0xFFFD))) == s
}

// MilestonesRaw returns the tag's registry value verbatim, or nil when the
// tag carries no registry.
func (f *File) MilestonesRaw(tag string) (json.RawMessage, bool) {
	blk, ok := f.blocks[tag]
	if !ok {
		return nil, false
	}
	raw, ok := blk.values[milestonesKey]
	return raw, ok
}

// Milestones parses the tag's registry. A registry that exists but is not
// §12.1-shaped refuses with ErrMilestoneRegistry: structure, status,
// name grammar, and uniqueness are all enforced (§12.1's rule that
// registry-mutating commands refuse a malformed registry rather than
// repair it).
func (f *File) Milestones(tag string) ([]MilestoneEntry, error) {
	raw, ok := f.MilestonesRaw(tag)
	if !ok {
		return nil, nil
	}
	return ParseMilestones(tag, raw)
}

func ParseMilestones(tag string, raw json.RawMessage) ([]MilestoneEntry, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, &ErrMilestoneRegistry{Tag: tag, Reason: "not an array"}
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(trimmed, &elems); err != nil {
		return nil, &ErrMilestoneRegistry{Tag: tag, Reason: "malformed array"}
	}
	entries := make([]MilestoneEntry, 0, len(elems))
	seen := make(map[string]bool, len(elems))
	for i, elem := range elems {
		var decoded struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(elem, &decoded); err != nil || decoded.Name == "" {
			return nil, &ErrMilestoneRegistry{Tag: tag, Reason: fmt.Sprintf("entry %d is not an object with a name", i)}
		}
		if !ValidMilestoneStatuses[decoded.Status] {
			return nil, &ErrMilestoneRegistry{Tag: tag, Reason: fmt.Sprintf("entry %d (%q) has invalid status %q", i, decoded.Name, decoded.Status)}
		}
		if err := ValidateMilestoneName(decoded.Name); err != nil {
			return nil, &ErrMilestoneRegistry{Tag: tag, Reason: fmt.Sprintf("entry %d: %v", i, err)}
		}
		if seen[decoded.Name] {
			return nil, &ErrMilestoneRegistry{Tag: tag, Reason: fmt.Sprintf("milestone %q is defined more than once", decoded.Name)}
		}
		seen[decoded.Name] = true
		entries = append(entries, MilestoneEntry{Name: decoded.Name, Status: decoded.Status, Raw: elem})
	}
	return entries, nil
}

// MilestoneCountsForList returns, per milestone name, the assigned task
// total and done count for the tag (the `milestone list` rollup).
func (f *File) MilestoneCountsForList(tag string) (map[string]int, map[string]int, error) {
	return f.milestoneTaskCounts(tag)
}

// MilestoneRemaining counts the tag's tasks assigned to name whose status
// is neither done nor cancelled.
func (f *File) MilestoneRemaining(tag, name string) int {
	remaining := 0
	if blk, ok := f.blocks[tag]; ok {
		for _, tr := range blk.tasks {
			if tr.task.Milestone == name && tr.task.Status != "done" && tr.task.Status != "cancelled" {
				remaining++
			}
		}
	}
	return remaining
}

// milestoneTaskCounts returns, per milestone name, the assigned task total
// and done count for the tag.
func (f *File) milestoneTaskCounts(tag string) (map[string]int, map[string]int, error) {
	tasks, err := f.Tasks(tag)
	if err != nil {
		return nil, nil, err
	}
	total := make(map[string]int)
	done := make(map[string]int)
	for _, t := range tasks {
		if t.Milestone == "" {
			continue
		}
		total[t.Milestone]++
		if t.Status == "done" {
			done[t.Milestone]++
		}
	}
	return total, done, nil
}

// marshalMilestones serializes entry raws as the registry array value: one
// entry per line at the task-element indent, matching the tasks array.
func MarshalMilestones(entries []MilestoneEntry) (json.RawMessage, error) {
	if len(entries) == 0 {
		return json.RawMessage("[]"), nil
	}
	var buf bytes.Buffer
	buf.WriteString("[\n")
	for i, e := range entries {
		buf.WriteString(taskElemIndent)
		buf.Write(e.Raw)
		if i < len(entries)-1 {
			buf.WriteString(",\n")
		} else {
			buf.WriteString("\n")
		}
	}
	buf.WriteString("    ]")
	return json.RawMessage(buf.Bytes()), nil
}

// arrayElem is one decoded element: its exact bytes and the byte span it
// occupies in the array text.
type arrayElem struct {
	val    json.RawMessage
	start  int
	end    int
	leadWS string // whitespace (and comma) text between it and the previous element
}

func decodeArrayElems(src []byte) ([]arrayElem, int, error) {
	trimmed := bytes.TrimSpace(src)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil, 0, fmt.Errorf("expected JSON array")
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	if _, err := dec.Token(); err != nil {
		return nil, 0, err
	}
	var elems []arrayElem
	boundary := 0
	for dec.More() {
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, 0, err
		}
		end := int(dec.InputOffset())
		start := end - len(val)
		elems = append(elems, arrayElem{
			val:    val,
			start:  start,
			end:    end,
			leadWS: string(src[boundary:start]),
		})
		boundary = end
	}
	closing := bytes.LastIndexByte(bytes.TrimSpace(src), ']')
	if closing < 0 {
		return nil, 0, fmt.Errorf("unterminated JSON array")
	}
	// Position of ']' in the original src (TrimSpace only strips the
	// outer whitespace, so add the leading offset back).
	closing += len(src) - len(bytes.TrimSpace(src))
	return elems, closing, nil
}

// SpliceArrayElement replaces one element's exact bytes in the array
// text; every other byte — framing, whitespace, untouched entries —
// stays identical (§12.2's single-entry splice).
func SpliceArrayElement(raw json.RawMessage, index int, newElem json.RawMessage) (json.RawMessage, error) {
	src := []byte(raw)
	elems, _, err := decodeArrayElems(src)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(elems) {
		return nil, fmt.Errorf("array element %d out of range", index)
	}
	e := elems[index]
	out := make([]byte, 0, len(src)+len(newElem))
	out = append(out, src[:e.start]...)
	out = append(out, newElem...)
	out = append(out, src[e.end:]...)
	return out, nil
}

// AppendArrayElement inserts newElem before the array's closing bracket,
// reusing the separator shape already present between elements.
func AppendArrayElement(raw json.RawMessage, newElem json.RawMessage) (json.RawMessage, error) {
	src := []byte(raw)
	elems, closing, err := decodeArrayElems(src)
	if err != nil {
		return nil, err
	}
	var out []byte
	if len(elems) == 0 {
		out = append(out, "[\n"...)
		out = append(out, taskElemIndent...)
		out = append(out, newElem...)
		out = append(out, "\n    ]"...)
		return out, nil
	}
	sep := elems[len(elems)-1].leadWS
	// The separator text may open with '[' (first element) or ','
	// (later elements); reuse only its trailing whitespace run as the
	// appended element's indentation.
	cut := strings.LastIndexFunc(sep, func(r rune) bool { return !unicode.IsSpace(r) })
	ws := ""
	if cut >= 0 && cut < len(sep) {
		ws = sep[cut+1:]
	} else if cut < 0 {
		ws = sep
	}
	out = append(out, src[:elems[len(elems)-1].end]...)
	out = append(out, ',')
	out = append(out, ws...)
	out = append(out, newElem...)
	out = append(out, src[elems[len(elems)-1].end:closing]...)
	out = append(out, src[closing:]...)
	return out, nil
}

// RemoveArrayElement deletes element index together with the separator
// that precedes it (or, for the last element, the separator that follows
// the previous one), keeping the remaining framing bytes identical.
func RemoveArrayElement(raw json.RawMessage, index int) (json.RawMessage, error) {
	src := []byte(raw)
	elems, closing, err := decodeArrayElems(src)
	if err != nil {
		return nil, err
	}
	if index < 0 || index >= len(elems) {
		return nil, fmt.Errorf("array element %d out of range", index)
	}
	if len(elems) == 1 {
		return json.RawMessage("[]"), nil
	}
	e := elems[index]
	var out []byte
	if index == len(elems)-1 {
		// Remove the comma after the previous element plus this element.
		prev := elems[index-1]
		out = append(out, src[:prev.end]...)
		out = append(out, src[e.end:closing]...)
		out = append(out, src[closing:]...)
		return out, nil
	}
	// Remove this element and the separator that leads into the next one.
	out = append(out, src[:e.start]...)
	out = append(out, src[elems[index+1].start:]...)
	return out, nil
}

// marshalMilestoneEntry serializes one new registry entry at the array's
// element indent with the canonical §12.1 key order.
func MarshalMilestoneEntry(name, status string) (json.RawMessage, error) {
	obj := &orderedObject{values: make(map[string]json.RawMessage)}
	obj.setRaw("name", MarshalString(name))
	obj.setRaw("status", mustMarshalJSON(status))
	return obj.marshalJSON(taskElemIndent)
}

// AddMilestone appends one open entry to the tag's registry.
func (f *File) AddMilestone(tag, name string) error {
	if err := ValidateMilestoneName(name); err != nil {
		return err
	}
	entries, err := f.Milestones(tag)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name == name {
			return &ErrMilestoneExists{Tag: tag, Name: name}
		}
	}
	entry, err := MarshalMilestoneEntry(name, "open")
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if prior, ok := f.MilestonesRaw(tag); ok {
		raw, err = AppendArrayElement(prior, entry)
	} else {
		raw, err = MarshalMilestones([]MilestoneEntry{{Name: name, Status: "open", Raw: entry}})
	}
	if err != nil {
		return err
	}
	return f.setMilestonesValue(tag, raw, time.Now().UTC().Format(time.RFC3339))
}

// SetMilestoneStatus flips one entry's status in place, splicing only that
// value inside the entry's object.
func (f *File) SetMilestoneStatus(tag, name, status string) error {
	if !ValidMilestoneStatuses[status] {
		return &ErrMilestoneRegistry{Tag: tag, Reason: fmt.Sprintf("invalid status %q", status)}
	}
	entries, err := f.Milestones(tag)
	if err != nil {
		return err
	}
	for i, e := range entries {
		if e.Name != name {
			continue
		}
		if e.Status == status {
			return nil
		}
		spliced, err := SpliceObjectValues(e.Raw, map[string]json.RawMessage{"status": mustMarshalJSON(status)})
		if err != nil {
			return err
		}
		prior, _ := f.MilestonesRaw(tag)
		raw, err := SpliceArrayElement(prior, i, spliced)
		if err != nil {
			return err
		}
		return f.setMilestonesValue(tag, raw, time.Now().UTC().Format(time.RFC3339))
	}
	return &ErrMilestoneNotFound{Tag: tag, Name: name}
}

// RemoveMilestone drops one entry from the registry. With strip it also
// clears the milestone field from every referencing task (bumping each
// updatedAt) and returns their ids; without strip it refuses with
// ErrMilestoneInUse while any reference remains.
func (f *File) RemoveMilestone(tag, name string, strip bool) ([]string, error) {
	entries, err := f.Milestones(tag)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, e := range entries {
		if e.Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, &ErrMilestoneNotFound{Tag: tag, Name: name}
	}

	var refs []string
	for _, tr := range f.blocks[tag].tasks {
		if tr.task.Milestone == name {
			refs = append(refs, tr.task.ID)
		}
	}
	if len(refs) > 0 && !strip {
		sort.Strings(refs)
		return nil, &ErrMilestoneInUse{Tag: tag, Name: name, TaskIDs: refs}
	}

	now := time.Now().UTC().Format(time.RFC3339)
	blk := f.blocks[tag]
	// All field clears and the registry edit publish in one atomic write:
	// a failure anywhere leaves the file untouched.
	var ids []string
	for _, tr := range blk.tasks {
		if tr.task.Milestone != name {
			continue
		}
		if err := clearMilestoneFieldInMemory(tag, blk, tr, now); err != nil {
			return nil, err
		}
		ids = append(ids, tr.task.ID)
	}
	prior, _ := f.MilestonesRaw(tag)
	if len(entries) == 1 {
		f.RemoveRegistry(tag)
		return ids, f.writeTouched(blk, now)
	}
	raw, err := RemoveArrayElement(prior, idx)
	if err != nil {
		return nil, err
	}
	f.setRegistryLocked(blk, raw)
	return ids, f.writeTouched(blk, now)
}

func clearMilestoneFieldInMemory(tag string, blk *tagBlock, tr *taskRec, now string) error {
	cleared, err := DeleteObjectMember(tr.raw, "milestone")
	if err != nil {
		return err
	}
	if string(cleared) == string(tr.raw) {
		return nil // field absent: nothing to do
	}
	newRaw, err := SpliceObjectValues(cleared, map[string]json.RawMessage{"updatedAt": mustMarshalJSON(now)})
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
	newObj, err := decodeOrderedObject(newRaw)
	if err != nil {
		return err
	}
	tr.task, tr.raw, tr.obj = *updated, newRaw, newObj
	return nil
}

// SetTaskMilestone assigns (name != "") or clears (name == "") a task's
// milestone field. Assigning the value the task already carries, or
// clearing an absent field, is a successful no-op that writes nothing.
func (f *File) SetTaskMilestone(tag, id, name string) error {
	t, err := f.Task(tag, id)
	if err != nil {
		return err
	}
	if name == "" {
		if t.Milestone == "" {
			return nil
		}
		return f.clearTaskField(tag, id, "milestone", time.Now().UTC().Format(time.RFC3339))
	}
	if err := ValidateMilestoneName(name); err != nil {
		return err
	}
	entries, err := f.Milestones(tag)
	if err != nil {
		return err
	}
	found := false
	for _, e := range entries {
		if e.Name == name {
			found = true
			break
		}
	}
	if !found {
		return &ErrMilestoneNotFound{Tag: tag, Name: name}
	}
	if t.Milestone == name {
		return nil
	}
	idx, err := f.TaskIndex(tag, id)
	if err != nil {
		return err
	}
	blk := f.blocks[tag]
	newRaw, err := AssignMilestone(blk.tasks[idx].raw, name)
	if err != nil {
		return err
	}
	return f.commitTaskEdit(tag, blk, blk.tasks[idx], newRaw, time.Now().UTC().Format(time.RFC3339))
}

// AssignMilestone sets the milestone field on a serialized §1 task object,
// keeping MarshalTask's key order (after priority, else after status) and
// splicing only the new member's bytes: every other member, its
// formatting, and unknown nested values stay byte-identical (§3).
func AssignMilestone(raw json.RawMessage, name string) (json.RawMessage, error) {
	anchor := "priority"
	if obj, err := decodeOrderedObject(raw); err == nil {
		if _, hasPriority := obj.values["priority"]; !hasPriority {
			anchor = "status"
		}
	}
	return InsertObjectMember(raw, anchor, "milestone", MarshalString(name))
}

// ClearMilestone removes the milestone field from a serialized §1 task
// object, splicing out only that member's span.
func ClearMilestone(raw json.RawMessage) (json.RawMessage, error) {
	return DeleteObjectMember(raw, "milestone")
}

// objectMemberSpans walks a JSON object recording each member's key span,
// value span, and the separator text before its key.
type objectMember struct {
	key      string
	keyStart int
	keyEnd   int
	valStart int
	valEnd   int
	leadSep  string
}

func decodeObjectMembers(src []byte) ([]objectMember, error) {
	trimmed := bytes.TrimSpace(src)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, fmt.Errorf("expected JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(src))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var members []objectMember
	boundary := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected string key, got %T", keyTok)
		}
		keyStart := int(dec.InputOffset()) - len(`"`+key+`"`)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, err
		}
		valEnd := int(dec.InputOffset())
		valStart := valEnd - len(val)
		members = append(members, objectMember{
			key:      key,
			keyStart: keyStart,
			keyEnd:   keyStart + len(`"`+key+`"`),
			valStart: valStart,
			valEnd:   valEnd,
			leadSep:  string(src[boundary:keyStart]),
		})
		boundary = valEnd
	}
	return members, nil
}

// InsertObjectMember inserts `key: value` immediately after afterKey's
// value (or appends before the closing brace when afterKey is absent),
// reusing the separator shape already present between members.
func InsertObjectMember(raw json.RawMessage, afterKey, key string, value json.RawMessage) (json.RawMessage, error) {
	src := []byte(raw)
	members, err := decodeObjectMembers(src)
	if err != nil {
		return nil, err
	}
	kvSep := ": "
	if len(members) > 0 && members[0].valStart > members[0].keyEnd {
		kvSep = string(src[members[0].keyEnd:members[0].valStart])
	}
	newMember := append(append(jsonString(key), kvSep...), value...)
	// Existing member with the same key: replace its value span.
	for _, m := range members {
		if m.key != key {
			continue
		}
		out := make([]byte, 0, len(src)+len(newMember))
		out = append(out, src[:m.valStart]...)
		out = append(out, value...)
		out = append(out, src[m.valEnd:]...)
		return out, nil
	}
	// The separator shape comes from the file's own between-member gap
	// (the last one when several exist), so the insert matches the
	// object's style instead of imposing one.
	gap := members[0].leadSep
	if len(members) > 1 {
		gap = members[len(members)-1].leadSep
	}
	ws := trailingWhitespace(gap)
	sep := "," + ws
	for _, m := range members {
		if m.key != afterKey {
			continue
		}
		out := make([]byte, 0, len(src)+len(newMember)+len(sep))
		out = append(out, src[:m.valEnd]...)
		out = append(out, []byte(sep)...)
		out = append(out, newMember...)
		out = append(out, src[m.valEnd:]...)
		return out, nil
	}
	// Anchor absent: insert before the closing brace.
	closing := bytes.LastIndexByte(bytes.TrimSpace(src), '}')
	if closing < 0 {
		return nil, fmt.Errorf("unterminated JSON object")
	}
	closing += len(src) - len(bytes.TrimSpace(src))
	lead := "{"
	if len(members) > 0 {
		lead = "," + ws
	}
	var out []byte
	out = append(out, src[:closing]...)
	out = append(out, []byte(lead)...)
	out = append(out, newMember...)
	out = append(out, src[closing:]...)
	return out, nil
}

// trailingWhitespace returns the trailing whitespace run of s.
func trailingWhitespace(s string) string {
	cut := strings.LastIndexFunc(s, func(r rune) bool { return !unicode.IsSpace(r) })
	if cut < 0 {
		return s
	}
	if cut+1 > len(s) {
		return ""
	}
	return s[cut+1:]
}

// DeleteObjectMember removes key's member together with the separator
// that binds it to a neighbor, keeping every other byte identical.
func DeleteObjectMember(raw json.RawMessage, key string) (json.RawMessage, error) {
	src := []byte(raw)
	members, err := decodeObjectMembers(src)
	if err != nil {
		return nil, err
	}
	idx := -1
	for i, m := range members {
		if m.key == key {
			idx = i
			break
		}
	}
	if idx < 0 {
		return src, nil
	}
	if len(members) == 1 {
		return json.RawMessage("{}"), nil
	}
	var out []byte
	if idx == len(members)-1 {
		// Remove the comma after the previous member plus this member.
		prev := members[idx-1]
		out = append(out, src[:prev.valEnd]...)
		out = append(out, src[members[idx].valEnd:]...)
		return out, nil
	}
	// Remove this member and the separator that leads into the next one.
	out = append(out, src[:members[idx].keyStart]...)
	out = append(out, src[members[idx+1].keyStart:]...)
	return out, nil
}

// SetRegistry installs raw as the tag's registry value without writing;
// the caller's next write publishes it. The key is created after "tasks"
// when absent.
func (f *File) SetRegistry(tag string, raw json.RawMessage) error {
	blk, ok := f.blocks[tag]
	if !ok {
		return fmt.Errorf("tag %q not found", tag)
	}
	f.setRegistryLocked(blk, raw)
	return nil
}

// setRegistryLocked mutates blk in place: creating the key after "tasks"
// when absent.
func (f *File) setRegistryLocked(blk *tagBlock, raw json.RawMessage) {
	if _, exists := blk.values[milestonesKey]; !exists {
		at := len(blk.keys)
		for i, key := range blk.keys {
			if key == "tasks" {
				at = i + 1
				break
			}
		}
		blk.keys = append(blk.keys, "")
		copy(blk.keys[at+1:], blk.keys[at:])
		blk.keys[at] = milestonesKey
	}
	blk.values[milestonesKey] = raw
}

// setMilestonesValue installs the registry value and writes the file with
// per-write metadata maintenance.
func (f *File) setMilestonesValue(tag string, raw json.RawMessage, now string) error {
	if err := f.SetRegistry(tag, raw); err != nil {
		return err
	}
	return f.writeTouched(f.blocks[tag], now)
}

// clearTaskField removes key from a task's object, bumps its updatedAt,
// preserves the rest of the object's members, order, and values, and
// writes the file.
func (f *File) clearTaskField(tag, id, key, now string) error {
	blk, ok := f.blocks[tag]
	if !ok {
		return fmt.Errorf("tag %q not found", tag)
	}
	for _, tr := range blk.tasks {
		if tr.task.ID != id {
			continue
		}
		obj := tr.obj.clone()
		if !obj.deleteKey(key) {
			return nil
		}
		obj.setRaw("updatedAt", mustMarshalJSON(now))
		newRaw, err := obj.marshalJSON(taskElemIndent)
		if err != nil {
			return err
		}
		return f.commitTaskEdit(tag, blk, tr, newRaw, now)
	}
	return fmt.Errorf("task %s not found in tag %q", id, tag)
}

// RemoveRegistry deletes the tag's registry key without writing. It is
// the materialize path's counterpart to SetRegistry for a store that no
// longer carries a registry.
func (f *File) RemoveRegistry(tag string) {
	blk, ok := f.blocks[tag]
	if !ok {
		return
	}
	delete(blk.values, milestonesKey)
	for i, key := range blk.keys {
		if key == milestonesKey {
			blk.keys = append(blk.keys[:i], blk.keys[i+1:]...)
			return
		}
	}
}

// ClosedMilestones returns the set of closed milestone names for tag. A
// registry that fails strict parsing yields no closed names: `next` then
// behaves as if no milestone were closed, and verify reports the registry.
func (f *File) ClosedMilestones(tag string) map[string]bool {
	closed := make(map[string]bool)
	entries, err := f.Milestones(tag)
	if err != nil {
		return closed
	}
	for _, e := range entries {
		if e.Status == "closed" {
			closed[e.Name] = true
		}
	}
	return closed
}
