package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/noisefloor-oss/tasks/internal/backlog"
)

// External-link operations (SPEC §1/§3/§8.2). The link is set-once task
// state; its (family, repository, issue) triple is unique store-wide,
// enforced by the schema v6 partial unique index inside the writing
// transaction.

// LinkSetConflict reports a set-link onto a task that already carries any
// link (SPEC §4, exit 3): the field is set-once and has no clear form.
type LinkSetConflict struct {
	Tag     string
	ID      string
	Current backlog.ExternalLink
}

func (e *LinkSetConflict) Error() string {
	if e.Current.Family == "" {
		return fmt.Sprintf("task %s already carries a non-conforming externalLink member; a link is set-once",
			e.ID)
	}
	return fmt.Sprintf("task %s already carries external link %s; a link is set-once",
		e.ID, e.Current.String())
}

// LinkExistsConflict reports a link triple already carried by another task
// (SPEC §4, exit 3).
type LinkExistsConflict struct {
	Link        backlog.ExternalLink
	ExistingTag string
	ExistingID  string
}

func (e *LinkExistsConflict) Error() string {
	return fmt.Sprintf("external link %s is already carried by task %s in tag %q",
		e.Link.String(), e.ExistingID, e.ExistingTag)
}

// findTaskByLink returns the task carrying the triple anywhere in the
// store, or nil. Store-wide scope matches the unique index (SPEC §1).
func (t *tx) findTaskByLink(link backlog.ExternalLink) (*Task, error) {
	row := t.QueryRow(`SELECT `+taskColumns+` FROM tasks
		WHERE link_family=? AND link_repository=? AND link_issue=? LIMIT 1`,
		link.Family, link.Repository, link.Issue)
	task, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

// isLinkConstraintViolation reports whether err is the unique index
// refusing the triple — the race window between the in-transaction check
// and the write is closed by the index itself.
func isLinkConstraintViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "tasks_external_link") || strings.Contains(msg, "UNIQUE constraint failed: tasks.link_family")
}

// SetLink assigns the task's set-once external link under CAS and the §8.3
// claim gate, following set-milestone exactly: expected revision, revision
// increment, one `update` event. Link grammar validation precedes fencing;
// a task that already carries any link, or a triple owned by another task,
// is a typed conflict (exit 3).
func (d *DB) SetLink(tag, id string, link backlog.ExternalLink, revision int, gate *ClaimGate) error {
	if err := backlog.ValidateExternalLink(&link); err != nil {
		return err
	}
	if err := ValidateTaskRunGate(gate); err != nil {
		return err
	}
	return d.immediate(func(t *tx) error {
		task, err := t.getTask(tag, id)
		if err != nil {
			return err
		}
		if task == nil {
			return &TaskNotFound{Tag: tag, ID: id}
		}
		now := time.Now().UTC()
		expireClaim(task, now)
		if task.Revision != revision {
			return t.staleConflict(tag, id, revision)
		}
		if task.Link != nil {
			return &LinkSetConflict{Tag: tag, ID: id, Current: *task.Link}
		}
		// A non-conforming externalLink member preserved in Extra (possible
		// for imported rows) is presence, not a repair target: setting a
		// link over it would render the field twice and collide in digests.
		if _, malformed := task.Extra["externalLink"]; malformed {
			return &LinkSetConflict{Tag: tag, ID: id}
		}
		if existing, err := t.findTaskByLink(link); err != nil {
			return err
		} else if existing != nil {
			return &LinkExistsConflict{Link: link, ExistingTag: existing.Tag, ExistingID: existing.ID}
		}
		claim := cloneClaim(task.Claim)
		fence, err := t.taskMutationRunFence(task, gate, now, "update")
		if err != nil {
			return err
		}
		task.Link = &link
		task.UpdatedAt = now.Format(time.RFC3339)
		old := task.Revision
		task.Revision++
		if err := taskMutationFaultHook(t, "before-write"); err != nil {
			return err
		}
		n, err := t.writeTaskWithRunFence(task, old, gate, claim, fence, task.UpdatedAt)
		if err != nil {
			if isLinkConstraintViolation(errors.Unwrap(err)) || isLinkConstraintViolation(err) {
				if existing, ferr := t.findTaskByLink(link); ferr == nil && existing != nil {
					return &LinkExistsConflict{Link: link, ExistingTag: existing.Tag, ExistingID: existing.ID}
				}
				return &LinkExistsConflict{Link: link}
			}
			return err
		}
		if n == 0 {
			return t.taskMutationWriteConflict(fence, claim, gate, "update", tag, id, revision)
		}
		return t.appendEvent(Event{Task: id, Tag: tag, Kind: "update", Revision: task.Revision, At: task.UpdatedAt})
	})
}
