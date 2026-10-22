package genai

import (
	"context"
	"fmt"
	"time"
)

// OMP drives task drafting through an installation-selected OMP executable.
// The caller owns fixed-basename resolution and project-containment checks;
// this type owns the fixed, no-tools invocation protocol.
type OMP struct {
	Executable       string
	Model            string
	WorkingDirectory string
	Timeout          time.Duration
}

func NewOMP(executable, model, workingDirectory string) (*OMP, error) {
	if err := validateModelArgument(model, true); err != nil {
		return nil, fmt.Errorf("omp: %w", err)
	}
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return nil, fmt.Errorf("omp: %w", err)
	}
	return &OMP{
		Executable:       canonical,
		Model:            model,
		WorkingDirectory: workingDirectory,
		Timeout:          defaultCLITimeout,
	}, nil
}

func (o *OMP) GenerateTasks(ctx context.Context, prdText string, n int) (Result, error) {
	n = clampDraftCount(n)
	if err := validateModelArgument(o.Model, true); err != nil {
		return Result{}, fmt.Errorf("omp: %w", err)
	}
	args := []string{
		"-p",
		"--mode", "text",
		"--no-tools",
		"--no-session",
		"--no-skills",
		"--no-rules",
		"--no-extensions",
		"--model", o.Model,
	}
	output, err := runCLI(ctx, "omp", o.Executable, o.WorkingDirectory, args,
		draftSystemPrompt+"\n\n"+draftUserPrompt(n, prdText), o.Timeout)
	if err != nil {
		return Result{}, err
	}
	drafts, err := finishDrafts(output, n)
	if err != nil {
		return Result{}, err
	}
	return Result{Drafts: drafts}, nil
}
