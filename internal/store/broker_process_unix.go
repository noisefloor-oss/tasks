//go:build unix

package store

import (
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const brokerCapabilityFD = 3

var (
	brokerCapabilityMu   sync.Mutex
	brokerHostCapability = captureBrokerCapability()
)

// configureBrokerProcess gives the broker its own process group so timeout
// cancellation kills descendants that inherited captured pipes. WaitDelay is
// a second bound in case a descendant escaped the group before cancellation.
func configureBrokerProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
}

// reapBrokerProcessGroup enforces the broker's no-daemon contract after every
// outcome, including a zero-exit broker that detached a redirected child. The
// direct child has already been waited by cmd.Run; any surviving process group
// therefore consists only of broker descendants.
func reapBrokerProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// openBrokerCapabilityFile duplicates the host-supplied fd-3 capability so the
// broker child receives it as its own fd 3 via ExtraFiles without consuming or
// closing the caller's descriptor. Absence means there is no available broker
// capability, which uses the pinned no-broker diagnostic.
func openBrokerCapabilityFile() (*os.File, error) {
	brokerCapabilityMu.Lock()
	defer brokerCapabilityMu.Unlock()
	if brokerHostCapability == nil {
		return nil, errWorktreeConfined
	}
	dup, err := unix.FcntlInt(brokerHostCapability.Fd(), unix.F_DUPFD_CLOEXEC, 100)
	if err != nil {
		return nil, errWorktreeConfined
	}
	return os.NewFile(uintptr(dup), "noise-worktree-broker-capability"), nil
}

// captureBrokerCapability snapshots the inherited host capability before any
// direct Git child can run. The private copy is CLOEXEC and the original fd 3 is
// marked CLOEXEC so only the deliberate broker ExtraFiles mapping can cross an
// exec boundary.
func captureBrokerCapability() *os.File {
	dup, err := unix.FcntlInt(uintptr(brokerCapabilityFD), unix.F_DUPFD_CLOEXEC, 100)
	if err != nil {
		return nil
	}
	if err := setCloseOnExec(brokerCapabilityFD); err != nil {
		_ = unix.Close(brokerCapabilityFD)
	}
	return os.NewFile(uintptr(dup), "noise-worktree-broker-host-capability")
}

func setCloseOnExec(fd int) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFD, flags|unix.FD_CLOEXEC)
	return err
}
