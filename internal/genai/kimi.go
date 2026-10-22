package genai

import (
	"context"
	"fmt"
	"os"
	"time"
)

type Kimi struct {
	Executable string
	Model      string
	Timeout    time.Duration
}

func NewKimi(executable, model string) (*Kimi, error) {
	if err := validateModelArgument(model, false); err != nil {
		return nil, fmt.Errorf("kimi: %w", err)
	}
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return nil, fmt.Errorf("kimi: %w", err)
	}
	return &Kimi{Executable: canonical, Model: model, Timeout: defaultCLITimeout}, nil
}

func (k *Kimi) GenerateTasks(ctx context.Context, prdText string, n int) (Result, error) {
	n = clampDraftCount(n)
	if err := validateModelArgument(k.Model, false); err != nil {
		return Result{}, fmt.Errorf("kimi: %w", err)
	}
	// Kimi has no no-tools switch; avoid discovering project files or sessions.
	workingDirectory, err := os.MkdirTemp("", "noise-tasks-kimi-")
	if err != nil {
		return Result{}, fmt.Errorf("kimi: create temporary directory")
	}
	defer os.RemoveAll(workingDirectory)
	args := []string{"-p", draftSystemPrompt + "\n\n" + draftUserPrompt(n, prdText), "--output-format", "text"}
	if k.Model != "" {
		args = append(args, "-m", k.Model)
	}
	output, err := runCLI(ctx, "kimi", k.Executable, workingDirectory, args, "", k.Timeout)
	if err != nil {
		return Result{}, err
	}
	drafts, err := finishDrafts(output, n)
	if err != nil {
		return Result{}, err
	}
	return Result{Drafts: drafts}, nil
}
