//go:build unix

package store

import (
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func withBrokerCapabilityFD(t *testing.T, doc string, fn func()) {
	t.Helper()
	withBrokerCapabilityFDPath(t, doc, func(_ string) { fn() })
}

func withBrokerCapabilityFDPath(t *testing.T, doc string, fn func(path string)) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "broker-capability")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(doc); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	if f.Fd() == uintptr(brokerCapabilityFD) {
		dup, err := unix.FcntlInt(f.Fd(), unix.F_DUPFD_CLOEXEC, 100)
		if err != nil {
			t.Skipf("cannot move capability descriptor away from fd 3: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		f = os.NewFile(uintptr(dup), "broker-capability")
	}
	defer f.Close()

	saved, err := unix.FcntlInt(uintptr(brokerCapabilityFD), unix.F_DUPFD_CLOEXEC, 100)
	hadFD3 := err == nil
	if !hadFD3 && err != unix.EBADF {
		t.Skipf("cannot inspect fd 3: %v", err)
	}
	if err := unix.Dup2(int(f.Fd()), brokerCapabilityFD); err != nil {
		t.Skipf("cannot place capability on fd 3: %v", err)
	}
	private := captureBrokerCapability()
	if private == nil {
		t.Skip("cannot capture broker capability from fd 3")
	}
	brokerCapabilityMu.Lock()
	oldCapability := brokerHostCapability
	brokerHostCapability = private
	brokerCapabilityMu.Unlock()
	defer func() {
		brokerCapabilityMu.Lock()
		current := brokerHostCapability
		brokerHostCapability = oldCapability
		brokerCapabilityMu.Unlock()
		if current != nil {
			_ = current.Close()
		}
		_ = unix.Close(brokerCapabilityFD)
		if hadFD3 {
			_ = unix.Dup2(saved, brokerCapabilityFD)
			_ = setCloseOnExec(brokerCapabilityFD)
			_ = unix.Close(saved)
		}
	}()
	fn(f.Name())
}

func withoutBrokerCapabilityFD(t *testing.T, fn func()) {
	t.Helper()
	saved, err := unix.FcntlInt(uintptr(brokerCapabilityFD), unix.F_DUPFD_CLOEXEC, 100)
	hadFD3 := err == nil
	if !hadFD3 && err != unix.EBADF {
		t.Skipf("cannot inspect fd 3: %v", err)
	}
	if hadFD3 {
		_ = unix.Close(brokerCapabilityFD)
		defer func() {
			_ = unix.Dup2(saved, brokerCapabilityFD)
			_ = setCloseOnExec(brokerCapabilityFD)
			_ = unix.Close(saved)
		}()
	}
	brokerCapabilityMu.Lock()
	oldCapability := brokerHostCapability
	brokerHostCapability = nil
	brokerCapabilityMu.Unlock()
	defer func() {
		brokerCapabilityMu.Lock()
		brokerHostCapability = oldCapability
		brokerCapabilityMu.Unlock()
	}()
	fn()
}
