package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// Claim records a task's claim state (SPEC §8.3). Token is the fencing
// counter value at acquisition or last renew; it is a write capability and is
// never reported by any surface except claim acquisition/renew output. Extra
// preserves unknown claim fields verbatim per the §1 round-trip rule.
type Claim struct {
	Owner      string
	Token      int64
	TTL        int
	AcquiredAt string
	ExpiresAt  string
	Extra      map[string]json.RawMessage
}

// ClaimExpired reports whether c has expired by the local clock (SPEC §8.3:
// expiry is judged on access, no reaper).
func ClaimExpired(c *Claim, now time.Time) bool {
	if c == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339, c.ExpiresAt)
	return err == nil && !now.Before(expires)
}

// WorktreeBinding records a task-bound worktree (SPEC §9.1) as a sibling of
// the claim. Binding mutations neither require nor increment the task
// revision (SPEC §8.2). Extra preserves unknown binding fields verbatim.
type WorktreeBinding struct {
	Path      string
	Boundary  string
	Branch    string
	CreatedAt string
	Keep      bool
	Extra     map[string]json.RawMessage
}

// Task is one §1 task record decomposed into relational fields. Extra holds
// unknown §1 fields preserved verbatim and re-emitted on export; Subtasks is
// the byte-faithful §1 array value.
type Task struct {
	Tag         string
	ID          string
	Title       string
	Description string
	Details     string
	Status      string
	Priority    string // empty = absent (medium everywhere, §1)
	Milestone   string
	// Link is the task's set-once external link (§1); nil is unset. Only
	// add --link and set-link assign it; every other write preserves it.
	Link         *backlog.ExternalLink
	Dependencies []string
	Subtasks     json.RawMessage
	Extra        map[string]json.RawMessage
	// OuterExtra preserves unknown fields of the outer (legacy) task record —
	// the siblings of task/revision/claim/worktree — verbatim. It is not part
	// of the §1 task object, so Fields never renders it; digests and the
	// §8.6 snapshot render it on the outer record instead.
	OuterExtra map[string]json.RawMessage
	Revision   int
	UpdatedAt  string
	Claim      *Claim
	Binding    *WorktreeBinding
}

// Fields renders the §1 task object: known members in canonical order, then
// unknown fields (Extra) sorted. This is the export/migration rendering; the
// §1 key order of a byte-faithful legacy document is normalized here, which
// the §8.6 semantic digest accounts for by comparing this canonical form.
func (t *Task) Fields() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(`{"id":`)
	writeJSONString(&buf, t.ID)
	buf.WriteString(`,"title":`)
	writeJSONString(&buf, t.Title)
	buf.WriteString(`,"description":`)
	writeJSONString(&buf, t.Description)
	buf.WriteString(`,"details":`)
	writeJSONString(&buf, t.Details)
	buf.WriteString(`,"status":`)
	writeJSONString(&buf, t.Status)
	if t.Priority != "" {
		buf.WriteString(`,"priority":`)
		writeJSONString(&buf, t.Priority)
	}
	if t.Milestone != "" {
		buf.WriteString(`,"milestone":`)
		writeJSONString(&buf, t.Milestone)
	}
	if t.Link != nil {
		buf.WriteString(`,"externalLink":`)
		buf.Write(backlog.MarshalExternalLink(*t.Link))
	}
	buf.WriteString(`,"dependencies":`)
	deps := t.Dependencies
	if deps == nil {
		deps = []string{}
	}
	raw, err := jsonMarshal(deps)
	if err != nil {
		return nil, err
	}
	buf.Write(raw)
	buf.WriteString(`,"subtasks":`)
	if len(t.Subtasks) == 0 {
		buf.WriteString(`[]`)
	} else {
		buf.Write(t.Subtasks)
	}
	buf.WriteString(`,"updatedAt":`)
	writeJSONString(&buf, t.UpdatedAt)
	keys := make([]string, 0, len(t.Extra))
	for k := range t.Extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if len(bytes.TrimSpace(t.Extra[k])) == 0 {
			return nil, fmt.Errorf("task %s unknown field %q has empty value", t.ID, k)
		}
		buf.WriteByte(',')
		writeJSONString(&buf, k)
		buf.WriteByte(':')
		buf.Write(t.Extra[k])
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// TagInfo is one tag's metadata row.
type TagInfo struct {
	Name           string
	NextID         int
	TaskCount      int
	CompletedCount int
	Milestones     json.RawMessage // §12 registry value, byte-faithful; nil when absent
	Extra          map[string]json.RawMessage
}

// Event is one content-free change-feed entry (SPEC §8.4).
type Event struct {
	Seq      int64
	Task     string
	Tag      string
	Kind     string
	Status   string
	Owner    string
	Revision int
	Count    int
	Action   string
	At       string
	Extra    map[string]json.RawMessage
}

// FeedResult is the replay result for the change feed (SPEC §8.4).
type FeedResult struct {
	Events            []Event `json:"events"`
	OldestRetainedSeq int64   `json:"oldestRetainedSeq"`
	Truncated         bool    `json:"truncated"`
}

// MarshalJSON renders the §8.4 event contract: seq, task, tag, kind first,
// then the optional members, with unknown Extra fields flattened inline last.
func (e Event) MarshalJSON() ([]byte, error) {
	var buf strings.Builder
	if err := writeEventJSON(&buf, &e); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// StaleWriteConflict is the typed CAS stale-write conflict (SPEC §8.2,
// exit 3): the command layer maps it to exit 3 with the documented stderr
// contract.
type StaleWriteConflict struct {
	Tag         string
	ID          string
	Supplied    int
	Current     int
	Status      string
	ClaimOwner  string
	ClaimExpiry string
}

func (e *StaleWriteConflict) Error() string {
	return fmt.Sprintf("stale write: task %s revision %d does not match current revision %d", e.ID, e.Supplied, e.Current)
}

// ClaimConflict is a typed claim conflict (SPEC §8.3, exit 3).
type ClaimConflict struct {
	Tag          string
	ID           string
	Kind         string // "claim" | "renew" | "release" | "replay" | "status" | "update"
	Owner        string
	Expiry       string
	FencingToken int64
}

func (e *ClaimConflict) Error() string {
	switch e.Kind {
	case "claim":
		return fmt.Sprintf("claim conflict: task %s is already claimed", e.ID)
	case "renew":
		return fmt.Sprintf("claim conflict: task %s renew token mismatch", e.ID)
	case "release":
		return fmt.Sprintf("claim conflict: task %s release token mismatch", e.ID)
	case "replay":
		return fmt.Sprintf("claim conflict: task %s no longer holds the acquisition recorded for this request", e.ID)
	default:
		return fmt.Sprintf("claim conflict: task %s owner or token mismatch", e.ID)
	}
}

// TaskNotFound is the shared §6 not-found error.
type TaskNotFound struct {
	Tag string
	ID  string
}

func (e *TaskNotFound) Error() string {
	return fmt.Sprintf("task %s not found in tag %q", e.ID, e.Tag)
}

// ClaimGate carries claim and optional live-attempt credentials for a task
// field or status mutation.
type ClaimGate struct {
	Owner        string
	Token        int64
	RunID        int64
	Attempt      int64
	AttemptToken int64
}

func writeJSONString(buf *bytes.Buffer, s string) {
	raw, err := jsonMarshal(s)
	if err != nil {
		panic(err) // strings always encode
	}
	buf.Write(raw)
}

// jsonMarshal encodes v without HTML escaping.
func jsonMarshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}
