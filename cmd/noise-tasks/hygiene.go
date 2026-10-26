package main

import (
	"fmt"
	"io"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdHygiene prints the §8.9 read-only lifecycle residue and worktree
// inventory report. It is authority-scoped: no --file or --tag exists, so
// an unknown flag is the ordinary usage error from flag parsing (exit 2).
func cmdHygiene(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("hygiene", stderr)
	jsonOutput := fs.Bool("json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks hygiene [--json]")
		return 2
	}
	ps, err := openProjectStore()
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	report, err := ps.db.Hygiene()
	if err != nil {
		return fail(stderr, err)
	}
	unowned, primary, err := scanHygieneWorktrees(ps)
	if err != nil {
		return fail(stderr, err)
	}
	out := &hygieneReportOut{
		SchemaVersion:       report.SchemaVersion,
		Now:                 report.Now,
		StaleRuns:           report.StaleRuns,
		ExpiredClaims:       report.ExpiredClaims,
		ClaimlessInProgress: report.ClaimlessInProgress,
		UnownedWorktrees:    unowned,
		PrimaryCheckout:     primary,
	}
	if *jsonOutput {
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	writeHygieneHuman(stdout, out)
	return 0
}

// hygieneReportOut is the exact §8.9 report: the residue classes the
// database judges plus the Git-registered worktree inventory and the
// primary checkout's state. The arrays are always non-nil so JSON renders
// empty classes as [], never null.
type hygieneReportOut struct {
	SchemaVersion       int                          `json:"schemaVersion"`
	Now                 string                       `json:"now"`
	StaleRuns           []store.HygieneStaleRun      `json:"staleRuns"`
	ExpiredClaims       []store.HygieneExpiredClaim  `json:"expiredClaims"`
	ClaimlessInProgress []store.HygieneClaimlessTask `json:"claimlessInProgress"`
	UnownedWorktrees    []HygieneWorktreeRow         `json:"unownedWorktrees"`
	PrimaryCheckout     HygienePrimaryRow            `json:"primaryCheckout"`
}

// writeHygieneHuman renders the §8.9 human report: one line per finding,
// grouped by class, with a summary line.
func writeHygieneHuman(w io.Writer, r *hygieneReportOut) {
	total := 0
	for _, row := range r.StaleRuns {
		total++
		fmt.Fprintf(w, "stale run %d %s/%s owner %s attempt %d expired %s\n",
			row.RunID, row.Tag, row.Task, row.Owner, row.Attempt, row.AttemptExpiresAt)
	}
	for _, row := range r.ExpiredClaims {
		total++
		fmt.Fprintf(w, "expired claim %s/%s owner %s expired %s\n",
			row.Tag, row.Task, row.Owner, row.ExpiredAt)
	}
	for _, row := range r.ClaimlessInProgress {
		total++
		fmt.Fprintf(w, "in-progress without live claim %s/%s updated %s\n",
			row.Tag, row.Task, row.UpdatedAt)
	}
	for _, row := range r.UnownedWorktrees {
		total++
		fmt.Fprintf(w, "unowned worktree %s %s %s%s\n", row.Path,
			worktreeStateLabel(row.Branch, row.Detached, row.Head),
			existsLabel(row.Exists), dirtyLabel(row.Dirty, row.Exists))
	}
	primaryFindings := 0
	if r.PrimaryCheckout.Flagged {
		primaryFindings = 1
		total++
		fmt.Fprintf(w, "primary checkout %s %s (default %s)%s\n", r.PrimaryCheckout.Path,
			worktreeStateLabel(r.PrimaryCheckout.Branch, r.PrimaryCheckout.Detached, ""),
			r.PrimaryCheckout.DefaultBranch, dirtyLabel(r.PrimaryCheckout.Dirty, true))
	}
	if total == 0 {
		fmt.Fprintln(w, "no lifecycle residue found")
		return
	}
	fmt.Fprintf(w, "%d lifecycle finding(s): %d stale run(s), %d expired claim(s), %d in-progress without live claim, %d unowned registered worktree(s), %d primary checkout finding(s)\n",
		total, len(r.StaleRuns), len(r.ExpiredClaims), len(r.ClaimlessInProgress),
		len(r.UnownedWorktrees), primaryFindings)
}

// worktreeStateLabel renders one worktree's checkout state: the short
// branch name, or the detached head when detached.
func worktreeStateLabel(branch string, detached bool, head string) string {
	if detached {
		if head != "" {
			return "detached at " + head
		}
		return "detached"
	}
	return "branch " + branch
}

// existsLabel renders the path-existence marker for an unowned worktree.
func existsLabel(exists bool) string {
	if exists {
		return "exists"
	}
	return "missing"
}

// dirtyLabel renders the dirty marker; a missing path is never dirty.
func dirtyLabel(dirty, exists bool) string {
	if exists && dirty {
		return " dirty"
	}
	return ""
}
