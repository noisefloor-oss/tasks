//go:build linux

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitRegistrationRunDoesNotLeakBrokerCapabilityFD(t *testing.T) {
	log := filepath.Join(t.TempDir(), "git-fds.log")
	withBrokerCapabilityFDPath(t, "target\nbranch\nref\n", func(capPath string) {
		bin := t.TempDir()
		script := "#!/bin/sh\n" + procFDCountScript(log, capPath, "git") + "exit 0\n"
		if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		if _, err := gitRegistrationRun(t.TempDir(), "rev-parse", "--git-common-dir"); err != nil {
			t.Fatalf("fake git failed: %v", err)
		}
	})
	assertNoCapabilityFDLeak(t, log, "git")
}

func TestWorktreeCreateDirectHookDoesNotLeakBrokerCapabilityFD(t *testing.T) {
	db, repo := brokerTestRepo(t)
	target := filepath.Join(repo, ".worktrees", "main-1")
	log := filepath.Join(t.TempDir(), "hook-fds.log")
	doc := brokerCapabilityDoc(target, "tasks/main/1", "main")
	var err error
	withBrokerCapabilityFDPath(t, doc, func(capPath string) {
		hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
		script := "#!/bin/sh\nprintf 'hook-ran\\n' >> " + shellQuote(log) + "\n" +
			procFDCountScript(log, capPath, "hook")
		if writeErr := os.WriteFile(hook, []byte(script), 0o755); writeErr != nil {
			t.Fatal(writeErr)
		}
		_, err = db.CreateWorktree(repo, "main", "1", false)
	})
	if err != nil {
		t.Fatalf("direct create failed: %v", err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("post-checkout hook did not run: %v", err)
	}
	if !strings.Contains(string(data), "hook-ran\n") {
		t.Fatalf("post-checkout hook log missing marker:\n%s", data)
	}
	assertNoCapabilityFDLeak(t, log, "hook")
}

func TestDelegateWorktreeBrokerPassesOnlyOneCapabilityFD(t *testing.T) {
	repo := t.TempDir()
	log := filepath.Join(t.TempDir(), "broker-fds.log")
	withBrokerCapabilityFDPath(t, "target\nbranch\nref\n", func(capPath string) {
		fakeBroker(t, "#!/bin/sh\n"+
			procFDCountScript(log, capPath, "broker")+
			"IFS= read -r _target <&3 || exit 8\n"+
			"exit 0\n")
		if err := delegateWorktreeBroker(repo, "create"); err != nil {
			t.Fatalf("broker delegate failed: %v", err)
		}
	})
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "broker-cap-count:1\n") {
		t.Fatalf("broker capability fd count mismatch:\n%s", data)
	}
	if !strings.Contains(string(data), "broker-cap-fd:3\n") {
		t.Fatalf("broker capability was not delivered on fd 3:\n%s", data)
	}
}

func assertNoCapabilityFDLeak(t *testing.T, log, label string) {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), label+"-cap-fd:") {
		t.Fatalf("%s inherited broker capability fd:\n%s", label, data)
	}
	want := label + "-cap-count:0\n"
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s capability fd count mismatch, want %q:\n%s", label, want, data)
	}
}

func procFDCountScript(log, capPath, label string) string {
	return fmt.Sprintf(`count=0
for fd in /proc/$$/fd/*; do
	target=$(readlink "$fd" 2>/dev/null || true)
	if [ "$target" = %s ]; then
		count=$((count + 1))
		printf '%s-cap-fd:%%s\n' "${fd##*/}" >> %s
	fi
done
printf '%s-cap-count:%%s\n' "$count" >> %s
`, shellQuote(capPath), label, shellQuote(log), label, shellQuote(log))
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
