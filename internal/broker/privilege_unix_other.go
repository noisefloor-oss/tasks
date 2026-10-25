//go:build unix && !linux && !darwin

package broker

import (
	"os"

	"golang.org/x/sys/unix"
)

func EnterPID1MountNamespace() error {
	return refuse("mount namespace re-entry is unsupported")
}

func AlreadyInPID1MountNamespace() bool { return false }

func IsMountNamespaceFD(int) bool { return false }

func StageCapabilityForReexec(int, []byte) error {
	return refuse("mount namespace re-entry is unsupported")
}

func TryEnterNamespaceFD(int, []byte) error {
	return refuse("mount namespace re-entry is unsupported")
}

func DropPrivilege(uid, gid int) error {
	if os.Geteuid() == 0 {
		if err := unix.Setgroups(nil); err != nil {
			return refuse("cannot clear supplementary groups")
		}
		if err := unix.Setgid(gid); err != nil {
			return refuse("cannot drop group privilege")
		}
		if err := unix.Setuid(uid); err != nil {
			return refuse("cannot drop user privilege")
		}
	}
	if os.Getuid() != uid || os.Getgid() != gid || os.Geteuid() != uid || os.Getegid() != gid {
		return refuse("privilege drop did not take effect")
	}
	return nil
}
