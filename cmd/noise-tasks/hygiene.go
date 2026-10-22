package main

import (
	"fmt"
	"io"

	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdHygiene prints the §8.9 read-only lifecycle residue report. It is
// authority-scoped: no --file or --tag exists, so an unknown flag is the
// ordinary usage error from flag parsing (exit 2).
func cmdHygiene(args []string, stdout, stderr io.Writer) int {
	fs := newCommandFlagSet("hygiene", stderr)
	jsonOutput := fs.Bool("json", false, "machine-readable JSON on stdout")
	if code, ok := parseFlags(fs, args); !ok {
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
	if *jsonOutput {
		if err := writeJSON(stdout, report); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	writeHygieneHuman(stdout, report)
	return 0
}

// writeHygieneHuman renders the §8.9 human report: one line per finding,
// grouped by class, with a summary line.
func writeHygieneHuman(w io.Writer, r *store.HygieneReport) {
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
	if total == 0 {
		fmt.Fprintln(w, "no lifecycle residue found")
		return
	}
	fmt.Fprintf(w, "%d lifecycle residue finding(s): %d stale run(s), %d expired claim(s), %d in-progress without live claim\n",
		total, len(r.StaleRuns), len(r.ExpiredClaims), len(r.ClaimlessInProgress))
}
