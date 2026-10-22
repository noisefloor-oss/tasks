package genai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultCLITimeout = 5 * time.Minute
	cliWaitDelay      = 10 * time.Second
	maxCLIStdoutBytes = 8 << 20
	maxCLIStderrBytes = 64 << 10
	maxModelBytes     = 256
)

type boundedBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func newBoundedBuffer(limit int) *boundedBuffer {
	return &boundedBuffer{limit: limit}
}

func (b *boundedBuffer) Write(data []byte) (int, error) {
	written := len(data)
	remaining := b.limit - b.buffer.Len()
	if remaining > len(data) {
		remaining = len(data)
	}
	if remaining > 0 {
		_, _ = b.buffer.Write(data[:remaining])
	}
	if remaining < len(data) {
		b.overflow = true
	}
	return written, nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }

func runCLI(ctx context.Context, provider, executable, workingDirectory string, args []string, prompt string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = defaultCLITimeout
	}
	callContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	stdout := newBoundedBuffer(maxCLIStdoutBytes)
	stderr := newBoundedBuffer(maxCLIStderrBytes)
	command := exec.CommandContext(callContext, executable, args...)
	command.Dir = workingDirectory
	command.Stdin = strings.NewReader(prompt)
	command.Stdout = stdout
	command.Stderr = stderr
	command.WaitDelay = cliWaitDelay

	runErr := command.Run()
	if errors.Is(callContext.Err(), context.DeadlineExceeded) {
		return "", fmt.Errorf("%s timed out after %s", provider, timeout)
	}
	if ctx.Err() != nil {
		return "", fmt.Errorf("%s was cancelled", provider)
	}
	if stdout.overflow {
		return "", fmt.Errorf("%s stdout exceeded %d bytes", provider, maxCLIStdoutBytes)
	}
	if stderr.overflow {
		return "", fmt.Errorf("%s stderr exceeded %d bytes", provider, maxCLIStderrBytes)
	}
	if runErr != nil {
		var exitError *exec.ExitError
		if errors.As(runErr, &exitError) {
			return "", fmt.Errorf("%s failed with exit code %d", provider, exitError.ExitCode())
		}
		if errors.Is(runErr, exec.ErrWaitDelay) {
			return "", fmt.Errorf("%s did not stop within %s", provider, cliWaitDelay)
		}
		return "", fmt.Errorf("%s failed to start", provider)
	}
	return stdout.String(), nil
}

func validateModelArgument(model string, required bool) error {
	if model == "" && !required {
		return nil
	}
	if model == "" || strings.TrimSpace(model) != model {
		return errors.New("model must be nonempty and trimmed")
	}
	if !utf8.ValidString(model) {
		return errors.New("model must be valid UTF-8")
	}
	if len(model) > maxModelBytes {
		return fmt.Errorf("model exceeds %d bytes", maxModelBytes)
	}
	if strings.HasPrefix(model, "-") {
		return errors.New("model must not begin with '-'")
	}
	for _, value := range []byte(model) {
		if value < 0x20 || value == 0x7f {
			return errors.New("model contains an ASCII control character")
		}
	}
	return nil
}

// ValidateRequestedModel applies the shared adapter argument boundary. The
// required flag is false for adapters that support an ambient default model.
func ValidateRequestedModel(model string, required bool) error {
	return validateModelArgument(model, required)
}

func canonicalExecutable(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", errors.New("resolve CLI executable: invalid path")
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", errors.New("resolve CLI executable: unavailable")
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("resolve CLI executable: not a regular executable")
	}
	return canonical, nil
}
