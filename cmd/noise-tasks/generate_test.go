package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/genai"
	"github.com/noisefloor-oss/tasks/internal/preferences"
	"github.com/noisefloor-oss/tasks/internal/store"
)

// fakeProvider satisfies genai.Provider without any network. It records
// what the command asked for.
type fakeProvider struct {
	drafts        []genai.Draft
	resolvedModel string
	err           error
	gotPRD        string
	gotN          int
	calls         int
}

func (p *fakeProvider) GenerateTasks(_ context.Context, prdText string, n int) (genai.Result, error) {
	p.calls++
	p.gotPRD = prdText
	p.gotN = n
	if p.err != nil {
		return genai.Result{}, p.err
	}
	return genai.Result{Drafts: p.drafts, ResolvedModel: p.resolvedModel}, nil
}

func fakeFactory(p genai.Provider) providerFactory {
	return func(generationSelection) (genai.Provider, error) { return p, nil }
}

func fakeSelection(string) (generationSelection, error) {
	return generationSelection{Adapter: "test", RequestedModel: "test-model"}, nil
}

type waitingProvider struct {
	started chan struct{}
	release chan struct{}
	drafts  []genai.Draft
}

func (p *waitingProvider) GenerateTasks(_ context.Context, _ string, _ int) (genai.Result, error) {
	close(p.started)
	<-p.release
	return genai.Result{Drafts: p.drafts}, nil
}

// runGenerate drives generateMain with a scripted stdin.
func runGenerate(factory providerFactory, stdin string, args ...string) (code int, stdout, stderr string) {
	return runGenerateWith(fakeSelection, factory, stdin, args...)
}

func runGenerateWith(selector generationSelector, factory providerFactory, stdin string, args ...string) (code int, stdout, stderr string) {
	var out, errBuf bytes.Buffer
	code = generateMain(args, strings.NewReader(stdin), &out, &errBuf, selector, factory)
	return code, out.String(), errBuf.String()
}

func clearGenerationSelectors(t *testing.T) {
	t.Helper()
	for _, name := range []string{
		"NOISE_TASKS_PROVIDER", "NOISETASK_PROVIDER",
		"NOISE_TASKS_MODEL", "NOISETASK_MODEL",
		"NOISE_TASKS_CLAUDE_EXECUTABLE", "NOISETASK_CLAUDE_EXECUTABLE",
	} {
		t.Setenv(name, "")
	}
}

func configuredBacklog(t *testing.T, adapter, model string) (project, path string) {
	t.Helper()
	project = setupProject(t)
	// The fixture lives outside the project: .tasks/tasks/tasks.json under the
	// boundary is the retired layout every command refuses on sight.
	path = filepath.Join(t.TempDir(), "tasks.json")
	if _, err := backlog.InitFileWithParents(path, "main"); err != nil {
		t.Fatal(err)
	}
	prefs := preferences.Preferences{
		SchemaVersion: preferences.SchemaVersion,
		Generation: preferences.Generation{
			Adapter: adapter,
			Model:   model,
		},
	}
	if err := preferences.Publish(project, prefs); err != nil {
		t.Fatal(err)
	}
	return project, path
}

func selectCompatibilityProviderForTest() (genai.Provider, error) {
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return nil, err
	}
	selection, err := selectCompatibilityGeneration(boundary)
	if err != nil {
		return nil, err
	}
	return newGenerationProvider(selection)
}

func prdFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prd.md")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// threeDrafts includes a forward dependency (draft 2 depends on draft
// 3) to prove remapping is not order-sensitive.
func threeDrafts() []genai.Draft {
	return []genai.Draft{
		{Title: "Scaffold", Description: "base", Details: "mkdir", Priority: "high"},
		{Title: "Parser", Description: "parse", Details: "lex", Priority: "medium", Dependencies: []int{1, 3}},
		{Title: "CLI", Description: "wire", Details: "flags", Priority: "low", Dependencies: []int{1}},
	}
}

