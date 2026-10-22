//go:build linux

package broker

import (
	"os"

	"golang.org/x/sys/unix"
)

// EnterPID1MountNamespace re-enters PID 1's mount namespace — one of the
// two permitted re-entry channels. It requires privileges and is attempted
// only before the privilege drop.
func EnterPID1MountNamespace() error {
	fd, err := unix.Open("/proc/1/ns/mnt", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return refuse("cannot open pid 1 mount namespace")
	}
	defer unix.Close(fd)
	if err := unix.Setns(fd, unix.CLONE_NEWNS); err != nil {
		return refuse("cannot enter pid 1 mount namespace")
	}
	return nil
}

// TryEnterNamespaceFD attempts namespace re-entry through an inherited
// descriptor — the alternative authenticated fd-3 channel, in which the
// host passes its own mount namespace as fd 3 and the capability document
// on stdin.
func TryEnterNamespaceFD(fd int) error {
	if err := unix.Setns(fd, unix.CLONE_NEWNS); err != nil {
		return refuse("inherited namespace descriptor is invalid")
	}
	return nil
}

// DropPrivilege permanently drops to uid/gid: supplementary groups are
// cleared and all three copies of each id are set, so the drop is
// irreversible. An unprivileged caller must already run as the bound
// identity, which invocation checks enforce.
func DropPrivilege(uid, gid int) error {
	if os.Geteuid() == 0 {
		if err := unix.Setgroups(nil); err != nil {
			return refuse("cannot clear supplementary groups")
		}
		if err := unix.Setresgid(gid, gid, gid); err != nil {
			return refuse("cannot drop group privilege")
		}
		if err := unix.Setresuid(uid, uid, uid); err != nil {
			return refuse("cannot drop user privilege")
		}
	}
	return verifyDroppedIdentity(uid, gid)
}

func verifyDroppedIdentity(uid, gid int) error {
	if os.Getuid() != uid || os.Getgid() != gid || os.Geteuid() != uid || os.Getegid() != gid {
		return refuse("privilege drop did not take effect")
	}
	return nil
}
