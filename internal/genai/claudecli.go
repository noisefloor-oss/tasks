package genai

import (
	"context"
	"fmt"
	"time"
)

// ClaudeCLI drives task drafting through a locally installed,
// already-authenticated claude CLI instead of the HTTP API — no
// ANTHROPIC_API_KEY needed. The headless invocation is
//
//	claude -p --output-format text [--model MODEL] --no-session-persistence --safe-mode --tools ""
//
// with the shared drafting prompt piped on stdin and the model's plain
// text reply read from stdout. Parsing and validation are the same
// shared path the API driver uses.
type ClaudeCLI struct {
	Executable       string
	Model            string
	WorkingDirectory string
	Timeout          time.Duration
}

// NewClaudeCLIConfigured builds a driver from a caller-selected executable and
// model. It reads no selector environment; authentication remains CLI-managed.
func NewClaudeCLIConfigured(executable, model, workingDirectory string) (*ClaudeCLI, error) {
	if err := validateModelArgument(model, false); err != nil {
		return nil, fmt.Errorf("claude-cli: %w", err)
	}
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return nil, fmt.Errorf("claude-cli: %w", err)
	}
	return &ClaudeCLI{
		Executable:       canonical,
		Model:            model,
		WorkingDirectory: workingDirectory,
		Timeout:          defaultCLITimeout,
	}, nil
}

// GenerateTasks runs the claude CLI once with the shared drafting
// prompt and parses the reply through the same defensive path as the
// API driver.
func (c *ClaudeCLI) GenerateTasks(ctx context.Context, prdText string, n int) (Result, error) {
	n = clampDraftCount(n)
	if err := validateModelArgument(c.Model, false); err != nil {
		return Result{}, fmt.Errorf("claude-cli: %w", err)
	}
	args := []string{"-p", "--output-format", "text"}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	args = append(args, "--no-session-persistence", "--safe-mode", "--tools", "")
	output, err := runCLI(ctx, "claude-cli", c.Executable, c.WorkingDirectory, args,
		draftSystemPrompt+"\n\n"+draftUserPrompt(n, prdText), c.Timeout)
	if err != nil {
		return Result{}, err
	}
	drafts, err := finishDrafts(output, n)
	if err != nil {
		return Result{}, err
	}
	return Result{Drafts: drafts}, nil
}
