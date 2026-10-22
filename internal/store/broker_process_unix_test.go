//go:build unix

package store

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestBrokerSuccessReapsBackgroundDescendant covers the outcome the timeout
// tests do not: the broker exits zero immediately after starting a redirected
// child. A successful direct child must not turn that child into a daemon.
func TestBrokerSuccessReapsBackgroundDescendant(t *testing.T) {
	repo := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	marker := filepath.Join(t.TempDir(), "child-survived")
	script := "#!/bin/sh\n" +
		"(sleep 1; touch '" + marker + "') </dev/null >/dev/null 2>&1 &\n" +
		"printf '%s\\n' \"$!\" > '" + pidFile + "'\n" +
		"exit 0\n"
	fakeBroker(t, script)

	withBrokerCapabilityFD(t, "target\nbranch\nbase\n", func() {
		if err := delegateWorktreeBroker(repo, "create"); err != nil {
			t.Fatalf("zero-exit broker: %v", err)
		}
	})
	data, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("child pid %q: %v", data, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		err = syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("broker descendant %d still exists after successful broker exit: %v", pid, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("broker descendant survived long enough to create marker: %v", err)
	}
}

func TestReadOnlyRegistrationIsPermissionClass(t *testing.T) {
	err := &os.PathError{Op: "rename", Path: "/registration", Err: syscall.EROFS}
	if !permissionClass(err) {
		t.Fatalf("read-only registration error not classified as confinement: %v", err)
	}
}
