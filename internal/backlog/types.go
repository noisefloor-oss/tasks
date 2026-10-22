package backlog

import (
	"encoding/json"
	"fmt"
)

var ValidStatuses = map[string]bool{
	"pending":     true,
	"in-progress": true,
	"review":      true,
	"done":        true,
	"deferred":    true,
	"cancelled":   true,
	"blocked":     true,
}

var ValidPriorities = map[string]bool{
	"high":   true,
	"medium": true,
	"low":    true,
}

type Task struct {
	ID           string
	Title        string
	Description  string
	Details      string
	Status       string
	Priority     string
	Dependencies []string
	// Milestone names an entry of the tag's §12 registry; empty is
	// unassigned.
	Milestone string
	Subtasks  []json.RawMessage
	UpdatedAt string
}

type Metadata struct {
	Version        string   `json:"version"`
	LastModified   string   `json:"lastModified"`
	TaskCount      int      `json:"taskCount"`
	CompletedCount int      `json:"completedCount"`
	Tags           []string `json:"tags"`
}

type ErrDuplicateID struct {
	Tag string
	ID  string
}

func (e *ErrDuplicateID) Error() string {
	return fmt.Sprintf("duplicate task id %q in tag %q", e.ID, e.Tag)
}

type ErrInvalidID struct {
	Tag string
	ID  string
}

func (e *ErrInvalidID) Error() string {
	return fmt.Sprintf("invalid task id %q in tag %q", e.ID, e.Tag)
}

type ErrInvalidStatus struct {
	ID     string
	Status string
}

func (e *ErrInvalidStatus) Error() string {
	return fmt.Sprintf("invalid status %q for task %s", e.Status, e.ID)
}

type ErrInvalidPriority struct {
	ID       string
	Priority string
}

func (e *ErrInvalidPriority) Error() string {
	return fmt.Sprintf("invalid priority %q for task %s", e.Priority, e.ID)
}

type orderedObject struct {
	keys   []string
	values map[string]json.RawMessage
}

func (o *orderedObject) setRaw(key string, val json.RawMessage) {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = val
}

func (o *orderedObject) clone() *orderedObject {
	c := &orderedObject{
		keys:   append([]string(nil), o.keys...),
		values: make(map[string]json.RawMessage, len(o.values)),
	}
	for k, v := range o.values {
		c.values[k] = v
	}
	return c
}

// deleteKey removes key from the object, reporting whether it was present.
func (o *orderedObject) deleteKey(key string) bool {
	if _, exists := o.values[key]; !exists {
		return false
	}
	delete(o.values, key)
	for i, k := range o.keys {
		if k == key {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
	return true
}

func isPositiveIntString(s string) bool {
	if s == "" || s[0] == '0' {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