func TestGenerateYesAppendsAndRemapsDeps(t *testing.T) {
	// Existing ids 1 and 3: new ids must continue monotonically at 4.
	path := backlogFile(t, "main",
		taskLit("1", "existing", "done", "high"),
		taskLit("3", "other", "pending", ""),
	)
	prd := prdFile(t, "# Product\n\nBuild the thing.\n")
	provider := &fakeProvider{drafts: threeDrafts()}

	code, stdout, stderr := runGenerate(fakeFactory(provider), "",
		"--file", path, "--yes", "--num-tasks", "3", prd)
	if code != 0 {
		t.Fatalf("generate exit %d, stderr: %s", code, stderr)
	}
	if provider.gotN != 3 || provider.gotPRD != "# Product\n\nBuild the thing." {
		t.Errorf("provider got n=%d prd=%q", provider.gotN, provider.gotPRD)
	}
	if !strings.Contains(stdout, "appended 3 task(s)") {
		t.Errorf("stdout = %q", stdout)
	}

	f := readTasks(t, path)
	tasks, err := f.Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 5 {
		t.Fatalf("got %d tasks, want 5", len(tasks))
	}

	scaffold, _ := f.Task("main", "4")
	parser, _ := f.Task("main", "5")
	cli, _ := f.Task("main", "6")
	if scaffold == nil || parser == nil || cli == nil {
		t.Fatalf("assigned ids should be 4, 5, 6; tasks = %+v", tasks)
	}
	if scaffold.Title != "Scaffold" || scaffold.Priority != "high" ||
		scaffold.Description != "base" || scaffold.Details != "mkdir" {
		t.Errorf("scaffold = %+v", scaffold)
	}
	if scaffold.Status != "pending" || parser.Status != "pending" {
		t.Errorf("drafted tasks must start pending")
	}
	if len(scaffold.Dependencies) != 0 {
		t.Errorf("scaffold deps = %v, want none", scaffold.Dependencies)
	}
	// Draft indexes [1,3] remap to real ids [4,6] — including the
	// forward reference to draft 3.
	if strings.Join(parser.Dependencies, ",") != "4,6" {
		t.Errorf("parser deps = %v, want [4 6]", parser.Dependencies)
	}
	if strings.Join(cli.Dependencies, ",") != "4" {
		t.Errorf("cli deps = %v, want [4]", cli.Dependencies)
	}

	// The written file passes its own structural check: every
	// dependency resolves even though draft 2 pointed forward.
	if code, _, vErr := runCmd(verifyMain, "--file", path); code != 0 {
		t.Errorf("verify after generate exit = %d, want 0 (stderr: %s)", code, vErr)
	}
}

