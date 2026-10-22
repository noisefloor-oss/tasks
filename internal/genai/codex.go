package genai

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Codex struct {
	Executable string
	Model      string
	Timeout    time.Duration
}

func NewCodex(executable, model string) (*Codex, error) {
	if err := validateModelArgument(model, false); err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	canonical, err := canonicalExecutable(executable)
	if err != nil {
		return nil, fmt.Errorf("codex: %w", err)
	}
	return &Codex{Executable: canonical, Model: model, Timeout: defaultCLITimeout}, nil
}

func (c *Codex) GenerateTasks(ctx context.Context, prdText string, n int) (Result, error) {
	n = clampDraftCount(n)
	if err := validateModelArgument(c.Model, false); err != nil {
		return Result{}, fmt.Errorf("codex: %w", err)
	}
	temp, err := os.MkdirTemp("", "noise-tasks-codex-")
	if err != nil {
		return Result{}, fmt.Errorf("codex: create temporary directory")
	}
	defer os.RemoveAll(temp)
	workingDirectory := filepath.Join(temp, "work")
	if err := os.Mkdir(workingDirectory, 0o700); err != nil {
		return Result{}, fmt.Errorf("codex: create working directory")
	}
	answerPath := filepath.Join(temp, "answer")
	args := []string{
		"exec", "-", "--sandbox", "read-only", "--skip-git-repo-check",
		"--ephemeral", "--color", "never", "-C", workingDirectory,
		"--output-last-message", answerPath,
		"--ignore-user-config", "--ignore-rules",
		"--disable", "shell_tool", "--disable", "apps", "--disable", "plugins",
		"--disable", "multi_agent", "--disable", "image_generation", "--disable", "view_image",
		"-c", `web_search="disabled"`,
	}
	if c.Model != "" {
		args = append(args, "-m", c.Model)
	}
	if _, err := runCLI(ctx, "codex", c.Executable, workingDirectory, args,
		draftSystemPrompt+"\n\n"+draftUserPrompt(n, prdText), c.Timeout); err != nil {
		return Result{}, err
	}
	// Stdout contains progress; the final message has its own output bound.
	output, err := readCodexAnswer(answerPath)
	if err != nil {
		return Result{}, err
	}
	drafts, err := finishDrafts(output, n)
	if err != nil {
		return Result{}, err
	}
	return Result{Drafts: drafts}, nil
}

func readCodexAnswer(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("codex: final answer is unavailable")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("codex: final answer is unavailable")
	}
	defer file.Close()
	output, err := io.ReadAll(io.LimitReader(file, maxCLIStdoutBytes+1))
	if err != nil {
		return "", fmt.Errorf("codex: read final answer failed")
	}
	if len(output) > maxCLIStdoutBytes {
		return "", fmt.Errorf("codex final answer exceeded %d bytes", maxCLIStdoutBytes)
	}
	return string(output), nil
}
