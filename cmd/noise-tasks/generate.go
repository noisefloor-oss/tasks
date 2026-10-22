package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/genai"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// generate (SPEC §4): generate tasks from a PRD via an LLM. The only
// networked command; the provider sits behind an interface so the
// command flow is fully testable without the API.

const maxPRDBytes = 1 << 20

func cmdGenerate(args []string, stdout, stderr io.Writer) int {
	return generateMain(args, os.Stdin, stdout, stderr, selectGeneration, newGenerationProvider)
}

// generateDraftOut is the machine-readable shape of one draft in the
// generate --json document. DraftDependencies are always the model's
// 1-based batch indexes; ID and Dependencies carry the assigned real
// ids once the drafts are written.
type generateDraftOut struct {
	Title             string   `json:"title"`
	Description       string   `json:"description"`
	Details           string   `json:"details"`
	Priority          string   `json:"priority"`
	DraftDependencies []int    `json:"draftDependencies"`
	ID                string   `json:"id,omitempty"`
	Dependencies      []string `json:"dependencies,omitempty"`
}

type generationOut struct {
	Adapter        string `json:"adapter"`
	RequestedModel string `json:"requestedModel"`
	ResolvedModel  string `json:"resolvedModel"`
}

type generateOut struct {
	SchemaVersion int                `json:"schemaVersion"`
	Generation    generationOut      `json:"generation"`
	Tag           string             `json:"tag"`
	Written       bool               `json:"written"`
	Drafts        []generateDraftOut `json:"drafts"`
}