func TestConcurrentGenerateCommitsDisjointAtomicBatches(t *testing.T) {
	// §8.1 gives fixtures atomic writes but no cross-process lock, so the
	// serialized-batches guarantee this test pins lives in the store.
	project := setupProject(t)
	db, err := store.CreateWithOwner(projectStorePath(project), project)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureTag("main"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	prd := prdFile(t, "# Concurrent generation\n")
	start := make(chan struct{})
	results := make(chan string, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			provider := &fakeProvider{drafts: threeDrafts()}
			code, _, stderr := runGenerate(fakeFactory(provider), "",
				"--yes", "--num-tasks", "3", prd)
			if code != 0 {
				results <- stderr
			}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	for failure := range results {
		t.Errorf("concurrent generate failed: %s", failure)
	}

	db, err = store.Open(projectStorePath(project))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tasks, err := db.ListTasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 6 {
		t.Fatalf("got %d tasks, want two complete batches (6)", len(tasks))
	}
	byID := make(map[string]*store.Task, len(tasks))
	for _, task := range tasks {
		byID[task.ID] = task
	}
	for _, base := range []int{1, 4} {
		parser := byID[fmt.Sprint(base+1)]
		if parser == nil {
			t.Fatalf("batch at %d missing task %d", base, base+1)
		}
		want := fmt.Sprintf("%d,%d", base, base+2)
		if strings.Join(parser.Dependencies, ",") != want {
			t.Errorf("batch at %d dependencies = %v, want %s", base, parser.Dependencies, want)
		}
	}
}

func TestGenerateInteractiveConfirm(t *testing.T) {
	path := backlogFile(t, "main")
	prd := prdFile(t, "PRD body")
	provider := &fakeProvider{drafts: threeDrafts()}

	code, stdout, stderr := runGenerate(fakeFactory(provider), "y\n", "--file", path, prd)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	// The drafts are shown (title + priority + deps) before the prompt.
	if !strings.Contains(stdout, "Parser") || !strings.Contains(stdout, "[medium]") ||
		!strings.Contains(stdout, "deps: 1,3") ||
		!strings.Contains(stdout, `generation: adapter=test requested-model="test-model" resolved-model=unreported`) {
		t.Errorf("draft listing missing from stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "[y/N]") {
		t.Errorf("confirmation prompt missing from stderr: %q", stderr)
	}
	tasks, err := readTasks(t, path).Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 3 {
		t.Errorf("confirmed run wrote %d tasks, want 3", len(tasks))
	}
}

func TestGenerateDenyWritesNothing(t *testing.T) {
	for _, stdin := range []string{"n\n", "\n", "", "nope\n"} {
		path := backlogFile(t, "main", taskLit("1", "existing", "pending", ""))
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		prd := prdFile(t, "PRD body")

		code, stdout, stderr := runGenerate(fakeFactory(&fakeProvider{drafts: threeDrafts()}),
			stdin, "--file", path, prd)
		if code != 0 {
			t.Errorf("stdin %q: exit %d, stderr: %s", stdin, code, stderr)
		}
		if !strings.Contains(stdout, "no tasks written") {
			t.Errorf("stdin %q: stdout = %q, want an aborted note", stdin, stdout)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(original) {
			t.Errorf("stdin %q: denied run mutated the file", stdin)
		}
	}
}

func TestGenerateJSONWritten(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "existing", "pending", ""))
	prd := prdFile(t, "PRD body")

	code, stdout, stderr := runGenerate(fakeFactory(&fakeProvider{drafts: threeDrafts(), resolvedModel: "resolved-test-model"}),
		"", "--file", path, "--json", "--yes", prd)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}

	var out generateOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("--json stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if out.SchemaVersion != 1 || out.Generation.Adapter != "test" ||
		out.Generation.RequestedModel != "test-model" || out.Generation.ResolvedModel != "resolved-test-model" ||
		out.Tag != "main" || !out.Written || len(out.Drafts) != 3 {
		t.Fatalf("doc = %+v", out)
	}
	if out.Drafts[0].ID != "2" || out.Drafts[1].ID != "3" || out.Drafts[2].ID != "4" {
		t.Errorf("assigned ids = %s, %s, %s, want 2, 3, 4",
			out.Drafts[0].ID, out.Drafts[1].ID, out.Drafts[2].ID)
	}
	if strings.Join(out.Drafts[1].Dependencies, ",") != "2,4" {
		t.Errorf("draft 2 remapped deps = %v, want [2 4]", out.Drafts[1].Dependencies)
	}
	if len(out.Drafts[1].DraftDependencies) != 2 || out.Drafts[1].DraftDependencies[0] != 1 {
		t.Errorf("draft 2 draftDependencies = %v, want [1 3]", out.Drafts[1].DraftDependencies)
	}
	if out.Drafts[0].Priority != "high" || out.Drafts[0].Title != "Scaffold" {
		t.Errorf("draft 1 = %+v", out.Drafts[0])
	}
}

func TestGenerateJSONDenied(t *testing.T) {
	path := backlogFile(t, "main")
	prd := prdFile(t, "PRD body")

	code, stdout, stderr := runGenerate(fakeFactory(&fakeProvider{drafts: threeDrafts()}),
		"n\n", "--file", path, "--json", prd)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	// stdout carries only the document; prompt and listing went to stderr.
	var out generateOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatalf("--json stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if out.Written {
		t.Error("denied run reported written: true")
	}
	if len(out.Drafts) != 3 || out.Drafts[0].ID != "" || out.Drafts[0].Dependencies != nil {
		t.Errorf("denied doc should carry drafts without ids: %+v", out.Drafts)
	}
	if !strings.Contains(stderr, "[y/N]") {
		t.Errorf("prompt should go to stderr in --json mode: %q", stderr)
	}
	tasks, err := readTasks(t, path).Tasks("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 0 {
		t.Errorf("denied --json run wrote %d tasks", len(tasks))
	}
}

func TestGenerateRefusals(t *testing.T) {
	path := backlogFile(t, "main", taskLit("1", "existing", "pending", ""))
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	goodPRD := prdFile(t, "PRD body")
	emptyPRD := prdFile(t, "   \n\t\n")
	missingPRD := filepath.Join(t.TempDir(), "nope.md")

	okProvider := func() *fakeProvider { return &fakeProvider{drafts: threeDrafts()} }

	cases := []struct {
		name     string
		provider genai.Provider
		args     []string
		code     int
		want     string
	}{
		{"missing PRD file", okProvider(), []string{"--file", path, missingPRD}, 1, "no such file"},
		{"empty PRD", okProvider(), []string{"--file", path, emptyPRD}, 1, "is empty"},
		{"provider error", &fakeProvider{err: errors.New("model exploded")},
			[]string{"--file", path, "--yes", goodPRD}, 1, "model exploded"},
		{"empty draft batch", &fakeProvider{},
			[]string{"--file", path, "--yes", goodPRD}, 1, "no task drafts"},
		{"invalid draft deps", &fakeProvider{drafts: []genai.Draft{{Title: "a", Dependencies: []int{9}}}},
			[]string{"--file", path, "--yes", goodPRD}, 1, "does not exist"},
		{"cyclic draft deps", &fakeProvider{drafts: []genai.Draft{
			{Title: "a", Dependencies: []int{2}}, {Title: "b", Dependencies: []int{1}},
		}}, []string{"--file", path, "--yes", goodPRD}, 1, "cycle"},
		{"unsafe resolved identity", &fakeProvider{drafts: threeDrafts(), resolvedModel: strings.Repeat("m", 257)},
			[]string{"--file", path, "--yes", goodPRD}, 1, "resolved model"},
		{"unknown tag", okProvider(), []string{"--file", path, "--tag", "nope", goodPRD}, 1, "not found"},
		{"missing arg", okProvider(), []string{"--file", path}, 2, "usage"},
		{"extra args", okProvider(), []string{"--file", path, goodPRD, "stray"}, 2, "usage"},
		{"num-tasks zero", okProvider(), []string{"--file", path, "--num-tasks", "0", goodPRD}, 2, "--num-tasks"},
		{"num-tasks too big", okProvider(), []string{"--file", path, "--num-tasks", "999", goodPRD}, 2, "--num-tasks"},
	}
	for _, tc := range cases {
		code, stdout, stderr := runGenerate(fakeFactory(tc.provider), "", tc.args...)
		if code != tc.code {
			t.Errorf("%s: exit = %d, want %d (stderr: %s)", tc.name, code, tc.code, stderr)
		}
		if stdout != "" {
			t.Errorf("%s: refusal must not write stdout, got %q", tc.name, stdout)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Errorf("%s: stderr = %q, want %q", tc.name, stderr, tc.want)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("a refused generate mutated the file")
	}
}

// TestGenerateCheapRefusalsSkipProvider pins that local refusals never
// cost an API call: the provider is not consulted when the PRD or
// backlog is unusable.
func TestGenerateCheapRefusalsSkipProvider(t *testing.T) {
	path := backlogFile(t, "main")
	provider := &fakeProvider{drafts: threeDrafts()}
	factory := fakeFactory(provider)

	runGenerate(factory, "", "--file", path, filepath.Join(t.TempDir(), "nope.md"))
	runGenerate(factory, "", "--file", path, prdFile(t, "  "))
	runGenerate(factory, "", "--file", path, "--tag", "nope", prdFile(t, "PRD"))
	if provider.calls != 0 {
		t.Errorf("provider was called %d time(s) on local refusals", provider.calls)
	}
}

func TestGenerateOversizedPRDSkipsProviderConstruction(t *testing.T) {
	path := backlogFile(t, "main")
	prd := prdFile(t, strings.Repeat("x", maxPRDBytes+1))
	constructed := 0
	factory := func(generationSelection) (genai.Provider, error) {
		constructed++
		return &fakeProvider{drafts: threeDrafts()}, nil
	}
	code, stdout, stderr := runGenerate(factory, "", "--file", path, "--yes", prd)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "exceeds") || constructed != 0 {
		t.Fatalf("oversized PRD = code %d stdout %q stderr %q constructed %d", code, stdout, stderr, constructed)
	}
}

// TestGenerateUnsetKeyWithRealConstructor exercises the real driver
// constructor through the command path: with ANTHROPIC_API_KEY unset
// the command refuses before any network use.
func TestGenerateUnsetKeyWithRealConstructor(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	path := backlogFile(t, "main")
	prd := prdFile(t, "PRD body")

	factory := func(generationSelection) (genai.Provider, error) {
		return genai.NewAnthropicConfigured("configured-model")
	}
	code, stdout, stderr := runGenerate(factory, "", "--file", path, "--yes", prd)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("refusal must not write stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "ANTHROPIC_API_KEY") {
		t.Errorf("stderr = %q, want it to name ANTHROPIC_API_KEY", stderr)
	}
}

// stubClaudeExe writes an executable stand-in for the claude CLI and
// returns its path. Selection only resolves it; nothing runs it.
func stubClaudeExe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "claude-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSelectCompatibilityProvider tables the transition driver-selection
// contract over the env permutations: an explicit provider wins, auto prefers
// the API key, falls back to a resolvable CLI, and otherwise refuses.
func TestSelectCompatibilityProvider(t *testing.T) {
	stub := stubClaudeExe(t)
	missing := filepath.Join(t.TempDir(), "no-such-claude")

	cases := []struct {
		name     string
		provider string // NOISE_TASKS_PROVIDER
		key      string // ANTHROPIC_API_KEY
		exe      string // NOISE_TASKS_CLAUDE_EXECUTABLE
		want     string // "api", "cli", or "" for an error
		wantErr  string
	}{
		{"explicit api with key", "api", "sk-test", stub, "api", ""},
		{"explicit api without key", "api", "", stub, "", "ANTHROPIC_API_KEY"},
		{"explicit claude-cli resolvable", "claude-cli", "sk-test", stub, "cli", ""},
		{"explicit claude-cli missing exe", "claude-cli", "sk-test", missing, "", "no LLM provider available"},
		{"unknown provider", "gpt", "sk-test", stub, "", "NOISE_TASKS_PROVIDER"},
		{"auto prefers api when key set", "", "sk-test", stub, "api", ""},
		{"auto api when key set and no cli", "", "sk-test", missing, "api", ""},
		{"auto falls back to cli without key", "", "", stub, "cli", ""},
		{"auto refuses with neither", "", "", missing, "", "ANTHROPIC_API_KEY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NOISE_TASKS_PROVIDER", tc.provider)
			t.Setenv("ANTHROPIC_API_KEY", tc.key)
			t.Setenv("NOISE_TASKS_CLAUDE_EXECUTABLE", tc.exe)

			p, err := selectCompatibilityProviderForTest()
			if tc.want == "" {
				if err == nil {
					t.Fatalf("want an error, got provider %T", p)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("select compatibility provider: %v", err)
			}
			switch tc.want {
			case "api":
				if _, ok := p.(*genai.Anthropic); !ok {
					t.Errorf("selected %T, want *genai.Anthropic", p)
				}
			case "cli":
				cli, ok := p.(*genai.ClaudeCLI)
				if !ok {
					t.Fatalf("selected %T, want *genai.ClaudeCLI", p)
				}
				if cli.Executable != stub {
					t.Errorf("cli executable = %q, want %q", cli.Executable, stub)
				}
			}
		})
	}

	// The auto refusal names both ways out.
	t.Setenv("NOISE_TASKS_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("NOISE_TASKS_CLAUDE_EXECUTABLE", missing)
	_, err := selectCompatibilityProviderForTest()
	if err == nil {
		t.Fatal("want a refusal")
	}
	for _, needle := range []string{"ANTHROPIC_API_KEY", "claude CLI", "NOISE_TASKS_PROVIDER"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("refusal = %q, want it to mention %q", err, needle)
		}
	}
}

func TestCompatibilityProviderEnvironmentAliases(t *testing.T) {
	stub := stubClaudeExe(t)
	t.Setenv("NOISE_TASKS_PROVIDER", "")
	t.Setenv("NOISETASK_PROVIDER", "claude-cli")
	t.Setenv("NOISE_TASKS_CLAUDE_EXECUTABLE", stub)
	p, err := selectCompatibilityProviderForTest()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*genai.ClaudeCLI); !ok {
		t.Fatalf("legacy provider selected %T, want *genai.ClaudeCLI", p)
	}

	t.Setenv("NOISE_TASKS_PROVIDER", "api")
	if _, err := selectCompatibilityProviderForTest(); err == nil || !strings.Contains(err.Error(), "conflicting environment variables") {
		t.Fatalf("conflicting provider error = %v", err)
	}
}

