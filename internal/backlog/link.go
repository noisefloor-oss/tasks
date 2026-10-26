package backlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// External links (SPEC §1): a task's optional, set-once field naming the
// external source issue it tracks — (family, repository, issue), unique
// across the authority. The link is ordinary §1 state; a malformed value
// is a verify finding, never a parse error.

// ExternalLink is one parsed `externalLink` value.
type ExternalLink struct {
	Family     string
	Repository string
	Issue      int64
}

// String renders the canonical text form <family>/<repository>#<issue>.
func (l ExternalLink) String() string {
	return l.Family + "/" + l.Repository + "#" + strconv.FormatInt(l.Issue, 10)
}

// Equal reports whether two links carry the same triple.
func (l ExternalLink) Equal(other ExternalLink) bool {
	return l.Family == other.Family && l.Repository == other.Repository && l.Issue == other.Issue
}

// ParseExternalLinkText parses the canonical <family>/<repository>#<issue>
// text form (the grammar of every --link-shaped flag and argument).
func ParseExternalLinkText(s string) (*ExternalLink, error) {
	hash := strings.LastIndex(s, "#")
	if hash < 0 {
		return nil, &ErrExternalLinkShape{Value: s, Reason: "expected <family>/<repository>#<issue>"}
	}
	repoPart, issuePart := s[:hash], s[hash+1:]
	slash := strings.Index(repoPart, "/")
	if slash <= 0 {
		if slash == 0 {
			return nil, &ErrExternalLinkShape{Value: s, Reason: "family is empty"}
		}
		return nil, &ErrExternalLinkShape{Value: s, Reason: "expected <family>/<repository>#<issue>"}
	}
	issue, err := parseExternalLinkIssue(issuePart)
	if err != nil {
		return nil, &ErrExternalLinkShape{Value: s, Reason: err.Error()}
	}
	link := &ExternalLink{Family: repoPart[:slash], Repository: repoPart[slash+1:], Issue: issue}
	if err := ValidateExternalLink(link); err != nil {
		return nil, err
	}
	return link, nil
}

// parseExternalLinkIssue parses the issue member grammar: digits only, no
// leading zero, a positive value below 2^63.
func parseExternalLinkIssue(s string) (int64, error) {
	if s == "" {
		return 0, fmt.Errorf("issue is empty")
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("issue %q has a leading zero", s)
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("issue %q is not a decimal integer", s)
		}
	}
	if len(s) > 19 {
		return 0, fmt.Errorf("issue %s does not fit a 64-bit integer", s)
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("issue %s does not fit a 64-bit integer", s)
	}
	if v <= 0 {
		return 0, fmt.Errorf("issue must be positive")
	}
	return v, nil
}

// ValidateExternalLink enforces the §1 component grammar on a parsed link.
func ValidateExternalLink(l *ExternalLink) error {
	if l == nil {
		return &ErrExternalLinkShape{Value: "", Reason: "link is empty"}
	}
	if err := validateExternalLinkFamily(l.Family); err != nil {
		return err
	}
	if err := validateExternalLinkRepository(l.Repository); err != nil {
		return err
	}
	// The upper bound is enforced at the parse site (parseExternalLinkIssue),
	// keeping the issue inside the store's signed 64-bit column.
	if l.Issue <= 0 {
		return &ErrExternalLinkShape{Value: l.String(), Reason: "issue must be a positive integer below 2^63"}
	}
	return nil
}

