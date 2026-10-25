//go:build linux

package broker

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestIsMountNamespaceFD(t *testing.T) {
	namespace, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		t.Skipf("mount namespace handle unavailable: %v", err)
	}
	defer namespace.Close()
	if !IsMountNamespaceFD(int(namespace.Fd())) {
		t.Fatal("mount namespace descriptor was not recognized")
	}

	regular, err := os.CreateTemp(t.TempDir(), "regular")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if IsMountNamespaceFD(int(regular.Fd())) {
		t.Fatal("regular file was recognized as a mount namespace")
	}
	if err := TryEnterNamespaceFD(int(regular.Fd()), []byte("capability")); err == nil {
		t.Fatal("non-namespace descriptor entered the re-exec path")
	}
}

func TestStageCapabilityIsRewoundAndCloseOnExec(t *testing.T) {
	want := []byte("bounded capability\n")
	fd, err := stageCapability(want)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("auxiliary capability descriptor would leak through exec")
	}
	got := make([]byte, len(want)+1)
	n, err := unix.Read(fd, got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:n], want) {
		t.Fatalf("staged capability = %q, want %q", got[:n], want)
	}
}

func TestStageCapabilityForReexecReplacesConsumedPipe(t *testing.T) {
	want := []byte("capability from a pipe\n")
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		t.Fatal(err)
	}
	defer unix.Close(pipe[0])
	if _, err := unix.Write(pipe[1], want); err != nil {
		t.Fatal(err)
	}
	unix.Close(pipe[1])
	consumed := make([]byte, len(want))
	if _, err := unix.Read(pipe[0], consumed); err != nil {
		t.Fatal(err)
	}
	if err := StageCapabilityForReexec(pipe[0], want); err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(pipe[0]), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		t.Fatal("staged fd 3 must remain inheritable for the broker re-exec")
	}
	got := make([]byte, len(want)+1)
	n, err := unix.Read(pipe[0], got)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got[:n], want) {
		t.Fatalf("re-exec capability = %q, want %q", got[:n], want)
	}
}

func TestAuthenticateNamespaceUser(t *testing.T) {
	initial, err := namespaceIdentity("/proc/1/ns/user")
	if err != nil {
		t.Skipf("initial user namespace unavailable: %v", err)
	}
	current, err := namespaceIdentity("/proc/self/ns/user")
	if err != nil {
		t.Skipf("current user namespace unavailable: %v", err)
	}
	if !sameNamespaceIdentity(&initial, &current) {
		t.Skip("test process is not in PID 1's user namespace")
	}
	mountNamespace, err := os.Open("/proc/self/ns/mnt")
	if err != nil {
		t.Skipf("mount namespace unavailable: %v", err)
	}
	defer mountNamespace.Close()
	if err := authenticateNamespaceUser(int(mountNamespace.Fd())); err != nil {
		t.Fatalf("initial user namespace ownership refused: %v", err)
	}
}

func TestNamespaceIdentityMismatch(t *testing.T) {
	initial := unix.Stat_t{Dev: 10, Ino: 20}
	same := initial
	if !sameNamespaceIdentity(&initial, &same) {
		t.Fatal("identical namespace identity refused")
	}
	otherDevice := unix.Stat_t{Dev: 11, Ino: 20}
	otherInode := unix.Stat_t{Dev: 10, Ino: 21}
	if sameNamespaceIdentity(&initial, &otherDevice) || sameNamespaceIdentity(&initial, &otherInode) {
		t.Fatal("different user namespace identity accepted")
	}
}

func TestExecveatPinnedExecutable(t *testing.T) {
	const helper = "NOISE_BROKER_EXECVEAT_TEST_HELPER"
	switch os.Getenv(helper) {
	case "2":
		fmt.Fprint(os.Stdout, "pinned-execveat-ok")
		os.Exit(0)
	case "1":
		fd, err := openCurrentExecutable()
		if err != nil {
			t.Fatal(err)
		}
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
		if err != nil {
			t.Fatal(err)
		}
		if flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("pinned executable descriptor is not close-on-exec")
		}
		if err := execveat(fd,
			[]string{"broker-test", "-test.run=^TestExecveatPinnedExecutable$"},
			[]string{helper + "=2"},
		); err != nil {
			t.Fatalf("execveat pinned executable: %v", err)
		}
		return
	default:
		cmd := exec.Command(os.Args[0], "-test.run=^TestExecveatPinnedExecutable$")
		cmd.Env = []string{helper + "=1"}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pinned executable helper: %v: %s", err, out)
		}
		if !bytes.Contains(out, []byte("pinned-execveat-ok")) {
			t.Fatalf("pinned executable did not reach fresh process: %q", out)
		}
	}
}
