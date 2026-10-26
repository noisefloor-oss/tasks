package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// §8.9 worktree hygiene: the read-only inventory of every Git-registered
// worktree of the family the engine does not own, plus the primary
// checkout's branch state. It enumerates `git worktree list --porcelain`
// rather than scanning directory shapes, so review checkouts outside the
// owner boundary's .worktrees root are reported too. Nothing here mutates:
// no worktree is pruned, removed, or checked out.

// HygieneWorktreeRow is one §8.9 unowned registered-worktree row: a
// Git-registered worktree no worktree saga row names. Branch is the short
// branch name, empty exactly when Detached is true; Head is the commit the
// worktree has checked out when Git reported it, else empty. Exists is
// false when the registered path is gone from the filesystem; Dirty is
// judged only for an existing worktree and is false otherwise.
type HygieneWorktreeRow struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Head     string `json:"head"`
	Detached bool   `json:"detached"`
	Exists   bool   `json:"exists"`
	Dirty    bool   `json:"dirty"`
}

// HygienePrimaryRow is the §8.9 primary-checkout state: the family's main
// worktree as Git lists it first. DefaultBranch is the default branch
// resolved from origin/HEAD, falling back to main. Flagged is true exactly
// when the primary is detached or on a branch other than the default.
type HygienePrimaryRow struct {
	Path          string `json:"path"`
	Branch        string `json:"branch"`
	Detached      bool   `json:"detached"`
	DefaultBranch string `json:"defaultBranch"`
	Flagged       bool   `json:"flagged"`
	Dirty         bool   `json:"dirty"`
}

// registeredWorktree is one parsed `git worktree list --porcelain` record.
type registeredWorktree struct {
	path     string
	head     string
	branch   string // full ref, e.g. refs/heads/main
	detached bool
	bare     bool
}

// scanHygieneWorktrees builds the §8.9 worktree inventory for the owner
// boundary from the family's saga rows: the unowned-worktree rows and
// the primary checkout's state, from one registered-worktree listing.
func scanHygieneWorktrees(ps *projectStore) ([]HygieneWorktreeRow, HygienePrimaryRow, error) {
	sagaPaths, err := ps.db.WorktreeSagaPaths()
	if err != nil {
		return nil, HygienePrimaryRow{}, err
	}
	worktrees, err := listRegisteredWorktrees(ps.boundary)
	if err != nil {
		return nil, HygienePrimaryRow{}, err
	}
	if len(worktrees) == 0 {
		return nil, HygienePrimaryRow{}, errors.New("git returned no worktrees")
	}
	return unboundWorktreeRows(worktrees, sagaPaths), hygienePrimaryRow(worktrees[0]), nil
}

// unboundWorktreeRows lists, bytewise sorted by path, every registered
// worktree no saga row owns — the primary checkout (the main worktree
// Git lists first) excluded, bare worktrees skipped. A saga row's path
// is ownership evidence whatever its status, terminal rows included,
// mirroring the §9.4 unknown-entry judgment §8.9's unowned-worktree
// report and §2.4's doctor finding both reuse; filesystem or Git shape
// alone never proves ownership, so nothing is ever removed here.
func unboundWorktreeRows(worktrees []registeredWorktree, sagaPaths []string) []HygieneWorktreeRow {
	owned := make(map[string]bool)
	for _, p := range sagaPaths {
		for _, s := range pathSpellings(p) {
			owned[s] = true
		}
	}
	rows := []HygieneWorktreeRow{}
	for _, wt := range worktrees[1:] {
		if wt.bare {
			continue
		}
		known := false
		for _, s := range pathSpellings(wt.path) {
			if owned[s] {
				known = true
				break
			}
		}
		if known {
			continue
		}
		row := HygieneWorktreeRow{
			Path:     wt.path,
			Branch:   shortBranch(wt.branch),
			Head:     wt.head,
			Detached: wt.detached,
		}
		if !pathMissing(wt.path) {
			row.Exists = true
			row.Dirty = worktreeDirty(wt.path)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	return rows
}

// hygienePrimaryRow projects the family's primary checkout (the main
// worktree Git lists first): its branch state against the family's
// default branch and its dirtiness.
func hygienePrimaryRow(primary registeredWorktree) HygienePrimaryRow {
	defaultBranch := resolveDefaultBranch(primary.path)
	row := HygienePrimaryRow{
		Path:          primary.path,
		Branch:        shortBranch(primary.branch),
		Detached:      primary.detached,
		DefaultBranch: defaultBranch,
		Dirty:         worktreeDirty(primary.path),
	}
	row.Flagged = primary.detached || row.Branch != defaultBranch
	return row
}

// listRegisteredWorktrees parses `git worktree list --porcelain -z` from
// the boundary: every worktree Git has registered, main first. Unknown
// attributes are ignored; a missing `worktree` header is malformed.
func listRegisteredWorktrees(boundary string) ([]registeredWorktree, error) {
	out, err := gitRun(boundary, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %s", err)
	}
	var worktrees []registeredWorktree
	var current *registeredWorktree
	for _, field := range bytes.Split(out, []byte{0}) {
		line := string(field)
		if line == "" {
			current = nil
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			path := strings.TrimPrefix(line, "worktree ")
			if path == "" {
				return nil, errors.New("git returned malformed worktree porcelain")
			}
			worktrees = append(worktrees, registeredWorktree{path: path})
			current = &worktrees[len(worktrees)-1]
		case current == nil:
			return nil, errors.New("git returned malformed worktree porcelain")
		case strings.HasPrefix(line, "HEAD "):
			current.head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.branch = strings.TrimPrefix(line, "branch ")
		case line == "detached":
			current.detached = true
		case line == "bare":
			current.bare = true
		}
	}
	return worktrees, nil
}

// resolveDefaultBranch resolves the family's default branch from
// origin/HEAD, falling back to main when no remote HEAD is recorded.
func resolveDefaultBranch(primaryPath string) string {
	out, err := gitRun(primaryPath, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		if v := strings.TrimSpace(string(out)); v != "" {
			return v
		}
	}
	return "main"
}

// worktreeDirty reports whether the worktree at path has local changes.
// --no-optional-locks keeps the read strictly report-only: Git takes no
// index.lock and never refreshes or rewrites an index hygiene does not
// own, and -c core.fsmonitor=false stops Git from executing a
// repo-configured fsmonitor helper — a walked family's config is never
// trusted enough to run its code. A tree Git cannot examine is reported
// clean, never fatal and never guessed.
func worktreeDirty(path string) bool {
	out, err := gitRun(path, "-c", "core.fsmonitor=false", "--no-optional-locks", "status", "--porcelain")
	if err != nil {
		return false
	}
	return len(bytes.TrimSpace(out)) > 0
}

// pathMissing reports whether the path is confirmed gone: only
// fs.ErrNotExist is missing. Any other stat failure (EACCES, say) leaves
// the path existing-but-unexaminable, judged by Git, never guessed here.
func pathMissing(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, fs.ErrNotExist)
}

// shortBranch shortens a full branch ref; a non-ref value is returned as is.
func shortBranch(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

// pathSpellings returns the path memberships ownership compares: the
// cleaned spelling plus a canonical spelling that resolves only the
// directory components, mirroring the store layer's known-path judgment.
func pathSpellings(p string) []string {
	clean := filepath.Clean(p)
	out := []string{clean}
	dir, base := filepath.Split(clean)
	if dir == "" {
		return out
	}
	if resolved, err := filepath.EvalSymlinks(filepath.Clean(dir)); err == nil {
		out = append(out, filepath.Join(resolved, base))
	}
	return out
}
