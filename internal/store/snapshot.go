package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Snapshot is a full read-model export of the database, used by the §8.6
// migration for the semantic comparison and by tests.
type Snapshot struct {
	Fencing int64
	Tags    map[string]*SnapshotTag
	Events  []Event
}

// SnapshotTag is one tag's exported state.
type SnapshotTag struct {
	Info  *TagInfo
	Tasks []*Task
}

// Snapshot reads the whole database in one deferred transaction.
func (d *DB) Snapshot() (*Snapshot, error) {
	snap := &Snapshot{Tags: map[string]*SnapshotTag{}, Events: []Event{}}
	err := d.deferred(func(t *tx) error {
		if err := t.QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='fencing'`).Scan(&snap.Fencing); err != nil {
			return fmt.Errorf("read fencing counter: %w", err)
		}
		rows, err := t.Query(`SELECT name, next_id, task_count, completed_count, milestones, extra FROM tags ORDER BY name`)
		if err != nil {
			return err
		}
		var infos []*TagInfo
		for rows.Next() {
			var info TagInfo
			var milestones, extra sql.NullString
			if err := rows.Scan(&info.Name, &info.NextID, &info.TaskCount, &info.CompletedCount, &milestones, &extra); err != nil {
				rows.Close()
				return err
			}
			if milestones.Valid {
				info.Milestones = json.RawMessage(milestones.String)
			}
			if err := decodeExtra(extra, &info.Extra); err != nil {
				rows.Close()
				return err
			}
			infos = append(infos, &info)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, info := range infos {
			entry := &SnapshotTag{Info: info, Tasks: []*Task{}}
			cols, err := t.taskSelectColumns()
			if err != nil {
				return err
			}
			taskRows, err := t.Query(`SELECT `+cols+` FROM tasks WHERE tag=? ORDER BY CAST(id AS INTEGER)`, info.Name)
			if err != nil {
				return err
			}
			for taskRows.Next() {
				task, err := scanTask(taskRows)
				if err != nil {
					taskRows.Close()
					return err
				}
				entry.Tasks = append(entry.Tasks, task)
			}
			taskRows.Close()
			if err := taskRows.Err(); err != nil {
				return err
			}
			snap.Tags[info.Name] = entry
		}
		feed, err := t.allEvents()
		if err != nil {
			return err
		}
		snap.Events = feed
		return nil
	})
	if err != nil {
		return nil, err
	}
	return snap, nil
}

func (t *tx) allEvents() ([]Event, error) {
	rows, err := t.Query(`SELECT seq, tag, task_id, kind, status, owner, revision, count, action, extra, created_at FROM events ORDER BY seq`)
	if err != nil {
		return nil, fmt.Errorf("read change feed: %w", err)
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var e Event
		var tag, task, status, owner, action, extra sql.NullString
		var revision, count sql.NullInt64
		if err := rows.Scan(&e.Seq, &tag, &task, &e.Kind, &status, &owner, &revision, &count, &action, &extra, &e.At); err != nil {
			return nil, err
		}
		e.Tag, e.Task, e.Status, e.Owner, e.Action = tag.String, task.String, status.String, owner.String, action.String
		e.Revision, e.Count = int(revision.Int64), int(count.Int64)
		if err := decodeExtra(extra, &e.Extra); err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

// Inventory counts the semantic record classes for the §8.6 receipt.
type Inventory struct {
	Tags       int `json:"tags"`
	Tasks      int `json:"tasks"`
	Milestones int `json:"milestones"`
	Claims     int `json:"claims"`
	Bindings   int `json:"bindings"`
	Events     int `json:"events"`
}

// Inventory computes the snapshot's inventory counts.
func (s *Snapshot) Inventory() Inventory {
	inv := Inventory{Tags: len(s.Tags), Events: len(s.Events)}
	for _, entry := range s.Tags {
		inv.Tasks += len(entry.Tasks)
		if len(entry.Info.Milestones) > 0 {
			var registry []json.RawMessage
			if json.Unmarshal(entry.Info.Milestones, &registry) == nil {
				inv.Milestones += len(registry)
			}
		}
		for _, task := range entry.Tasks {
			if task.Claim != nil {
				inv.Claims++
			}
			if task.Binding != nil {
				inv.Bindings++
			}
		}
	}
	return inv
}

// Digest is the §8.6 semantic digest: a canonical JSON rendering covering
// every task field, revision, claim, milestone registry, binding, per-tag
// counter, the fencing counter, and every retained event. Unknown §1 fields
// are part of the rendering (sorted), so the §1 round-trip rule is verified,
// not bypassed.
func (s *Snapshot) Digest() (string, error) {
	var buf strings.Builder
	buf.WriteString(`{"fencing":`)
	fmt.Fprintf(&buf, "%d", s.Fencing)
	buf.WriteString(`,"tags":{`)
	tags := make([]string, 0, len(s.Tags))
	for name := range s.Tags {
		tags = append(tags, name)
	}
	sort.Strings(tags)
	for i, name := range tags {
		if i > 0 {
			buf.WriteByte(',')
		}
		nameJSON, _ := jsonMarshal(name)
		buf.Write(nameJSON)
		buf.WriteByte(':')
		if err := writeSnapshotTag(&buf, s.Tags[name]); err != nil {
			return "", err
		}
	}
	buf.WriteString(`},"events":[`)
	for i, e := range s.Events {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeEventJSON(&buf, &e); err != nil {
			return "", err
		}
	}
	buf.WriteString(`]}`)
	sum := sha256.Sum256([]byte(buf.String()))
	return hex.EncodeToString(sum[:]), nil
}

func writeSnapshotTag(buf *strings.Builder, entry *SnapshotTag) error {
	info := entry.Info
	fmt.Fprintf(buf, `{"nextId":%d,"taskCount":%d,"completedCount":%d`, info.NextID, info.TaskCount, info.CompletedCount)
	if len(info.Milestones) > 0 {
		buf.WriteString(`,"milestones":`)
		buf.WriteString(string(info.Milestones))
	}
	if err := writeExtraMap(buf, info.Extra); err != nil {
		return err
	}
	buf.WriteString(`,"tasks":[`)
	for i, task := range entry.Tasks {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeSnapshotTask(buf, task); err != nil {
			return err
		}
	}
	buf.WriteString(`]}`)
	return nil
}

func writeSnapshotTask(buf *strings.Builder, task *Task) error {
	fields, err := task.Fields()
	if err != nil {
		return err
	}
	buf.WriteString(`{"task":`)
	// Source formatting inside the task object (unknown §1 fields, the
	// byte-faithful subtasks array) is insignificant to the §8.6 semantic
	// digest and does not survive the import's re-marshal, so compare the
	// compacted form.
	var compacted bytes.Buffer
	if err := json.Compact(&compacted, fields); err != nil {
		return fmt.Errorf("task %s fields: %w", task.ID, err)
	}
	buf.Write(compacted.Bytes())
	fmt.Fprintf(buf, `,"revision":%d`, task.Revision)
	if task.Claim != nil {
		buf.WriteString(`,"claim":`)
		if err := writeClaimJSON(buf, task.Claim); err != nil {
			return err
		}
	}
	if task.Binding != nil {
		buf.WriteString(`,"worktree":`)
		if err := writeBindingJSON(buf, task.Binding); err != nil {
			return err
		}
	}
	if err := writeExtraMap(buf, task.OuterExtra); err != nil {
		return err
	}
	buf.WriteByte('}')
	return nil
}

func writeClaimJSON(buf *strings.Builder, c *Claim) error {
	owner, _ := jsonMarshal(c.Owner)
	acquired, _ := jsonMarshal(c.AcquiredAt)
	expires, _ := jsonMarshal(c.ExpiresAt)
	fmt.Fprintf(buf, `{"owner":%s,"token":%d,"ttl":%d,"acquiredAt":%s,"expiresAt":%s`,
		owner, c.Token, c.TTL, acquired, expires)
	if err := writeExtraMap(buf, c.Extra); err != nil {
		return err
	}
	buf.WriteByte('}')
	return nil
}

func writeBindingJSON(buf *strings.Builder, b *WorktreeBinding) error {
	path, _ := jsonMarshal(b.Path)
	boundary, _ := jsonMarshal(b.Boundary)
	branch, _ := jsonMarshal(b.Branch)
	created, _ := jsonMarshal(b.CreatedAt)
	fmt.Fprintf(buf, `{"path":%s,"boundary":%s,"branch":%s,"createdAt":%s,"keep":%t`,
		path, boundary, branch, created, b.Keep)
	if err := writeExtraMap(buf, b.Extra); err != nil {
		return err
	}
	buf.WriteByte('}')
	return nil
}

func writeEventJSON(buf *strings.Builder, e *Event) error {
	fmt.Fprintf(buf, `{"seq":%d`, e.Seq)
	if e.Task != "" {
		task, _ := jsonMarshal(e.Task)
		fmt.Fprintf(buf, `,"task":%s`, task)
	}
	tag, _ := jsonMarshal(e.Tag)
	kind, _ := jsonMarshal(e.Kind)
	fmt.Fprintf(buf, `,"tag":%s,"kind":%s`, tag, kind)
	if e.Status != "" {
		status, _ := jsonMarshal(e.Status)
		fmt.Fprintf(buf, `,"status":%s`, status)
	}
	if e.Owner != "" {
		owner, _ := jsonMarshal(e.Owner)
		fmt.Fprintf(buf, `,"owner":%s`, owner)
	}
	if e.Revision != 0 {
		fmt.Fprintf(buf, `,"revision":%d`, e.Revision)
	}
	if e.Count != 0 {
		fmt.Fprintf(buf, `,"count":%d`, e.Count)
	}
	if e.Action != "" {
		action, _ := jsonMarshal(e.Action)
		fmt.Fprintf(buf, `,"action":%s`, action)
	}
	at, _ := jsonMarshal(e.At)
	fmt.Fprintf(buf, `,"at":%s`, at)
	if err := writeExtraMap(buf, e.Extra); err != nil {
		return err
	}
	buf.WriteByte('}')
	return nil
}

func writeExtraMap(buf *strings.Builder, extra map[string]json.RawMessage) error {
	if len(extra) == 0 {
		return nil
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(strings.TrimSpace(string(extra[k]))) == 0 {
			return fmt.Errorf("unknown field %q has empty value", k)
		}
		key, _ := jsonMarshal(k)
		buf.WriteByte(',')
		buf.Write(key)
		buf.WriteByte(':')
		// The value's source formatting is insignificant to the §8.6
		// semantic digest and does not survive the import's re-marshal, so
		// compare the compacted form. Compaction (not an unmarshal/marshal
		// round-trip) preserves string escape forms and number spellings
		// exactly; equivalence under those assumes the legacy document was
		// written by this tool's Go encoder, which §8.6's precondition
		// (a store written by a released binary) guarantees.
		var compacted bytes.Buffer
		if err := json.Compact(&compacted, extra[k]); err != nil {
			return fmt.Errorf("unknown field %q: %w", k, err)
		}
		buf.Write(compacted.Bytes())
	}
	return nil
}
