package genai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func stubOMP(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "omp-stub")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOMPGenerateTasksUsesExactBoundedProtocol(t *testing.T) {
	records := t.TempDir()
	script := `printf '<%s>\n' "$@" > "` + records + `/args"
pwd > "` + records + `/cwd"
cat > "` + records + `/stdin"
printf '%s' '` + validDraftsJSON + `'
`
	executable := stubOMP(t, script)
	provider, err := NewOMP(executable, "zai/glm-5.2", records)
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.GenerateTasks(context.Background(), "the PRD text", 3)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "<-p>\n<--mode>\n<text>\n<--no-tools>\n<--no-session>\n<--no-skills>\n<--no-rules>\n<--no-extensions>\n<--model>\n<zai/glm-5.2>"
	args, err := os.ReadFile(filepath.Join(records, "args"))
	if err != nil || strings.TrimSpace(string(args)) != wantArgs {
		t.Fatalf("argv = %q, %v; want %q", args, err, wantArgs)
	}
	stdin, err := os.ReadFile(filepath.Join(records, "stdin"))
	if err != nil || !strings.Contains(string(stdin), "STRICT JSON") ||
		!strings.Contains(string(stdin), "at most 3 tasks") || !strings.Contains(string(stdin), "the PRD text") {
		t.Fatalf("stdin = %q, %v", stdin, err)
	}
	cwd, err := os.ReadFile(filepath.Join(records, "cwd"))
	if err != nil || strings.TrimSpace(string(cwd)) != records {
		t.Fatalf("cwd = %q, %v", cwd, err)
	}
	if len(result.Drafts) != 3 || result.ResolvedModel != "" {
		t.Fatalf("result = %+v", result)
	}
}

func TestOMPRejectsUnsafeModel(t *testing.T) {
	executable := stubOMP(t, "exit 0")
	for _, model := range []string{"", " model ", "--danger", "line\nbreak", strings.Repeat("m", maxModelBytes+1)} {
		if _, err := NewOMP(executable, model, t.TempDir()); err == nil {
			t.Errorf("model %q was accepted", model)
		}
	}
}

func TestOMPSanitizesFailureAndBoundsOutput(t *testing.T) {
	t.Run("nonzero stderr", func(t *testing.T) {
		executable := stubOMP(t, "echo 'credential-shaped-secret' >&2\nexit 7")
		provider, _ := NewOMP(executable, "model", t.TempDir())
		_, err := provider.GenerateTasks(context.Background(), "prd", 1)
		if err == nil || !strings.Contains(err.Error(), "exit code 7") || strings.Contains(err.Error(), "credential-shaped-secret") {
			t.Fatalf("sanitized error = %v", err)
		}
	})
	t.Run("stdout overflow", func(t *testing.T) {
		executable := stubOMP(t, "dd if=/dev/zero bs=1048576 count=9 2>/dev/null")
		provider, _ := NewOMP(executable, "model", t.TempDir())
		_, err := provider.GenerateTasks(context.Background(), "prd", 1)
		if err == nil || !strings.Contains(err.Error(), "stdout exceeded") {
			t.Fatalf("stdout error = %v", err)
		}
	})
	t.Run("stderr overflow", func(t *testing.T) {
		executable := stubOMP(t, "head -c 65537 /dev/zero >&2")
		provider, _ := NewOMP(executable, "model", t.TempDir())
		_, err := provider.GenerateTasks(context.Background(), "prd", 1)
		if err == nil || !strings.Contains(err.Error(), "stderr exceeded") {
			t.Fatalf("stderr error = %v", err)
		}
	})
}

func TestOMPTimeoutAndCancellation(t *testing.T) {
	executable := stubOMP(t, "exec sleep 30")
	provider, _ := NewOMP(executable, "model", t.TempDir())
	provider.Timeout = 100 * time.Millisecond
	if _, err := provider.GenerateTasks(context.Background(), "prd", 1); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}

	provider.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.GenerateTasks(ctx, "prd", 1); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancellation error = %v", err)
	}
}
