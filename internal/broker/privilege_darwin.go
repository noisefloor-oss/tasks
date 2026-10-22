//go:build darwin

package broker

import (
	"os"

	"golang.org/x/sys/unix"
)

// Darwin has no Linux mount namespaces. A capability document on fd 3 and a
// directly reachable registration root remain supported; namespace re-entry
// is refused fail-closed.
func EnterPID1MountNamespace() error {
	return refuse("mount namespace re-entry is unsupported")
}

func TryEnterNamespaceFD(int) error {
	return refuse("mount namespace re-entry is unsupported")
}

func DropPrivilege(uid, gid int) error {
	if os.Geteuid() == 0 {
		if err := unix.Setgroups(nil); err != nil {
			return refuse("cannot clear supplementary groups")
		}
		// For a privileged process Darwin's setgid/setuid set the real,
		// effective, and saved identities, making the drop irreversible.
		if err := unix.Setgid(gid); err != nil {
			return refuse("cannot drop group privilege")
		}
		if err := unix.Setuid(uid); err != nil {
			return refuse("cannot drop user privilege")
		}
	}
	return verifyDroppedIdentityDarwin(uid, gid)
}

func verifyDroppedIdentityDarwin(uid, gid int) error {
	if os.Getuid() != uid || os.Getgid() != gid || os.Geteuid() != uid || os.Getegid() != gid {
		return refuse("privilege drop did not take effect")
	}
	return nil
}