func validateExternalLinkFamily(family string) error {
	if family == "" {
		return &ErrExternalLinkShape{Value: family, Reason: "family is empty"}
	}
	if len(family) > 64 {
		return &ErrExternalLinkShape{Value: family, Reason: "family exceeds 64 bytes"}
	}
	if family[0] == '-' {
		return &ErrExternalLinkShape{Value: family, Reason: "family begins with '-'"}
	}
	for i := range len(family) {
		c := family[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '.' || c == '_' || c == '-' {
			continue
		}
		return &ErrExternalLinkShape{Value: family, Reason: fmt.Sprintf("family contains %q", string(rune(c)))}
	}
	return nil
}

func validateExternalLinkRepository(repo string) error {
	if repo == "" {
		return &ErrExternalLinkShape{Value: repo, Reason: "repository is empty"}
	}
	if len(repo) > 256 {
		return &ErrExternalLinkShape{Value: repo, Reason: "repository exceeds 256 bytes"}
	}
	if !utf8Valid(repo) {
		return &ErrExternalLinkShape{Value: repo, Reason: "repository is not valid UTF-8"}
	}
	for _, r := range repo {
		if r == '#' {
			return &ErrExternalLinkShape{Value: repo, Reason: "repository contains '#'"}
		}
		if unicode.IsSpace(r) {
			return &ErrExternalLinkShape{Value: repo, Reason: "repository contains whitespace"}
		}
		if r < 0x20 || r == 0x7f {
			return &ErrExternalLinkShape{Value: repo, Reason: "repository contains a control character"}
		}
	}
	return nil
}

// DecodeExternalLink decodes one `externalLink` member value strictly: an
// object of exactly the three lower-case members family, repository, and
// issue, each a string appearing at most once — encoding/json's
// case-insensitive member matching and duplicate-key last-wins behavior
// are both refused. A value that is not §1-shaped — not an object, a
// missing, mistyped, duplicated, unknown, or wrongly-cased member, or a
// grammar violation — is an ErrExternalLinkShape; the caller keeps the
// raw bytes so the document still round-trips.
func DecodeExternalLink(raw json.RawMessage) (*ExternalLink, error) {
	shape := func(reason string) (*ExternalLink, error) {
		return nil, &ErrExternalLinkShape{Value: compactValue(raw), Reason: reason}
	}
	trimmed := bytes.TrimSpace(raw)
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return shape("not an object of family, repository, issue")
	}
	var family, repository, issue string
	seen := map[string]bool{}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return shape("not an object of family, repository, issue")
		}
		key, ok := keyTok.(string)
		if !ok {
			return shape("not an object of family, repository, issue")
		}
		if key != "family" && key != "repository" && key != "issue" {
			return shape(fmt.Sprintf("unknown member %q", key))
		}
		if seen[key] {
			return shape(fmt.Sprintf("duplicate member %q", key))
		}
		seen[key] = true
		valTok, err := dec.Token()
		if err != nil {
			return shape("not an object of family, repository, issue")
		}
		val, ok := valTok.(string)
		if !ok {
			return shape(fmt.Sprintf("member %q is not a string", key))
		}
		switch key {
		case "family":
			family = val
		case "repository":
			repository = val
		default:
			issue = val
		}
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return shape("not an object of family, repository, issue")
	}
	if _, err := dec.Token(); err != io.EOF { // no trailing content
		return shape("not an object of family, repository, issue")
	}
	if !seen["family"] || !seen["repository"] || !seen["issue"] {
		return shape("family, repository, and issue are all required")
	}
	parsedIssue, err := parseExternalLinkIssue(issue)
	if err != nil {
		return shape(err.Error())
	}
	link := &ExternalLink{Family: family, Repository: repository, Issue: parsedIssue}
	if err := ValidateExternalLink(link); err != nil {
		return nil, err
	}
	return link, nil
}

// MarshalExternalLink renders the canonical §1 member value.
func MarshalExternalLink(l ExternalLink) json.RawMessage {
	var b strings.Builder
	b.WriteString(`{"family":`)
	b.Write(MarshalString(l.Family))
	b.WriteString(`,"repository":`)
	b.Write(MarshalString(l.Repository))
	b.WriteString(`,"issue":"`)
	b.WriteString(strconv.FormatInt(l.Issue, 10))
	b.WriteString(`"}`)
	return json.RawMessage(b.String())
}

// ErrExternalLinkShape reports a link value or flag argument that violates
// the §1 grammar (a usage error, exit 2, at the CLI; a verify finding when
// read from a document).
type ErrExternalLinkShape struct {
	Value  string
	Reason string
}

func (e *ErrExternalLinkShape) Error() string {
	return fmt.Sprintf("invalid external link %q: %s", e.Value, e.Reason)
}

