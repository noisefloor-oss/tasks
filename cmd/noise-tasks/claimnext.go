package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// cmdClaimNext implements §8.3's claim-next: selection and acquisition in
// one immediate transaction. The store re-evaluates every dependency and
// filter under the write lock, so this command either reports a task it
// claimed in that transaction or the successful empty result — never a task
// another owner holds and never a claim conflict.
func cmdClaimNext(args []string, stdout, stderr io.Writer) int {
	var cf commonFlags
	fs := newFlagSet("claim-next", stderr, &cf)
	ownerF := fs.String("owner", "", "claim owner identity")
	ttlF := fs.Int("ttl", 0, "claim time-to-live in seconds")
	milestoneF := fs.String("milestone", "", "restrict candidates to this milestone")
	assignmentF := fs.String("assignment", "", "restrict candidates to tasks assigned to this owner")
	requestID := fs.String("request-id", "", "request id for §8.4 acquisition replay")
	if code, ok := parseFlags(fs, stdout, args); !ok {
		return code
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: noise-tasks claim-next [options]")
		return 2
	}
	if cf.file != "" {
		fmt.Fprintln(stderr, "noise-tasks claim-next: --file is not accepted for claim-next")
		return 2
	}
	if *ownerF == "" {
		fmt.Fprintln(stderr, "noise-tasks claim-next: --owner is required")
		return 2
	}
	// One owner-label grammar governs claim owners, claim-next owners and
	// its --assignment filter, and §9.2 plan owners (SPEC §8.3); it is
	// enforced before any store is opened.
	if err := store.ValidateOwnerLabel(*ownerF); err != nil {
		fmt.Fprintf(stderr, "noise-tasks claim-next: --owner %v\n", err)
		return 2
	}
	if *assignmentF != "" {
		if err := store.ValidateOwnerLabel(*assignmentF); err != nil {
			fmt.Fprintf(stderr, "noise-tasks claim-next: --assignment %v\n", err)
			return 2
		}
	}
	if *requestID == "" {
		fmt.Fprintln(stderr, "noise-tasks claim-next: --request-id is required")
		return 2
	}
	if !store.ValidLocalKey(*requestID) {
		fmt.Fprintf(stderr, "noise-tasks claim-next: --request-id must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}\n")
		return 2
	}
	if *milestoneF != "" {
		if err := backlog.ValidateMilestoneName(*milestoneF); err != nil {
			return reportMilestoneError(stderr, cf.json, err)
		}
	}

	ps, tag, err := resolveStoreTag(&cf)
	if err != nil {
		return fail(stderr, err)
	}
	defer ps.db.Close()
	// An unknown --milestone value is a usage error exactly as in next
	// (§12.2); a milestone removed after this check simply leaves no
	// candidates inside the transaction.
	entries, err := ps.db.MilestoneEntries(tag)
	if err != nil {
		return reportMilestoneError(stderr, cf.json, err)
	}
	if code := requireKnownMilestone(entries, *milestoneF, stderr); code != 0 {
		return code
	}

	task, claim, _, err := ps.db.ClaimNext(tag, *ownerF, *ttlF, *milestoneF, *assignmentF, *requestID)
	if err != nil {
		var rc *store.ReplayConflict
		if errors.As(err, &rc) {
			return writeLiveConflict(stderr, cf.json, replayConflictOut(rc), rc.Error())
		}
		var cc *store.ClaimConflict
		if errors.As(err, &cc) {
			return writeLiveConflict(stderr, cf.json, claimConflictOut(cc), cc.Error())
		}
		return fail(stderr, err)
	}
	if task == nil {
		// The successful empty result records no receipt: the request id is
		// not reserved without a committed effect (§8.3/§8.4).
		if cf.json {
			if err := writeJSON(stdout, nil); err != nil {
				return fail(stderr, err)
			}
			return 0
		}
		fmt.Fprintln(stdout, "no actionable task")
		return 0
	}
	if cf.json {
		out := struct {
			ID        string `json:"id"`
			Token     int64  `json:"token"`
			ExpiresAt string `json:"expiresAt"`
			Revision  int    `json:"revision"`
		}{task.ID, claim.Token, claim.ExpiresAt, task.Revision}
		if err := writeJSON(stdout, out); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "id=%s token=%d expiresAt=%s revision=%d\n", task.ID, claim.Token, claim.ExpiresAt, task.Revision)
	return 0
}
