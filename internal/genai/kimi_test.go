package genai

import (
	"context"
	"strings"
	"testing"
)

func TestKimiGenerateTasksProtocol(t *testing.T) {
	for _, model := range []string{"", "kimi-k2.5"} {
		t.Run("model="+model, func(t *testing.T) {
			records := t.TempDir()
			executable := stubDraftCLI(t, "kimi", `
printf '<%s>\n' "$@" > "`+records+`/args"
pwd > "`+records+`/cwd"
ls -A > "`+records+`/entries"
cat > "`+records+`/stdin"
printf '%s' '`+"```json\n"+validDraftsJSON+"\n```"+`'
`)
			provider, err := NewKimi(executable, model)
			if err != nil {
				t.Fatal(err)
			}
			if provider.Timeout != defaultCLITimeout {
				t.Fatalf("timeout = %s", provider.Timeout)
			}
			result, err := provider.GenerateTasks(context.Background(), "the PRD text", 2)
			if err != nil || len(result.Drafts) != 2 || result.ResolvedModel != "" {
				t.Fatalf("result = %+v, %v", result, err)
			}
			assertTemporaryWorkingDirectory(t, records)
			args := []string{"-p", draftSystemPrompt + "\n\n" + draftUserPrompt(2, "the PRD text"), "--output-format", "text"}
			if model != "" {
				args = append(args, "-m", model)
			}
			wantArgs := "<" + strings.Join(args, ">\n<") + ">\n"
			if got := recordedCLIFile(t, records, "args"); got != wantArgs {
				t.Fatalf("argv = %q, want %q", got, wantArgs)
			}
			if got := recordedCLIFile(t, records, "stdin"); got != "" {
				t.Fatalf("stdin = %q, want empty", got)
			}
		})
	}
}

func TestKimiRejectsInvalidDrafts(t *testing.T) {
	for _, reply := range []string{"not JSON", "[]", `[{"title":"a","dependencies":[1]}]`} {
		executable := stubDraftCLI(t, "kimi", "printf '%s' '"+reply+"'")
		provider, err := NewKimi(executable, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.GenerateTasks(context.Background(), "prd", 3); err == nil {
			t.Fatalf("accepted reply %q", reply)
		}
	}
}