// ErrLinkSet reports a set-link onto a task that already carries any link
// (a typed conflict, exit 3: the field is set-once and has no clear form).
type ErrLinkSet struct {
	Tag     string
	ID      string
	Current ExternalLink
}

func (e *ErrLinkSet) Error() string {
	return fmt.Sprintf("task %s in tag %q already carries external link %s; a link is set-once",
		e.ID, e.Tag, e.Current.String())
}

// ErrLinkExists reports a link triple already carried by another task
// (a typed conflict, exit 3).
type ErrLinkExists struct {
	Link        ExternalLink
	ExistingTag string
	ExistingID  string
}

func (e *ErrLinkExists) Error() string {
	return fmt.Sprintf("external link %s is already carried by task %s in tag %q",
		e.Link.String(), e.ExistingID, e.ExistingTag)
}

// FindTaskByLink returns the tag and id of the task carrying link's triple
// anywhere in the document, mirroring the store-wide uniqueness scope.
func (f *File) FindTaskByLink(link ExternalLink) (tag, id string, ok bool) {
	for _, tag := range f.tags {
		for _, tr := range f.blocks[tag].tasks {
			if tr.task.Link != nil && tr.task.Link.Equal(link) {
				return tag, tr.task.ID, true
			}
		}
	}
	return "", "", false
}

// TaskHasExternalLinkMember reports whether the task's serialized object
// carries any `externalLink` member, valid or not. Presence — not validity
// — blocks a set-link: a malformed value is never repaired or replaced.
func (f *File) TaskHasExternalLinkMember(tag, id string) (bool, error) {
	idx, err := f.TaskIndex(tag, id)
	if err != nil {
		return false, err
	}
	obj, err := decodeOrderedObject(f.blocks[tag].tasks[idx].raw)
	if err != nil {
		return false, err
	}
	_, present := obj.values["externalLink"]
	return present, nil
}

// SetTaskLink assigns the task's set-once external link. The link grammar
// is validated first; a task that already carries any link — or a document
// where another task carries the same triple — refuses with a typed
// conflict (ErrLinkSet, ErrLinkExists). The member is spliced in after
// milestone when present, else after priority, else after status; every
// other byte of the task object stays identical.
func (f *File) SetTaskLink(tag, id string, link *ExternalLink) error {
	if err := ValidateExternalLink(link); err != nil {
		return err
	}
	if _, err := f.Task(tag, id); err != nil {
		return err
	}
	present, err := f.TaskHasExternalLinkMember(tag, id)
	if err != nil {
		return err
	}
	if present {
		t, _ := f.Task(tag, id)
		current := ExternalLink{}
		if t.Link != nil {
			current = *t.Link
		}
		return &ErrLinkSet{Tag: tag, ID: id, Current: current}
	}
	if existingTag, existingID, ok := f.FindTaskByLink(*link); ok {
		return &ErrLinkExists{Link: *link, ExistingTag: existingTag, ExistingID: existingID}
	}
	idx, err := f.TaskIndex(tag, id)
	if err != nil {
		return err
	}
	blk := f.blocks[tag]
	newRaw, err := AssignExternalLink(blk.tasks[idx].raw, *link)
	if err != nil {
		return err
	}
	return f.commitTaskEdit(tag, blk, blk.tasks[idx], newRaw, time.Now().UTC().Format(time.RFC3339))
}

// AssignExternalLink sets the externalLink member on a serialized §1 task
// object, keeping MarshalTask's key order (after milestone, else priority,
// else status) and splicing only the new member's bytes.
func AssignExternalLink(raw json.RawMessage, link ExternalLink) (json.RawMessage, error) {
	anchor := "status"
	obj, err := decodeOrderedObject(raw)
	if err != nil {
		return nil, err
	}
	if _, has := obj.values["priority"]; has {
		anchor = "priority"
	}
	if _, has := obj.values["milestone"]; has {
		anchor = "milestone"
	}
	return InsertObjectMember(raw, anchor, "externalLink", MarshalExternalLink(link))
}

func compactValue(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return b.String()
}