func TestCompatibilityFixedCLIAdapters(t *testing.T) {
	for _, adapter := range []string{preferences.AdapterOMP, preferences.AdapterCodex, preferences.AdapterKimi} {
		t.Run(adapter, func(t *testing.T) {
			clearGenerationSelectors(t)
			boundary := setupProject(t)
			external := t.TempDir()
			executable := filepath.Join(external, adapter)
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", external)
			t.Setenv("NOISE_TASKS_PROVIDER", adapter)
			t.Setenv("NOISE_TASKS_MODEL", "provider/model")
			t.Setenv("ANTHROPIC_API_KEY", "unused")
			t.Setenv("NOISE_TASKS_CLAUDE_EXECUTABLE", filepath.Join(external, "unused"))
			selection, err := selectCompatibilityGeneration(boundary)
			if err != nil || selection.Adapter != adapter || selection.RequestedModel != "provider/model" ||
				selection.Executable != executable || !selection.CompatibilityMode {
				t.Fatalf("selection = %+v, %v", selection, err)
			}
			if _, err := newGenerationProvider(selection); err != nil {
				t.Fatal(err)
			}
			t.Setenv("NOISE_TASKS_MODEL", "")
			selection, err = selectCompatibilityGeneration(boundary)
			if adapter == preferences.AdapterOMP {
				if err == nil {
					t.Fatal("OMP accepted an empty model")
				}
			} else if err != nil || selection.RequestedModel != "" {
				t.Fatalf("default model selection = %+v, %v", selection, err)
			}
			for _, model := range []string{"--option", " padded ", "line\nbreak"} {
				t.Setenv("NOISE_TASKS_MODEL", model)
				if _, err := selectCompatibilityGeneration(boundary); err == nil {
					t.Fatalf("accepted model %q", model)
				}
			}
			t.Setenv("NOISE_TASKS_MODEL", "provider/model")
			t.Setenv("PATH", t.TempDir())
			if _, err := selectCompatibilityGeneration(boundary); !errors.Is(err, preferences.ErrAdapterUnavailable) {
				t.Fatalf("missing executable = %v", err)
			}
			if err := os.WriteFile(filepath.Join(boundary, adapter), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", boundary)
			if _, err := selectCompatibilityGeneration(boundary); !errors.Is(err, preferences.ErrAdapterContained) {
				t.Fatalf("contained executable = %v", err)
			}
		})
	}
	clearGenerationSelectors(t)
	t.Setenv("NOISE_TASKS_PROVIDER", "unknown")
	if _, err := selectCompatibilityGeneration(t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "valid: api, claude-cli, omp, codex, kimi") {
		t.Fatalf("unknown provider = %v", err)
	}
}

// TestGenerateNoProviderRefusal drives the real selection through the
// command path: with no key and no resolvable CLI the command refuses
// with exit 1 before touching any provider, naming both options.
func TestGenerateNoProviderRefusal(t *testing.T) {
	t.Setenv("NOISE_TASKS_PROVIDER", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("NOISE_TASKS_CLAUDE_EXECUTABLE", filepath.Join(t.TempDir(), "no-such-claude"))
	path := backlogFile(t, "main")
	prd := prdFile(t, "PRD body")

	factory := func(generationSelection) (genai.Provider, error) { return selectCompatibilityProviderForTest() }
	code, stdout, stderr := runGenerate(factory, "", "--file", path, "--yes", prd)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if stdout != "" {
		t.Errorf("refusal must not write stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "ANTHROPIC_API_KEY") || !strings.Contains(stderr, "claude-cli") {
		t.Errorf("stderr = %q, want it to name both providers", stderr)
	}
}

func TestSelectGenerationUsesAuthoritativePreferences(t *testing.T) {
	clearGenerationSelectors(t)
	project, path := configuredBacklog(t, preferences.AdapterAnthropicAPI, "configured-model")
	selection, err := selectGeneration(path)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Adapter != preferences.AdapterAnthropicAPI || selection.RequestedModel != "configured-model" ||
		selection.ProjectBoundary != project || selection.CompatibilityMode || selection.Executable != "" {
		t.Fatalf("selection = %+v", selection)
	}

	t.Setenv("NOISE_TASKS_MODEL", "credential-shaped-selector")
	_, err = selectGeneration(path)
	if err == nil || !strings.Contains(err.Error(), "conflicts with project preferences") ||
		strings.Contains(err.Error(), "credential-shaped-selector") {
		t.Fatalf("selector conflict = %v", err)
	}
}

func TestConfiguredGenerationNeverFallsBackToAmbientProvider(t *testing.T) {
	t.Run("API preference does not fall back to CLI", func(t *testing.T) {
		clearGenerationSelectors(t)
		_, path := configuredBacklog(t, preferences.AdapterAnthropicAPI, "configured-model")
		t.Setenv("ANTHROPIC_API_KEY", "")
		cliDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(cliDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", cliDir)
		code, stdout, stderr := runGenerateWith(selectGeneration, newGenerationProvider, "",
			"--file", path, "--yes", prdFile(t, "PRD"))
		if code != 1 || stdout != "" || !strings.Contains(stderr, "ANTHROPIC_API_KEY") ||
			strings.Contains(stderr, legacyGenerationWarning) {
			t.Fatalf("configured API = code %d stdout %q stderr %q", code, stdout, stderr)
		}
	})

	t.Run("OMP preference does not fall back to API", func(t *testing.T) {
		clearGenerationSelectors(t)
		_, path := configuredBacklog(t, preferences.AdapterOMP, "zai/glm-5.2")
		t.Setenv("ANTHROPIC_API_KEY", "available-but-not-selected")
		t.Setenv("PATH", t.TempDir())
		constructed := 0
		factory := func(selection generationSelection) (genai.Provider, error) {
			constructed++
			return newGenerationProvider(selection)
		}
		code, stdout, stderr := runGenerateWith(selectGeneration, factory, "",
			"--file", path, "--yes", prdFile(t, "PRD"))
		if code != 1 || stdout != "" || !strings.Contains(stderr, "adapter executable is unavailable") ||
			constructed != 0 || strings.Contains(stderr, legacyGenerationWarning) {
			t.Fatalf("configured OMP = code %d stdout %q stderr %q constructed %d", code, stdout, stderr, constructed)
		}
	})
}

func TestSelectGenerationFailsClosedForInvalidAndTransitionState(t *testing.T) {
	t.Run("malformed preferences", func(t *testing.T) {
		clearGenerationSelectors(t)
		project, path := configuredBacklog(t, preferences.AdapterAnthropicAPI, "model")
		if err := os.WriteFile(preferences.Path(project), []byte(`{"schemaVersion":99}`), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("ANTHROPIC_API_KEY", "available-but-must-not-fallback")
		selection, err := selectGeneration(path)
		if err == nil || !strings.Contains(err.Error(), "invalid preferences") || selection.CompatibilityMode {
			t.Fatalf("invalid preference selection = %+v, %v", selection, err)
		}
	})

	t.Run("transition conflict", func(t *testing.T) {
		clearGenerationSelectors(t)
		project := setupProject(t)
		transition := filepath.Join(project, ".taskmaster", "tasks", "tasks.json")
		if _, err := backlog.InitFileWithParents(transition, "main"); err != nil {
			t.Fatal(err)
		}
		if err := preferences.Publish(project, preferences.Preferences{
			SchemaVersion: preferences.SchemaVersion,
			Generation:    preferences.Generation{Adapter: preferences.AdapterAnthropicAPI, Model: "model"},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := selectGeneration(transition); err == nil || !strings.Contains(err.Error(), "transition-only backlog conflicts") {
			t.Fatalf("transition conflict = %v", err)
		}
	})
}

func TestSelectGenerationResolvesFixedCLIOutsideProject(t *testing.T) {
	for _, test := range []struct {
		adapter, executable, model string
	}{
		{preferences.AdapterOMP, "omp", "zai/glm-5.2"},
		{preferences.AdapterCodex, "codex", "gpt-5.3-codex"},
		{preferences.AdapterKimi, "kimi", "kimi-k2.5"},
		{preferences.AdapterClaudeCLI, "claude", "claude-fable-5"},
	} {
		t.Run(test.adapter, func(t *testing.T) {
			clearGenerationSelectors(t)
			project, path := configuredBacklog(t, test.adapter, test.model)
			external := t.TempDir()
			executable := filepath.Join(external, test.executable)
			if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", external)
			selection, err := selectGeneration(path)
			if err != nil || selection.Executable != executable || selection.RequestedModel != test.model {
				t.Fatalf("external selection = %+v, %v", selection, err)
			}

			inside := filepath.Join(project, "bin")
			if err := os.Mkdir(inside, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(inside, test.executable), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", inside)
			if _, err := selectGeneration(path); !errors.Is(err, preferences.ErrAdapterContained) {
				t.Fatalf("contained selection error = %v", err)
			}
		})
	}
}

func TestNewGenerationProviderConstructsEveryAdapter(t *testing.T) {
	executable := stubClaudeExe(t)
	boundary := t.TempDir()
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	tests := []struct {
		name      string
		selection generationSelection
		check     func(genai.Provider) bool
	}{
		{
			name: "anthropic-api",
			selection: generationSelection{Adapter: preferences.AdapterAnthropicAPI,
				RequestedModel: "claude-fable-5", ProjectBoundary: boundary},
			check: func(provider genai.Provider) bool {
				driver, ok := provider.(*genai.Anthropic)
				return ok && driver.Model == "claude-fable-5"
			},
		},
		{
			name: "claude-cli",
			selection: generationSelection{Adapter: preferences.AdapterClaudeCLI,
				RequestedModel: "claude-fable-5", Executable: executable, ProjectBoundary: boundary},
			check: func(provider genai.Provider) bool {
				driver, ok := provider.(*genai.ClaudeCLI)
				return ok && driver.Model == "claude-fable-5" && driver.WorkingDirectory == boundary
			},
		},
		{
			name: "omp",
			selection: generationSelection{Adapter: preferences.AdapterOMP,
				RequestedModel: "zai/glm-5.2", Executable: executable, ProjectBoundary: boundary},
			check: func(provider genai.Provider) bool {
				driver, ok := provider.(*genai.OMP)
				return ok && driver.Model == "zai/glm-5.2" && driver.WorkingDirectory == boundary
			},
		},
		{
			name: "codex",
			selection: generationSelection{Adapter: preferences.AdapterCodex,
				RequestedModel: "gpt-5.3-codex", Executable: executable, ProjectBoundary: boundary},
			check: func(provider genai.Provider) bool {
				driver, ok := provider.(*genai.Codex)
				return ok && driver.Model == "gpt-5.3-codex" && driver.Executable == executable
			},
		},
		{
			name: "kimi",
			selection: generationSelection{Adapter: preferences.AdapterKimi,
				RequestedModel: "kimi-k2.5", Executable: executable, ProjectBoundary: boundary},
			check: func(provider genai.Provider) bool {
				driver, ok := provider.(*genai.Kimi)
				return ok && driver.Model == "kimi-k2.5" && driver.Executable == executable
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, err := newGenerationProvider(test.selection)
			if err != nil || !test.check(provider) {
				t.Fatalf("provider = %T, %v", provider, err)
			}
		})
	}
}

func TestCompatibilityGenerationRejectsProjectContainedExecutable(t *testing.T) {
	clearGenerationSelectors(t)
	project := setupProject(t)
	bin := filepath.Join(project, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(bin, "claude")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("NOISE_TASKS_PROVIDER", "claude-cli")
	_, err := selectCompatibilityGeneration(project)
	if err == nil || !strings.Contains(err.Error(), "inside the project boundary") {
		t.Fatalf("contained compatibility executable error = %v", err)
	}
}

func TestGenerateCompatibilityWarningAndIdentity(t *testing.T) {
	clearGenerationSelectors(t)
	project := setupProject(t)
	path := filepath.Join(project, ".tasks", "tasks", "tasks.json")
	if _, err := backlog.InitFileWithParents(path, "main"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	provider := &fakeProvider{drafts: threeDrafts(), resolvedModel: "resolved-api-model"}
	var captured generationSelection
	factory := func(selection generationSelection) (genai.Provider, error) {
		captured = selection
		return provider, nil
	}
	code, stdout, stderr := runGenerateWith(selectGeneration, factory, "", "--file", path, "--json", "--yes", prdFile(t, "PRD"))
	if code != 0 || stderr != legacyGenerationWarning+"\n" {
		t.Fatalf("compat generate = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	var out generateOut
	if err := json.Unmarshal([]byte(stdout), &out); err != nil {
		t.Fatal(err)
	}
	if captured.Adapter != preferences.AdapterAnthropicAPI || captured.RequestedModel != genai.DefaultAnthropicModel ||
		!captured.CompatibilityMode || out.Generation.Adapter != captured.Adapter ||
		out.Generation.RequestedModel != captured.RequestedModel || out.Generation.ResolvedModel != "resolved-api-model" {
		t.Fatalf("captured=%+v output=%+v", captured, out.Generation)
	}
}

func TestGenerateSnapshotsPreferencesAcrossSlowProvider(t *testing.T) {
	clearGenerationSelectors(t)
	project, path := configuredBacklog(t, preferences.AdapterAnthropicAPI, "model-before")
	provider := &waitingProvider{started: make(chan struct{}), release: make(chan struct{}), drafts: threeDrafts()}
	var captured generationSelection
	factory := func(selection generationSelection) (genai.Provider, error) {
		captured = selection
		return provider, nil
	}
	type outcome struct {
		code           int
		stdout, stderr string
	}
	done := make(chan outcome, 1)
	prd := prdFile(t, "PRD")
	go func() {
		code, stdout, stderr := runGenerateWith(selectGeneration, factory, "", "--file", path, "--json", "--yes", prd)
		done <- outcome{code: code, stdout: stdout, stderr: stderr}
	}()
	<-provider.started
	code, stdout, stderr := runSetup("", false,
		"--adapter", preferences.AdapterAnthropicAPI, "--model", "model-after", "--yes", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("concurrent setup = code %d stdout %q stderr %q", code, stdout, stderr)
	}
	close(provider.release)
	result := <-done
	if result.code != 0 || result.stderr != "" {
		t.Fatalf("generate = code %d stdout %q stderr %q", result.code, result.stdout, result.stderr)
	}
	var out generateOut
	if err := json.Unmarshal([]byte(result.stdout), &out); err != nil {
		t.Fatal(err)
	}
	if captured.RequestedModel != "model-before" || out.Generation.RequestedModel != "model-before" {
		t.Fatalf("snapshot changed: captured=%+v output=%+v", captured, out.Generation)
	}
	snapshot, err := preferences.Load(project)
	if err != nil || snapshot.Preferences.Generation.Model != "model-after" {
		t.Fatalf("current preferences = %+v, %v", snapshot, err)
	}
}