func generateMain(args []string, stdin io.Reader, stdout, stderr io.Writer, selectConfig generationSelector, newProvider providerFactory) int {
	var cf commonFlags
	fs := newFlagSet("generate", stderr, &cf)
	numF := fs.Int("num-tasks", 10, fmt.Sprintf("number of tasks to generate (1-%d)", genai.MaxDrafts))
	yesF := fs.Bool("yes", false, "append the drafted tasks without interactive confirmation")
	if code, ok := parseFlags(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: noise-tasks generate [options] <prd.md>")
		return 2
	}
	if *numF < 1 || *numF > genai.MaxDrafts {
		fmt.Fprintf(stderr, "noise-tasks generate: --num-tasks must be between 1 and %d, got %d\n", genai.MaxDrafts, *numF)
		return 2
	}
	prdPath := fs.Arg(0)

	// Routing (§8.1): implicit invocations use the project database; --file
	// selects the standalone fixture document.
	var tag string
	var ps *projectStore
	if cf.file != "" {
		var err error
		if _, tag, err = loadFixture(&cf); err != nil {
			return fail(stderr, err)
		}
	} else {
		var err error
		if ps, tag, err = resolveStoreTag(&cf); err != nil {
			return fail(stderr, err)
		}
		defer ps.db.Close()
	}
	selection, err := selectConfig(cf.file)
	if err != nil {
		return fail(stderr, err)
	}
	if selection.CompatibilityMode {
		fmt.Fprintln(stderr, legacyGenerationWarning)
	}

	prd, err := readPRD(prdPath)
	if err != nil {
		return fail(stderr, err)
	}
	provider, err := newProvider(selection)
	if err != nil {
		return fail(stderr, err)
	}
	result, err := provider.GenerateTasks(context.Background(), prd, *numF)
	if err != nil {
		return fail(stderr, err)
	}
	drafts := result.Drafts
	// Trust no provider: the fake in tests and any future driver go
	// through the same batch validation as the Anthropic one.
	if err := genai.ValidateResult(result); err != nil {
		return fail(stderr, err)
	}

	// Present the drafts. In --json mode stdout carries only the final
	// document, so the human-readable listing goes to stderr alongside
	// the confirmation prompt.
	if cf.json {
		if !*yesF {
			printGenerationIdentity(stderr, selection, result.ResolvedModel)
			printDrafts(stderr, drafts)
		}
	} else {
		printGenerationIdentity(stdout, selection, result.ResolvedModel)
		fmt.Fprintf(stdout, "%d task(s) drafted from %s for tag %q:\n\n", len(drafts), prdPath, tag)
		printDrafts(stdout, drafts)
	}

	confirmed := *yesF
	if !confirmed {
		fmt.Fprintf(stderr, "append %d task(s) to tag %q? [y/N] ", len(drafts), tag)
		confirmed = readConfirm(stdin)
	}

	if !confirmed {
		if cf.json {
			if err := writeJSON(stdout, generateJSON(selection, result.ResolvedModel, tag, drafts, nil, nil)); err != nil {
				return fail(stderr, err)
			}
			return 0
		}
		fmt.Fprintln(stdout, "aborted; no tasks written")
		return 0
	}

	var ids []string
	var depIDs [][]string
	if ps != nil {
		ids, depIDs, err = ps.db.AddBatch(tag, draftsToStoreTasks(drafts), draftsBatchDeps(drafts))
	} else {
		err = mutateFixture(&cf, func(f *backlog.File, currentTag string) error {
			if currentTag != tag {
				return fmt.Errorf("backlog tag changed during generation: previewed %q, current %q", tag, currentTag)
			}
			var err error
			ids, depIDs, err = appendDrafts(f, currentTag, drafts)
			return err
		})
	}
	if err != nil {
		return fail(stderr, err)
	}

	if cf.json {
		if err := writeJSON(stdout, generateJSON(selection, result.ResolvedModel, tag, drafts, ids, depIDs)); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	fmt.Fprintf(stdout, "appended %d task(s) to tag %q:\n", len(ids), tag)
	for i, id := range ids {
		fmt.Fprintf(stdout, "  %s  %s\n", id, drafts[i].Title)
	}
	return 0
}

func readPRD(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxPRDBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxPRDBytes {
		return "", fmt.Errorf("PRD %s exceeds %d bytes", path, maxPRDBytes)
	}
	prd := strings.TrimSpace(string(data))
	if prd == "" {
		return "", fmt.Errorf("PRD %s is empty", path)
	}
	return prd, nil
}

func printGenerationIdentity(w io.Writer, selection generationSelection, resolvedModel string) {
	resolved := fmt.Sprintf("%q", resolvedModel)
	if resolvedModel == "" {
		resolved = "unreported"
	}
	fmt.Fprintf(w, "generation: adapter=%s requested-model=%q resolved-model=%s\n",
		selection.Adapter, selection.RequestedModel, resolved)
}

// appendDrafts assigns the batch's real IDs against the locked current
// snapshot, remaps intra-batch dependencies, and publishes once.
func appendDrafts(f *backlog.File, tag string, drafts []genai.Draft) (ids []string, depIDs [][]string, err error) {
	ids, err = f.NextTaskIDs(tag, len(drafts))
	if err != nil {
		return nil, nil, err
	}
	depIDs = make([][]string, len(drafts))
	tasks := make([]backlog.Task, len(drafts))
	for i, d := range drafts {
		deps := make([]string, len(d.Dependencies))
		for j, idx := range d.Dependencies {
			deps[j] = ids[idx-1]
		}
		if len(deps) > 0 {
			depIDs[i] = deps
		}
		tasks[i] = backlog.Task{
			Title:        d.Title,
			Description:  d.Description,
			Details:      d.Details,
			Status:       "pending",
			Priority:     d.Priority,
			Dependencies: deps,
		}
	}
	ids, err = f.AddTasks(tag, tasks)
	if err != nil {
		return nil, nil, fmt.Errorf("appending generated batch: %w", err)
	}
	return ids, depIDs, nil
}

// draftsToStoreTasks converts validated provider drafts into store tasks.
func draftsToStoreTasks(drafts []genai.Draft) []*store.Task {
	tasks := make([]*store.Task, len(drafts))
	for i, d := range drafts {
		tasks[i] = &store.Task{
			Title:       d.Title,
			Description: d.Description,
			Details:     d.Details,
			Status:      "pending",
			Priority:    d.Priority,
		}
	}
	return tasks
}

// draftsBatchDeps passes the model's 1-based batch-relative dependency
// indexes through to the store's batch insert.
func draftsBatchDeps(drafts []genai.Draft) [][]int {
	deps := make([][]int, len(drafts))
	for i, d := range drafts {
		deps[i] = d.Dependencies
	}
	return deps
}

// generateJSON builds the single --json document. ids and depIDs are
// nil when nothing was written.
func generateJSON(selection generationSelection, resolvedModel, tag string, drafts []genai.Draft, ids []string, depIDs [][]string) generateOut {
	out := generateOut{
		SchemaVersion: 1,
		Generation: generationOut{
			Adapter:        selection.Adapter,
			RequestedModel: selection.RequestedModel,
			ResolvedModel:  resolvedModel,
		},
		Tag:     tag,
		Written: ids != nil,
		Drafts:  make([]generateDraftOut, len(drafts)),
	}
	for i, d := range drafts {
		draftDeps := d.Dependencies
		if draftDeps == nil {
			draftDeps = []int{}
		}
		out.Drafts[i] = generateDraftOut{
			Title:             d.Title,
			Description:       d.Description,
			Details:           d.Details,
			Priority:          effectivePriority(d.Priority),
			DraftDependencies: draftDeps,
		}
		if ids != nil {
			out.Drafts[i].ID = ids[i]
			out.Drafts[i].Dependencies = depIDs[i]
		}
	}
	return out
}

func printDrafts(w io.Writer, drafts []genai.Draft) {
	for i, d := range drafts {
		fmt.Fprintf(w, "  %2d. %s  [%s]  deps: %s\n",
			i+1, d.Title, effectivePriority(d.Priority), draftDepsString(d.Dependencies))
	}
}

func draftDepsString(deps []int) string {
	if len(deps) == 0 {
		return "-"
	}
	parts := make([]string, len(deps))
	for i, d := range deps {
		parts[i] = strconv.Itoa(d)
	}
	return strings.Join(parts, ",")
}

// readConfirm reads one line and reports whether it is an explicit
// yes. Anything else — including EOF on a closed stdin — is a no.
func readConfirm(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}
