package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/noisefloor-oss/tasks/internal/backlog"
	"github.com/noisefloor-oss/tasks/internal/envcompat"
	"github.com/noisefloor-oss/tasks/internal/genai"
	"github.com/noisefloor-oss/tasks/internal/preferences"
)

const legacyGenerationWarning = "warning: generation preferences are not configured; run noise tasks setup; ambient provider selection is deprecated"

type generationSelection struct {
	Adapter           string
	RequestedModel    string
	Executable        string
	ProjectBoundary   string
	CompatibilityMode bool
}

type generationSelector func(backlogPath string) (generationSelection, error)
type providerFactory func(generationSelection) (genai.Provider, error)

func selectGeneration(backlogPath string) (generationSelection, error) {
	_, boundary, err := backlog.ProjectContext()
	if err != nil {
		return generationSelection{}, err
	}
	snapshot, err := preferences.Load(boundary)
	if err != nil {
		return generationSelection{}, err
	}
	if !snapshot.Exists {
		return selectCompatibilityGeneration(boundary)
	}
	if isTransitionBacklog(backlogPath) {
		return generationSelection{}, errors.New("transition-only backlog conflicts with .tasks/preferences.json; migrate task state before generating")
	}
	if generationSelectorEnvironmentPresent() {
		return generationSelection{}, errors.New("generation selector environment conflicts with project preferences; remove provider, model, and executable selectors or rerun noise tasks setup")
	}

	generation := snapshot.Preferences.Generation
	selection := generationSelection{
		Adapter:         generation.Adapter,
		RequestedModel:  generation.Model,
		ProjectBoundary: boundary,
	}
	if generation.Adapter == preferences.AdapterAnthropicAPI {
		return selection, nil
	}
	executable, err := preferences.ResolveAdapterExecutable(boundary, generation.Adapter)
	if err != nil {
		return generationSelection{}, err
	}
	selection.Executable = executable
	return selection, nil
}

func selectCompatibilityGeneration(boundary string) (generationSelection, error) {
	selection := generationSelection{ProjectBoundary: boundary, CompatibilityMode: true}
	provider, model, executable, err := compatibilityEnvironment()
	if err != nil {
		return selection, err
	}

	switch provider {
	case "api":
		return compatibilityAPISelection(selection, model)
	case "claude-cli":
		return compatibilityCLISelection(selection, model, executable)
	case preferences.AdapterOMP, preferences.AdapterCodex, preferences.AdapterKimi:
		if err := genai.ValidateRequestedModel(model, provider == preferences.AdapterOMP); err != nil {
			return selection, fmt.Errorf("%s: %w", provider, err)
		}
		resolved, err := preferences.ResolveAdapterExecutable(boundary, provider)
		if err != nil {
			return selection, err
		}
		selection.Adapter = provider
		selection.RequestedModel = model
		selection.Executable = resolved
		return selection, nil
	case "":
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			return compatibilityAPISelection(selection, model)
		}
		return compatibilityCLISelection(selection, model, executable)
	default:
		return selection, fmt.Errorf("unknown NOISE_TASKS_PROVIDER %q (valid: api, claude-cli, omp, codex, kimi)", provider)
	}
}

func compatibilityEnvironment() (provider, model, executable string, err error) {
	provider, err = envcompat.Lookup("NOISE_TASKS_PROVIDER", "NOISETASK_PROVIDER")
	if err != nil {
		return "", "", "", err
	}
	model, err = envcompat.Lookup("NOISE_TASKS_MODEL", "NOISETASK_MODEL")
	if err != nil {
		return "", "", "", err
	}
	executable, err = envcompat.Lookup("NOISE_TASKS_CLAUDE_EXECUTABLE", "NOISETASK_CLAUDE_EXECUTABLE")
	return provider, model, executable, err
}

func compatibilityAPISelection(selection generationSelection, model string) (generationSelection, error) {
	if model == "" {
		model = genai.DefaultAnthropicModel
	}
	if err := genai.ValidateRequestedModel(model, true); err != nil {
		return selection, fmt.Errorf("anthropic-api: %w", err)
	}
	selection.Adapter = preferences.AdapterAnthropicAPI
	selection.RequestedModel = model
	return selection, nil
}

func compatibilityCLISelection(selection generationSelection, model, configuredExecutable string) (generationSelection, error) {
	if err := genai.ValidateRequestedModel(model, false); err != nil {
		return selection, fmt.Errorf("claude-cli: %w", err)
	}
	executable, err := resolveCompatibilityExecutable(selection.ProjectBoundary, configuredExecutable)
	if err != nil {
		return selection, err
	}
	selection.Adapter = preferences.AdapterClaudeCLI
	selection.RequestedModel = model
	selection.Executable = executable
	return selection, nil
}

func resolveCompatibilityExecutable(boundary, configured string) (string, error) {
	name := configured
	if name == "" {
		name = "claude"
	}
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", errors.New("no LLM provider available: set ANTHROPIC_API_KEY to use the api provider, or install the claude CLI (or set NOISE_TASKS_CLAUDE_EXECUTABLE) to use the claude-cli provider (NOISE_TASKS_PROVIDER=api|claude-cli|omp|codex|kimi forces one)")
	}
	canonical, err := preferences.ValidateCLIExecutable(boundary, resolved)
	if errors.Is(err, preferences.ErrAdapterContained) {
		return "", errors.New("claude CLI executable is inside the project boundary")
	}
	if err != nil {
		return "", errors.New("claude CLI is not available; install it or configure NOISE_TASKS_CLAUDE_EXECUTABLE")
	}
	return canonical, nil
}

func newGenerationProvider(selection generationSelection) (genai.Provider, error) {
	switch selection.Adapter {
	case preferences.AdapterAnthropicAPI:
		return genai.NewAnthropicConfigured(selection.RequestedModel)
	case preferences.AdapterClaudeCLI:
		return genai.NewClaudeCLIConfigured(selection.Executable, selection.RequestedModel, selection.ProjectBoundary)
	case preferences.AdapterOMP:
		return genai.NewOMP(selection.Executable, selection.RequestedModel, selection.ProjectBoundary)
	case preferences.AdapterCodex:
		return genai.NewCodex(selection.Executable, selection.RequestedModel)
	case preferences.AdapterKimi:
		return genai.NewKimi(selection.Executable, selection.RequestedModel)
	default:
		return nil, fmt.Errorf("unsupported generation adapter %q", selection.Adapter)
	}
}

func generationSelectorEnvironmentPresent() bool {
	for _, name := range []string{
		"NOISE_TASKS_PROVIDER", "NOISETASK_PROVIDER",
		"NOISE_TASKS_MODEL", "NOISETASK_MODEL",
		"NOISE_TASKS_CLAUDE_EXECUTABLE", "NOISETASK_CLAUDE_EXECUTABLE",
	} {
		if os.Getenv(name) != "" {
			return true
		}
	}
	return false
}

func isTransitionBacklog(path string) bool {
	normalized := filepath.ToSlash(path)
	return hasPathSuffix(normalized, ".taskmaster/tasks/tasks.json") ||
		hasPathSuffix(normalized, ".noisetask/tasks.json")
}

func hasPathSuffix(path, suffix string) bool {
	return path == suffix || strings.HasSuffix(path, "/"+suffix)
}
