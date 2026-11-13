package store

import (
	"bytes"
	"database/sql"
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
