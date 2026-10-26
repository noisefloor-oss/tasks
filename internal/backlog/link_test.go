package backlog

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseExternalLinkTextGrammar(t *testing.T) {
	valid := []struct{ in, want string }{
		{"github/acme/widgets#214", "github/acme/widgets#214"},
		{"github/a/b/c#1", "github/a/b/c#1"},
		{"git.example_2/x-y/z.1#9223372036854775807", "git.example_2/x-y/z.1#9223372036854775807"},
	}
	for _, tt := range valid {
		link, err := ParseExternalLinkText(tt.in)
		if err != nil || link.String() != tt.want {
			t.Errorf("ParseExternalLinkText(%q) = %v, %v; want %q", tt.in, link, err, tt.want)
		}
	}
	// The §1 family bound is inclusive at 64 bytes.
	family64 := strings.Repeat("f", 64)
	if link, err := ParseExternalLinkText(family64 + "/acme#1"); err != nil {
		t.Errorf("64-byte family rejected: %v", err)
	} else if link.Family != family64 {
		t.Errorf("64-byte family = %q", link.Family)
	}
	if _, err := ParseExternalLinkText(strings.Repeat("f", 65) + "/acme#1"); err == nil {
		t.Error("65-byte family accepted")
	}
	if err := ValidateExternalLink(&ExternalLink{Family: strings.Repeat("f", 5000), Repository: "a/b", Issue: 1}); err == nil {
		t.Error("5000-byte family accepted")
	}
	invalid := []string{
		"", "github", "github/acme", "github#12", "/acme#12", "github/#12",
		"-github/acme#12", "git hub/acme#12", "github/acme#0",
		"github/acme#012", "github/acme#-1", "github/acme#1a", "github/acme#",
		"github/acme#99999999999999999999", "github/a#c#1",
		// Whitespace is refused in the Unicode sense, not just ASCII.
		"github/ac\u00a0me#12", "github/acme\u2028x#12",
	}
	for _, in := range invalid {
		if link, err := ParseExternalLinkText(in); err == nil {
			t.Errorf("ParseExternalLinkText(%q) = %v; want error", in, link)
		}
	}
}

func writeFixture(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const twoTaskFixture = `{
  "main": {
    "tasks": [
      {
        "id": "1",
        "title": "first",
        "description": "",
        "details": "",
        "status": "pending",
        "dependencies": [],
        "subtasks": [],
        "updatedAt": "2026-01-01T00:00:00Z"
      },
      {
        "id": "2",
        "title": "second",
        "description": "",
        "details": "",
        "status": "pending",
        "dependencies": [],
        "subtasks": [],
        "updatedAt": "2026-01-01T00:00:00Z"
      }
    ],
    "metadata": {
      "version": "1.0.0",
      "lastModified": "2026-01-01T00:00:00Z",
      "taskCount": 2,
      "completedCount": 0,
      "tags": ["main"]
    }
  }
}`

func TestFixtureSetTaskLinkSetOnceAndUniqueness(t *testing.T) {
	path := writeFixture(t, twoTaskFixture)
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	link := ExternalLink{Family: "github", Repository: "acme/widgets", Issue: 214}
	if err := f.SetTaskLink("main", "1", &link); err != nil {
		t.Fatalf("SetTaskLink: %v", err)
	}
	// The member renders after updatedAt? No: after milestone/priority,
	// before dependencies — verify canonical member and position.
	raw := f.blocks["main"].tasks[0].raw
	if !strings.Contains(string(raw), `"externalLink": {"family":"github","repository":"acme/widgets","issue":"214"}`) {
		t.Fatalf("rendered task = %s", raw)
	}
	// Set-once refusal, even for the identical value.
	err = f.SetTaskLink("main", "1", &link)
	var once *ErrLinkSet
	if !errors.As(err, &once) {
		t.Fatalf("second set = %v; want ErrLinkSet", err)
	}
	// Duplicate triple on another task refuses naming the holder.
	err = f.SetTaskLink("main", "2", &link)
	var exists *ErrLinkExists
	if !errors.As(err, &exists) || exists.ExistingID != "1" {
		t.Fatalf("duplicate set = %v; want ErrLinkExists for task 1", err)
	}
	// A distinct triple assigns.
	other := ExternalLink{Family: "github", Repository: "acme/widgets", Issue: 215}
	if err := f.SetTaskLink("main", "2", &other); err != nil {
		t.Fatalf("distinct set: %v", err)
	}
	// FindTaskByLink resolves the document-wide holder.
	if _, id, ok := f.FindTaskByLink(other); !ok || id != "2" {
		t.Fatalf("FindTaskByLink = %s, %v", id, ok)
	}
}

func TestFixtureLinkRoundTripAndReplay(t *testing.T) {
	path := writeFixture(t, twoTaskFixture)
	f, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	link := ExternalLink{Family: "github", Repository: "acme/widgets", Issue: 214}
	if err := f.SetTaskLink("main", "1", &link); err != nil {
		t.Fatal(err)
	}
	if err := f.Write(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := Parse(data)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	got, err := f2.Task("main", "1")
	if err != nil || got.Link == nil || got.Link.String() != link.String() {
		t.Fatalf("round-tripped link = %+v, %v", got.Link, err)
	}
	// A status splice keeps the member byte-identical.
	updates := map[string]json.RawMessage{"status": json.RawMessage(`"done"`)}
	newRaw, err := UpdateTaskObject(f2.blocks["main"].tasks[0].raw, updates)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(newRaw), `"externalLink": {"family":"github","repository":"acme/widgets","issue":"214"}`) {
		t.Fatalf("spliced task lost link: %s", newRaw)
	}
	// MarshalTask renders the member for new tasks.
	rendered := MarshalTask(Task{ID: "3", Title: "t", Status: "pending", Link: &link})
	if !strings.Contains(string(rendered), `"externalLink"`) {
		t.Fatalf("MarshalTask dropped link: %s", rendered)
	}
}

func TestDecodeExternalLinkMemberStrict(t *testing.T) {
	bad := []string{
		`null`, `"github/a#1"`, `{"family":"github"}`, `{"family":"github","repository":"a"}`,
		`{"family":"github","repository":"a","issue":"01"}`,
		`{"family":"github","repository":"a","issue":12}`,
		`{"family":"github","repository":"a","issue":"1","extra":1}`,
		`{"family":"git hub","repository":"a","issue":"1"}`,
		// Strict member names: case-insensitive matching and duplicate
		// keys (encoding/json's last-wins) are both shape errors.
		`{"Family":"github","REPOSITORY":"a/b","Issue":"1"}`,
		`{"family":"github","family":"gitlab","repository":"a/b","issue":"1"}`,
		`{"family":"github","repository":"a/b","repository":"c/d","issue":"1"}`,
		`{"family":"github","repository":"a/b","issue":"1","issue":"2"}`,
		`{"family":"github","repository":"a/b","issue":"1"} trailing`,
		`["family","repository","issue"]`,
	}
	for _, raw := range bad {
		if link, err := DecodeExternalLink(json.RawMessage(raw)); err == nil {
			t.Errorf("DecodeExternalLink(%s) = %v; want error", raw, link)
		}
	}
	good := `{"family":"github","repository":"acme/widgets","issue":"214"}`
	link, err := DecodeExternalLink(json.RawMessage(good))
	if err != nil || link.String() != "github/acme/widgets#214" {
		t.Fatalf("DecodeExternalLink(good) = %+v, %v", link, err)
	}
}
