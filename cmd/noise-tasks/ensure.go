package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/genai"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// ensure (SPEC §4): the documented happy path for issue-driven work —
// create the linked pending task for a repository issue the caller
// names, or return the existing task carrying that (family,
// repository, issue) triple. The caller supplies the issue's title and
// body; this engine does no network, and it never applies later edits
// or closure of the issue. Idempotency rides the link field's
// store-wide uniqueness exactly like add --link replay: same tag
// returns the existing id with no second write, no counter bump, and
// no event; a holder in another tag is the typed link conflict
// (exit 3). Concurrent callers race inside the same immediate
// transaction, so an importer and an agent racing on one issue get one
// task.
func cmdEnsure(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("ensure", stderr, &cf)
	repoF := fs.String("repo", "", "repository of the issue as <owner/name> (required)")
	issueF := fs.String("issue", "", "issue number (required)")
	familyF := fs.String("family", "github", "source repository family of --repo")
	titleF := fs.String("title", "", "issue title (required, one line, at most 200 bytes)")
	descF := fs.String("description", "", "issue body summary (one line, at most 500 bytes)")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "noise-tasks ensure: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *repoF == "" {
		fmt.Fprintln(stderr, "noise-tasks ensure: --repo is required")
		return 2
	}
	if *issueF == "" {
		fmt.Fprintln(stderr, "noise-tasks ensure: --issue is required")
		return 2
	}
	if strings.TrimSpace(*titleF) == "" {
		fmt.Fprintln(stderr, "noise-tasks ensure: --title is required")
		return 2
	}
	// The generated triple passes the exact §1 link grammar through the
	// canonical text form, so a grammar violation is the same typed
	// usage error as every --link-shaped flag.
	link, err := backlog.ParseExternalLinkText(*familyF + "/" + *repoF + "#" + *issueF)
	if err != nil {
		return reportLinkError(stderr, cf.json, err)
	}
	// Untrusted text from outside the store passes the same §4.0 machine
	// boundary as generate output: single-line title and description
	// within their byte bounds.
	if err := genai.ValidateTitle(*titleF); err != nil {
		fmt.Fprintf(stderr, "noise-tasks ensure: --title %s\n", err)
		return 2
	}
	if *descF != "" {
		if err := genai.ValidateDescription(*descF); err != nil {
			fmt.Fprintf(stderr, "noise-tasks ensure: --description %s\n", err)
			return 2
		}
	}

	var id, tag string
	var created bool
	if cf.file != "" {
		err := mutateFixture(&cf, func(f *backlog.File, t string) error {
			tag = t
			// A link already carried by a task of this tag makes ensure a
			// replay (SPEC §4): return the existing id and write nothing.
			// A holder in another tag is a typed conflict.
			if holderTag, existingID, ok := f.FindTaskByLink(*link); ok {
				if holderTag != t {
					return &backlog.ErrLinkExists{Link: *link, ExistingTag: holderTag, ExistingID: existingID}
				}
				id, created = existingID, false
				return nil
			}
			var err error
			id, err = f.AddTask(t, backlog.Task{
				Title:       *titleF,
				Description: *descF,
				Status:      "pending",
				Priority:    "medium",
				Link:        link,
			})
			created = err == nil
			return err
		})
		if err != nil {
			var linkExists *backlog.ErrLinkExists
			if errors.As(err, &linkExists) {
				return reportLinkError(stderr, cf.json, err)
			}
			return fail(stderr, err)
		}
	} else {
		ps, resolvedTag, err := resolveStoreTag(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		tag = resolvedTag
		id, created, err = ps.db.EnsureTask(tag, &store.Task{
			Title:       *titleF,
			Description: *descF,
			Status:      "pending",
			Priority:    "medium",
			Link:        link,
		})
		if err != nil {
			var linkExists *store.LinkExistsConflict
			if errors.As(err, &linkExists) {
				return reportLinkError(stderr, cf.json, err)
			}
			return fail(stderr, err)
		}
	}

	if cf.json {
		out := struct {
			ID      string `json:"id"`
			Tag     string `json:"tag"`
			Created bool   `json:"created"`
		}{id, tag, created}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	if created {
		fmt.Fprintf(stdout, "created task %s for %s\n", id, link.String())
		return 0
	}
	fmt.Fprintf(stdout, "task %s already tracks %s\n", id, link.String())
	return 0
}
