package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// set-link (SPEC §4): assigns a task's set-once external link. On the
// database path it is a §1 field mutation with full §8.2 CAS; on --file the
// same refusals apply without --revision.

func cmdSetLink(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("set-link", stderr, &cf)
	var lf liveFlags
	lf.register(fs)
	var rf taskMutationRunFlags
	rf.register(fs)
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 2 {
		fmt.Fprintln(stderr, "usage: noise-tasks set-link [options] <id> <family>/<repository>#<issue>")
		return 2
	}
	id, linkArg := fs.Arg(0), fs.Arg(1)
	if id == "" {
		fmt.Fprintln(stderr, "usage: noise-tasks set-link [options] <id> <family>/<repository>#<issue>")
		return 2
	}
	if _, subID, refErr := backlog.ParseTaskReference(id); refErr != nil || subID != "" {
		fmt.Fprintln(stderr, "noise-tasks set-link: subtask ids do not carry an external link")
		return 2
	}
	link, err := backlog.ParseExternalLinkText(linkArg)
	if err != nil {
		return reportLinkError(stderr, cf.json, err)
	}

	if cf.file != "" {
		if code := lf.rejectOnFixturePath(stderr, "set-link"); code >= 0 {
			return code
		}
		if code := rf.rejectOnFixturePath(fs, stderr, "set-link"); code >= 0 {
			return code
		}
		err := mutateFixture(&cf, func(f *backlog.File, tag string) error {
			return f.SetTaskLink(tag, id, link)
		})
		if err != nil {
			return reportLinkError(stderr, cf.json, err)
		}
	} else {
		gate, ok := taskMutationGate(fs, &lf, &rf, stderr, "set-link")
		if !ok {
			return 2
		}
		ps, tag, err := resolveStoreTag(&cf)
		if err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
		if !lf.requireRevision(stderr) {
			return 2
		}
		if err := ps.db.SetLink(tag, id, *link, lf.revision, gate); err != nil {
			var sw *store.StaleWriteConflict
			if errors.As(err, &sw) {
				return writeLiveConflict(stderr, cf.json, staleConflictOut(sw), sw.Error())
			}
			var cc *store.ClaimConflict
			if errors.As(err, &cc) {
				return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
			}
			var rc *store.RunConflict
			if errors.As(err, &rc) {
				return writeLiveConflict(stderr, cf.json, runConflictOut(rc), rc.Error())
			}
			return reportLinkError(stderr, cf.json, err)
		}
	}

	if cf.json {
		return writeLinkJSON(stdout, stderr, id, link)
	}
	fmt.Fprintf(stdout, "task %s: external link %s\n", id, link.String())
	return 0
}

// reportLinkError maps link errors to §6 exit codes: typed conflicts
// (set-once, duplicate triple) exit 3, grammar violations exit 2,
// everything else — including an unknown id, like set-milestone — exits 1.
func reportLinkError(stderr io.Writer, jsonOut bool, err error) int {
	var shape *backlog.ErrExternalLinkShape
	if errors.As(err, &shape) {
		fmt.Fprintf(stderr, "noise-tasks: %s\n", shape.Error())
		return 2
	}
	var set *store.LinkSetConflict
	if errors.As(err, &set) {
		return writeLiveConflict(stderr, jsonOut, linkSetConflictOut(set), set.Error())
	}
	var exists *store.LinkExistsConflict
	if errors.As(err, &exists) {
		return writeLiveConflict(stderr, jsonOut, linkExistsConflictOut(exists), exists.Error())
	}
	var fset *backlog.ErrLinkSet
	if errors.As(err, &fset) {
		out := &store.LinkSetConflict{Tag: fset.Tag, ID: fset.ID, Current: fset.Current}
		return writeLiveConflict(stderr, jsonOut, linkSetConflictOut(out), out.Error())
	}
	var fexists *backlog.ErrLinkExists
	if errors.As(err, &fexists) {
		out := &store.LinkExistsConflict{Link: fexists.Link, ExistingTag: fexists.ExistingTag, ExistingID: fexists.ExistingID}
		return writeLiveConflict(stderr, jsonOut, linkExistsConflictOut(out), out.Error())
	}
	return fail(stderr, err)
}

func linkSetConflictOut(e *store.LinkSetConflict) any {
	return struct {
		Kind    string          `json:"kind"`
		Tag     string          `json:"tag"`
		Task    string          `json:"task"`
		Current externalLinkOut `json:"current"`
	}{"link-set", e.Tag, e.ID, toExternalLinkOut(&e.Current)}
}

func linkExistsConflictOut(e *store.LinkExistsConflict) any {
	return struct {
		Kind        string          `json:"kind"`
		Link        externalLinkOut `json:"link"`
		ExistingTag string          `json:"existingTag"`
		ExistingID  string          `json:"existingTask"`
	}{"link-exists", toExternalLinkOut(&e.Link), e.ExistingTag, e.ExistingID}
}

func writeLinkJSON(stdout, stderr io.Writer, id string, link *backlog.ExternalLink) int {
	out := struct {
		ID           string           `json:"id"`
		ExternalLink *externalLinkOut `json:"externalLink"`
	}{id, toExternalLinkOutPtr(link)}
	if err := writeJSON(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}
