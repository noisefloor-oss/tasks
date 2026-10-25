//go:build linux

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/noisefloor-oss/tasks/internal/broker"
)

func TestPrivilegedNamespaceFDChannelCreatesInHostNamespace(t *testing.T) {
	testPrivilegedConfinedCreate(t, true)
}

func TestPrivilegedPID1ChannelCreatesInHostNamespace(t *testing.T) {
	testPrivilegedConfinedCreate(t, false)
}

func testPrivilegedConfinedCreate(t *testing.T, namespaceFD bool) {
	t.Helper()
	fx := newPrivFixture(t)
	fx.chownFamily(t)
	for _, name := range []string{"unshare", "mount", "setpriv"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("%s unavailable", name)
		}
	}

	doc, err := broker.Mint(fx.key, fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID))
	if err != nil {
		t.Fatal(err)
	}
	args := []string{
		"--mount", "--propagation", "private", "/bin/sh", "-c",
		`set -eu
mount -t tmpfs -o mode=0755 tmpfs "$1"
cd "$1"
shift
exec "$@"`,
		"sh", fx.boundary, "setpriv",
		"--reuid=" + strconv.Itoa(nobodyUID),
		"--regid=" + strconv.Itoa(nobodyGID),
		"--clear-groups", fx.binary, broker.VerbCreate,
	}
	cmd := exec.Command("unshare", args...)
	cmd.Env = hostileEnv
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if namespaceFD {
		hostNamespace, err := os.Open("/proc/self/ns/mnt")
		if err != nil {
			t.Skipf("host mount namespace unavailable: %v", err)
		}
		defer hostNamespace.Close()
		cmd.ExtraFiles = []*os.File{hostNamespace}
		cmd.Stdin = bytes.NewReader(doc)
	} else {
		capability, err := os.CreateTemp("", "broker-capability")
		if err != nil {
			t.Fatal(err)
		}
		defer os.Remove(capability.Name())
		if _, err := capability.Write(doc); err != nil {
			t.Fatal(err)
		}
		if _, err := capability.Seek(0, 0); err != nil {
			t.Fatal(err)
		}
		defer capability.Close()
		cmd.ExtraFiles = []*os.File{capability}
	}

	err = cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			if isNamespaceSetupRefusal(stderr.String()) {
				t.Skipf("kernel refused mount-namespace fixture: %s", strings.TrimSpace(stderr.String()))
			}
			t.Fatalf("confined create (fd3 namespace=%t): exit %d, stdout=%q stderr=%q", namespaceFD, ee.ExitCode(), stdout.String(), stderr.String())
		}
		t.Fatalf("start confined broker: %v", err)
	}
	if !strings.Contains(stdout.String(), `"changed":true`) || stderr.Len() != 0 {
		t.Fatalf("confined create output: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	target := fx.capFor(broker.VerbCreate, nobodyUID, nobodyGID).Target
	info, err := os.Stat(filepath.Join(target, "f.txt"))
	if err != nil {
		t.Fatalf("Git child did not create host worktree: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != nobodyUID || int(stat.Gid) != nobodyGID {
		t.Fatalf("host worktree was not created after privilege drop: %s", fmt.Sprint(info.Sys()))
	}
}

func isNamespaceSetupRefusal(stderr string) bool {
	return (strings.Contains(stderr, "unshare:") || strings.Contains(stderr, "mount:")) &&
		strings.Contains(stderr, "Operation not permitted")
}
