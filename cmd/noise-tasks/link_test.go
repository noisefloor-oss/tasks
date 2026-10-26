package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

func TestCLISetLinkAndAddLinkLifecycle(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "first", "second")

	// Grammar violations are usage errors (exit 2) with empty stdout.
	for _, bad := range []string{"github/acme", "github/acme#", "acme#12", "github/acme#01"} {
		code, stdout, _ := runCmd(cmdSetLink, "--revision", "1", "1", bad)
		if code != 2 || stdout != "" {
			t.Fatalf("set-link %q exit %d stdout %q", bad, code, stdout)
		}
	}
	// Subtask ids are usage errors.
	if code, _, _ := runCmd(cmdSetLink, "--revision", "1", "1.1", "github/a/b#1"); code != 2 {
		t.Fatalf("subtask set-link exit %d", code)
	}

	// Happy path: assignment bumps the revision and renders in show.
	code, stdout, stderr := runCmd(cmdSetLink, "--revision", "1", "1", "github/acme/widgets#214")
	if code != 0 {
		t.Fatalf("set-link exit %d: %s", code, stderr)
	}
	if stdout != "task 1: external link github/acme/widgets#214\n" {
		t.Fatalf("set-link stdout = %q", stdout)
	}
	code, stdout, _ = runCmd(cmdShow, "--json", "1")
	if code != 0 || !strings.Contains(stdout, `"externalLink": {
    "family": "github",
    "repository": "acme/widgets",
    "issue": "214"
  }`) {
		t.Fatalf("show --json = %q", stdout)
	}
	code, stdout, _ = runCmd(cmdShow, "1")
	if code != 0 || !strings.Contains(stdout, "externalLink: github/acme/widgets#214\n") {
		t.Fatalf("show stdout = %q", stdout)
	}

	// Set-once refusal is a typed conflict (exit 3, empty stdout).
	code, stdout, stderr = runCmd(cmdSetLink, "--revision", "2", "1", "github/acme/widgets#214")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "set-once") {
		t.Fatalf("set-once exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// Duplicate triple on another task names the holder.
	code, stdout, stderr = runCmd(cmdSetLink, "--revision", "1", "2", "github/acme/widgets#214")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "task 1") {
		t.Fatalf("duplicate exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A distinct triple assigns on the second task, then a stale
	// revision keeps the ordinary CAS conflict (checked before set-once).
	code, stdout, stderr = runCmd(cmdSetLink, "--revision", "1", "2", "github/acme/other#1")
	if code != 0 {
		t.Fatalf("distinct set-link exit %d: %s", code, stderr)
	}
	code, stdout, stderr = runCmd(cmdSetLink, "--revision", "1", "2", "github/acme/another#2")
	if code != 3 || stdout != "" || !strings.Contains(stderr, "stale write") {
		t.Fatalf("stale exit %d stdout %q stderr %q", code, stdout, stderr)
	}

	// add --link replays the existing task instead of duplicating.
	code, stdout, stderr = runCmd(cmdAdd, "--title", "duplicate issue", "--link", "github/acme/widgets#214")
	if code != 0 || strings.TrimSpace(stdout) != "1" {
		t.Fatalf("replay add exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// A distinct link creates the task.
	code, stdout, stderr = runCmd(cmdAdd, "--title", "new issue", "--link", "github/acme/widgets#215")
	if code != 0 || strings.TrimSpace(stdout) != "3" {
		t.Fatalf("distinct add exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// Invalid link grammar refuses before any write.
	if code, stdout, _ := runCmd(cmdAdd, "--title", "bad", "--link", "github/acme/widgets#0"); code != 2 || stdout != "" {
		t.Fatalf("invalid add exit %d stdout %q", code, stdout)
	}
}

func TestCLIShowJSONExternalLinkNullWhenUnset(t *testing.T) {
	dir := chdirGitDir(t)
	seedProjectStore(t, dir, "main", "first")
	code, stdout, _ := runCmd(cmdShow, "--json", "1")
	if code != 0 {
		t.Fatalf("show exit %d", code)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if v, present := out["externalLink"]; !present || v != nil {
		t.Fatalf("unset externalLink = %v, present=%v", v, present)
	}
}

func TestCLIVerifyLinkFindingsOnFixture(t *testing.T) {
	dir := chdirGitDir(t)
	path := dir + "/fixture.json"
	bad := strings.Replace(twoTaskFixtureDoc, `"title": "second"`,
		`"title": "second",
        "externalLink": {"family": "github", "repository": "a b", "issue": "12"}`, 1)
	bad = strings.Replace(bad, `"title": "first"`,
		`"title": "first",
        "externalLink": {"family":"github","repository":"acme/widgets","issue":"214"}`, 1)
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := runCmd(verifyMain, "--file", path)
	if code != 1 {
		t.Fatalf("verify exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "invalid external link") || !strings.Contains(stdout, `repository contains whitespace`) {
		t.Fatalf("verify missing invalid-link: %q", stdout)
	}

	// A duplicate triple is a duplicate-link finding.
	dup := strings.Replace(twoTaskFixtureDoc, `"title": "second"`,
		`"title": "second",
        "externalLink": {"family":"github","repository":"acme/widgets","issue":"214"}`, 1)
	dup = strings.Replace(dup, `"title": "first"`,
		`"title": "first",
        "externalLink": {"family":"github","repository":"acme/widgets","issue":"214"}`, 1)
	if err := os.WriteFile(dir+"/dup.json", []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, _ = runCmd(verifyMain, "--file", dir+"/dup.json")
	if code != 1 || !strings.Contains(stdout, "duplicate external link") {
		t.Fatalf("verify duplicate exit %d stdout %q", code, stdout)
	}
}

func TestCLIFixtureSetLinkSurface(t *testing.T) {
	dir := chdirGitDir(t)
	path := dir + "/fixture.json"
	if err := os.WriteFile(path, []byte(twoTaskFixtureDoc), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCmd(cmdSetLink, "--file", path, "1", "github/acme/widgets#214"); code != 0 {
		t.Fatalf("fixture set-link exit %d: %s", code, stderr)
	}
	// Set-once refusal without --revision on the fixture surface.
	if code, stdout, stderr := runCmd(cmdSetLink, "--file", path, "1", "github/other/x#1"); code != 3 || stdout != "" || !strings.Contains(stderr, "set-once") {
		t.Fatalf("fixture set-once exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	// Replay: add --link returns the existing id and writes nothing.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if code, stdout, stderr := runCmd(cmdAdd, "--file", path, "--title", "dup", "--link", "github/acme/widgets#214"); code != 0 || strings.TrimSpace(stdout) != "1" {
		t.Fatalf("fixture replay add exit %d stdout %q stderr %q", code, stdout, stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("fixture replay rewrote the document")
	}
}

func TestCLIFixtureAddLinkCrossTagRefuses(t *testing.T) {
	dir := chdirGitDir(t)
	path := dir + "/twotags.json"
	doc := twoTaskFixtureDoc[:len(twoTaskFixtureDoc)-1] + `,
  "other": {
    "tasks": [
      {
        "id": "1",
        "title": "elsewhere",
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
      "taskCount": 1,
      "completedCount": 0,
      "tags": ["other"]
    }
  }
}`
	doc = strings.Replace(doc, `"title": "first"`,
		`"title": "first",
        "externalLink": {"family":"github","repository":"acme/widgets","issue":"214"}`, 1)
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	// The holder lives in main; adding to other must refuse naming it,
	// never return main's id as if it were other's.
	code, stdout, stderr := runCmd(cmdAdd, "--file", path, "--tag", "other",
		"--title", "wrong tag", "--link", "github/acme/widgets#214")
	if code != 3 || stdout != "" || !strings.Contains(stderr, `task 1 in tag "main"`) {
		t.Fatalf("cross-tag add exit %d stdout %q stderr %q", code, stdout, stderr)
	}
}

func TestReportLinkErrorMapsStoreConflicts(t *testing.T) {
	var stderr strings.Builder
	code := reportLinkError(&stderr, false, &store.LinkExistsConflict{
		Link:        backlog.ExternalLink{Family: "github", Repository: "a/b", Issue: 1},
		ExistingTag: "main", ExistingID: "7",
	})
	if code != 3 || !strings.Contains(stderr.String(), "task 7 in tag") {
		t.Fatalf("link-exists mapping exit %d stderr %q", code, stderr.String())
	}
	stderr.Reset()
	code = reportLinkError(&stderr, true, &store.LinkSetConflict{
		Tag: "main", ID: "7",
		Current: backlog.ExternalLink{Family: "github", Repository: "a/b", Issue: 1},
	})
	if code != 3 || !strings.Contains(stderr.String(), `"kind": "link-set"`) {
		t.Fatalf("link-set mapping exit %d stderr %q", code, stderr.String())
	}
}

const twoTaskFixtureDoc = `{
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
